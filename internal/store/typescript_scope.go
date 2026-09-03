package store

import (
	"context"
	"path"
	"sort"
	"strconv"
	"strings"
)

const (
	tsScopeVeto       = "tmp_typescript_scope_veto"
	tsScopeResolution = "tmp_typescript_scope_resolution"
	tsScopeStrategy   = "typescript_module_scope"
)

type tsScopeFile struct {
	id   int64
	path string
}
type tsScopeImport struct {
	file                                    int64
	source, imported, local, kind           string
	wildcard, reexport, namespace, typeOnly bool
}
type tsScopeSymbol struct {
	id, file               int64
	name, visibility, kind string
}
type tsScopeExport struct {
	symbols   []int64
	namespace int64
}

// resolveTypeScriptScope is the sole TS/JS implicit resolver. It loads all
// syntax evidence once, but applies only the requested edge set. Unsupported
// or ambiguous module decisions are vetoed before generic name strategies.
func resolveTypeScriptScope(ctx context.Context, q execQuerier, repoID int64, only map[int64]struct{}) (int, error) {
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+tsScopeVeto+`(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM `+tsScopeVeto); err != nil {
		return 0, err
	}
	where, args := "", []any{repoID}
	if len(only) > 0 {
		ids := sortedIDs(only)
		where = ` AND e.id IN (` + tsPlaceholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT e.id,e.file_id,e.dst_name FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.language='typescript' AND e.dst_symbol_id IS NULL`+where, args...)
	if err != nil {
		return 0, err
	}
	type edge struct {
		id, file int64
		name     string
	}
	var edges []edge
	for rows.Next() {
		var e edge
		if err := rows.Scan(&e.id, &e.file, &e.name); err != nil {
			rows.Close()
			return 0, err
		}
		edges = append(edges, e)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(edges) == 0 {
		return 0, nil
	}
	for start := 0; start < len(edges); start += 400 {
		end := start + 400
		if end > len(edges) {
			end = len(edges)
		}
		v := make([]any, 0, end-start)
		for _, e := range edges[start:end] {
			v = append(v, e.id)
		}
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO `+tsScopeVeto+`(edge_id) VALUES `+valuePlaceholders(end-start), v...); err != nil {
			return 0, err
		}
	}

	files := map[int64]tsScopeFile{}
	byPath := map[string]int64{}
	rows, err = q.QueryContext(ctx, `SELECT id,path FROM files WHERE repo_id=? AND is_deleted=0`, repoID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var f tsScopeFile
		if err := rows.Scan(&f.id, &f.path); err != nil {
			rows.Close()
			return 0, err
		}
		f.path = canonicalStoredPath(f.path)
		files[f.id] = f
		byPath[f.path] = f.id
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	symbols := map[int64]tsScopeSymbol{}
	byFileName := map[int64]map[string][]int64{}
	rows, err = q.QueryContext(ctx, `SELECT s.id,s.file_id,s.name,s.visibility,s.kind FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND s.language='typescript' AND f.is_deleted=0`, repoID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var s tsScopeSymbol
		if err := rows.Scan(&s.id, &s.file, &s.name, &s.visibility, &s.kind); err != nil {
			rows.Close()
			return 0, err
		}
		symbols[s.id] = s
		if byFileName[s.file] == nil {
			byFileName[s.file] = map[string][]int64{}
		}
		byFileName[s.file][s.name] = append(byFileName[s.file][s.name], s.id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	imports := map[int64][]tsScopeImport{}
	rows, err = q.QueryContext(ctx, `SELECT file_id,source_specifier,imported_name,local_name,import_kind,wildcard,is_reexport,is_namespace_export,is_type_only FROM scope_import_evidence WHERE repo_id=? AND language='typescript'`, repoID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var i tsScopeImport
		var w, r, n, t int
		if err := rows.Scan(&i.file, &i.source, &i.imported, &i.local, &i.kind, &w, &r, &n, &t); err != nil {
			rows.Close()
			return 0, err
		}
		i.wildcard = w != 0
		i.reexport = r != 0
		i.namespace = n != 0
		i.typeOnly = t != 0
		imports[i.file] = append(imports[i.file], i)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	resolveModule := func(from int64, spec string) (int64, bool) {
		if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
			return 0, false
		}
		base := path.Clean(path.Join(path.Dir(files[from].path), spec))
		if strings.HasPrefix(base, "../") || base == ".." {
			return 0, false
		}
		if path.Ext(base) != "" {
			id, ok := byPath[base]
			return id, ok
		}
		var id int64
		n := 0
		for _, ext := range []string{".ts", ".tsx"} {
			if x, ok := byPath[base+ext]; ok {
				id = x
				n++
			}
		}
		return id, n == 1
	}
	var export func(int64, string, map[string]bool) tsScopeExport
	export = func(mod int64, name string, seen map[string]bool) tsScopeExport {
		if name == "default" {
			return tsScopeExport{}
		}
		key := intKey(mod, name)
		if seen[key] {
			return tsScopeExport{}
		}
		seen[key] = true
		var explicit []tsScopeImport
		var stars []tsScopeImport
		for _, i := range imports[mod] {
			if !i.reexport {
				continue
			}
			if i.wildcard && i.namespace && i.local == name {
				explicit = append(explicit, i)
			} else if i.wildcard {
				stars = append(stars, i)
			} else if i.local == name {
				explicit = append(explicit, i)
			}
		}
		collect := func(list []tsScopeImport) tsScopeExport {
			var out tsScopeExport
			for _, i := range list {
				if i.source == "" {
					for _, id := range byFileName[mod][i.imported] {
						out.symbols = append(out.symbols, id)
					}
					continue
				}
				target, ok := resolveModule(mod, i.source)
				if !ok {
					continue
				}
				if i.namespace && i.wildcard {
					out.namespace = target
					continue
				}
				x := export(target, i.imported, cloneSeen(seen))
				out.symbols = append(out.symbols, x.symbols...)
				if x.namespace != 0 {
					out.namespace = x.namespace
				}
			}
			return uniqueExport(out)
		}
		if len(explicit) > 0 {
			return collect(explicit)
		}
		var direct tsScopeExport
		for _, id := range byFileName[mod][name] {
			if symbols[id].visibility == "public" {
				direct.symbols = append(direct.symbols, id)
			}
		}
		direct = uniqueExport(direct)
		if len(direct.symbols) > 0 {
			return direct
		}
		return collect(stars)
	}
	results := map[int64]int64{}
	for _, e := range edges {
		var target tsScopeExport
		parts := strings.Split(e.name, ".")
		if len(parts) > 1 {
			local := parts[0]
			member := strings.Join(parts[1:], ".")
			for _, i := range imports[e.file] {
				if i.local == local && !i.typeOnly && i.kind == "namespace" {
					if m, ok := resolveModule(e.file, i.source); ok {
						target = export(m, member, map[string]bool{})
					}
				}
				if i.local == local && !i.typeOnly && i.kind != "namespace" && i.kind != "side_effect" {
					if m, ok := resolveModule(e.file, i.source); ok {
						target = export(m, i.imported, map[string]bool{})
						if target.namespace != 0 {
							target = export(target.namespace, member, map[string]bool{})
						}
					}
				}
			}
			if target.namespace != 0 {
				target = export(target.namespace, member, map[string]bool{})
			}
		} else {
			hasBindingEvidence := false
			for _, i := range imports[e.file] {
				if i.local != parts[0] || i.kind == "side_effect" || i.typeOnly {
					if i.local == parts[0] && i.kind != "side_effect" {
						hasBindingEvidence = true
					}
					continue
				}
				hasBindingEvidence = true
				m, ok := resolveModule(e.file, i.source)
				if !ok {
					continue
				}
				target = export(m, i.imported, map[string]bool{})
				if len(target.symbols) > 0 || target.namespace != 0 {
					break
				}
			}
			if len(target.symbols) == 0 && target.namespace == 0 && !hasBindingEvidence {
				for _, id := range byFileName[e.file][parts[0]] {
					target.symbols = append(target.symbols, id)
				}
			}
		}
		target = uniqueExport(target)
		if len(target.symbols) == 1 {
			results[e.id] = target.symbols[0]
		}
	}
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+tsScopeResolution+`(edge_id INTEGER PRIMARY KEY,dst_symbol_id INTEGER NOT NULL) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM `+tsScopeResolution); err != nil {
		return 0, err
	}
	if len(results) > 0 {
		ids := make([]int64, 0, len(results))
		for id := range results {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		vals := make([]any, 0, len(ids)*2)
		for _, id := range ids {
			vals = append(vals, id, results[id])
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO `+tsScopeResolution+`(edge_id,dst_symbol_id) VALUES `+pairPlaceholders(len(ids)), vals...); err != nil {
			return 0, err
		}
		if _, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=(SELECT dst_symbol_id FROM `+tsScopeResolution+` r WHERE r.edge_id=edges.id),resolution_strategy='`+tsScopeStrategy+`',resolution_confidence='high' WHERE id IN (SELECT edge_id FROM `+tsScopeResolution+`)`); err != nil {
			return 0, err
		}
	}
	return len(results), nil
}

func uniqueExport(x tsScopeExport) tsScopeExport {
	seen := map[int64]bool{}
	out := x.symbols[:0]
	for _, id := range x.symbols {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	x.symbols = out
	return x
}
func intKey(id int64, n string) string { return strconv.FormatInt(id, 10) + "\x00" + n }
func cloneSeen(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}
func sortedIDs(m map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
func tsPlaceholders(n int) string { return strings.TrimRight(strings.Repeat("?,", n), ",") }
func valuePlaceholders(n int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = "(?)"
	}
	return strings.Join(p, ",")
}
func pairPlaceholders(n int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = "(?,?)"
	}
	return strings.Join(p, ",")
}

// invalidateTypeScriptScopeBindings clears the dependent module component for
// changed source files. The component is built from persisted relative import
// evidence, so removed or renamed exports cannot leave an old binding behind.
func (s *Store) invalidateTypeScriptScopeBindings(ctx context.Context, repoID int64, paths []string) ([]string, error) {
	changed, err := fileIDsByPaths(ctx, s.db, repoID, paths)
	if err != nil || len(changed) == 0 {
		return nil, err
	}
	files := map[int64]tsScopeFile{}
	byPath := map[string]int64{}
	rows, err := s.db.QueryContext(ctx, `SELECT id,path FROM files WHERE repo_id=? AND language='typescript' AND is_deleted=0`, repoID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f tsScopeFile
		if err := rows.Scan(&f.id, &f.path); err != nil {
			rows.Close()
			return nil, err
		}
		f.path = canonicalStoredPath(f.path)
		files[f.id] = f
		byPath[f.path] = f.id
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	reverse := map[int64][]int64{}
	rows, err = s.db.QueryContext(ctx, `SELECT file_id,source_specifier FROM scope_import_evidence WHERE repo_id=? AND language='typescript' AND (source_specifier LIKE './%' OR source_specifier LIKE '../%')`, repoID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var file int64
		var spec string
		if err := rows.Scan(&file, &spec); err != nil {
			rows.Close()
			return nil, err
		}
		base := path.Clean(path.Join(path.Dir(files[file].path), spec))
		if path.Ext(base) == "" {
			for _, ext := range []string{".ts", ".tsx"} {
				if id, ok := byPath[base+ext]; ok {
					reverse[id] = append(reverse[id], file)
				}
			}
		} else if id, ok := byPath[base]; ok {
			reverse[id] = append(reverse[id], file)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	affected := map[int64]struct{}{}
	queue := append([]int64(nil), changed...)
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		if _, ok := affected[m]; ok {
			continue
		}
		affected[m] = struct{}{}
		for _, viewer := range reverse[m] {
			queue = append(queue, viewer)
		}
	}
	ids := make([]int64, 0, len(affected))
	for id := range affected {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	args := make([]any, 0, len(ids)+2)
	args = append(args, repoID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT e.id,e.dst_name FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.language='typescript' AND e.edge_kind <> 'cross_language_ref' AND e.file_id IN (`+tsPlaceholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	var edgeIDs []int64
	names := map[string]struct{}{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, err
		}
		edgeIDs = append(edgeIDs, id)
		if name != "" {
			names[name] = struct{}{}
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := clearEdgeResolutions(ctx, s.db, edgeIDs); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

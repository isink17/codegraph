package store

import (
	"context"
	"database/sql"
	"path"
	"sort"
	"strconv"
	"strings"
)

const (
	tsScopeVeto       = "tmp_typescript_scope_veto"
	tsScopeResolution = "tmp_typescript_scope_resolution"
	tsScopeStrategy   = "typescript_module_scope"

	// tsScopeResolutionBatchSize caps the multi-row resolution INSERT. Each row
	// binds two parameters, so rows*2 stays under sqliteDefaultMaxVariables.
	tsScopeResolutionBatchSize = sqliteInClauseBatchSize / 2

	// tsScopeVetoBatchSize caps the multi-row veto INSERT, which binds one
	// parameter per row and carries no fixed parameters.
	tsScopeVetoBatchSize = sqliteInClauseBatchSize
)

// tsScopeBatchSize is the number of dynamic parameters a single TypeScript
// scope statement may bind once its fixed parameters are accounted for.
func tsScopeBatchSize(fixedArgs int) int { return sqliteInClauseBatchSize - fixedArgs }

// tsScopeBatchedQuery runs one statement per batch of dynamic parameters so no
// statement ever binds more than sqliteDefaultMaxVariables arguments, and feeds
// every row of every batch to scan. base is the statement without its IN
// clause; clause is appended for a filtered pass and must contain a single %s
// for the placeholder list. An unfiltered pass (filtered=false) runs base once
// with the fixed arguments alone, and a filtered pass over an empty set runs
// nothing at all -- neither ever emits "IN ()".
//
// The accumulated rows are exactly those one hypothetical unlimited statement
// would return: batches are deterministic slices of a caller-sorted set, and
// every batch is read before the caller acts on the result.
func tsScopeBatchedQuery(
	ctx context.Context,
	q queryContexter,
	base, clause string,
	fixed, items []any,
	filtered bool,
	scan func(*sql.Rows) error,
) error {
	batches := [][]any{nil}
	if filtered {
		batches = nil
		size := tsScopeBatchSize(len(fixed))
		for start := 0; start < len(items); start += size {
			batches = append(batches, items[start:min(start+size, len(items))])
		}
	}
	for _, batch := range batches {
		query := base
		if filtered {
			query += strings.Replace(clause, "%s", tsPlaceholders(len(batch)), 1)
		}
		args := make([]any, 0, len(fixed)+len(batch))
		args = append(args, fixed...)
		args = append(args, batch...)
		if err := tsScopeScanRows(ctx, q, query, args, scan); err != nil {
			return err
		}
	}
	return nil
}

func tsScopeScanRows(ctx context.Context, q queryContexter, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// tsScopeIDArgs renders a sorted id set as bind arguments.
func tsScopeIDArgs(ids map[int64]struct{}) []any {
	return int64SliceToAny(sortedIDs(ids))
}

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
	id, file                      int64
	name, qname, visibility, kind string
}
type tsScopeExport struct {
	symbols   []int64
	namespace int64
}

func typescriptModuleCandidatePaths(sourceFile, specifier string) []string {
	if !strings.HasPrefix(specifier, "./") && !strings.HasPrefix(specifier, "../") {
		return nil
	}
	if strings.HasSuffix(specifier, "/") {
		return nil
	}
	base := path.Clean(path.Join(path.Dir(canonicalStoredPath(sourceFile)), specifier))
	if base == "." || base == ".." || strings.HasPrefix(base, "../") {
		return nil
	}
	switch strings.ToLower(path.Ext(base)) {
	case "":
		return []string{base + ".ts", base + ".tsx"}
	case ".ts", ".tsx", ".js", ".jsx", ".mjs":
		return []string{base}
	default:
		return nil
	}
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
	type edge struct {
		id, file int64
		name     string
	}
	var edges []edge
	if err := tsScopeBatchedQuery(ctx, q,
		`SELECT e.id,e.file_id,e.dst_name FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.language='typescript' AND e.dst_symbol_id IS NULL`,
		` AND e.id IN (%s)`, []any{repoID}, tsScopeIDArgs(only), len(only) > 0,
		func(rows *sql.Rows) error {
			var e edge
			if err := rows.Scan(&e.id, &e.file, &e.name); err != nil {
				return err
			}
			edges = append(edges, e)
			return nil
		}); err != nil {
		return 0, err
	}
	if len(edges) == 0 {
		return 0, nil
	}
	for start := 0; start < len(edges); start += tsScopeVetoBatchSize {
		end := min(start+tsScopeVetoBatchSize, len(edges))
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
	fileIDs := make(map[int64]struct{})
	if len(only) > 0 {
		for _, e := range edges {
			fileIDs[e.file] = struct{}{}
		}
	}
	if err := tsScopeBatchedQuery(ctx, q,
		`SELECT id,path FROM files WHERE repo_id=? AND is_deleted=0`, ` AND id IN (%s)`,
		[]any{repoID}, tsScopeIDArgs(fileIDs), len(fileIDs) > 0,
		func(rows *sql.Rows) error {
			var f tsScopeFile
			if err := rows.Scan(&f.id, &f.path); err != nil {
				return err
			}
			f.path = canonicalStoredPath(f.path)
			files[f.id] = f
			byPath[f.path] = f.id
			return nil
		}); err != nil {
		return 0, err
	}
	// A scoped pass grows from the selected caller files through relative
	// module evidence. This keeps files outside the affected module component
	// out of memory and out of the candidate population.
	if len(only) > 0 {
		for {
			frontier := tsScopeIDArgs(fileIDs)
			clear(fileIDs)
			if len(frontier) == 0 {
				break
			}
			// The whole frontier level is read before any candidate is
			// resolved, so a SQL batch boundary never becomes a graph
			// traversal boundary.
			candidatePaths := map[string]struct{}{}
			if err := tsScopeBatchedQuery(ctx, q,
				`SELECT file_id,source_specifier FROM scope_import_evidence WHERE repo_id=? AND language='typescript'`,
				` AND file_id IN (%s)`, []any{repoID}, frontier, true,
				func(rows *sql.Rows) error {
					var file int64
					var spec string
					if err := rows.Scan(&file, &spec); err != nil {
						return err
					}
					from, ok := files[file]
					if !ok {
						return nil
					}
					for _, candidate := range typescriptModuleCandidatePaths(from.path, spec) {
						candidatePaths[candidate] = struct{}{}
					}
					return nil
				}); err != nil {
				return 0, err
			}
			if len(candidatePaths) == 0 {
				continue
			}
			// Batching happens after variant expansion: one logical candidate
			// can be persisted under several path spellings, so the parameter
			// count is bounded by the variants, not by the candidates.
			storedPaths := map[string]struct{}{}
			for p := range candidatePaths {
				for _, variant := range storedPathVariants(p) {
					storedPaths[variant] = struct{}{}
				}
			}
			variants := make([]string, 0, len(storedPaths))
			for p := range storedPaths {
				variants = append(variants, p)
			}
			sort.Strings(variants)
			pathArgs := make([]any, 0, len(variants))
			for _, p := range variants {
				pathArgs = append(pathArgs, p)
			}
			if err := tsScopeBatchedQuery(ctx, q,
				`SELECT id,path FROM files WHERE repo_id=? AND is_deleted=0`, ` AND path IN (%s)`,
				[]any{repoID}, pathArgs, true,
				func(rows *sql.Rows) error {
					var f tsScopeFile
					if err := rows.Scan(&f.id, &f.path); err != nil {
						return err
					}
					f.path = canonicalStoredPath(f.path)
					byPath[f.path] = f.id
					if _, exists := files[f.id]; !exists {
						files[f.id] = f
						fileIDs[f.id] = struct{}{}
					}
					return nil
				}); err != nil {
				return 0, err
			}
		}
	}

	symbols := map[int64]tsScopeSymbol{}
	byFileName := map[int64]map[string][]int64{}
	byFileQName := map[int64]map[string][]int64{}
	// One sorted component id list feeds both scoped evidence loads below.
	componentIDs := make([]int64, 0, len(files))
	if len(only) > 0 {
		for id := range files {
			componentIDs = append(componentIDs, id)
		}
		sort.Slice(componentIDs, func(i, j int) bool { return componentIDs[i] < componentIDs[j] })
	}
	componentArgs := int64SliceToAny(componentIDs)
	if err := tsScopeBatchedQuery(ctx, q,
		`SELECT s.id,s.file_id,s.name,s.qualified_name,s.visibility,s.kind FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND s.language='typescript' AND f.is_deleted=0`,
		` AND s.file_id IN (%s)`, []any{repoID}, componentArgs, len(only) > 0,
		func(rows *sql.Rows) error {
			var s tsScopeSymbol
			if err := rows.Scan(&s.id, &s.file, &s.name, &s.qname, &s.visibility, &s.kind); err != nil {
				return err
			}
			symbols[s.id] = s
			if byFileName[s.file] == nil {
				byFileName[s.file] = map[string][]int64{}
			}
			byFileName[s.file][s.name] = append(byFileName[s.file][s.name], s.id)
			if byFileQName[s.file] == nil {
				byFileQName[s.file] = map[string][]int64{}
			}
			byFileQName[s.file][s.qname] = append(byFileQName[s.file][s.qname], s.id)
			return nil
		}); err != nil {
		return 0, err
	}

	imports := map[int64][]tsScopeImport{}
	defaultNames := map[int64]map[string]struct{}{}
	if err := tsScopeBatchedQuery(ctx, q,
		`SELECT file_id,source_specifier,imported_name,local_name,import_kind,wildcard,is_reexport,is_namespace_export,is_type_only FROM scope_import_evidence WHERE repo_id=? AND language='typescript'`,
		` AND file_id IN (%s)`, []any{repoID}, componentArgs, len(only) > 0,
		func(rows *sql.Rows) error {
			var i tsScopeImport
			var w, r, n, t int
			if err := rows.Scan(&i.file, &i.source, &i.imported, &i.local, &i.kind, &w, &r, &n, &t); err != nil {
				return err
			}
			i.wildcard = w != 0
			i.reexport = r != 0
			i.namespace = n != 0
			i.typeOnly = t != 0
			imports[i.file] = append(imports[i.file], i)
			if i.reexport && i.source == "" && i.imported == "default" && i.local != "" {
				if defaultNames[i.file] == nil {
					defaultNames[i.file] = map[string]struct{}{}
				}
				defaultNames[i.file][i.local] = struct{}{}
			}
			return nil
		}); err != nil {
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
			} else if i.local == name || (i.source == "" && i.imported == name) {
				explicit = append(explicit, i)
			}
		}
		collect := func(list []tsScopeImport) tsScopeExport {
			var out tsScopeExport
			for _, i := range list {
				if i.source == "" {
					localName := i.imported
					if localName == "default" {
						localName = i.local
					}
					for _, id := range byFileName[mod][localName] {
						out.symbols = append(out.symbols, id)
					}
					if len(byFileName[mod][localName]) == 0 {
						for _, imported := range imports[mod] {
							if imported.local != localName || imported.source == "" || imported.reexport || imported.typeOnly || imported.kind == "side_effect" {
								continue
							}
							target, ok := resolveModule(mod, imported.source)
							if ok {
								x := export(target, imported.imported, cloneSeen(seen))
								out.symbols = append(out.symbols, x.symbols...)
								if x.namespace != 0 {
									out.namespace = x.namespace
								}
							}
						}
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
				exportedName := i.imported
				if i.wildcard {
					exportedName = name
				}
				x := export(target, exportedName, cloneSeen(seen))
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
			if name != "default" {
				if _, isDefault := defaultNames[mod][name]; isDefault {
					continue
				}
			}
			if symbols[id].visibility == "public" {
				direct.symbols = append(direct.symbols, id)
			}
		}
		direct = uniqueExport(direct)
		if len(direct.symbols) > 0 {
			return direct
		}
		if name == "default" {
			return tsScopeExport{}
		}
		return collect(stars)
	}
	results := map[int64]int64{}
	for _, e := range edges {
		var target tsScopeExport
		parts := strings.Split(e.name, ".")
		if len(parts) > 1 {
			for _, id := range byFileQName[e.file][e.name] {
				target.symbols = append(target.symbols, id)
			}
			local := parts[0]
			member := strings.Join(parts[1:], ".")
			for _, i := range imports[e.file] {
				if i.local == local && !i.typeOnly && i.kind == "namespace" {
					if m, ok := resolveModule(e.file, i.source); ok {
						target = export(m, member, map[string]bool{})
					}
				}
				if i.local == local && !i.typeOnly && i.kind != "namespace" {
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
		for _, chunk := range chunkInt64s(ids, tsScopeResolutionBatchSize) {
			vals := make([]any, 0, len(chunk)*2)
			for _, id := range chunk {
				vals = append(vals, id, results[id])
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO `+tsScopeResolution+`(edge_id,dst_symbol_id) VALUES `+pairPlaceholders(len(chunk)), vals...); err != nil {
				return 0, err
			}
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
// changed source files. Candidate paths are persisted by the caller, so this
// reverse lookup remains useful when a target is absent or ambiguous.
func (s *Store) invalidateTypeScriptScopeBindings(ctx context.Context, repoID int64, paths []string) ([]string, error) {
	return invalidateTypeScriptScopeBindingsQuery(ctx, s.db, repoID, paths)
}

// invalidateTypeScriptScopeBindingsQuery carries the work of
// invalidateTypeScriptScopeBindings against an explicit querier so the
// statement budget can be observed in tests.
func invalidateTypeScriptScopeBindingsQuery(ctx context.Context, q execQuerier, repoID int64, paths []string) ([]string, error) {
	changed, err := fileIDsByPaths(ctx, q, repoID, paths)
	if err != nil {
		return nil, err
	}
	affected := make(map[int64]struct{}, len(changed))
	for _, id := range changed {
		affected[id] = struct{}{}
	}
	frontier := make([]string, 0, len(paths))
	seenPaths := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		p = CanonicalRelPath(p)
		if p != "" {
			seenPaths[p] = struct{}{}
			frontier = append(frontier, p)
		}
	}
	seenModules := make(map[int64]struct{}, len(changed))
	for len(frontier) > 0 {
		// The whole frontier level is read before the next one is formed, so
		// batching never truncates the dependent module closure.
		pathArgs := make([]any, 0, len(frontier))
		for _, p := range frontier {
			pathArgs = append(pathArgs, p)
		}
		next := make([]string, 0)
		if err := tsScopeBatchedQuery(ctx, q, `
			SELECT c.source_file_id, f.path
			FROM scope_module_candidate_evidence c
			JOIN files f ON f.id=c.source_file_id AND f.repo_id=c.repo_id AND f.language='typescript'
			WHERE c.repo_id=?`, ` AND c.candidate_path IN (%s)`,
			[]any{repoID}, pathArgs, true,
			func(rows *sql.Rows) error {
				var fileID int64
				var filePath string
				if err := rows.Scan(&fileID, &filePath); err != nil {
					return err
				}
				affected[fileID] = struct{}{}
				if _, ok := seenModules[fileID]; ok {
					return nil
				}
				seenModules[fileID] = struct{}{}
				modulePath := canonicalStoredPath(filePath)
				if modulePath != "" {
					if _, ok := seenPaths[modulePath]; !ok {
						seenPaths[modulePath] = struct{}{}
						next = append(next, modulePath)
					}
				}
				return nil
			}); err != nil {
			return nil, err
		}
		frontier = next
	}
	if len(affected) == 0 {
		return nil, nil
	}
	var edgeIDs []int64
	names := map[string]struct{}{}
	if err := tsScopeBatchedQuery(ctx, q,
		`SELECT e.id,e.dst_name FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.language='typescript' AND e.edge_kind <> 'cross_language_ref'`,
		` AND e.file_id IN (%s)`, []any{repoID}, tsScopeIDArgs(affected), true,
		func(rows *sql.Rows) error {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				return err
			}
			edgeIDs = append(edgeIDs, id)
			if name != "" {
				names[name] = struct{}{}
			}
			return nil
		}); err != nil {
		return nil, err
	}
	if _, err := clearEdgeResolutions(ctx, q, edgeIDs); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

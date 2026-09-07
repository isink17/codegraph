package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
)

const csharpScopeVeto = "tmp_csharp_scope_veto"

type csharpScopeSymbol struct {
	id, file                                                    int64
	name, qname, container, kind, stable, signature, visibility string
	static                                                      sql.NullInt64
}

type csharpScopeEdge struct {
	id, file                                              int64
	name, srcStable, srcContainer, srcQName, srcNamespace string
}

type csharpScopeImport struct {
	source, imported, local, kind, owner string
	static                               bool
}

func resolveCSharpScope(ctx context.Context, q javaQuery, repoID int64, only map[int64]struct{}) (int, error) {
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+csharpScopeVeto+`(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	ids := sortedIDs(only)
	edges := []csharpScopeEdge{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.file_id,e.dst_name,src.stable_key,src.container_name,src.qualified_name,COALESCE(fs.package_name,'')
FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols src ON src.id=e.src_symbol_id
LEFT JOIN file_scope_evidence fs ON fs.repo_id=e.repo_id AND fs.file_id=e.file_id
WHERE e.repo_id=? AND f.language='csharp' AND e.dst_symbol_id IS NULL`, " AND e.id IN (%s)", []any{repoID}, int64SliceToAny(ids), len(only) > 0,
		func(rows *sql.Rows) error {
			var e csharpScopeEdge
			if err := rows.Scan(&e.id, &e.file, &e.name, &e.srcStable, &e.srcContainer, &e.srcQName, &e.srcNamespace); err != nil {
				return err
			}
			edges = append(edges, e)
			return nil
		}); err != nil {
		return 0, err
	}
	veto := make([][]any, 0, len(edges))
	for _, e := range edges {
		veto = append(veto, []any{e.id})
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT OR IGNORE INTO `+csharpScopeVeto+`(edge_id) VALUES `, "(?)", nil, veto); err != nil {
		return 0, err
	}
	if len(edges) == 0 {
		return 0, nil
	}

	symbols := map[int64]csharpScopeSymbol{}
	byName := map[string][]csharpScopeSymbol{}
	byQName := map[string][]csharpScopeSymbol{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.name,s.qualified_name,s.container_name,s.kind,s.stable_key,s.signature,s.visibility,s.is_static
FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.language='csharp' AND f.is_deleted=0`, "", []any{repoID}, nil, false,
		func(rows *sql.Rows) error {
			var s csharpScopeSymbol
			if err := rows.Scan(&s.id, &s.file, &s.name, &s.qname, &s.container, &s.kind, &s.stable, &s.signature, &s.visibility, &s.static); err != nil {
				return err
			}
			symbols[s.id] = s
			byName[s.name] = append(byName[s.name], s)
			byQName[s.qname] = append(byQName[s.qname], s)
			return nil
		}); err != nil {
		return 0, err
	}

	files := map[int64]struct{}{}
	for _, e := range edges {
		files[e.file] = struct{}{}
	}
	imports := map[int64][]csharpScopeImport{}
	if err := sqliteBatchedIDQuery(ctx, q, sortedIDs(files), `SELECT file_id,source_specifier,imported_name,local_name,import_kind,owner_module,is_static FROM scope_import_evidence WHERE repo_id=? AND language='csharp' AND file_id IN (`, []any{repoID}, func(scan func(...any) error) error {
		var f int64
		var i csharpScopeImport
		var st int
		if err := scan(&f, &i.source, &i.imported, &i.local, &i.kind, &i.owner, &st); err != nil {
			return err
		}
		i.static = st != 0
		imports[f] = append(imports[f], i)
		return nil
	}); err != nil {
		return 0, err
	}
	bindings := map[int64]map[string]map[string]struct{}{}
	if err := sqliteBatchedIDQuery(ctx, q, sortedIDs(files), `SELECT file_id,local_name,owner_module FROM scope_import_evidence WHERE repo_id=? AND language='csharp' AND import_kind='local_binding' AND file_id IN (`, []any{repoID}, func(scan func(...any) error) error {
		var f int64
		var name, owner string
		if err := scan(&f, &name, &owner); err != nil {
			return err
		}
		if bindings[f] == nil {
			bindings[f] = map[string]map[string]struct{}{}
		}
		if bindings[f][owner] == nil {
			bindings[f][owner] = map[string]struct{}{}
		}
		bindings[f][owner][name] = struct{}{}
		return nil
	}); err != nil {
		return 0, err
	}

	res := map[int64]struct {
		dst      int64
		strategy string
	}{}
	for _, e := range edges {
		if dst, strategy, ok := csharpResolveEdge(e, byName, byQName, imports[e.file], bindings[e.file]); ok {
			res[e.id] = struct {
				dst      int64
				strategy string
			}{dst.id, strategy}
		}
	}
	if len(res) == 0 {
		return 0, nil
	}
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_csharp_scope_resolution(edge_id INTEGER PRIMARY KEY,dst_symbol_id INTEGER NOT NULL,strategy TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM tmp_csharp_scope_resolution`); err != nil {
		return 0, err
	}
	rows := make([][]any, 0, len(res))
	ids = ids[:0]
	for id := range res {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		r := res[id]
		rows = append(rows, []any{id, r.dst, r.strategy})
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT INTO tmp_csharp_scope_resolution(edge_id,dst_symbol_id,strategy) VALUES `, "(?,?,?)", nil, rows); err != nil {
		return 0, err
	}
	_, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=(SELECT dst_symbol_id FROM tmp_csharp_scope_resolution r WHERE r.edge_id=edges.id),resolution_strategy=(SELECT strategy FROM tmp_csharp_scope_resolution r WHERE r.edge_id=edges.id),resolution_confidence='high' WHERE id IN (SELECT edge_id FROM tmp_csharp_scope_resolution)`)
	return len(res), err
}

func csharpResolveEdge(e csharpScopeEdge, byName map[string][]csharpScopeSymbol, byQName map[string][]csharpScopeSymbol, imports []csharpScopeImport, bindings map[string]map[string]struct{}) (csharpScopeSymbol, string, bool) {
	name := strings.TrimPrefix(e.name, "global::")
	if name == "" || strings.HasPrefix(name, "base.") {
		return csharpScopeSymbol{}, "", false
	}
	parts := strings.Split(name, ".")
	method := parts[len(parts)-1]
	qualifier := strings.Join(parts[:len(parts)-1], ".")
	shadowed := func(n string) bool {
		for owner, names := range bindings {
			if owner == e.srcStable || owner == e.srcContainer {
				if _, ok := names[n]; ok {
					return true
				}
			}
		}
		return false
	}
	applicable := func(i csharpScopeImport) bool { return i.owner == "" || i.owner == e.srcNamespace }
	visible := func(s csharpScopeSymbol) bool {
		if s.container == e.srcContainer {
			return s.visibility != ""
		}
		return s.visibility == "public"
	}
	choose := func(c []csharpScopeSymbol, static bool) (csharpScopeSymbol, bool) {
		var out csharpScopeSymbol
		n := 0
		for _, s := range c {
			if s.kind != "function" || !visible(s) {
				continue
			}
			if static && (!s.static.Valid || s.static.Int64 == 0) {
				continue
			}
			if !static && s.static.Valid && s.static.Int64 != 0 && qualifier != "" {
				continue
			}
			out = s
			n++
		}
		return out, n == 1
	}
	if qualifier == "" {
		if shadowed(method) {
			return csharpScopeSymbol{}, "", false
		}
		var c []csharpScopeSymbol
		for _, s := range byName[method] {
			if s.container == e.srcContainer {
				c = append(c, s)
			}
		}
		if out, ok := choose(c, false); ok {
			return out, "csharp_same_type", true
		}
		for _, i := range imports {
			if applicable(i) && i.static && !strings.HasPrefix(i.kind, "global_") {
				if out, ok := choose(byQName[i.source+"."+method], true); ok {
					return out, "csharp_static_using", true
				}
			}
		}
		return csharpScopeSymbol{}, "", false
	}
	if qualifier == "this" {
		for _, s := range byName[method] {
			if s.container == e.srcContainer {
				if out, ok := choose([]csharpScopeSymbol{s}, false); ok {
					return out, "csharp_this_scope", true
				}
			}
		}
		return csharpScopeSymbol{}, "", false
	}
	if shadowed(strings.TrimPrefix(qualifier, "global::")) {
		return csharpScopeSymbol{}, "", false
	}
	typeQ := qualifier
	alias := false
	namespaceQ := map[string]struct{}{}
	for _, i := range imports {
		if applicable(i) && i.local == qualifier && !strings.HasPrefix(i.kind, "global_") {
			typeQ = i.source
			alias = i.kind == "alias"
		}
		if applicable(i) && i.kind == "namespace" && !strings.HasPrefix(i.kind, "global_") {
			namespaceQ[i.source+"."+qualifier] = struct{}{}
		}
	}
	var c []csharpScopeSymbol
	types := map[string]struct{}{}
	for q, ss := range byQName {
		_, importedType := namespaceQ[q]
		currentType := e.srcNamespace != "" && q == e.srcNamespace+"."+typeQ
		if q == typeQ || importedType || currentType || (strings.Contains(typeQ, ".") && strings.HasSuffix(q, "."+typeQ)) {
			for _, s := range ss {
				if s.kind == "type" {
					types[s.qname] = struct{}{}
				}
			}
		}
	}
	for _, s := range byName[method] {
		if _, ok := types[s.container]; ok {
			c = append(c, s)
		}
	}
	if out, ok := choose(c, true); ok {
		if alias {
			return out, "csharp_alias_scope", true
		}
		return out, "csharp_type_scope", true
	}
	return csharpScopeSymbol{}, "", false
}

package store

import (
	"context"
	"database/sql"
	"strings"
)

type kotlinScopeSymbol struct {
	id, file                                                 int64
	name, qname, container, kind, visibility, signature, pkg string
}

type kotlinScopeImport struct {
	source, imported, local string
	wildcard                bool
}

// resolveKotlinScope is deliberately source-evidence only. It owns every edge
// from a file with Kotlin scope evidence, including unresolved edges, so the
// generic resolver cannot turn a refused package decision into a global guess.
func resolveKotlinScope(ctx context.Context, q javaQuery, repoID int64, only map[int64]struct{}) (int, error) {
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_kotlin_scope_veto(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	type edge struct {
		id, file                             int64
		name, kind, evidence, pkg, container string
	}
	var edges []edge
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.dst_name,e.edge_kind,e.evidence,e.file_id,
		COALESCE(fs.package_name,''),s.container_name
		FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols s ON s.id=e.src_symbol_id
		JOIN file_scope_evidence fs ON fs.repo_id=e.repo_id AND fs.file_id=e.file_id
		WHERE e.repo_id=? AND f.language='kotlin' AND e.dst_symbol_id IS NULL`, " AND e.id IN (%s)",
		[]any{repoID}, int64SliceToAny(sortedIDs(only)), len(only) > 0,
		func(rows *sql.Rows) error {
			var e edge
			if err := rows.Scan(&e.id, &e.name, &e.kind, &e.evidence, &e.file, &e.pkg, &e.container); err != nil {
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
	vetoRows := make([][]any, 0, len(edges))
	for _, e := range edges {
		vetoRows = append(vetoRows, []any{e.id})
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT OR IGNORE INTO tmp_kotlin_scope_veto(edge_id) VALUES `, "(?)", nil, vetoRows); err != nil {
		return 0, err
	}

	fileIDs := map[int64]struct{}{}
	packages := map[string]struct{}{}
	names := map[string]struct{}{}
	for _, e := range edges {
		fileIDs[e.file] = struct{}{}
		packages[e.pkg] = struct{}{}
		for _, p := range strings.Split(e.name, ".") {
			if p != "" {
				names[p] = struct{}{}
			}
		}
	}
	scopeFileIDs := sortedIDs(fileIDs)
	if err := sqliteBatchedIDQuery(ctx, q, scopeFileIDs,
		`SELECT source_specifier,imported_name,wildcard FROM scope_import_evidence WHERE repo_id=? AND language='kotlin' AND file_id IN (`,
		[]any{repoID}, func(scan func(...any) error) error {
			var source, name string
			var wildcard int
			if err := scan(&source, &name, &wildcard); err != nil {
				return err
			}
			if name != "" {
				names[name] = struct{}{}
			}
			if wildcard == 0 {
				if dot := strings.LastIndexByte(source, '.'); dot >= 0 {
					source = source[:dot]
				}
			}
			packages[source] = struct{}{}
			return nil
		}); err != nil {
		return 0, err
	}
	// Both the name set and the package set are unbounded, so the candidate load
	// is batched on both axes. Every symbol carries exactly one name and one
	// package, so it is returned by exactly one (name batch, package batch)
	// pair: the union across batches is the unlimited query's result, with no
	// duplicates introduced.
	nameList := sortedKeys(names)
	packageList := sortedKeys(packages)
	var syms []kotlinScopeSymbol
	for _, nameChunk := range chunkStrings(nameList, sqliteBatchSize(1, 1)/2) {
		fixed := make([]any, 0, 1+len(nameChunk))
		fixed = append(fixed, repoID)
		for _, n := range nameChunk {
			fixed = append(fixed, n)
		}
		if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.name,s.qualified_name,s.container_name,s.kind,s.visibility,s.signature,COALESCE(fs.package_name,'')
		FROM symbols s JOIN files f ON f.id=s.file_id LEFT JOIN file_scope_evidence fs ON fs.repo_id=s.repo_id AND fs.file_id=s.file_id
		WHERE s.repo_id=? AND f.language='kotlin' AND f.is_deleted=0 AND s.name IN (`+sqlitePlaceholders(len(nameChunk))+`)`,
			" AND COALESCE(fs.package_name,'') IN (%s)",
			fixed, stringSliceToAny(packageList), true,
			func(rows *sql.Rows) error {
				var s kotlinScopeSymbol
				if err := rows.Scan(&s.id, &s.file, &s.name, &s.qname, &s.container, &s.kind, &s.visibility, &s.signature, &s.pkg); err != nil {
					return err
				}
				syms = append(syms, s)
				return nil
			}); err != nil {
			return 0, err
		}
	}
	imports := map[int64][]kotlinScopeImport{}
	if err := sqliteBatchedIDQuery(ctx, q, scopeFileIDs,
		`SELECT file_id,source_specifier,imported_name,local_name,wildcard FROM scope_import_evidence WHERE repo_id=? AND language='kotlin' AND file_id IN (`,
		[]any{repoID}, func(scan func(...any) error) error {
			var file int64
			var i kotlinScopeImport
			var w int
			if err := scan(&file, &i.source, &i.imported, &i.local, &w); err != nil {
				return err
			}
			i.wildcard = w != 0
			imports[file] = append(imports[file], i)
			return nil
		}); err != nil {
		return 0, err
	}
	res := make([]struct {
		edge, dst int64
		strategy  string
	}, 0)
	for _, e := range edges {
		var dst kotlinScopeSymbol
		strategy := ""
		if e.kind == "constructs" {
			continue
		}
		if strings.Contains(e.name, ".") {
			parts := strings.Split(e.name, ".")
			member := parts[len(parts)-1]
			qualifier := strings.Join(parts[:len(parts)-1], ".")
			owner := kotlinType(qualifier, e, syms, imports[e.file])
			if owner.id != 0 {
				dst, strategy = kotlinMember(owner, member, e, syms)
			}
		} else {
			dst, strategy = kotlinBare(e, syms, imports[e.file])
		}
		if dst.id != 0 {
			res = append(res, struct {
				edge, dst int64
				strategy  string
			}{e.id, dst.id, strategy})
		}
	}
	if len(res) == 0 {
		return 0, nil
	}
	resRows := make([][]any, 0, len(res))
	for _, r := range res {
		resRows = append(resRows, []any{r.edge, r.dst, r.strategy})
	}
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_kotlin_scope_resolution(edge_id INTEGER PRIMARY KEY,dst_symbol_id INTEGER NOT NULL,strategy TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM tmp_kotlin_scope_resolution`); err != nil {
		return 0, err
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT INTO tmp_kotlin_scope_resolution(edge_id,dst_symbol_id,strategy) VALUES `, "(?,?,?)", nil, resRows); err != nil {
		return 0, err
	}
	_, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=(SELECT dst_symbol_id FROM tmp_kotlin_scope_resolution WHERE edge_id=edges.id),resolution_strategy=(SELECT strategy FROM tmp_kotlin_scope_resolution WHERE edge_id=edges.id),resolution_confidence='high' WHERE id IN (SELECT edge_id FROM tmp_kotlin_scope_resolution)`)
	return len(res), err
}

func kotlinType(name string, e struct {
	id, file                             int64
	name, kind, evidence, pkg, container string
}, syms []kotlinScopeSymbol, imports []kotlinScopeImport) kotlinScopeSymbol {
	var c []kotlinScopeSymbol
	for _, s := range syms {
		if s.kind != "class" && s.kind != "interface" && s.kind != "object" && s.kind != "enum" {
			continue
		}
		if s.qname == name || s.qname == kotlinJoin(e.pkg, name) {
			if kotlinVisible(s, e) {
				c = append(c, s)
			}
		}
	}
	for _, i := range imports {
		if i.wildcard {
			for _, s := range syms {
				if s.qname == i.source+"."+name && kotlinVisible(s, e) {
					c = append(c, s)
				}
			}
			continue
		}
		if i.local == name {
			for _, s := range syms {
				if s.qname == i.source && kotlinVisible(s, e) {
					c = append(c, s)
				}
			}
		}
	}
	return kotlinUnique(c)
}

func kotlinBare(e struct {
	id, file                             int64
	name, kind, evidence, pkg, container string
}, syms []kotlinScopeSymbol, imports []kotlinScopeImport) (kotlinScopeSymbol, string) {
	var c []kotlinScopeSymbol
	strategy := "kotlin_package_scope"
	for _, s := range syms {
		if s.name == e.name && s.kind == "function" && s.pkg == e.pkg && s.container == e.pkg && kotlinVisible(s, e) && !kotlinExtension(s) {
			c = append(c, s)
		}
	}
	if e.container != "" {
		owner := kotlinJoin(e.pkg, e.container)
		for _, s := range syms {
			if s.name == e.name && s.kind == "function" && s.qname == owner+"."+e.name && kotlinVisible(s, e) && !kotlinExtension(s) {
				c = append(c, s)
			}
		}
	}
	for _, i := range imports {
		if i.wildcard {
			for _, s := range syms {
				if s.qname == i.source+"."+e.name && s.kind == "function" && kotlinVisible(s, e) && !kotlinExtension(s) {
					c = append(c, s)
				}
			}
			strategy = "kotlin_import_scope"
			continue
		}
		if i.local == e.name {
			for _, s := range syms {
				if s.qname == i.source && s.kind == "function" && kotlinVisible(s, e) && !kotlinExtension(s) {
					c = append(c, s)
				}
			}
			strategy = "kotlin_import_scope"
		}
	}
	return kotlinUnique(c), strategy
}

func kotlinMember(owner kotlinScopeSymbol, name string, e struct {
	id, file                             int64
	name, kind, evidence, pkg, container string
}, syms []kotlinScopeSymbol) (kotlinScopeSymbol, string) {
	var c []kotlinScopeSymbol
	for _, s := range syms {
		if s.name == name && s.container == strings.TrimPrefix(owner.qname, owner.pkg+".") && kotlinVisible(s, e) && !kotlinExtension(s) {
			c = append(c, s)
		}
	}
	if len(c) != 1 {
		return kotlinScopeSymbol{}, ""
	}
	return c[0], "kotlin_package_scope"
}
func kotlinUnique(c []kotlinScopeSymbol) kotlinScopeSymbol {
	if len(c) != 1 {
		return kotlinScopeSymbol{}
	}
	return c[0]
}
func kotlinJoin(a, b string) string {
	if a == "" {
		return b
	}
	return a + "." + b
}
func kotlinExtension(s kotlinScopeSymbol) bool {
	return strings.Contains(strings.TrimSpace(s.signature), "."+s.name+"(")
}
func kotlinVisible(s kotlinScopeSymbol, e struct {
	id, file                             int64
	name, kind, evidence, pkg, container string
}) bool {
	switch s.visibility {
	case "private":
		return s.file == e.file
	case "protected":
		return s.file == e.file || s.container == e.container
	case "internal", "public", "":
		return true
	}
	return false
}

package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
)

type javaScopeSymbol struct {
	id, file                                                 int64
	name, qname, container, kind, signature, visibility, pkg string
	static                                                   sql.NullInt64
}

type javaScopeImport struct {
	source, local    string
	wildcard, static bool
}

type javaScopeEdge struct {
	id                                   int64
	name, kind, evidence, pkg, container string
	file                                 int64
	scoped                               bool
}

// resolveJavaScope is intentionally small and conservative. It is the only
// Java path before generic resolution; every selected Java edge is vetoed from
// weaker global strategies, including edges left unresolved here.
type javaQuery interface {
	queryContexter
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func resolveJavaScope(ctx context.Context, q javaQuery, repoID int64, only map[int64]struct{}) (int, error) {
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_java_scope_veto(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	onlyIDs := sortedIDs(only)
	var edges []javaScopeEdge
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.dst_name,e.edge_kind,e.evidence,e.file_id,s.container_name,COALESCE(fs.package_name,''),fs.file_id IS NOT NULL
		FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols s ON s.id=e.src_symbol_id
		LEFT JOIN file_scope_evidence fs ON fs.file_id=e.file_id AND fs.repo_id=e.repo_id
		WHERE e.repo_id=? AND f.language='java' AND e.dst_symbol_id IS NULL`, " AND e.id IN (%s)",
		[]any{repoID}, int64SliceToAny(onlyIDs), len(only) > 0,
		func(rows *sql.Rows) error {
			var x javaScopeEdge
			var scoped int
			if err := rows.Scan(&x.id, &x.name, &x.kind, &x.evidence, &x.file, &x.container, &x.pkg, &scoped); err != nil {
				return err
			}
			x.scoped = scoped != 0
			edges = append(edges, x)
			return nil
		}); err != nil {
		return 0, err
	}
	vetoRows := make([][]any, 0, len(edges))
	for _, e := range edges {
		if e.scoped {
			vetoRows = append(vetoRows, []any{e.id})
		}
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT OR IGNORE INTO tmp_java_scope_veto(edge_id) VALUES `, "(?)", nil, vetoRows); err != nil {
		return 0, err
	}
	if len(edges) == 0 {
		return 0, nil
	}
	// Incremental calls carry only the affected edge ids. Restrict the Java
	// candidate population to lexical components named by those edges; a local
	// edit therefore cannot turn this pass into a repository-wide symbol scan.
	scopeNames := map[string]struct{}{}
	for _, e := range edges {
		for _, part := range strings.FieldsFunc(e.name, func(r rune) bool { return r == '.' || r == '$' }) {
			if part != "" {
				scopeNames[part] = struct{}{}
			}
		}
	}
	nameList := sortedKeys(scopeNames)
	if len(nameList) == 0 {
		return 0, nil
	}

	symbols := map[int64]javaScopeSymbol{}
	byQName := map[string][]javaScopeSymbol{}
	byName := map[string][]javaScopeSymbol{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.name,s.qualified_name,s.container_name,s.kind,s.signature,s.visibility,s.is_static,COALESCE(fs.package_name,'')
		FROM symbols s JOIN files f ON f.id=s.file_id LEFT JOIN file_scope_evidence fs ON fs.file_id=s.file_id AND fs.repo_id=s.repo_id
		WHERE s.repo_id=? AND f.language='java' AND f.is_deleted=0`, " AND s.name IN (%s)",
		[]any{repoID}, stringSliceToAny(nameList), true,
		func(rows *sql.Rows) error {
			var s javaScopeSymbol
			if err := rows.Scan(&s.id, &s.file, &s.name, &s.qname, &s.container, &s.kind, &s.signature, &s.visibility, &s.static, &s.pkg); err != nil {
				return err
			}
			symbols[s.id] = s
			byQName[s.qname] = append(byQName[s.qname], s)
			byName[s.name] = append(byName[s.name], s)
			return nil
		}); err != nil {
		return 0, err
	}
	imports := map[int64][]javaScopeImport{}
	// Load the evidence by file rather than through the selected edges: the
	// edges are already in hand and carry their file, and `imports` is only ever
	// read as imports[e.file]. Batching an edge-id subquery would select the
	// same file from several batches and duplicate its import rows, which the
	// unique-candidate rules below read as ambiguity.
	importFiles := map[int64]struct{}{}
	for _, e := range edges {
		importFiles[e.file] = struct{}{}
	}
	if err := sqliteBatchedIDQuery(ctx, q, sortedIDs(importFiles),
		`SELECT file_id,source_specifier,local_name,wildcard,is_static FROM scope_import_evidence WHERE repo_id=? AND language='java' AND file_id IN (`,
		[]any{repoID}, func(scan func(...any) error) error {
			var file int64
			var i javaScopeImport
			var w, st int
			if err := scan(&file, &i.source, &i.local, &w, &st); err != nil {
				return err
			}
			i.wildcard = w != 0
			i.static = st != 0
			imports[file] = append(imports[file], i)
			return nil
		}); err != nil {
		return 0, err
	}

	res := map[int64]struct {
		dst      int64
		strategy string
	}{}
	for _, e := range edges {
		if !e.scoped {
			continue
		}
		var dst javaScopeSymbol
		var strategy string
		if e.kind == "constructs" {
			dst, strategy = javaConstructor(e, byQName, byName, imports)
		} else {
			dst, strategy = javaMember(e, byQName, byName, imports, symbols)
		}
		if dst.id != 0 {
			res[e.id] = struct {
				dst      int64
				strategy string
			}{dst: dst.id, strategy: strategy}
		}
	}
	if len(res) == 0 {
		return 0, nil
	}
	if _, err := q.ExecContext(ctx, `DROP TABLE IF EXISTS tmp_java_scope_resolution`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE tmp_java_scope_resolution(edge_id INTEGER PRIMARY KEY,dst_symbol_id INTEGER NOT NULL,strategy TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM tmp_java_scope_resolution`); err != nil {
		return 0, err
	}
	resRows := make([][]any, 0, len(res))
	resIDs := make([]int64, 0, len(res))
	for id := range res {
		resIDs = append(resIDs, id)
	}
	sort.Slice(resIDs, func(i, j int) bool { return resIDs[i] < resIDs[j] })
	for _, id := range resIDs {
		resRows = append(resRows, []any{id, res[id].dst, res[id].strategy})
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT INTO tmp_java_scope_resolution(edge_id,dst_symbol_id,strategy) VALUES `, "(?,?,?)", nil, resRows); err != nil {
		return 0, err
	}
	_, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=(SELECT dst_symbol_id FROM tmp_java_scope_resolution r WHERE r.edge_id=edges.id),resolution_strategy=(SELECT strategy FROM tmp_java_scope_resolution r WHERE r.edge_id=edges.id),resolution_confidence='high' WHERE id IN (SELECT edge_id FROM tmp_java_scope_resolution)`)
	return len(res), err
}

func javaType(eName, pkg, container string, byQName map[string][]javaScopeSymbol, byName map[string][]javaScopeSymbol, imps []javaScopeImport) (javaScopeSymbol, bool, string) {
	name := eName
	if i := strings.LastIndex(name, "."); i >= 0 { // fully qualified or nested spelling
		var exact []javaScopeSymbol
		for _, s := range byQName[name] {
			if s.kind == "type" {
				exact = append(exact, s)
			}
		}
		return javaUniqueVisible(exact, pkg, "java_package_scope")
	}
	var c []javaScopeSymbol
	for _, s := range byName[name] {
		if s.kind != "type" {
			continue
		}
		if container != "" && (s.qname == pkg+"."+container+"."+name || s.qname == container+"."+name) {
			c = append(c, s)
			continue
		}
		if s.pkg == pkg && s.container == s.pkg {
			c = append(c, s)
		}
	}
	for _, i := range imps {
		if i.static {
			continue
		}
		if !i.wildcard && i.local == name {
			c = nil
			for _, s := range byQName[i.source] {
				if s.kind == "type" {
					c = append(c, s)
				}
			}
			return javaUniqueVisible(c, pkg, "java_import_scope")
		}
		if i.wildcard {
			for _, s := range byQName[i.source+"."+name] {
				if s.kind == "type" {
					c = append(c, s)
				}
			}
		}
	}
	return javaUniqueVisible(c, pkg, "java_package_scope")
}

func javaUniqueVisible(c []javaScopeSymbol, pkg, strategy string) (javaScopeSymbol, bool, string) {
	var out javaScopeSymbol
	n := 0
	for _, s := range c {
		if javaVisible(s, pkg, s.pkg) {
			out = s
			n++
		}
	}
	return out, n == 1, strategy
}
func javaVisible(s javaScopeSymbol, fromPkg, ownerPkg string) bool {
	if s.visibility == "private" {
		return false
	}
	if s.visibility == "package" && fromPkg != ownerPkg {
		return false
	}
	if s.visibility == "protected" && fromPkg != ownerPkg {
		return false
	}
	return true
}
func javaVisibleFrom(s javaScopeSymbol, fromPkg, ownerPkg string, sameOwner bool) bool {
	if s.visibility == "private" {
		return sameOwner
	}
	return javaVisible(s, fromPkg, ownerPkg)
}
func javaArity(sig string) int {
	a := strings.Index(sig, "(")
	b := strings.LastIndex(sig, ")")
	if a < 0 || b < a {
		return -1
	}
	x := strings.TrimSpace(sig[a+1 : b])
	if x == "" {
		return 0
	}
	return strings.Count(x, ",") + 1
}
func javaConstructor(e javaScopeEdge, byQName map[string][]javaScopeSymbol, byName map[string][]javaScopeSymbol, imps map[int64][]javaScopeImport) (javaScopeSymbol, string) {
	t, ok, _ := javaType(e.name, e.pkg, e.container, byQName, byName, imps[e.file])
	if !ok {
		return javaScopeSymbol{}, ""
	}
	want := javaArity(e.evidence)
	var out javaScopeSymbol
	n := 0
	for _, s := range byQName[t.qname+"."+t.name] {
		if s.kind == "constructor" && javaArity(s.signature) == want && javaVisibleFrom(s, e.pkg, t.pkg, e.container == t.name) {
			out = s
			n++
		}
	}
	if n != 1 {
		return javaScopeSymbol{}, ""
	}
	return out, "java_constructor"
}
func javaMember(e javaScopeEdge, byQName map[string][]javaScopeSymbol, byName map[string][]javaScopeSymbol, imps map[int64][]javaScopeImport, symbols map[int64]javaScopeSymbol) (javaScopeSymbol, string) {
	name := e.name
	if name == "super." || strings.HasPrefix(name, "super.") {
		return javaScopeSymbol{}, ""
	}
	if strings.HasPrefix(name, "this.") {
		name = strings.TrimPrefix(name, "this.")
		s, ok, strategy := javaMethods(e.container, name, e.pkg, e.container, byQName, "java_package_scope")
		return s, strategyIf(ok, strategy)
	}
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		owner, ok, _ := javaType(name[:dot], e.pkg, e.container, byQName, byName, imps[e.file])
		if !ok {
			return javaScopeSymbol{}, ""
		}
		s, ok, strategy := javaMethods(owner.qname, name[dot+1:], e.pkg, e.container, byQName, "java_package_scope")
		return s, strategyIf(ok, strategy)
	}
	if s, ok, str := javaMethods(e.container, name, e.pkg, e.container, byQName, "java_package_scope"); ok {
		return s, str
	}
	var staticCandidates []javaScopeSymbol
	for _, i := range imps[e.file] {
		if !i.static {
			continue
		}
		if !i.wildcard && i.local == name {
			p := strings.LastIndex(i.source, ".")
			if p < 0 {
				return javaScopeSymbol{}, ""
			}
			owner := i.source[:p]
			for _, s := range byQName[owner+"."+name] {
				if s.static.Valid && s.static.Int64 != 0 && javaVisible(s, e.pkg, s.pkg) {
					staticCandidates = append(staticCandidates, s)
				}
			}
		}
		if i.static && i.wildcard {
			for _, s := range byQName[i.source+"."+name] {
				if s.kind == "function" && s.static.Valid && s.static.Int64 != 0 && javaVisible(s, e.pkg, s.pkg) {
					staticCandidates = append(staticCandidates, s)
				}
			}
		}
	}
	if len(staticCandidates) == 1 {
		return staticCandidates[0], "java_static_import"
	}
	return javaScopeSymbol{}, ""
}

func strategyIf(ok bool, strategy string) string {
	if ok {
		return strategy
	}
	return ""
}
func javaMethods(owner, name, pkg, caller string, byQName map[string][]javaScopeSymbol, strategy string) (javaScopeSymbol, bool, string) {
	var out javaScopeSymbol
	n := 0
	for _, s := range byQName[owner+"."+name] {
		if s.kind == "function" && javaVisibleFrom(s, pkg, s.pkg, owner == caller) {
			out = s
			n++
		}
	}
	return out, n == 1, strategy
}

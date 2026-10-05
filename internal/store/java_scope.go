package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/isink17/codegraph/internal/graph"
)

type javaScopeSymbol struct {
	id, file                                                           int64
	name, qname, container, kind, signature, visibility, pkg, language string
	static, arityMin, arityMax                                         sql.NullInt64
	jvmEvidence, jvmKnown, jvmArityMin, jvmArityMax                    sql.NullInt64
	jvmNameEvidence, jvmNameKnown                                      sql.NullInt64
	jvmName                                                            sql.NullString
	jvmStaticAlias                                                     bool
	jvmNameAliases, jvmSyntheticAliases                                string
}

type javaScopeImport struct {
	source, local    string
	wildcard, static bool
}

type javaScopeEdge struct {
	id                                   int64
	name, kind, evidence, pkg, container string
	file                                 int64
	callArity                            sql.NullInt64
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
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.dst_name,e.edge_kind,e.evidence,e.file_id,s.container_name,COALESCE(fs.package_name,''),fs.file_id IS NOT NULL,e.call_arity
		FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols s ON s.id=e.src_symbol_id
		LEFT JOIN file_scope_evidence fs ON fs.file_id=e.file_id AND fs.repo_id=e.repo_id
		WHERE e.repo_id=? AND f.language='java' AND e.dst_symbol_id IS NULL`, " AND e.id IN (%s)",
		[]any{repoID}, int64SliceToAny(onlyIDs), len(only) > 0,
		func(rows *sql.Rows) error {
			var x javaScopeEdge
			var scoped int
			if err := rows.Scan(&x.id, &x.name, &x.kind, &x.evidence, &x.file, &x.container, &x.pkg, &scoped, &x.callArity); err != nil {
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
	imports := map[int64][]javaScopeImport{}
	importFiles := map[int64]struct{}{}
	for _, e := range edges {
		importFiles[e.file] = struct{}{}
		for _, part := range strings.FieldsFunc(e.name, func(r rune) bool { return r == '.' || r == '$' }) {
			if part != "" {
				scopeNames[part] = struct{}{}
			}
		}
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
	for _, fileImports := range imports {
		for _, i := range fileImports {
			if !i.static || i.wildcard {
				continue
			}
			for _, part := range strings.FieldsFunc(i.source, func(r rune) bool { return r == '.' || r == '$' }) {
				if part != "" {
					scopeNames[part] = struct{}{}
				}
			}
		}
	}
	nameList := sortedKeys(scopeNames)
	if len(nameList) == 0 {
		return 0, nil
	}

	byQName := map[string][]javaScopeSymbol{}
	byName := map[string][]javaScopeSymbol{}
	companions := map[string][]javaScopeSymbol{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.name,s.qualified_name,s.container_name,s.kind,s.signature,s.visibility,s.is_static,COALESCE(fs.package_name,''),f.language,s.arity_min,s.arity_max,ke.symbol_id,ke.is_known,ke.jvm_arity_min,ke.jvm_arity_max,
		EXISTS(SELECT 1 FROM scope_import_evidence ki WHERE ki.repo_id=s.repo_id AND ki.file_id=s.file_id AND ki.language='kotlin' AND ki.source_specifier='kotlin.jvm.JvmStatic' AND ki.local_name!='JvmStatic'),
		COALESCE((SELECT group_concat(ki.local_name, ',') FROM scope_import_evidence ki WHERE ki.repo_id=s.repo_id AND ki.file_id=s.file_id AND ki.language='kotlin' AND ki.source_specifier='kotlin.jvm.JvmName' AND ki.local_name!='JvmName' AND instr(s.signature,'@'||ki.local_name)>0),''),
		COALESCE((SELECT group_concat(ki.local_name, ',') FROM scope_import_evidence ki WHERE ki.repo_id=s.repo_id AND ki.file_id=s.file_id AND ki.language='kotlin' AND ki.source_specifier='kotlin.jvm.JvmSynthetic' AND ki.local_name!='JvmSynthetic' AND instr(s.signature,'@'||ki.local_name)>0),''),
		kn.symbol_id,kn.is_known,kn.jvm_name
		FROM symbols s JOIN files f ON f.id=s.file_id LEFT JOIN file_scope_evidence fs ON fs.file_id=s.file_id AND fs.repo_id=s.repo_id
		LEFT JOIN kotlin_jvm_callable_evidence ke ON ke.repo_id=s.repo_id AND ke.symbol_id=s.id
		LEFT JOIN kotlin_jvm_name_evidence kn ON kn.repo_id=s.repo_id AND kn.symbol_id=s.id
		WHERE s.repo_id=? AND f.language IN ('java','kotlin') AND f.is_deleted=0`, " AND s.name IN (%s)",
		[]any{repoID}, stringSliceToAny(nameList), true,
		func(rows *sql.Rows) error {
			var s javaScopeSymbol
			var alias int
			if err := rows.Scan(&s.id, &s.file, &s.name, &s.qname, &s.container, &s.kind, &s.signature, &s.visibility, &s.static, &s.pkg, &s.language, &s.arityMin, &s.arityMax, &s.jvmEvidence, &s.jvmKnown, &s.jvmArityMin, &s.jvmArityMax, &alias, &s.jvmNameAliases, &s.jvmSyntheticAliases, &s.jvmNameEvidence, &s.jvmNameKnown, &s.jvmName); err != nil {
				return err
			}
			s.jvmStaticAlias = alias != 0
			byQName[s.qname] = append(byQName[s.qname], s)
			byName[s.name] = append(byName[s.name], s)
			return nil
		}); err != nil {
		return 0, err
	}
	// Companion rows are owner evidence, not Java types. Load only companions
	// from Kotlin class files already selected by the edge's outer name. The
	// file_id lookup uses the existing symbol index and avoids a repo-wide scan.
	outerFiles := map[int64]struct{}{}
	for _, candidates := range byName {
		for _, candidate := range candidates {
			if candidate.language == "kotlin" && candidate.kind == "class" {
				outerFiles[candidate.file] = struct{}{}
			}
		}
	}
	if err := sqliteBatchedIDQuery(ctx, q, sortedIDs(outerFiles), `SELECT s.id,s.file_id,s.name,s.qualified_name,s.container_name,s.kind,s.signature,s.visibility,s.is_static,COALESCE(fs.package_name,''),f.language,s.arity_min,s.arity_max,
		EXISTS(SELECT 1 FROM scope_import_evidence ki WHERE ki.repo_id=s.repo_id AND ki.file_id=s.file_id AND ki.language='kotlin' AND ki.source_specifier='kotlin.jvm.JvmStatic' AND ki.local_name!='JvmStatic'),
		COALESCE((SELECT group_concat(ki.local_name, ',') FROM scope_import_evidence ki WHERE ki.repo_id=s.repo_id AND ki.file_id=s.file_id AND ki.language='kotlin' AND ki.source_specifier='kotlin.jvm.JvmName' AND ki.local_name!='JvmName' AND instr(s.signature,'@'||ki.local_name)>0),'')
		FROM symbols s JOIN files f ON f.id=s.file_id LEFT JOIN file_scope_evidence fs ON fs.file_id=s.file_id AND fs.repo_id=s.repo_id
		WHERE s.repo_id=? AND f.language='kotlin' AND s.kind='companion_object' AND f.is_deleted=0 AND s.file_id IN (`,
		[]any{repoID},
		func(scan func(...any) error) error {
			var s javaScopeSymbol
			var alias int
			if err := scan(&s.id, &s.file, &s.name, &s.qname, &s.container, &s.kind, &s.signature, &s.visibility, &s.static, &s.pkg, &s.language, &s.arityMin, &s.arityMax, &alias, &s.jvmNameAliases); err != nil {
				return err
			}
			s.jvmStaticAlias = alias != 0
			byQName[s.qname] = append(byQName[s.qname], s)
			byName[s.name] = append(byName[s.name], s)
			companions[kotlinJoin(s.pkg, s.container)] = append(companions[kotlinJoin(s.pkg, s.container)], s)
			return nil
		}); err != nil {
		return 0, err
	}
	// Kotlin file facades are source facts, not symbols. Each distinct owner
	// joins the lookup maps as an in-memory type candidate only, so Java's own
	// package/import rules select it and a same-spelling type makes it
	// ambiguous instead of being substituted.
	facades := map[string][]javaFacadePart{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT fs.file_id,fs.package_name,fs.jvm_facade_class,fs.jvm_multifile
		FROM file_scope_evidence fs JOIN files f ON f.id=fs.file_id AND f.repo_id=fs.repo_id
		WHERE fs.repo_id=? AND fs.language='kotlin' AND fs.jvm_facade_class!='' AND f.is_deleted=0`, " AND fs.jvm_facade_class IN (%s)",
		[]any{repoID}, stringSliceToAny(nameList), true,
		func(rows *sql.Rows) error {
			var part javaFacadePart
			var pkg, class string
			var multifile int
			if err := rows.Scan(&part.file, &pkg, &class, &multifile); err != nil {
				return err
			}
			part.multifile = multifile != 0
			owner := kotlinJoin(pkg, class)
			if len(facades[owner]) == 0 {
				f := javaScopeSymbol{name: class, qname: owner, container: pkg, pkg: pkg, kind: kotlinFileFacadeKind, language: "kotlin", visibility: "public"}
				byQName[owner] = append(byQName[owner], f)
				byName[class] = append(byName[class], f)
			}
			facades[owner] = append(facades[owner], part)
			return nil
		}); err != nil {
		return 0, err
	}
	// @JvmName can make a declaration with a different Kotlin source name share
	// this Java spelling. Load renamed functions only from the already-resolved
	// object, companion, and facade files so those declarations can participate
	// in uniqueness without scanning the repository's Kotlin function
	// population. Only rows carrying name evidence -- or, for a file an older
	// parser wrote, visible JvmName syntax -- are read; every other function in
	// those files keeps its source name and is already in the lookup maps.
	callNames := map[string]struct{}{}
	for _, e := range edges {
		name := e.name
		if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
			name = name[dot+1:]
		}
		if name != "" {
			callNames[name] = struct{}{}
		}
	}
	ownerFiles := map[int64]struct{}{}
	for _, candidates := range byName {
		for _, candidate := range candidates {
			if candidate.language == "kotlin" && (candidate.kind == "class" || candidate.kind == "object") {
				ownerFiles[candidate.file] = struct{}{}
			}
		}
	}
	for _, parts := range facades {
		for _, part := range parts {
			ownerFiles[part.file] = struct{}{}
		}
	}
	if files := sortedIDs(ownerFiles); len(files) != 0 && len(callNames) != 0 {
		allCallNames := sortedKeys(callNames)
		if err := sqliteBatchedIDQuery(ctx, q, files, `SELECT s.id,s.file_id,s.name,s.qualified_name,s.container_name,s.kind,s.signature,s.visibility,s.is_static,COALESCE(fs.package_name,''),f.language,s.arity_min,s.arity_max,ke.symbol_id,ke.is_known,ke.jvm_arity_min,ke.jvm_arity_max,
			EXISTS(SELECT 1 FROM scope_import_evidence ki WHERE ki.repo_id=s.repo_id AND ki.file_id=s.file_id AND ki.language='kotlin' AND ki.source_specifier='kotlin.jvm.JvmStatic' AND ki.local_name!='JvmStatic'),
			COALESCE((SELECT group_concat(ki.local_name, ',') FROM scope_import_evidence ki WHERE ki.repo_id=s.repo_id AND ki.file_id=s.file_id AND ki.language='kotlin' AND ki.source_specifier='kotlin.jvm.JvmName' AND ki.local_name!='JvmName' AND instr(s.signature,'@'||ki.local_name)>0),'') AS name_aliases,
			COALESCE((SELECT group_concat(ki.local_name, ',') FROM scope_import_evidence ki WHERE ki.repo_id=s.repo_id AND ki.file_id=s.file_id AND ki.language='kotlin' AND ki.source_specifier='kotlin.jvm.JvmSynthetic' AND ki.local_name!='JvmSynthetic' AND instr(s.signature,'@'||ki.local_name)>0),''),
			kn.symbol_id,kn.is_known,kn.jvm_name
			FROM symbols s JOIN files f ON f.id=s.file_id LEFT JOIN file_scope_evidence fs ON fs.file_id=s.file_id AND fs.repo_id=s.repo_id
			LEFT JOIN kotlin_jvm_callable_evidence ke ON ke.repo_id=s.repo_id AND ke.symbol_id=s.id
			LEFT JOIN kotlin_jvm_name_evidence kn ON kn.repo_id=s.repo_id AND kn.symbol_id=s.id
			WHERE s.repo_id=? AND f.language='kotlin' AND s.kind='function' AND f.is_deleted=0
			AND (kn.symbol_id IS NOT NULL OR instr(s.signature,'JvmName')>0 OR name_aliases!='') AND s.file_id IN (`,
			[]any{repoID}, func(scan func(...any) error) error {
				var s javaScopeSymbol
				var staticAlias int
				if err := scan(&s.id, &s.file, &s.name, &s.qname, &s.container, &s.kind, &s.signature, &s.visibility, &s.static, &s.pkg, &s.language, &s.arityMin, &s.arityMax, &s.jvmEvidence, &s.jvmKnown, &s.jvmArityMin, &s.jvmArityMax, &staticAlias, &s.jvmNameAliases, &s.jvmSyntheticAliases, &s.jvmNameEvidence, &s.jvmNameKnown, &s.jvmName); err != nil {
					return err
				}
				s.jvmStaticAlias = staticAlias != 0
				for _, javaName := range kotlinJavaNameCandidates(s, callNames, allCallNames) {
					if s.container == s.pkg {
						for _, parts := range facades {
							for _, part := range parts {
								if part.file == s.file {
									appendJavaNameCandidate(byQName, kotlinJoin(s.pkg, javaName), s)
								}
							}
						}
					} else {
						appendJavaNameCandidate(byQName, kotlinJoin(s.pkg, s.container)+"."+javaName, s)
					}
				}
				return nil
			}); err != nil {
			return 0, err
		}
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
			dst, strategy = javaMember(e, byQName, byName, imports, facades, companions)
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
	return javaTypeInScope(eName, pkg, container, byQName, byName, imps, true)
}

// javaTypeInScope is javaType; mayInherit false states that the caller's
// classes inherit no member type (graph.JavaNoInheritedTypeEvidence), so a
// member type declared outside them does not block the name.
func javaTypeInScope(eName, pkg, container string, byQName map[string][]javaScopeSymbol, byName map[string][]javaScopeSymbol, imps []javaScopeImport, mayInherit bool) (javaScopeSymbol, bool, string) {
	name := eName
	if i := strings.LastIndex(name, "."); i >= 0 { // fully qualified or nested spelling
		var exact []javaScopeSymbol
		for _, s := range byQName[name] {
			if javaTypeIdentityEligible(s) {
				exact = append(exact, s)
			}
		}
		return javaUniqueVisible(exact, pkg, "java_package_scope")
	}
	// A member type of an enclosing class shadows every import and package
	// type (JLS 6.4.1), but the graph cannot tell which one is meant: the
	// edge may sit in an anonymous or local class body, credited to the
	// enclosing method, whose own inherited or local type hides it, and a
	// member type a nearer class inherits is not recorded either. So any
	// enclosing member of that name leaves the name unresolved.
	for rel := container; rel != ""; {
		for _, s := range byName[name] {
			if javaTypeIdentityEligible(s) && (s.qname == pkg+"."+rel+"."+name || s.qname == rel+"."+name) {
				return javaScopeSymbol{}, false, ""
			}
		}
		dot := strings.LastIndex(rel, ".")
		if dot < 0 {
			break
		}
		rel = rel[:dot]
	}
	// Supertypes are not recorded, so any non-private member type of that
	// name the caller's classes could inherit may be the one meant. A member
	// recorded as package-private may be an interface member, which is
	// implicitly public, so packages are not compared. Refusing costs every
	// simple name that some member type also uses.
	// A member type a single-type import names is exempt: an inherited member
	// type that hides the import is a different type of that name, which
	// still refuses.
	// A single-static-import of the name may import a member type, which
	// shadows every package and on-demand type of that name (JLS 6.4.1,
	// 7.5.3); a static import is not resolved to a type here, so the name
	// stays unresolved.
	// Import evidence that is no dotted name (a declaration with a syntax
	// error, or a v9-or-earlier parse of one spelled with spaces or comments
	// inside its name) carries no usable local name; it may name this one, so
	// it refuses.
	imported := ""
	for _, i := range imps {
		if !i.wildcard && !javaDottedName(i.source) {
			return javaScopeSymbol{}, false, ""
		}
		if i.static && !i.wildcard && i.local == name {
			return javaScopeSymbol{}, false, ""
		}
		if !i.static && !i.wildcard && i.local == name {
			imported = i.source
		}
	}
	for _, s := range byName[name] {
		if mayInherit && javaTypeIdentityEligible(s) && s.qname != s.name && s.qname != s.pkg+"."+s.name && s.visibility != "private" && s.qname != imported {
			return javaScopeSymbol{}, false, ""
		}
	}
	var c []javaScopeSymbol
	for _, s := range byName[name] {
		if !javaTypeIdentityEligible(s) {
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
				if javaTypeIdentityEligible(s) {
					c = append(c, s)
				}
			}
			return javaUniqueVisible(c, pkg, "java_import_scope")
		}
		if i.wildcard {
			for _, s := range byQName[i.source+"."+name] {
				if javaTypeIdentityEligible(s) {
					c = append(c, s)
				}
			}
		}
	}
	return javaUniqueVisible(c, pkg, "java_package_scope")
}

// javaDottedName reports whether s is Java identifiers joined by dots, with
// nothing else between them.
func javaDottedName(s string) bool {
	for _, seg := range strings.Split(s, ".") {
		if seg == "" {
			return false
		}
		for k, r := range seg {
			if r != '_' && r != '$' && !unicode.IsLetter(r) && (k == 0 || !unicode.IsDigit(r)) {
				return false
			}
		}
	}
	return true
}

func javaUniqueVisible(c []javaScopeSymbol, pkg, strategy string) (javaScopeSymbol, bool, string) {
	var out javaScopeSymbol
	n := 0
	for _, s := range c {
		if javaVisibleToJava(s, pkg) {
			out = s
			n++
		}
	}
	if n != 1 {
		return javaScopeSymbol{}, false, ""
	}
	return out, true, strategy
}
func javaTypeIdentityEligible(s javaScopeSymbol) bool {
	return s.language == "java" && s.kind == "type" || s.language == "kotlin" && (s.kind == "class" || s.kind == "object" || s.kind == kotlinFileFacadeKind)
}

func javaVisibleToJava(s javaScopeSymbol, fromPkg string) bool {
	if s.language == "kotlin" {
		return s.visibility == "" || s.visibility == "public"
	}
	return javaVisible(s, fromPkg, s.pkg)
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

// javaConstructorAccepts reports whether a constructor declared with the
// parser's AST parameter count accepts a call with the AST argument count.
// Either count missing -- a recovery error, or a graph from a parser that did
// not record them -- accepts nothing. A nil maximum is a trailing varargs
// parameter. Argument types are not modelled, so every constructor the count
// admits stays a competitor.
func javaConstructorAccepts(s javaScopeSymbol, call sql.NullInt64) bool {
	if !call.Valid || !s.arityMin.Valid || call.Int64 < s.arityMin.Int64 {
		return false
	}
	return !s.arityMax.Valid || call.Int64 <= s.arityMax.Int64
}

// javaUnqualifiedCreation reports whether a construction's source text starts
// with the `new` keyword. A qualified creation (`outer.new Inner()`,
// `this.new Inner()`, `Outer.this.new Inner()`) starts with its qualifier and
// constructs a member class of the qualifier's type (JLS 15.9.1), which a
// lookup of the bare class name does not find; it stays unresolved.
func javaUnqualifiedCreation(evidence string) bool {
	rest, ok := strings.CutPrefix(evidence, "new")
	if !ok {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return r != '_' && r != '$' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// javaOwnMemberType returns the member type named e.name that the calling
// class declares, for a construction the adapter marked as naming one. That
// declaration hides every other type of the name, so its accessibility and
// the caller's supertypes do not matter. A caller whose container the
// adapter collapsed to the package (a class named like its package) is not
// told apart from a top-level class there, so it finds nothing.
func javaOwnMemberType(e javaScopeEdge, byQName map[string][]javaScopeSymbol) (javaScopeSymbol, bool) {
	if e.container == "" || e.container == e.pkg {
		return javaScopeSymbol{}, false
	}
	var out javaScopeSymbol
	n := 0
	for _, s := range byQName[javaEdgeOwner(e)+"."+e.name] {
		if s.language == "java" && s.kind == "type" && s.container == e.container {
			out = s
			n++
		}
	}
	return out, n == 1
}

func javaConstructor(e javaScopeEdge, byQName map[string][]javaScopeSymbol, byName map[string][]javaScopeSymbol, imps map[int64][]javaScopeImport) (javaScopeSymbol, string) {
	var t javaScopeSymbol
	var ok bool
	switch {
	case e.evidence == graph.JavaOwnMemberTypeEvidence:
		t, ok = javaOwnMemberType(e, byQName)
	case e.evidence == graph.JavaNoInheritedTypeEvidence:
		// A caller whose container the adapter collapsed to the package is
		// not told apart from a top-level class, so its enclosing member
		// types cannot be checked; it finds nothing.
		if e.container != "" && e.container != e.pkg {
			t, ok, _ = javaTypeInScope(e.name, e.pkg, e.container, byQName, byName, imps[e.file], false)
		}
	case e.evidence == graph.JavaLocalTypeScopeEvidence || !javaUnqualifiedCreation(e.evidence):
	default:
		t, ok, _ = javaType(e.name, e.pkg, e.container, byQName, byName, imps[e.file])
	}
	if !ok {
		return javaScopeSymbol{}, ""
	}
	if t.language != "java" {
		return javaScopeSymbol{}, ""
	}
	var out javaScopeSymbol
	n := 0
	for _, s := range byQName[t.qname+"."+t.name] {
		if s.kind == "constructor" && javaConstructorAccepts(s, e.callArity) && javaVisibleFrom(s, e.pkg, t.pkg, javaEdgeOwner(e) == t.qname) {
			out = s
			n++
		}
	}
	if n != 1 {
		return javaScopeSymbol{}, ""
	}
	return out, "java_constructor"
}
func javaMember(e javaScopeEdge, byQName map[string][]javaScopeSymbol, byName map[string][]javaScopeSymbol, imps map[int64][]javaScopeImport, facades map[string][]javaFacadePart, companions map[string][]javaScopeSymbol) (javaScopeSymbol, string) {
	name := e.name
	if name == "super." || strings.HasPrefix(name, "super.") {
		return javaScopeSymbol{}, ""
	}
	if e.evidence == graph.JavaCallNestedClassScopeEvidence || e.evidence == graph.JavaLocalTypeScopeEvidence {
		// The call's own class is one the adapter does not model, and its
		// members shadow every enclosing class and static import; or its
		// owner names a local type, which hides every recorded type.
		return javaScopeSymbol{}, ""
	}
	if strings.HasPrefix(name, "this.") {
		name = strings.TrimPrefix(name, "this.")
		s, _, strategy := javaMethods(javaEdgeOwner(e), name, e.pkg, javaEdgeOwner(e), byQName, "java_package_scope", false)
		return s, strategy
	}
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		ownerName, memberName := name[:dot], name[dot+1:]
		if strings.HasSuffix(ownerName, ".INSTANCE") {
			owner, ok, ownerStrategy := javaType(strings.TrimSuffix(ownerName, ".INSTANCE"), e.pkg, e.container, byQName, byName, imps[e.file])
			if !ok || owner.language != "kotlin" || owner.kind != "object" {
				return javaScopeSymbol{}, ""
			}
			return kotlinObjectMember(owner, memberName, e, byQName, ownerStrategy, false)
		}
		if fieldDot := strings.LastIndex(ownerName, "."); fieldDot >= 0 {
			outerName, fieldName := ownerName[:fieldDot], ownerName[fieldDot+1:]
			if outer, ok, strategy := javaType(outerName, e.pkg, e.container, byQName, byName, imps[e.file]); ok && outer.language == "kotlin" && outer.kind == "class" {
				if companion, unique := kotlinOwnedCompanion(outer, fieldName, e.pkg, companions); unique {
					return kotlinCompanionMember(companion, memberName, e, byQName, strategy, false)
				}
				// A nested object or class with the same spelling is not evidence
				// for the companion field ABI.
				return javaScopeSymbol{}, ""
			}
		}
		owner, ok, ownerStrategy := javaType(ownerName, e.pkg, e.container, byQName, byName, imps[e.file])
		if !ok {
			return javaScopeSymbol{}, ""
		}
		if owner.language == "kotlin" {
			switch owner.kind {
			case "object":
				return kotlinObjectMember(owner, memberName, e, byQName, ownerStrategy, true)
			case "class":
				return kotlinCompanionStaticMember(owner, memberName, e, byQName, companions, ownerStrategy)
			case kotlinFileFacadeKind:
				return kotlinFacadeMember(owner, memberName, e, byQName, facades[owner.qname], ownerStrategy)
			}
			return javaScopeSymbol{}, ""
		}
		s, _, strategy := javaMethods(owner.qname, memberName, e.pkg, javaEdgeOwner(e), byQName, ownerStrategy, true)
		return s, strategy
	}
	if s, ok, str := javaMethods(javaEdgeOwner(e), name, e.pkg, javaEdgeOwner(e), byQName, "java_package_scope", false); ok {
		return s, str
	}
	// A method of that name declared by the calling class or any class
	// enclosing it is in scope and shadows every static import (JLS 6.4.1).
	// The call stays unresolved rather than binding an enclosing class's
	// method: whether it is callable from here is not modelled.
	for rel := e.container; rel != ""; {
		owner := javaEdgeOwner(javaScopeEdge{pkg: e.pkg, container: rel})
		for _, s := range byQName[owner+"."+name] {
			if s.kind == "function" {
				return javaScopeSymbol{}, ""
			}
		}
		dot := strings.LastIndex(rel, ".")
		if dot < 0 {
			break
		}
		rel = rel[:dot]
	}
	// A method of that name the calling class or an enclosing class inherits
	// is in scope too and shadows every static import, whatever its arity
	// (JLS 6.4.1, 15.12.1). Supertypes are not recorded, so a static import
	// binds only a call whose classes spell no supertype clause and so
	// inherit only java.lang.Object, and only a name no Object method uses.
	if e.evidence != graph.JavaNoSupertypeCallEvidence || javaObjectMethodName(name) {
		return javaScopeSymbol{}, ""
	}
	explicitStaticOwners := map[string]struct{}{}
	for _, i := range imps[e.file] {
		if i.static && !i.wildcard && i.local == name {
			p := strings.LastIndex(i.source, ".")
			if p < 0 || i.source[p+1:] != name {
				return javaScopeSymbol{}, ""
			}
			explicitStaticOwners[i.source[:p]] = struct{}{}
		}
	}
	if len(explicitStaticOwners) > 1 {
		return javaScopeSymbol{}, ""
	}
	var staticCandidates []javaScopeSymbol
	for _, i := range imps[e.file] {
		if !i.static {
			continue
		}
		if !i.wildcard && i.local == name {
			owner := i.source[:strings.LastIndex(i.source, ".")]
			if target := javaStaticImportMember(e, owner, name, byQName, byName, facades, companions); target.id != 0 {
				staticCandidates = append(staticCandidates, target)
			}
		}
		if i.static && i.wildcard {
			for _, s := range byQName[i.source+"."+name] {
				if s.language == "java" && s.kind == "function" && s.static.Valid && s.static.Int64 != 0 && javaVisible(s, e.pkg, s.pkg) {
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

// javaObjectMethodName reports whether name is a method java.lang.Object
// declares, which every class and interface has in scope (JLS 4.3.2, 9.2).
func javaObjectMethodName(name string) bool {
	switch name {
	case "clone", "equals", "finalize", "getClass", "hashCode", "notify", "notifyAll", "toString", "wait":
		return true
	}
	return false
}

// javaStaticImportMember follows an explicit import's exact JVM owner. In
// particular, Kotlin declarations participate only through their established
// facade/object/companion ABI rules; bare-name candidates remain inadmissible.
func javaStaticImportMember(e javaScopeEdge, ownerName, name string, byQName, byName map[string][]javaScopeSymbol, facades map[string][]javaFacadePart, companions map[string][]javaScopeSymbol) javaScopeSymbol {
	owner, ok, strategy := javaType(ownerName, e.pkg, e.container, byQName, byName, nil)
	if !ok {
		return javaScopeSymbol{}
	}
	switch {
	case owner.language == "java":
		s, ok, _ := javaMethods(owner.qname, name, e.pkg, javaEdgeOwner(e), byQName, strategy, true)
		if ok {
			return s
		}
	case owner.kind == kotlinFileFacadeKind:
		s, _ := kotlinFacadeMember(owner, name, e, byQName, facades[owner.qname], strategy)
		return s
	case owner.kind == "object":
		s, _ := kotlinObjectMember(owner, name, e, byQName, strategy, true)
		return s
	case owner.kind == "class":
		s, _ := kotlinCompanionStaticMember(owner, name, e, byQName, companions, strategy)
		return s
	}
	return javaScopeSymbol{}
}

func kotlinOwnedCompanion(outer javaScopeSymbol, field, pkg string, companions map[string][]javaScopeSymbol) (javaScopeSymbol, bool) {
	wantContainer := strings.TrimPrefix(outer.qname, outer.pkg+".")
	var out javaScopeSymbol
	n := 0
	for _, s := range companions[outer.qname] {
		if s.language == "kotlin" && s.kind == "companion_object" && s.name == field && s.qname == outer.qname+"."+field && s.container == wantContainer && javaVisibleToJava(s, pkg) {
			out = s
			n++
		}
	}
	return out, n == 1
}

func kotlinCompanionStaticMember(outer javaScopeSymbol, name string, e javaScopeEdge, byQName map[string][]javaScopeSymbol, companions map[string][]javaScopeSymbol, strategy string) (javaScopeSymbol, string) {
	owned := companions[outer.qname]
	var companion javaScopeSymbol
	count := 0
	for _, candidate := range owned {
		if candidate.kind == "companion_object" && candidate.qname == outer.qname+"."+candidate.name && candidate.container == strings.TrimPrefix(outer.qname, outer.pkg+".") && javaVisibleToJava(candidate, e.pkg) {
			companion = candidate
			count++
		}
	}
	if count != 1 {
		return javaScopeSymbol{}, ""
	}
	return kotlinCompanionMember(companion, name, e, byQName, strategy, true)
}

func kotlinCompanionMember(companion javaScopeSymbol, name string, e javaScopeEdge, byQName map[string][]javaScopeSymbol, strategy string, requireJvmStatic bool) (javaScopeSymbol, string) {
	if javaNoArgCall(e) && !kotlinNeedsJVMEvidenceClassification(byQName[companion.qname+"."+name]) {
		var out javaScopeSymbol
		n := 0
		for _, s := range byQName[companion.qname+"."+name] {
			if s.language != "kotlin" || s.kind != "function" || s.container != strings.TrimPrefix(companion.qname, companion.pkg+".") || !javaVisibleToJava(s, e.pkg) {
				continue
			}
			out = s
			n++
			if !kotlinFacadeCallable(s.name, s.signature) || kotlinHasAliasedAnnotation(s.signature, s.jvmSyntheticAliases) || !kotlinNoArgFunction(s.signature) || requireJvmStatic && !kotlinHasJvmStatic(s.signature) {
				return javaScopeSymbol{}, ""
			}
		}
		if n != 1 {
			return javaScopeSymbol{}, ""
		}
		return out, strategy
	}
	var out javaScopeSymbol
	n := 0
	unknown := false
	for _, s := range byQName[companion.qname+"."+name] {
		if s.language != "kotlin" || s.kind != "function" || s.container != strings.TrimPrefix(companion.qname, companion.pkg+".") {
			continue
		}
		staticMode := 2 // companion field calls support ordinary and @JvmStatic members
		if requireJvmStatic {
			staticMode = 1
		}
		if status := kotlinCallableArityStatus(s, e, name, staticMode); status < 0 {
			unknown = true
		} else if status > 0 {
			out = s
			n++
		}
	}
	if unknown || n != 1 {
		return javaScopeSymbol{}, ""
	}
	return out, strategy
}

func kotlinObjectMember(owner javaScopeSymbol, name string, e javaScopeEdge, byQName map[string][]javaScopeSymbol, strategy string, requireJvmStatic bool) (javaScopeSymbol, string) {
	if javaNoArgCall(e) && !kotlinNeedsJVMEvidenceClassification(byQName[owner.qname+"."+name]) {
		var out javaScopeSymbol
		n := 0
		for _, s := range byQName[owner.qname+"."+name] {
			if s.language != "kotlin" || s.kind != "function" || s.container != strings.TrimPrefix(owner.qname, owner.pkg+".") || !javaVisibleToJava(s, e.pkg) {
				continue
			}
			out = s
			n++
			jvmStatic := kotlinHasJvmStatic(s.signature)
			if kotlinExtensionSignature(s.name, s.signature) || kotlinHasJvmName(s.signature) || kotlinHasJvmSynthetic(s.signature) || kotlinHasAliasedAnnotation(s.signature, s.jvmSyntheticAliases) || !kotlinNoArgFunction(s.signature) || jvmStatic != requireJvmStatic {
				return javaScopeSymbol{}, ""
			}
		}
		if n != 1 {
			return javaScopeSymbol{}, ""
		}
		return out, strategy
	}
	var out javaScopeSymbol
	n := 0
	unknown := false
	for _, s := range byQName[owner.qname+"."+name] {
		if s.language != "kotlin" || s.kind != "function" || s.container != strings.TrimPrefix(owner.qname, owner.pkg+".") {
			continue
		}
		staticMode := 0
		if requireJvmStatic {
			staticMode = 1
		}
		if status := kotlinCallableArityStatus(s, e, name, staticMode); status < 0 {
			unknown = true
		} else if status > 0 {
			out = s
			n++
		}
	}
	if unknown || n != 1 {
		return javaScopeSymbol{}, ""
	}
	return out, strategy
}

// kotlinFileFacadeKind marks the in-memory facade owner candidate. It never
// reaches the symbols table and is never a destination.
const kotlinFileFacadeKind = "kotlin_file_facade"

type javaFacadePart struct {
	file      int64
	multifile bool
}

// kotlinFacadeMember resolves `Facade.name()` to the one top-level Kotlin
// function declared in a file whose persisted facade is exactly owner. Parts
// sharing an owner must all be @JvmMultifileClass, or the JVM classes clash.
func kotlinFacadeMember(owner javaScopeSymbol, name string, e javaScopeEdge, byQName map[string][]javaScopeSymbol, parts []javaFacadePart, strategy string) (javaScopeSymbol, string) {
	files := make(map[int64]struct{}, len(parts))
	for _, part := range parts {
		if len(parts) > 1 && !part.multifile {
			return javaScopeSymbol{}, ""
		}
		files[part.file] = struct{}{}
	}
	if javaNoArgCall(e) && !kotlinNeedsJVMEvidenceClassification(byQName[kotlinJoin(owner.pkg, name)]) {
		var out javaScopeSymbol
		n := 0
		for _, s := range byQName[kotlinJoin(owner.pkg, name)] {
			if _, inFacade := files[s.file]; inFacade && s.language == "kotlin" && s.kind == "function" && s.container == s.pkg {
				out = s
				n++
			}
		}
		if n != 1 || !javaVisibleToJava(out, e.pkg) || !kotlinFacadeCallable(out.name, out.signature) || !kotlinNoArgFunction(out.signature) {
			return javaScopeSymbol{}, ""
		}
		return out, strategy
	}
	var out javaScopeSymbol
	n := 0
	unknown := false
	for _, s := range byQName[kotlinJoin(owner.pkg, name)] {
		if _, inFacade := files[s.file]; !inFacade || s.language != "kotlin" || s.kind != "function" || s.container != s.pkg {
			continue
		}
		if status := kotlinCallableArityStatus(s, e, name, 0); status < 0 {
			unknown = true
		} else if status > 0 {
			out = s
			n++
		}
	}
	if unknown || n != 1 {
		return javaScopeSymbol{}, ""
	}
	return out, strategy
}

// kotlinNeedsJVMEvidenceClassification reports whether a zero-argument call
// must use the evidence-aware candidate status instead of the historical
// signature path: some candidate carries JVM arity or JVM name evidence, or
// older-parser rename syntax (including an aliased JvmName) that only the
// status path can refuse.
func kotlinNeedsJVMEvidenceClassification(candidates []javaScopeSymbol) bool {
	for _, candidate := range candidates {
		if candidate.jvmEvidence.Valid || candidate.jvmNameEvidence.Valid || candidate.jvmNameAliases != "" {
			return true
		}
	}
	return false
}

// kotlinCallableArityStatus returns 1 for a fixed-arity ABI candidate, 0 for a
// candidate proven irrelevant to this Java spelling, and -1 when unsupported
// syntax could still participate and must veto selection.
func kotlinCallableArityStatus(s javaScopeSymbol, e javaScopeEdge, javaName string, staticMode int) int {
	if kotlinHasJvmSynthetic(s.signature) || kotlinHasAliasedAnnotation(s.signature, s.jvmSyntheticAliases) {
		return 0
	}
	if s.visibility == "private" {
		return 0
	}
	if s.visibility != "" && s.visibility != "public" {
		return -1
	}
	if status := kotlinJVMNameStatus(s, javaName); status <= 0 {
		return status
	}
	if kotlinExtensionSignature(s.name, s.signature) || !kotlinCallableShape(s.name, s.signature) {
		return -1
	}
	if !javaVisibleToJava(s, e.pkg) {
		return -1
	}
	jvmStatic := kotlinHasJvmStatic(s.signature)
	if staticMode != 2 && (staticMode == 1) != jvmStatic {
		if s.jvmStaticAlias {
			return -1
		}
		return 0
	}
	if !e.callArity.Valid {
		return -1
	}
	if s.jvmEvidence.Valid {
		if s.jvmKnown.Int64 == 0 || !s.jvmArityMin.Valid || !s.jvmArityMax.Valid {
			return -1
		}
		if e.callArity.Int64 < s.jvmArityMin.Int64 || e.callArity.Int64 > s.jvmArityMax.Int64 {
			return 0
		}
		return 1
	}
	if e.callArity.Int64 == 0 {
		if kotlinNoArgFunction(s.signature) {
			return 1
		}
		return 0
	}
	if e.callArity.Int64 < 0 || !s.arityMin.Valid || !s.arityMax.Valid || s.arityMin.Int64 != s.arityMax.Int64 {
		return -1
	}
	if e.callArity.Int64 != s.arityMin.Int64 {
		return 0
	}
	return 1
}

func kotlinHasAliasedAnnotation(signature, aliases string) bool {
	for _, alias := range strings.Split(aliases, ",") {
		if alias != "" && kotlinAnnotationPresent(signature, "@"+alias) {
			return true
		}
	}
	return false
}

func kotlinHasJvmSynthetic(signature string) bool {
	fun := strings.Index(signature, "fun ")
	if fun < 0 {
		return false
	}
	prefix := signature[:fun]
	return kotlinAnnotationPresent(prefix, "@JvmSynthetic") || kotlinAnnotationPresent(prefix, "@kotlin.jvm.JvmSynthetic")
}

func kotlinAnnotationPresent(source, spelling string) bool {
	for start := 0; ; {
		rel := strings.Index(source[start:], spelling)
		if rel < 0 {
			return false
		}
		end := start + rel + len(spelling)
		if end == len(source) || source[end] == '(' || source[end] == '[' || source[end] == ':' || source[end] == ' ' || source[end] == '\t' || source[end] == '\r' || source[end] == '\n' {
			return true
		}
		start = end
	}
}

// kotlinJVMNameStatus classifies a candidate's Java method spelling: 1 when
// it is javaName, 0 when it provably is not, -1 when it cannot be known.
// Persisted name evidence is authoritative. A row without it keeps its source
// name, unless an older parser wrote it and JvmName syntax is visible: that
// legacy signature reading can only refuse, never select a renamed call.
func kotlinJVMNameStatus(s javaScopeSymbol, javaName string) int {
	if s.jvmNameEvidence.Valid {
		if s.jvmNameKnown.Int64 == 0 || !s.jvmName.Valid || s.jvmName.String == "" {
			return -1
		}
		if s.jvmName.String != javaName || !javaSourceMethodName(javaName) {
			return 0
		}
		return 1
	}
	if renamed, known := kotlinJvmNameForSymbol(s); known {
		if renamed != javaName {
			return 0
		}
		return -1
	}
	if s.jvmNameAliases != "" || kotlinHasJvmName(s.signature) {
		return -1
	}
	if s.name != javaName {
		return 0
	}
	return 1
}

// javaReservedWords are the Java 17 keywords and literals that can never be a
// method name in Java source, although the JVM accepts them. Contextual
// keywords such as var, record and yield remain valid qualified method names.
var javaReservedWords = map[string]struct{}{
	"_": {}, "abstract": {}, "assert": {}, "boolean": {}, "break": {}, "byte": {}, "case": {}, "catch": {},
	"char": {}, "class": {}, "const": {}, "continue": {}, "default": {}, "do": {}, "double": {}, "else": {},
	"enum": {}, "extends": {}, "false": {}, "final": {}, "finally": {}, "float": {}, "for": {}, "goto": {},
	"if": {}, "implements": {}, "import": {}, "instanceof": {}, "int": {}, "interface": {}, "long": {},
	"native": {}, "new": {}, "null": {}, "package": {}, "private": {}, "protected": {}, "public": {},
	"return": {}, "short": {}, "static": {}, "strictfp": {}, "super": {}, "switch": {}, "synchronized": {},
	"this": {}, "throw": {}, "throws": {}, "transient": {}, "true": {}, "try": {}, "void": {}, "volatile": {},
	"while": {},
}

// javaSourceMethodName reports whether a JVM method name can be spelled as a
// method call in Java source, within the conservative ASCII identifier subset.
func javaSourceMethodName(name string) bool {
	if _, reserved := javaReservedWords[name]; reserved {
		return false
	}
	return kotlinPlainJavaName(name)
}

func kotlinJvmName(signature string) (string, bool) {
	fun := strings.Index(signature, "fun ")
	if fun < 0 {
		return "", false
	}
	prefix := signature[:fun]
	for _, annotation := range []string{"@JvmName(\"", "@kotlin.jvm.JvmName(\""} {
		if at := strings.Index(prefix, annotation); at >= 0 {
			value := prefix[at+len(annotation):]
			end := strings.IndexByte(value, '"')
			if end >= 0 && !strings.ContainsAny(value[:end], "\\$") && kotlinPlainJavaName(value[:end]) {
				return value[:end], true
			}
			return "", false
		}
	}
	return "", false
}

func kotlinJvmNameForSymbol(s javaScopeSymbol) (string, bool) {
	if name, known := kotlinJvmName(s.signature); known {
		return name, true
	}
	for _, alias := range strings.Split(s.jvmNameAliases, ",") {
		if alias == "" {
			continue
		}
		annotation := "@" + alias + "(\""
		if at := strings.Index(s.signature, annotation); at >= 0 {
			value := s.signature[at+len(annotation):]
			end := strings.IndexByte(value, '"')
			if end >= 0 && !strings.ContainsAny(value[:end], "\\$") && kotlinPlainJavaName(value[:end]) {
				return value[:end], true
			}
			return "", false
		}
	}
	return "", false
}

// kotlinJavaNameCandidates lists the Java spellings under which a renamed
// function joins the lookup maps: its exact known JVM name, or every call name
// when the name is unknown so the declaration vetoes instead of vanishing.
func kotlinJavaNameCandidates(s javaScopeSymbol, callNames map[string]struct{}, allCallNames []string) []string {
	if s.jvmNameEvidence.Valid {
		if s.jvmNameKnown.Int64 == 1 && s.jvmName.Valid {
			if _, called := callNames[s.jvmName.String]; called {
				return []string{s.jvmName.String}
			}
			return nil
		}
		return allCallNames
	}
	if name, known := kotlinJvmNameForSymbol(s); known {
		return []string{name}
	}
	if kotlinHasJvmName(s.signature) || s.jvmNameAliases != "" {
		return allCallNames
	}
	return nil
}

func appendJavaNameCandidate(byQName map[string][]javaScopeSymbol, qname string, candidate javaScopeSymbol) {
	for _, existing := range byQName[qname] {
		if existing.id == candidate.id {
			return
		}
	}
	byQName[qname] = append(byQName[qname], candidate)
}

func kotlinPlainJavaName(name string) bool {
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for _, c := range name {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// kotlinFacadeCallable excludes declarations whose Java-visible form is not
// the plain static `name()` modelled by the historical zero-argument path:
// renamed functions and every kotlinCallableShape exclusion.
func kotlinFacadeCallable(name, signature string) bool {
	return !kotlinHasJvmName(signature) && kotlinCallableShape(name, signature)
}

// kotlinCallableShape excludes declarations whose JVM method is not a plain
// call on the owner: extensions, synthetic, suspend, expect and reified
// functions. The JVM method name is classified separately.
func kotlinCallableShape(name, signature string) bool {
	fun := strings.Index(signature, "fun ")
	if fun < 0 || kotlinExtensionSignature(name, signature) || strings.Contains(signature, "reified ") {
		return false
	}
	for _, token := range strings.Fields(signature[:fun]) {
		if token == "suspend" || token == "expect" || strings.HasPrefix(token, "@JvmSynthetic") || strings.HasPrefix(token, "@kotlin.jvm.JvmSynthetic") {
			return false
		}
	}
	return true
}

// javaNoArgCall reports whether e passes no arguments. A call marked
// graph.JavaNoSupertypeCallEvidence keeps no call text, so its recorded
// argument count decides.
func javaNoArgCall(e javaScopeEdge) bool {
	if e.evidence == graph.JavaNoSupertypeCallEvidence {
		return e.callArity.Valid && e.callArity.Int64 == 0
	}
	return kotlinNoArgCall(e.evidence)
}

func kotlinNoArgCall(evidence string) bool {
	evidence = strings.TrimSpace(evidence)
	close := strings.LastIndexByte(evidence, ')')
	open := strings.LastIndexByte(evidence, '(')
	return close == len(evidence)-1 && open >= 0 && open < close && strings.TrimSpace(evidence[open+1:close]) == ""
}

func kotlinNoArgFunction(signature string) bool {
	fun := strings.Index(signature, "fun ")
	if fun < 0 {
		return false
	}
	open := strings.IndexByte(signature[fun:], '(')
	if open < 0 {
		return false
	}
	open += fun
	close := strings.IndexByte(signature[open+1:], ')')
	return close >= 0 && strings.TrimSpace(signature[open+1:open+1+close]) == ""
}

func kotlinHasJvmStatic(signature string) bool {
	fun := strings.Index(signature, "fun ")
	if fun < 0 {
		return false
	}
	for _, annotation := range strings.Fields(signature[:fun]) {
		if annotation == "@JvmStatic" || annotation == "@kotlin.jvm.JvmStatic" {
			return true
		}
	}
	return false
}

func kotlinHasJvmName(signature string) bool {
	fun := strings.Index(signature, "fun ")
	if fun < 0 {
		return false
	}
	for _, annotation := range strings.Fields(signature[:fun]) {
		if strings.HasPrefix(annotation, "@JvmName") || strings.HasPrefix(annotation, "@kotlin.jvm.JvmName") {
			return true
		}
	}
	return false
}

// javaEdgeOwner is the qualified name of the class whose body holds the
// call. The caller's container is stored relative to its package (`Caller`,
// `Outer.Inner`) while every member is declared under the package-qualified
// name, so the two are joined here rather than compared as spelled.
func javaEdgeOwner(e javaScopeEdge) string {
	// The adapter collapses a container equal to the package to the package
	// itself (a class named like its package declares `app.helper`).
	if e.pkg == "" || e.container == e.pkg {
		return e.container
	}
	return e.pkg + "." + e.container
}

func javaMethods(owner, name, pkg, caller string, byQName map[string][]javaScopeSymbol, strategy string, requireStatic bool) (javaScopeSymbol, bool, string) {
	var out javaScopeSymbol
	n := 0
	for _, s := range byQName[owner+"."+name] {
		if s.kind == "function" && (!requireStatic || (s.static.Valid && s.static.Int64 != 0)) && javaVisibleFrom(s, pkg, s.pkg, owner == caller) {
			out = s
			n++
		}
	}
	if n != 1 {
		return javaScopeSymbol{}, false, ""
	}
	return out, true, strategy
}

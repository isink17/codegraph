package store

import (
	"context"
	"database/sql"
	"slices"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

const csharpScopeVeto = "tmp_csharp_scope_veto"

type csharpScopeSymbol struct {
	id, file                                                    int64
	name, qname, container, kind, stable, signature, visibility string
	static                                                      sql.NullInt64
	arityMin, arityMax                                          sql.NullInt64
}

type csharpScopeEdge struct {
	id, file                                              int64
	name, srcStable, srcContainer, srcQName, srcNamespace string
	callArity                                             sql.NullInt64
	srcStatic                                             sql.NullInt64
}

type csharpScopeImport struct {
	file                                 int64
	source, imported, local, kind, owner string
	static                               bool
}

type csharpScopeBindings struct {
	unknown map[string]map[string]struct{}
	typed   map[string]map[string]map[string]struct{}
}

func resolveCSharpScope(ctx context.Context, q javaQuery, repoID int64, only map[int64]struct{}) (int, error) {
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+csharpScopeVeto+`(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	ids := sortedIDs(only)
	edges := []csharpScopeEdge{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.file_id,e.dst_name,src.stable_key,src.container_name,src.qualified_name,COALESCE(fs.package_name,''),e.call_arity,src.is_static
FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols src ON src.id=e.src_symbol_id
LEFT JOIN file_scope_evidence fs ON fs.repo_id=e.repo_id AND fs.file_id=e.file_id
WHERE e.repo_id=? AND f.language='csharp' AND e.dst_symbol_id IS NULL`, " AND e.id IN (%s)", []any{repoID}, int64SliceToAny(ids), len(only) > 0,
		func(rows *sql.Rows) error {
			var e csharpScopeEdge
			if err := rows.Scan(&e.id, &e.file, &e.name, &e.srcStable, &e.srcContainer, &e.srcQName, &e.srcNamespace, &e.callArity, &e.srcStatic); err != nil {
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

	byName := map[string][]csharpScopeSymbol{}
	byQName := map[string][]csharpScopeSymbol{}
	if err := sqliteBatchedQuery(ctx, q, `SELECT s.id,s.file_id,s.name,s.qualified_name,s.container_name,s.kind,s.stable_key,s.signature,s.visibility,s.is_static,s.arity_min,s.arity_max
FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.language='csharp' AND f.is_deleted=0`, "", []any{repoID}, nil, false,
		func(rows *sql.Rows) error {
			var s csharpScopeSymbol
			if err := rows.Scan(&s.id, &s.file, &s.name, &s.qname, &s.container, &s.kind, &s.stable, &s.signature, &s.visibility, &s.static, &s.arityMin, &s.arityMax); err != nil {
				return err
			}
			byName[s.name] = append(byName[s.name], s)
			byQName[s.qname] = append(byQName[s.qname], s)
			return nil
		}); err != nil {
		return 0, err
	}
	for i := range edges {
		edges[i].srcNamespace = csharpNamespaceForType(edges[i].srcContainer, byQName)
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
		i.file = f
		i.static = st != 0
		imports[f] = append(imports[f], i)
		return nil
	}); err != nil {
		return 0, err
	}
	// The index has no C# project or compilation membership fact. Keep global
	// using rows from other files as ambiguity evidence only; never import them
	// repo-wide as if every indexed source file shared one compilation.
	var globalImports []csharpScopeImport
	if err := sqliteBatchedQuery(ctx, q, `SELECT file_id,source_specifier,imported_name,local_name,import_kind,owner_module,is_static FROM scope_import_evidence WHERE repo_id=? AND language='csharp' AND import_kind IN ('global_alias','global_namespace','global_static')`, "", []any{repoID}, nil, false,
		func(rows *sql.Rows) error {
			var i csharpScopeImport
			var st int
			if err := rows.Scan(&i.file, &i.source, &i.imported, &i.local, &i.kind, &i.owner, &st); err != nil {
				return err
			}
			i.static = st != 0
			globalImports = append(globalImports, i)
			return nil
		}); err != nil {
		return 0, err
	}
	bindings := map[int64]csharpScopeBindings{}
	for file, rows := range imports {
		b := csharpScopeBindings{unknown: map[string]map[string]struct{}{}, typed: map[string]map[string]map[string]struct{}{}}
		for _, i := range rows {
			switch i.kind {
			case graph.ScopeImportLocalBinding:
				if b.unknown[i.owner] == nil {
					b.unknown[i.owner] = map[string]struct{}{}
				}
				b.unknown[i.owner][i.local] = struct{}{}
			case graph.ScopeImportTypedBinding:
				if b.typed[i.owner] == nil {
					b.typed[i.owner] = map[string]map[string]struct{}{}
				}
				if b.typed[i.owner][i.local] == nil {
					b.typed[i.owner][i.local] = map[string]struct{}{}
				}
				b.typed[i.owner][i.local][i.source] = struct{}{}
			}
		}
		bindings[file] = b
	}

	res := map[int64]struct {
		dst      int64
		strategy string
	}{}
	for _, e := range edges {
		if dst, strategy, ok := csharpResolveEdge(e, byName, byQName, imports[e.file], globalImports, bindings[e.file]); ok {
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

func csharpResolveEdge(e csharpScopeEdge, byName map[string][]csharpScopeSymbol, byQName map[string][]csharpScopeSymbol, imports, globalImports []csharpScopeImport, bindings csharpScopeBindings) (csharpScopeSymbol, string, bool) {
	name := e.name
	if name == "" || strings.HasPrefix(name, "base.") {
		return csharpScopeSymbol{}, "", false
	}
	parts := strings.Split(name, ".")
	method := parts[len(parts)-1]
	qualifier := strings.Join(parts[:len(parts)-1], ".")
	if qualifier != "" {
		head, _, _ := strings.Cut(qualifier, ".")
		globalTypeImportCanChangeLookup := true
		if head == qualifier {
			for level := e.srcNamespace; level != ""; {
				if csharpHasTypeAtNamespace(level, qualifier, byQName) {
					globalTypeImportCanChangeLookup = false
					break
				}
				for _, i := range imports {
					if i.kind == "alias" && i.owner == level && i.local == qualifier {
						globalTypeImportCanChangeLookup = false
						break
					}
				}
				if !globalTypeImportCanChangeLookup {
					break
				}
				if dot := strings.LastIndexByte(level, '.'); dot >= 0 {
					level = level[:dot]
				} else {
					level = ""
				}
			}
		}
		for _, i := range globalImports {
			switch i.kind {
			case "global_alias":
				if globalTypeImportCanChangeLookup && head == i.local {
					return csharpScopeSymbol{}, "", false
				}
			case "global_namespace":
				if globalTypeImportCanChangeLookup {
					for _, candidate := range byQName[i.source+"."+qualifier] {
						if candidate.kind == "type" {
							return csharpScopeSymbol{}, "", false
						}
					}
				}
			}
		}
	}
	shadowed := func(n string) bool {
		_, local := bindings.unknown[e.srcStable][n]
		_, typed := bindings.typed[e.srcStable][n]
		_, memberUnknown := bindings.unknown[e.srcContainer][n]
		_, memberTyped := bindings.typed[e.srcContainer][n]
		return local || typed || memberUnknown || memberTyped
	}
	valueType := func(owner, n string) (string, bool) {
		if _, ok := bindings.unknown[owner][n]; ok {
			return "", false
		}
		facts := bindings.typed[owner][n]
		if len(facts) != 1 {
			return "", false
		}
		for fact := range facts {
			return fact, true
		}
		return "", false
	}
	valueOwner := func(n string) (string, bool) {
		if _, ok := bindings.unknown[e.srcStable][n]; ok {
			return e.srcStable, true
		}
		if _, ok := bindings.typed[e.srcStable][n]; ok {
			return e.srcStable, true
		}
		if _, ok := bindings.unknown[e.srcContainer][n]; ok {
			return e.srcContainer, true
		}
		if _, ok := bindings.typed[e.srcContainer][n]; ok {
			return e.srcContainer, true
		}
		return "", false
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
		unknownArity := false
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
			if e.callArity.Valid {
				applicable, known := csharpArityApplicable(s, e.callArity)
				if !known {
					unknownArity = true
				} else if !applicable {
					continue
				}
			}
			out = s
			n++
		}
		return out, n == 1 && !unknownArity
	}
	chooseInstance := func(c []csharpScopeSymbol) (csharpScopeSymbol, bool) {
		var out csharpScopeSymbol
		n := 0
		unknownArity := false
		for _, s := range c {
			if s.kind != "function" || !visible(s) || !s.static.Valid || s.static.Int64 != 0 {
				continue
			}
			if e.callArity.Valid {
				applicable, known := csharpArityApplicable(s, e.callArity)
				if !known {
					unknownArity = true
				} else if !applicable {
					continue
				}
			}
			out, n = s, n+1
		}
		return out, n == 1 && !unknownArity
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
		if len(c) > 0 {
			c = csharpBareCallableCandidates(c, e.srcStatic)
			if out, ok := choose(c, false); ok {
				return out, "csharp_same_type", true
			}
			return csharpScopeSymbol{}, "", false
		}
		for _, i := range globalImports {
			if i.kind != "global_static" {
				continue
			}
			for _, candidate := range byQName[i.source+"."+method] {
				if candidate.kind == "function" && candidate.static.Valid && candidate.static.Int64 != 0 {
					return csharpScopeSymbol{}, "", false
				}
			}
		}
		var c2 []csharpScopeSymbol
		seen := map[string]struct{}{}
		for _, i := range imports {
			if !applicable(i) || !i.static || strings.HasPrefix(i.kind, "global_") {
				continue
			}
			typeAccessible := false
			for _, typeSymbol := range byQName[i.source] {
				if typeSymbol.kind == "type" && csharpTypeAccessibleByQName(typeSymbol.qname, e.srcContainer, byQName) {
					typeAccessible = true
					break
				}
			}
			if !typeAccessible {
				continue
			}
			for _, s := range byQName[i.source+"."+method] {
				if _, ok := seen[s.stable]; !ok {
					seen[s.stable] = struct{}{}
					c2 = append(c2, s)
				}
			}
		}
		if out, ok := choose(c2, true); ok {
			return out, "csharp_static_using", true
		}
		return csharpScopeSymbol{}, "", false
	}
	if qualifier == "this" {
		var c []csharpScopeSymbol
		for _, s := range byName[method] {
			if s.container == e.srcContainer {
				c = append(c, s)
			}
		}
		if out, ok := choose(c, false); ok {
			return out, "csharp_this_scope", true
		}
		return csharpScopeSymbol{}, "", false
	}
	if len(parts) == 2 {
		if owner, exists := valueOwner(qualifier); exists {
			typeSpelling, ok := valueType(owner, qualifier)
			if !ok {
				return csharpScopeSymbol{}, "", false
			}
			global := strings.HasPrefix(typeSpelling, "global::")
			typeQ := strings.TrimPrefix(typeSpelling, "global::")
			qname, _, ok := csharpResolveTypeIdentity(typeQ, e.srcNamespace, imports, byQName, e.srcContainer, global)
			if !ok {
				return csharpScopeSymbol{}, "", false
			}
			var c []csharpScopeSymbol
			for _, s := range byName[method] {
				if s.container == qname {
					c = append(c, s)
				}
			}
			if out, ok := chooseInstance(c); ok {
				return out, "csharp_typed_receiver", true
			}
			return csharpScopeSymbol{}, "", false
		}
	}
	if len(parts) == 3 && parts[0] == "this" {
		typeSpelling, ok := valueType(e.srcContainer, parts[1])
		if !ok {
			return csharpScopeSymbol{}, "", false
		}
		global := strings.HasPrefix(typeSpelling, "global::")
		typeQ := strings.TrimPrefix(typeSpelling, "global::")
		qname, _, ok := csharpResolveTypeIdentity(typeQ, e.srcNamespace, imports, byQName, e.srcContainer, global)
		if !ok {
			return csharpScopeSymbol{}, "", false
		}
		var c []csharpScopeSymbol
		for _, s := range byName[method] {
			if s.container == qname {
				c = append(c, s)
			}
		}
		if out, ok := chooseInstance(c); ok {
			return out, "csharp_typed_receiver", true
		}
		return csharpScopeSymbol{}, "", false
	}
	global := strings.HasPrefix(qualifier, "global::")
	if !global && shadowed(strings.Split(qualifier, ".")[0]) {
		return csharpScopeSymbol{}, "", false
	}
	typeQ := strings.TrimPrefix(qualifier, "global::")
	qname, alias, ok := csharpResolveTypeIdentity(typeQ, e.srcNamespace, imports, byQName, e.srcContainer, global)
	if !ok {
		return csharpScopeSymbol{}, "", false
	}
	var c []csharpScopeSymbol
	for _, s := range byName[method] {
		if s.container == qname {
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

func csharpBareCallableCandidates(c []csharpScopeSymbol, sourceStatic sql.NullInt64) []csharpScopeSymbol {
	if sourceStatic.Valid && sourceStatic.Int64 == 0 {
		return c
	}
	out := make([]csharpScopeSymbol, 0, len(c))
	for _, candidate := range c {
		if candidate.static.Valid && candidate.static.Int64 != 0 {
			out = append(out, candidate)
		}
	}
	return out
}

// csharpArityApplicable returns (applicable, known). Unknown call or
// declaration arity never removes a candidate from an overload set.
func csharpArityApplicable(s csharpScopeSymbol, call sql.NullInt64) (bool, bool) {
	if !call.Valid || !s.arityMin.Valid || !s.arityMax.Valid {
		return false, false
	}
	count := call.Int64
	if count < s.arityMin.Int64 {
		return false, true
	}
	return s.arityMax.Int64 == -1 || count <= s.arityMax.Int64, true
}

func csharpNamespaceForType(qname string, byQName map[string][]csharpScopeSymbol) string {
	if qname == "" {
		return ""
	}
	var containers []string
	for _, s := range byQName[qname] {
		if s.kind == "type" {
			containers = append(containers, s.container)
		}
	}
	if len(containers) == 0 {
		return ""
	}
	first := containers[0]
	for _, c := range containers[1:] {
		if c != first {
			return ""
		}
	}
	if first == "" {
		return ""
	}
	if parent := byQName[first]; len(parent) > 0 {
		for _, s := range parent {
			if s.kind == "type" {
				return csharpNamespaceForType(first, byQName)
			}
		}
	}
	return first
}

func csharpHasTypeAtNamespace(namespace, name string, byQName map[string][]csharpScopeSymbol) bool {
	for _, s := range byQName[namespace+"."+name] {
		if s.kind == "type" {
			return true
		}
	}
	return false
}

func csharpResolveTypeIdentity(qualifier, namespace string, imports []csharpScopeImport, byQName map[string][]csharpScopeSymbol, sourceContainer string, global bool) (string, bool, bool) {
	semanticTypes := func(qnames []string) []string {
		seen := map[string]struct{}{}
		for _, q := range qnames {
			for _, s := range byQName[q] {
				if s.kind == "type" {
					seen[s.qname] = struct{}{}
				}
			}
		}
		out := make([]string, 0, len(seen))
		for q := range seen {
			out = append(out, q)
		}
		sort.Strings(out)
		return out
	}
	decide := func(qnames []string, alias bool) (string, bool, bool) {
		types := semanticTypes(qnames)
		if len(types) == 1 {
			if !csharpTypeAccessibleByQName(types[0], sourceContainer, byQName) {
				return "", alias, false
			}
			return types[0], alias, true
		}
		return "", alias, false
	}
	if global {
		return decide([]string{qualifier}, false)
	}
	// Lookup follows C# 7.7.1: for each namespace level from the innermost
	// outwards, a type declared in that level wins, then an alias, then the
	// types imported by that level's using-namespace directives. Evidence
	// carries only the canonical namespace of a directive's owner, never the
	// declaration that encloses the call. Imports owned by an enclosing
	// namespace are therefore not applied, and a result an import could
	// overturn by sitting at a different level than assumed is refused,
	// including one an enclosing namespace's import could preempt.
	levels := []string{}
	for current := namespace; current != ""; {
		levels = append(levels, current)
		if dot := strings.LastIndexByte(current, '.'); dot >= 0 {
			current = current[:dot]
		} else {
			current = ""
		}
	}
	levels = append(levels, "")
	// A dotted qualifier whose first segment names an alias is resolved by
	// C# 7.7.1 through that alias (a namespace alias prefix), not through the
	// declared namespaces. Evidence models only aliases of a whole type, so
	// the lookup refuses rather than treat the segment as a namespace name.
	if head, _, dotted := strings.Cut(qualifier, "."); dotted {
		for _, i := range imports {
			if i.kind == "alias" && i.local == head && slices.Contains(levels, i.owner) {
				return "", true, false
			}
		}
	}
	candidate := func(level string) string {
		if level == "" {
			return qualifier
		}
		return level + "." + qualifier
	}
	imported := func(owner string) (aliases, namespaces []string) {
		for _, i := range imports {
			if i.owner != owner {
				continue
			}
			switch i.kind {
			case "alias":
				if i.local == qualifier {
					aliases = append(aliases, i.source)
				}
			case "namespace":
				namespaces = append(namespaces, i.source+"."+qualifier)
			}
		}
		return aliases, namespaces
	}
	// Directives with an empty owner are compilation-unit or file-scoped
	// namespace directives; evidence cannot tell which level they sit at.
	rootAliases, rootNamespaces := imported("")
	rootTypes := semanticTypes(append(append([]string{}, rootAliases...), rootNamespaces...))
	conflicts := func(qname string, qnames ...string) bool {
		return len(qnames) > 0 && (len(qnames) != 1 || qnames[0] != qname)
	}
	// preempted reports whether a directive owned by an enclosing namespace
	// level strictly inside the level that supplied qname could, by C# 7.7.1,
	// supply the name first. Such a directive is never applied (the evidence
	// cannot tell whether the call sits in its declaration), so it can only
	// refuse: an alias of that name always, a using-namespace when it
	// supplies a different indexed type. A namespace the graph has no type
	// for is not modeled, as at the call's own level. found is the index of
	// the supplying level; levels[0] is the call's own level, whose imports
	// are applied, and the "" level carries root evidence handled above.
	preempted := func(found int, qname string) bool {
		for _, level := range levels[min(1, found):found] {
			if level == "" {
				continue
			}
			aliases, namespaces := imported(level)
			if len(aliases) > 0 || conflicts(qname, semanticTypes(namespaces)...) {
				return true
			}
		}
		return false
	}
	for index, level := range levels {
		ownType := semanticTypes([]string{candidate(level)})
		var aliases, namespaces []string
		if level == namespace {
			aliases, namespaces = imported(level)
		}
		if len(ownType) > 0 || len(aliases) > 0 || len(semanticTypes(namespaces)) > 0 {
			var qname string
			var alias, ok bool
			fromImport := len(ownType) == 0
			switch {
			case len(ownType) > 0 && len(aliases) > 0:
				return "", true, false
			case len(ownType) > 0:
				qname, alias, ok = decide([]string{candidate(level)}, false)
			case len(aliases) > 0:
				qname, alias, ok = decide(aliases, true)
			default:
				qname, alias, ok = decide(namespaces, false)
			}
			if !ok {
				return "", alias, false
			}
			if fromImport && level != "" {
				for _, outer := range levels[index+1:] {
					if conflicts(qname, semanticTypes([]string{candidate(outer)})...) {
						return "", alias, false
					}
				}
			}
			if namespace != "" && !(level == namespace && !fromImport) && conflicts(qname, rootTypes...) {
				return "", alias, false
			}
			if namespace != "" && level == "" && !fromImport && len(rootAliases) > 0 {
				return "", true, false
			}
			if preempted(index, qname) {
				return "", alias, false
			}
			return qname, alias, true
		}
	}
	// Directives with an empty owner at the global level.
	qname, alias, ok := decide(rootNamespaces, false)
	if len(rootAliases) > 0 {
		qname, alias, ok = decide(rootAliases, true)
	}
	if ok && preempted(len(levels), qname) {
		return "", alias, false
	}
	return qname, alias, ok
}

func csharpTypeAccessibleByQName(qname, sourceContainer string, byQName map[string][]csharpScopeSymbol) bool {
	if qname == sourceContainer {
		return true
	}
	for current := qname; current != ""; {
		var typeSymbol *csharpScopeSymbol
		for i := range byQName[current] {
			if byQName[current][i].kind == "type" {
				candidate := byQName[current][i]
				typeSymbol = &candidate
				break
			}
		}
		if typeSymbol == nil {
			return true
		}
		if current == sourceContainer || typeSymbol.container == sourceContainer {
			return true
		}
		if typeSymbol.visibility != "public" {
			return false
		}
		current = typeSymbol.container
	}
	return true
}

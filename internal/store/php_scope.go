package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
)

// PHP scoped static-call resolution (P22.44).
//
// P22.43 persists namespace-aware PHP identity (`App.Service`,
// `App.Service.run`), method visibility and staticness, structured `use` facts
// (scope_import_evidence, kind php_type/php_function/php_const, OwnerModule =
// exact lexical namespace, "" = global), and scoped-call spellings that keep
// their syntax: `Foo::run`, `\Foo\Bar::run`, `namespace\Foo::run`,
// `self::run`, `static::run`, `parent::run`, `$obj->run`, `$obj?->run`.
//
// This pass OWNS every PHP call edge spelled with `::` or `->`. Ownership means
// two things, and the second holds even when the first proves nothing:
//
//  1. It binds a `Type::method` edge only when PHP namespace/import/type
//     evidence proves exactly one semantic type and exactly one syntax-proven
//     static method on it that the caller may see.
//  2. Every owned edge is withheld from the generic strategies (exact_name,
//     exact_qualified, dot_tail*, dot_suffix, ...): in SQL by the static
//     ownership predicate phpScopeVetoSQL inside resolverBindableCandidateSQL,
//     in the Go-side binder by phpScopeOwned dropping the target. A PHP scoped
//     call the language rules cannot prove stays unresolved; it is never handed
//     down to a repository-wide name match. The veto is a predicate rather
//     than a per-edge temp table because `->` member calls dominate PHP call
//     volume and can never bind in this phase; materialising them on every
//     incremental save would cost O(all unresolved member calls) for nothing.
//
// Type identity is decided BEFORE the member is looked at, and by exactly one
// evidence level -- the first that syntactically owns the spelling:
//
//	A. leading `\`            absolute; imports and namespace are ignored
//	B. leading `namespace\`   relative to the caller's lexical namespace
//	C. first segment is an applicable php_type import's local name
//	                          alias substitution on that segment only
//	D. otherwise              caller's lexical namespace + spelling
//
// No level falls through to a weaker one when its type lacks the method, and
// there is no lexical-parent walk and no global-namespace fallback: inside
// `namespace App\Sub`, an unqualified `Service` is `App.Sub.Service` or an
// import, never `App.Service` or `Service`. That is PHP, not C#.
//
// The caller's lexical namespace comes from the source symbol, never from the
// file (a file may hold several namespaces): a top-level function's
// ContainerName IS its namespace; a method's ContainerName is its type, and
// that type row's ContainerName is the namespace. An ambiguous type row set
// fails closed. Bare `run()` and `Foo\run()` calls are not owned here and keep
// their existing behaviour.

const phpScopeResolution = "tmp_php_scope_resolution"

// phpScopeStrategies is every strategy this pass writes; the one-time repair
// and the incremental invalidation both key on it.
var phpScopeStrategies = []string{
	ResolutionStrategyPHPTypeScope,
	ResolutionStrategyPHPAliasStatic,
	ResolutionStrategyPHPSelfStatic,
}

// phpScopeOwnedSQL is the SQL twin of phpScopeOwned: the edge spellings this
// pass owns. It requires the edges table to be addressable as `e`.
const phpScopeOwnedSQL = `(instr(e.dst_name, '::') > 0 OR instr(e.dst_name, '->') > 0)`

// phpScopeStaticSQL selects the owned spellings that can bind here: a scoped
// call that is not part of a member-call chain (`Foo::bar()->baz` is owned but
// is a member call).
const phpScopeStaticSQL = `(instr(e.dst_name, '::') > 0 AND instr(e.dst_name, '->') = 0)`

// phpScopeVetoSQL keeps every repo-wide strategy off the owned PHP spellings.
// It requires the surroundings every strategy UPDATE already has: the update
// target is `edges` and `files` is joined in as `f`. Same shape as the C++
// ownership rows beside it in resolverBindableCandidateSQL.
const phpScopeVetoSQL = `NOT (f.language = 'php' AND (instr(edges.dst_name, '::') > 0 OR instr(edges.dst_name, '->') > 0))`

// phpScopeOwned reports whether a PHP call spelling belongs to this pass. It
// is the Go-side twin of phpScopeVetoSQL.
func phpScopeOwned(dstName string) bool {
	return strings.Contains(dstName, "::") || strings.Contains(dstName, "->")
}

type phpScopeSymbol struct {
	id                                int64
	name, qname, container, kind, vis string
	static                            sql.NullInt64
}

type phpScopeEdge struct {
	id, file     int64
	name         string
	hasSrc       bool
	srcKind      string
	srcQName     string
	srcContainer string
	srcStatic    sql.NullInt64
	// derived
	namespace   string // lexical PHP namespace of the caller ("" = global)
	currentType string // containing type qname, "" when the caller is not inside a type
	sourceOK    bool   // namespace derivation succeeded
}

type phpScopeImport struct {
	source, local, kind, owner string
}

// phpScopeQuery is the read/write surface the pass needs; *sql.Tx and *sql.DB
// both satisfy it. Callers on a pooled *sql.DB must wrap the pass in a
// transaction (resolvePHPScopeStandalone) so the resolution temp table stays
// on one connection.
type phpScopeQuery = javaQuery

// resolvePHPScope runs the pass over every unresolved scoped (`::`) PHP edge of
// the repository, or only over `only` when it is non-empty. It returns how many
// edges it bound. Member (`->`) edges are owned but never loaded: nothing here
// can bind them, and the veto that keeps generic strategies off them is a
// static predicate.
func resolvePHPScope(ctx context.Context, q phpScopeQuery, repoID int64, only map[int64]struct{}) (int, error) {
	ids := sortedIDs(only)
	var edges []phpScopeEdge
	// LEFT JOIN on the source: an edge without a trustworthy source symbol is
	// still owned (and vetoed); it just cannot prove a namespace and abstains.
	if err := sqliteBatchedQuery(ctx, q, `SELECT e.id,e.file_id,e.dst_name,src.id IS NOT NULL,COALESCE(src.kind,''),COALESCE(src.qualified_name,''),COALESCE(src.container_name,''),src.is_static
FROM edges e JOIN files f ON f.id=e.file_id LEFT JOIN symbols src ON src.id=e.src_symbol_id
WHERE e.repo_id=? AND f.language='php' AND e.dst_symbol_id IS NULL AND `+phpScopeStaticSQL, " AND e.id IN (%s)", []any{repoID}, int64SliceToAny(ids), len(only) > 0,
		func(rows *sql.Rows) error {
			var e phpScopeEdge
			var hasSrc int
			if err := rows.Scan(&e.id, &e.file, &e.name, &hasSrc, &e.srcKind, &e.srcQName, &e.srcContainer, &e.srcStatic); err != nil {
				return err
			}
			e.hasSrc = hasSrc != 0
			edges = append(edges, e)
			return nil
		}); err != nil {
		return 0, err
	}
	if len(edges) == 0 {
		return 0, nil
	}

	// Phase 1: the containing type of every method source, to derive the
	// caller's lexical namespace. Loaded by exact qualified name only.
	containerQNames := map[string]struct{}{}
	for _, e := range edges {
		if e.hasSrc && e.srcKind == "function" && e.srcStatic.Valid && e.srcContainer != "" {
			containerQNames[e.srcContainer] = struct{}{}
		}
	}
	byQName := map[string][]phpScopeSymbol{}
	if err := phpLoadSymbolsByQName(ctx, q, repoID, sortedKeys(containerQNames), byQName); err != nil {
		return 0, err
	}
	for i := range edges {
		phpDeriveSource(&edges[i], byQName)
	}

	// Imports for the calling files, keyed by file; applicability to a caller
	// is decided per edge against its exact namespace.
	files := map[int64]struct{}{}
	for _, e := range edges {
		files[e.file] = struct{}{}
	}
	imports := map[int64][]phpScopeImport{}
	if err := sqliteBatchedIDQuery(ctx, q, sortedIDs(files), `SELECT file_id,source_specifier,local_name,import_kind,owner_module FROM scope_import_evidence WHERE repo_id=? AND language='php' AND import_kind='php_type' AND file_id IN (`, []any{repoID}, func(scan func(...any) error) error {
		var f int64
		var i phpScopeImport
		if err := scan(&f, &i.source, &i.local, &i.kind, &i.owner); err != nil {
			return err
		}
		imports[f] = append(imports[f], i)
		return nil
	}); err != nil {
		return 0, err
	}

	// Phase 2: decide the type identity of every edge from syntax, namespace
	// and imports alone, then load exactly those types and their requested
	// members. No symbol is loaded that no edge names.
	type decision struct {
		typeQ, method, strategy string
	}
	decisions := make([]decision, len(edges))
	wanted := map[string]struct{}{}
	for i, e := range edges {
		typeQ, method, strategy, ok := phpDecideType(e, imports[e.file])
		if !ok {
			continue
		}
		decisions[i] = decision{typeQ: typeQ, method: method, strategy: strategy}
		wanted[typeQ] = struct{}{}
		wanted[typeQ+"."+method] = struct{}{}
	}
	if err := phpLoadSymbolsByQName(ctx, q, repoID, sortedKeys(wanted), byQName); err != nil {
		return 0, err
	}

	res := map[int64]struct {
		dst      int64
		strategy string
	}{}
	for i, e := range edges {
		d := decisions[i]
		if d.typeQ == "" {
			continue
		}
		if !phpUniqueType(byQName[d.typeQ]) {
			continue
		}
		if dst, ok := phpChooseStaticMethod(byQName[d.typeQ+"."+d.method], d.typeQ, e.currentType); ok {
			res[e.id] = struct {
				dst      int64
				strategy string
			}{dst.id, d.strategy}
		}
	}
	if len(res) == 0 {
		return 0, nil
	}
	if _, err := q.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+phpScopeResolution+`(edge_id INTEGER PRIMARY KEY,dst_symbol_id INTEGER NOT NULL,strategy TEXT NOT NULL) WITHOUT ROWID`); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM `+phpScopeResolution); err != nil {
		return 0, err
	}
	ids = ids[:0]
	for id := range res {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rows := make([][]any, 0, len(ids))
	for _, id := range ids {
		r := res[id]
		rows = append(rows, []any{id, r.dst, r.strategy})
	}
	if err := sqliteBatchedValuesExec(ctx, q, `INSERT INTO `+phpScopeResolution+`(edge_id,dst_symbol_id,strategy) VALUES `, "(?,?,?)", nil, rows); err != nil {
		return 0, err
	}
	// Every PHP strategy is registered at the same tier, so the confidence is a
	// constant here; resolutionConfidenceFor keeps that honest at compile time.
	confidence := resolutionConfidenceFor(ResolutionStrategyPHPTypeScope)
	if _, err := q.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=(SELECT dst_symbol_id FROM `+phpScopeResolution+` r WHERE r.edge_id=edges.id),resolution_strategy=(SELECT strategy FROM `+phpScopeResolution+` r WHERE r.edge_id=edges.id),resolution_confidence='`+confidence+`' WHERE id IN (SELECT edge_id FROM `+phpScopeResolution+`)`); err != nil {
		return 0, err
	}
	_, err := q.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+phpScopeResolution)
	return len(res), err
}

// resolvePHPScopeStandalone runs the pass in its own transaction, for callers
// holding a pooled *sql.DB: the resolution temp table must see the same
// connection as the statements that fill and read it.
func (s *Store) resolvePHPScopeStandalone(ctx context.Context, repoID int64, only map[int64]struct{}) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := resolvePHPScope(ctx, tx, repoID, only)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// phpLoadSymbolsByQName appends every active PHP type/function row whose
// qualified name is in qnames to byQName. Rows already present are replaced,
// so calling it twice with overlapping sets cannot duplicate a candidate.
func phpLoadSymbolsByQName(ctx context.Context, q phpScopeQuery, repoID int64, qnames []string, byQName map[string][]phpScopeSymbol) error {
	if len(qnames) == 0 {
		return nil
	}
	for _, qn := range qnames {
		delete(byQName, qn)
	}
	return sqliteBatchedQuery(ctx, q, `SELECT s.id,s.name,s.qualified_name,s.container_name,s.kind,s.visibility,s.is_static
FROM symbols s JOIN files f ON f.id=s.file_id
WHERE s.repo_id=? AND s.language='php' AND f.is_deleted=0 AND s.kind IN ('type','function')`, " AND s.qualified_name IN (%s)", []any{repoID}, stringSliceToAny(qnames), true,
		func(rows *sql.Rows) error {
			var s phpScopeSymbol
			if err := rows.Scan(&s.id, &s.name, &s.qname, &s.container, &s.kind, &s.vis, &s.static); err != nil {
				return err
			}
			byQName[s.qname] = append(byQName[s.qname], s)
			return nil
		})
}

// phpDeriveSource fills the caller's lexical namespace and containing type
// from the source symbol. P22.43 facts: a top-level function has NULL
// staticness and its namespace as ContainerName; a method has non-NULL
// staticness and its type as ContainerName. A type's ContainerName is its
// namespace.
func phpDeriveSource(e *phpScopeEdge, byQName map[string][]phpScopeSymbol) {
	if !e.hasSrc {
		return
	}
	switch e.srcKind {
	case "type":
		e.namespace, e.currentType, e.sourceOK = e.srcContainer, e.srcQName, true
	case "function":
		if !e.srcStatic.Valid {
			e.namespace, e.sourceOK = e.srcContainer, true
			return
		}
		var types []phpScopeSymbol
		for _, s := range byQName[e.srcContainer] {
			if s.kind == "type" {
				types = append(types, s)
			}
		}
		if len(types) != 1 {
			return // no containing type row, or an ambiguous one: fail closed
		}
		e.namespace, e.currentType, e.sourceOK = types[0].container, types[0].qname, true
	}
}

// phpDecideType turns one owned spelling into the single semantic type it can
// name plus the requested method, or abstains. It never consults symbols: type
// identity is a function of syntax, the caller's namespace and its imports.
func phpDecideType(e phpScopeEdge, imports []phpScopeImport) (typeQ, method, strategy string, ok bool) {
	name := e.name
	if strings.Contains(name, "->") {
		return "", "", "", false // object/member call: no receiver typing here
	}
	sep := strings.Index(name, "::")
	if sep <= 0 {
		return "", "", "", false
	}
	scope, member := name[:sep], name[sep+2:]
	if !phpIdentifier(member) {
		return "", "", "", false // `Foo::$method`, `Foo::{expr}`, `Foo::BAR::baz`
	}
	if !e.sourceOK {
		return "", "", "", false // no proven lexical namespace
	}
	switch strings.ToLower(scope) {
	case "self":
		if e.currentType == "" {
			return "", "", "", false
		}
		return e.currentType, member, ResolutionStrategyPHPSelfStatic, true
	case "static", "parent":
		return "", "", "", false // late static binding / inheritance: not modelled
	}
	if strings.HasPrefix(scope, "$") || strings.HasPrefix(scope, "(") {
		return "", "", "", false // value receiver, not a type spelling
	}
	// A. absolute
	if strings.HasPrefix(scope, `\`) {
		q := phpSemanticName(scope[1:])
		if q == "" {
			return "", "", "", false
		}
		return q, member, ResolutionStrategyPHPTypeScope, true
	}
	segments := strings.Split(scope, `\`)
	for _, seg := range segments {
		if !phpIdentifier(seg) {
			return "", "", "", false
		}
	}
	// B. namespace-relative
	if strings.EqualFold(segments[0], "namespace") {
		if len(segments) < 2 {
			return "", "", "", false
		}
		if e.namespace == "" {
			// PHP resolves `namespace\Foo` in global code to `\Foo`; this
			// phase does not assert runtime semantics it cannot pin from the
			// parser, so the global form fails closed.
			return "", "", "", false
		}
		return e.namespace + "." + strings.Join(segments[1:], "."), member, ResolutionStrategyPHPTypeScope, true
	}
	// C. import alias on the first segment, exact local spelling, exact owner.
	sources := map[string]struct{}{}
	for _, i := range imports {
		if i.kind == "php_type" && i.owner == e.namespace && i.local == segments[0] {
			sources[i.source] = struct{}{}
		}
	}
	if len(sources) > 0 {
		if len(sources) != 1 {
			return "", "", "", false // several imports own the local name
		}
		var source string
		for source = range sources {
		}
		q := source
		if len(segments) > 1 {
			q += "." + strings.Join(segments[1:], ".")
		}
		return q, member, ResolutionStrategyPHPAliasStatic, true
	}
	// D. current namespace + relative spelling. No parent walk, no global
	// fallback: a global namespace is simply the empty prefix.
	q := strings.Join(segments, ".")
	if e.namespace != "" {
		q = e.namespace + "." + q
	}
	return q, member, ResolutionStrategyPHPTypeScope, true
}

// phpUniqueType reports whether exactly one active type row claims the qname.
// PHP has no partial types: two rows are an ambiguous identity, not one type.
func phpUniqueType(rows []phpScopeSymbol) bool {
	n := 0
	for _, s := range rows {
		if s.kind == "type" {
			n++
		}
	}
	return n == 1
}

// phpChooseStaticMethod picks the one syntax-proven static method on typeQ the
// caller may see: kind function, container exactly typeQ, is_static = 1, and
// public unless the caller's containing type is typeQ itself. Zero or several
// survivors bind nothing.
func phpChooseStaticMethod(rows []phpScopeSymbol, typeQ, currentType string) (phpScopeSymbol, bool) {
	var out phpScopeSymbol
	n := 0
	for _, s := range rows {
		if s.kind != "function" || s.container != typeQ {
			continue
		}
		if !s.static.Valid || s.static.Int64 != 1 {
			continue
		}
		if typeQ == currentType {
			if s.vis == "" {
				continue
			}
		} else if s.vis != "public" {
			continue
		}
		out, n = s, n+1
	}
	return out, n == 1
}

// phpIdentifier is the PHP label grammar: ASCII letter/underscore or a byte
// >= 0x80 first, then letters, digits, underscores or bytes >= 0x80.
func phpIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_' || c >= 0x80 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// phpSemanticName maps a backslash-qualified spelling to the persisted dotted
// qname, or "" when any segment is not an identifier.
func phpSemanticName(spelling string) string {
	segments := strings.Split(spelling, `\`)
	for _, seg := range segments {
		if !phpIdentifier(seg) {
			return ""
		}
	}
	return strings.Join(segments, ".")
}

// -- incremental invalidation ---------------------------------------------------

// repoHasPHP reports whether the repository holds any PHP file, so PHP-only
// work can be skipped for every other repository with one indexed probe.
func repoHasPHP(ctx context.Context, q queryContexter, repoID int64) (bool, error) {
	var has int
	err := sqliteScanRows(ctx, q, `SELECT EXISTS(SELECT 1 FROM files WHERE repo_id = ? AND language = 'php')`, []any{repoID},
		func(rows *sql.Rows) error { return rows.Scan(&has) })
	return has != 0, err
}

// phpStaleScopeBindings returns the PHP-owned bound edges whose evidence a
// batch of changed names may have altered: the bound method's own name, the
// bound type's own name (a new or removed `class Service` changes whether the
// destination identity is unique even when it declares no method), or the
// calling method's containing type name (a duplicate `class Caller` makes the
// source namespace unprovable). The `::` spelling has no '.' tail, so the
// generic dotted-tail selection cannot see it. Bindings cleared here are
// re-decided by the PHP pass that resolveDotSuffixIncrementally runs over every
// unresolved scoped edge.
func (s *Store) phpStaleScopeBindings(ctx context.Context, repoID int64, wanted map[string]struct{}) ([]int64, error) {
	if len(wanted) == 0 {
		return nil, nil
	}
	if has, err := repoHasPHP(ctx, s.db, repoID); err != nil || !has {
		return nil, err
	}
	lastSegment := func(qname string) string {
		if dot := strings.LastIndexByte(qname, '.'); dot >= 0 {
			return qname[dot+1:]
		}
		return qname
	}
	var stale []int64
	err := sqliteScanRows(ctx, s.db, `
		SELECT e.id, d.name, d.container_name, COALESCE(src.container_name, ''), src.is_static IS NOT NULL
		FROM edges e JOIN symbols d ON d.id = e.dst_symbol_id
		LEFT JOIN symbols src ON src.id = e.src_symbol_id
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NOT NULL
		  AND e.resolution_strategy IN `+sqlQuotedList(phpScopeStrategies), []any{repoID},
		func(rows *sql.Rows) error {
			var id int64
			var name, container, srcContainer string
			var srcIsMethod int
			if err := rows.Scan(&id, &name, &container, &srcContainer, &srcIsMethod); err != nil {
				return err
			}
			_, byMethod := wanted[name]
			_, byType := wanted[lastSegment(container)]
			_, bySource := wanted[lastSegment(srcContainer)]
			if byMethod || byType || (srcIsMethod != 0 && bySource) {
				stale = append(stale, id)
			}
			return nil
		})
	return stale, err
}

// -- one-time upgrade repair --------------------------------------------------

// phpScopeRepairSettingKey records that a repository's PHP-owned scoped call
// edges have been re-decided by this pass once.
//
// P22.44 changes no parser fact, so `treesitter:php:v2` stays and an indexed
// repository reparses nothing on upgrade. Its owned edges are either still
// unresolved (no strategy reconsiders them without a name event) or carry a
// generic target this pass now refuses to let stand. The repair clears every
// owned PHP edge's binding -- bound or not -- and runs the repo-wide resolve in
// the same transaction, so the PHP pass re-decides them under its own rules and
// the reference identities that derive from them converge afterwards
// (resolverRepairs orders this before referenceIdentityRepair, and a repo-wide
// repair drops the reference marker). Ordinary bare PHP calls are untouched.
const phpScopeRepairSettingKey = "resolver.php_scope_repaired.v1"

// phpScopeRepairApplies limits the repair to repositories that hold PHP at
// all: everywhere else there is nothing to clear, and running the repo-wide
// resolve anyway would make every upgraded Go/TypeScript/Python repository pay
// for a pass that can only reproduce what it already has.
func (s *Store) phpScopeRepairApplies(ctx context.Context, repoID int64) (bool, error) {
	return repoHasPHP(ctx, s.db, repoID)
}

func (s *Store) repairPHPScopeBindings(ctx context.Context, repoID int64) error {
	clear := func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE edges SET `+resolverClearResolutionSQL+`
			WHERE id IN (
				SELECT e.id FROM edges e JOIN files f ON f.id = e.file_id
				WHERE e.repo_id = ? AND f.language = 'php' AND e.dst_symbol_id IS NOT NULL
				  AND e.edge_kind <> '`+EdgeKindCrossLanguageRef+`'
				  AND `+phpScopeOwnedSQL+`
			)`, repoID)
		return err
	}
	_, err := s.resolveEdgesWithPreStep(ctx, repoID, clear)
	return err
}

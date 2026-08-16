package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
)

// Go bare-name scope precision (P22.6).
//
// A bare identifier in Go source -- `Foo()`, not `pkg.Foo()` -- is resolved by
// the language against the file's own scope: the current package, a local
// declaration, a predeclared builtin, or a dot-imported package. It is never
// resolved against "whatever package in the repository happens to declare that
// name". Before P22.6 the resolver and the public name-evidence queries treated
// a repo-wide short-name match as sufficient, which produced bindings like
//
//	profile/filter_test.go: countTags()   ->  internal/graph/graph.go: graph.countTags
//
// where `countTags` is a local closure in the calling function and the two
// packages have nothing to do with each other. That is the bare-name
// counterpart of the member-call class P22.1 fixed, and it contaminates
// callers, callees, context expansion and `test_calls` related-test evidence
// alike.
//
// The rule, applied identically by the repo-wide resolver, the Go-side binder,
// and every public name-evidence query:
//
//	A bare call spelling in a Go file may only claim a PACKAGE-LEVEL Go
//	declaration that lives in the calling file's OWN Go package. Anything else
//	stays unresolved / unclaimed.
//
// Consequences, and why each is deliberate:
//
//   - Repo-global name uniqueness is never evidence. Even if exactly one `Helper`
//     exists in the whole repository, a bare call from another package does not
//     reach it. Uniqueness says nothing about visibility.
//   - Export status is irrelevant. `NewThing()` written bare in package `a` does
//     not name `b.NewThing`; Go spells that `b.NewThing()`. So the rule gates all
//     cross-package bare calls, not merely unexported ones.
//   - Package scope vetoes the weaker repo-global fallbacks the same way P22.5's
//     own-module evidence does: if the caller's package holds no eligible
//     candidate, the edge stays unresolved rather than falling through to
//     exact_name / bare_tail / suffix matching in a foreign package.
//   - Methods cannot be claimed. Go spells a method call through a receiver, so
//     a bare spelling never denotes `func (T) Close()`. Package-level values are
//     still eligible (`var osExit = os.Exit; osExit()` is a real bare call).
//   - Local declarations the parsers do not model (closures, parameters) simply
//     find no package-level candidate and stay unresolved, instead of being
//     attributed to an unrelated project symbol.
//   - Predeclared builtins (`len`, `make`, `append`, `close`) can only bind a
//     project symbol that the caller's own package declares -- which is exactly
//     when Go itself would shadow the builtin.
//   - Dot imports (`import . "x"`) are NOT supported. `file_imports` persists
//     only `import_path`; the parsers drop the `.` and `_` import names before
//     the row is written (golang/adapter.go, treesitter/go_adapter.go), so a dot
//     import is byte-identical to a plain one in the database. Inferring one from
//     the target's name alone is the guess this phase exists to remove, so a
//     bare call into a dot-imported package stays unresolved. Recording the
//     import name would need a schema change and is deferred.
//
// Package identity comes from facts already persisted, with no migration:
//
//	package name  the first dot-separated segment of `symbols.qualified_name`.
//	              Both Go adapters build every qualified name as `pkg.Name` or
//	              `pkg.Recv.Name`, so the segment is adapter-independent -- unlike
//	              `container_name`, which is the receiver for methods and which
//	              the two adapters spell differently for pointer receivers
//	              (`*Store` vs `Store`).
//	directory     the stored `files.path` prefix up to and including the last
//	              separator.
//
// Both halves are required. The directory alone cannot tell `package foo` from
// `package foo_test`, which share one directory and are genuinely different
// packages: an external test package has no bare access to the production
// package's declarations. The package name alone cannot tell two directories'
// `package util` apart. Together they are the Go package.
//
// Paths are compared as stored, without canonicalisation, because the SQL twin
// of this comparison cannot canonicalise. Two files of one package are written
// by one indexing run and so are stored in one form; a hypothetical mixed-form
// database fails closed (no bind) rather than binding across packages. Storing
// canonical paths is P23.

// goPackageScopeUnknown is what every helper here returns when the facts do not
// prove a package. It never compares equal to a real scope key, so an unprovable
// scope abstains instead of matching.
const goPackageScopeUnknown = ""

// goBareCallName reports whether a destination spelling is a bare identifier --
// the only spelling this rule governs. Anything carrying a '.', '/' or ':'
// names a qualifier, and qualifier-bearing spellings are P22.1's and P22.5's
// business, not this one.
func goBareCallName(name string) bool {
	return name != "" &&
		!strings.ContainsRune(name, '.') &&
		!strings.ContainsRune(name, '/') &&
		!strings.ContainsRune(name, ':')
}

// goPackageNameOf returns the package segment of a Go qualified_name, or "" when
// the name carries no package prefix (which no current Go adapter emits, and
// which fails closed if one ever does).
func goPackageNameOf(qualifiedName string) string {
	dot := strings.IndexByte(qualifiedName, '.')
	if dot <= 0 {
		return ""
	}
	return qualifiedName[:dot]
}

// storedPathDir returns the directory prefix of a stored path, including the
// trailing separator, or "" for a repository-root file. It is the Go twin of
// sqlStoredPathDir and must stay byte-identical to it: both accept '/' and '\'
// so a Windows-written row is treated the same on either side.
func storedPathDir(filePath string) string {
	if i := strings.LastIndexAny(filePath, `/\`); i >= 0 {
		return filePath[:i+1]
	}
	return ""
}

// goPackageScopeKey identifies the Go package a symbol belongs to.
//
// The key is `<directory prefix><package name>`. Concatenation is unambiguous
// without a separator because a non-empty directory prefix always ends in '/'
// or '\' and a Go package name never contains one.
func goPackageScopeKey(filePath, qualifiedName string) string {
	pkg := goPackageNameOf(qualifiedName)
	if pkg == "" {
		return goPackageScopeUnknown
	}
	return storedPathDir(filePath) + pkg
}

// goPackageLevelDeclaration reports whether a symbol is declared at package
// level, which is the only thing a bare Go call can name. A method's qualified
// name carries its receiver between the package and the name, so it fails this
// test under either adapter's receiver spelling.
func goPackageLevelDeclaration(qualifiedName, name string) bool {
	pkg := goPackageNameOf(qualifiedName)
	return pkg != "" && name != "" && qualifiedName == pkg+"."+name
}

// goSymbolScope is one symbol's answer to both questions this rule asks.
type goSymbolScope struct {
	// key is the symbol's package scope, or goPackageScopeUnknown.
	key string
	// packageLevel reports whether a bare call could name this symbol at all.
	packageLevel bool
}

// -- SQL twins -------------------------------------------------------------
//
// Every expression below takes a column expression and renders the same
// derivation the Go helpers perform, so the resolver's set-based UPDATEs, the
// Go-side binder and the public queries cannot drift apart.

// sqlNotBareName is true for spellings this rule does not govern. It is written
// as the negation because it guards an OR chain that must short-circuit before
// the correlated package lookup for the overwhelming majority of edges.
func sqlNotBareName(expr string) string {
	return `(instr(` + expr + `, '.') > 0 OR instr(` + expr + `, '/') > 0 OR instr(` + expr + `, ':') > 0)`
}

// sqlStoredPathDir renders storedPathDir. rtrim(X, Y) strips trailing characters
// that occur in Y, and Y here is exactly X's non-separator characters, so what
// survives is the prefix through the last '/' or '\' -- or '' when the path has
// neither.
func sqlStoredPathDir(expr string) string {
	return `rtrim(` + expr + `, replace(replace(` + expr + `, '/', ''), '\', ''))`
}

// sqlGoHasPackageName renders goPackageNameOf's non-empty test. SQLite's instr
// is 1-based, so a leading dot reports 1 and a missing dot reports 0; both mean
// "no package segment", which is Go's `dot <= 0`.
func sqlGoHasPackageName(expr string) string {
	return `instr(` + expr + `, '.') > 1`
}

// sqlGoPackageNameOf renders goPackageNameOf. substr(x, 1, 0) and substr(x, 1, -1)
// are both '', so a name with no package segment yields '' on this side too.
func sqlGoPackageNameOf(expr string) string {
	return `substr(` + expr + `, 1, instr(` + expr + `, '.') - 1)`
}

// sqlGoPackageScopeKey renders goPackageScopeKey, including its abstention: a
// name with no package segment is goPackageScopeUnknown ('') and must not
// become a bare directory, which would make every such symbol share one scope.
func sqlGoPackageScopeKey(pathExpr, qnameExpr string) string {
	return `(CASE WHEN ` + sqlGoHasPackageName(qnameExpr) + `
		THEN ` + sqlStoredPathDir(pathExpr) + ` || ` + sqlGoPackageNameOf(qnameExpr) + `
		ELSE '' END)`
}

// sqlGoPackageLevelDeclaration renders goPackageLevelDeclaration for a symbol
// alias.
func sqlGoPackageLevelDeclaration(alias string) string {
	return `(` + alias + `.qualified_name = ` + sqlGoPackageNameOf(alias+`.qualified_name`) + ` || '.' || ` + alias + `.name
		AND ` + sqlGoHasPackageName(alias+`.qualified_name`) + `)`
}

// sqlGoBareSourceScope restricts an unresolved-edge leg to sources whose Go
// package scope is one of `n` bound keys. Non-Go sources are unaffected: this
// rule is about Go's scoping, and other languages keep the evidence they had.
//
// It requires the surrounding statement to expose the edge's source symbol as
// `src` and that symbol's file as `srcf`.
func sqlGoBareSourceScope(n int) string {
	if n == 0 {
		// No Go destination scope to match: Go sources contribute nothing, but
		// the leg still serves every other language.
		return `src.language <> 'go'`
	}
	return `(src.language <> 'go' OR ` +
		sqlGoPackageScopeKey("srcf.path", "src.qualified_name") +
		` IN (` + strings.TrimRight(strings.Repeat("?,", n), ",") + `))`
}

// resolverGoBareScopeSQL is the repo-wide resolver's half of the rule: a Go
// bare call is off limits to every generic strategy.
//
// It is folded into resolverBindableCandidateSQL, so exact_qualified,
// exact_name, receiver_method and the suffix family all carry it and none can be
// added that forgets it. Those strategies decide uniqueness over the whole
// repository, which is precisely the wrong question for a bare Go call, so they
// are not narrowed -- they are excluded. resolveGoPackageScopedBareNames is the
// only thing that binds this class, and it asks the right question inside one
// package.
//
// It requires what every resolver strategy already provides: the update target
// is `edges` and `files` is joined as `f`.
var resolverGoBareScopeSQL = `(
		f.language <> 'go'
		OR ` + sqlNotBareName("edges.dst_name") + `
	)`

// goBareScopeTables are the temp tables resolveGoPackageScopedBareNames owns.
var goBareScopeTables = []string{"tmp_go_bare_edges", "tmp_go_bare_candidates"}

// resolveGoPackageScopedBareNames binds every Go bare call that its own package
// answers, and only those.
//
// Shape, and why it is a separate pass rather than a filter on exact_name:
// uniqueness has to be evaluated *inside the calling package*. A repository with
// `a.Helper` and `b.Helper` makes the name ambiguous repo-wide, so the generic
// strategies refuse both -- yet each package's call is completely unambiguous to
// Go. Filtering the repo-wide winner would therefore lose real same-package
// recall, and worse, it would make a full index disagree with an incremental
// one: whichever package was indexed first would resolve, and adding the second
// package would not unbind it.
//
// Everything else is the resolver's existing policy, applied per package scope:
// candidate groups of one bind (P3), a production caller binds the sole
// production candidate while a test caller binds the sole candidate (P7), the
// language is Go on both sides (P2), and an unprovable scope binds nothing.
//
// Two set-based statements plus one UPDATE. No per-edge query.
func (s *Store) resolveGoPackageScopedBareNames(ctx context.Context, tx *sql.Tx, repoID int64) (int, error) {
	for _, table := range goBareScopeTables {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+table); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_go_bare_edges(
		edge_id INTEGER PRIMARY KEY,
		scope TEXT NOT NULL,
		name TEXT NOT NULL,
		caller_is_test INTEGER NOT NULL)`); err != nil {
		return 0, err
	}
	// The scope comes from the edge's own source symbol, not from its file, so
	// it is the package of the declaration that actually wrote the call. Rows
	// whose scope cannot be derived are dropped here and simply never bind.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tmp_go_bare_edges(edge_id, scope, name, caller_is_test)
		SELECT e.id,
		       `+sqlGoPackageScopeKey("srcf.path", "src.qualified_name")+`,
		       e.dst_name,
		       CASE WHEN ctf.file_id IS NOT NULL THEN 1 ELSE 0 END
		FROM edges e
		JOIN files f ON f.id = e.file_id
		JOIN symbols src ON src.id = e.src_symbol_id
		JOIN files srcf ON srcf.id = src.file_id
		LEFT JOIN `+resolverTestFilesTable+` ctf ON ctf.file_id = f.id
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL AND e.dst_name != ''
		  AND f.language = 'go' AND src.language = 'go'
		  AND NOT `+sqlNotBareName("e.dst_name")+`
		  AND ` + sqlGoHasPackageName("src.qualified_name") + `
	`, repoID); err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE tmp_go_bare_candidates(
		scope TEXT NOT NULL,
		name TEXT NOT NULL,
		any_symbol_id INTEGER,
		production_symbol_id INTEGER,
		PRIMARY KEY(scope, name))`); err != nil {
		return 0, err
	}
	// Candidates are the package-level declarations of the wanted names, grouped
	// by package scope. The two nullable ids are exactly what
	// resolverCandidateAggregatesSQL computes for the generic strategies, so the
	// caller-kind rule below is the same rule.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tmp_go_bare_candidates(scope, name, any_symbol_id, production_symbol_id)
		SELECT `+sqlGoPackageScopeKey("cf.path", "s.qualified_name")+` AS scope, s.name AS name,
		       CASE WHEN COUNT(*) = 1 THEN MIN(s.id) END,
		       CASE WHEN SUM(CASE WHEN tf.file_id IS NULL THEN 1 ELSE 0 END) = 1
		            THEN MIN(CASE WHEN tf.file_id IS NULL THEN s.id END) END
		FROM symbols s
		JOIN files cf ON cf.id = s.file_id
		LEFT JOIN `+resolverTestFilesTable+` tf ON tf.file_id = s.file_id
		WHERE s.repo_id = ? AND s.language = 'go'
		  AND s.name IN (SELECT name FROM tmp_go_bare_edges)
		  AND `+sqlGoPackageLevelDeclaration("s")+`
		GROUP BY scope, name
	`, repoID); err != nil {
		return 0, err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE edges
		SET dst_symbol_id = CASE WHEN b.caller_is_test = 1 THEN c.any_symbol_id ELSE c.production_symbol_id END,
		    resolution_strategy = '`+ResolutionStrategyGoPackageScope+`',
		    resolution_confidence = '`+resolutionConfidenceFor(ResolutionStrategyGoPackageScope)+`'
		FROM tmp_go_bare_edges b
		JOIN tmp_go_bare_candidates c ON c.scope = b.scope AND c.name = b.name
		WHERE edges.id = b.edge_id AND edges.dst_symbol_id IS NULL
		  AND (CASE WHEN b.caller_is_test = 1 THEN c.any_symbol_id ELSE c.production_symbol_id END) IS NOT NULL
	`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	for _, table := range goBareScopeTables {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+table); err != nil {
			return 0, err
		}
	}
	return int(n), nil
}

// goScopeName keys a candidate group by the package it lives in and the name it
// answers -- the Go-side twin of tmp_go_bare_candidates' primary key.
type goScopeName struct {
	scope string
	name  string
}

// goBareCandidateGroups is the binder's twin of tmp_go_bare_candidates: the
// package-level Go declarations of `names`, grouped per package scope, counted
// the same way candidateGroup counts everywhere else.
//
// Only the requested scopes are kept, so the group a binder sees is the group
// the repo-wide pass would have seen for the same edge.
func goBareCandidateGroups(ctx context.Context, q queryContexter, repoID int64, scopes map[string]struct{}, names []string, testFileIDs map[int64]struct{}) (map[goScopeName]candidateGroup, error) {
	out := map[goScopeName]candidateGroup{}
	if len(scopes) == 0 || len(names) == 0 {
		return out, nil
	}
	for start := 0; start < len(names); start += nameLookupChunk {
		end := min(start+nameLookupChunk, len(names))
		chunk := names[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, name := range chunk {
			args = append(args, name)
		}
		rows, err := q.QueryContext(ctx, `
			SELECT s.id, s.name, s.qualified_name, s.file_id, f.path
			FROM symbols s
			JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ? AND s.language = 'go'
			  AND s.name IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, fileID int64
			var name, qualifiedName, filePath string
			if err := rows.Scan(&id, &name, &qualifiedName, &fileID, &filePath); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if !goPackageLevelDeclaration(qualifiedName, name) {
				continue
			}
			key := goScopeName{scope: goPackageScopeKey(filePath, qualifiedName), name: name}
			if _, wanted := scopes[key.scope]; !wanted {
				continue
			}
			_, isTest := testFileIDs[fileID]
			out[key] = out[key].add(id, isTest)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// goSymbolScopesByIDs loads the package scope of each of the given symbols.
// Symbols that are not Go are omitted, so a caller can tell "not a Go symbol"
// from "Go symbol whose scope is unknown".
func goSymbolScopesByIDs(ctx context.Context, q queryContexter, repoID int64, ids []int64) (map[int64]goSymbolScope, error) {
	out := make(map[int64]goSymbolScope, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	for _, chunk := range chunkInt64s(ids, sqliteInClauseBatchSize) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		args = append(args, int64SliceToAny(chunk)...)
		rows, err := q.QueryContext(ctx, `
			SELECT s.id, s.qualified_name, s.name, f.path
			FROM symbols s
			JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ? AND s.language = 'go'
			  AND s.id IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
		`, args...)
		if err != nil {
			return nil, err
		}
		if err := scanGoSymbolScopes(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// goSymbolScopesByEdgeSource loads the package scope of each edge's source
// symbol, keyed by edge id.
func goSymbolScopesByEdgeSource(ctx context.Context, q queryContexter, repoID int64, edgeIDs []int64) (map[int64]goSymbolScope, error) {
	out := make(map[int64]goSymbolScope, len(edgeIDs))
	if len(edgeIDs) == 0 {
		return out, nil
	}
	for _, chunk := range chunkInt64s(edgeIDs, sqliteInClauseBatchSize) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		args = append(args, int64SliceToAny(chunk)...)
		rows, err := q.QueryContext(ctx, `
			SELECT e.id, s.qualified_name, s.name, f.path
			FROM edges e
			JOIN symbols s ON s.id = e.src_symbol_id
			JOIN files f ON f.id = s.file_id
			WHERE e.repo_id = ? AND s.language = 'go'
			  AND e.id IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
		`, args...)
		if err != nil {
			return nil, err
		}
		if err := scanGoSymbolScopes(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scanGoSymbolScopes(rows *sql.Rows, out map[int64]goSymbolScope) error {
	defer rows.Close()
	for rows.Next() {
		var key int64
		var qualifiedName, name, filePath string
		if err := rows.Scan(&key, &qualifiedName, &name, &filePath); err != nil {
			return err
		}
		out[key] = goSymbolScope{
			key:          goPackageScopeKey(filePath, qualifiedName),
			packageLevel: goPackageLevelDeclaration(qualifiedName, name),
		}
	}
	return rows.Err()
}

// goBareTargetScopes returns the Go package scopes from which a bare call could
// name one of the given symbols: the scopes of the package-level Go symbols
// among them. Non-Go targets and Go methods contribute nothing, because no bare
// Go spelling names them.
//
// The result is sorted so statement text and bound arguments are a function of
// the input, never of map iteration.
func (s *Store) goBareTargetScopes(ctx context.Context, repoID int64, ids []int64) ([]string, error) {
	scopes, err := goSymbolScopesByIDs(ctx, neighborQuerier{s}, repoID, ids)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if !scope.packageLevel || scope.key == goPackageScopeUnknown {
			continue
		}
		if _, ok := seen[scope.key]; ok {
			continue
		}
		seen[scope.key] = struct{}{}
		out = append(out, scope.key)
	}
	sort.Strings(out)
	return out, nil
}

// contextSeedGoScopes loads the Go package scope of every live seed that has a
// resolved symbol id, keyed by seed index.
//
// A seed with no id contributes nothing, and is therefore treated as not-Go by
// goBareSeedScope. That is not a hole: AllowShortEvidence is derived from a
// count over `symbols`, so a seed with no symbol row never carries bare
// short-name evidence in the first place.
func (s *Store) contextSeedGoScopes(ctx context.Context, repoID int64, seeds []ContextSeed, live []int) (map[int]goSymbolScope, error) {
	ids := make([]int64, 0, len(live))
	idxsByID := make(map[int64][]int, len(live))
	for _, i := range live {
		id := seeds[i].SymbolID
		if id == 0 {
			continue
		}
		if _, ok := idxsByID[id]; !ok {
			ids = append(ids, id)
		}
		idxsByID[id] = append(idxsByID[id], i)
	}
	byID, err := goSymbolScopesByIDs(ctx, neighborQuerier{s}, repoID, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int]goSymbolScope, len(byID))
	for id, scope := range byID {
		for _, idx := range idxsByID[id] {
			out[idx] = scope
		}
	}
	return out, nil
}

// goBareTargetSeedScope answers, for a seed used as a CALL TARGET and a
// spelling that might name it, whether the P22.6 rule governs the pair and if
// so which package scope the writer must be in.
//
// gated is false for a non-Go seed or a qualifier-bearing spelling: the caller
// keeps whatever evidence it had. gated is true and ok is false when the rule
// applies but the seed cannot be named by a bare call at all -- an unprovable
// package, or a method, which Go spells through a receiver. Then the evidence
// is dropped rather than widened.
func goBareTargetSeedScope(scopes map[int]goSymbolScope, idx int, name string) (scope string, gated, ok bool) {
	seed, isGo := scopes[idx]
	if !isGo || !goBareCallName(name) {
		return "", false, false
	}
	if !seed.packageLevel || seed.key == goPackageScopeUnknown {
		return "", true, false
	}
	return seed.key, true, true
}

// goBareSourceSeedScope is the callee-direction twin: the seed is the symbol
// that WROTE the spelling, so only its package matters. Whether the writer is
// itself package-level or a method is irrelevant -- a method body writes bare
// calls into its own package just as a function does.
func goBareSourceSeedScope(scopes map[int]goSymbolScope, idx int, name string) (scope string, gated, ok bool) {
	seed, isGo := scopes[idx]
	if !isGo || !goBareCallName(name) {
		return "", false, false
	}
	if seed.key == goPackageScopeUnknown {
		return "", true, false
	}
	return seed.key, true, true
}

// splitGoBareCalleeNames divides a callee-side name lookup in two.
//
// `unscoped` is every name the repo-wide cascade may still answer: names from
// non-Go sources and every qualifier-bearing spelling. `scoped` is the ids the
// bare Go spellings resolved to inside their own writer's package. A bare name
// written by a Go source whose package cannot be proven is in neither: an
// unprovable scope is not evidence of a shared one, so it resolves nothing --
// the same call expandContextCallees makes.
//
// A name can be in both halves: `Helper` written bare by a Go function and also
// written as `pkg.Helper` by a Python file is two different pieces of evidence,
// so the two sets are tracked independently.
func (s *Store) splitGoBareCalleeNames(ctx context.Context, repoID int64, srcIDs []int64, allNames []string, namesBySrc map[int64][]string) ([]string, []int64, error) {
	srcScopes, err := goSymbolScopesByIDs(ctx, neighborQuerier{s}, repoID, srcIDs)
	if err != nil {
		return nil, nil, err
	}
	if len(srcScopes) == 0 {
		// No Go source symbol: nothing to scope.
		return allNames, nil, nil
	}

	// A bare name is scoped only where every source that spelled it is a Go
	// symbol with a known package. Any other writer keeps the name in the
	// unscoped half, so this can never remove evidence another language owns.
	bareScopes := map[string]map[string]struct{}{}
	bareUnscoped := map[string]struct{}{}
	bareDropped := map[string]struct{}{}
	for src, names := range namesBySrc {
		scope, isGo := srcScopes[src]
		for _, name := range names {
			if !goBareCallName(name) {
				continue
			}
			if !isGo {
				bareUnscoped[name] = struct{}{}
				continue
			}
			if scope.key == goPackageScopeUnknown {
				bareDropped[name] = struct{}{}
				continue
			}
			if bareScopes[name] == nil {
				bareScopes[name] = map[string]struct{}{}
			}
			bareScopes[name][scope.key] = struct{}{}
		}
	}

	unscoped := make([]string, 0, len(allNames))
	scopedNames := make([]string, 0, len(allNames))
	for _, name := range allNames {
		_, scoped := bareScopes[name]
		if scoped {
			scopedNames = append(scopedNames, name)
		}
		if _, dropped := bareDropped[name]; dropped {
			// Some Go writer of this bare name has no provable package. That
			// writer contributes nothing; another writer that does have a scope
			// still contributes through scopedNames above.
			if _, alsoUnscoped := bareUnscoped[name]; !alsoUnscoped {
				continue
			}
		}
		if scoped {
			if _, alsoUnscoped := bareUnscoped[name]; !alsoUnscoped {
				continue
			}
		}
		unscoped = append(unscoped, name)
	}
	if len(scopedNames) == 0 {
		return unscoped, nil, nil
	}

	scopeKeys := map[string]struct{}{}
	for _, keys := range bareScopes {
		for key := range keys {
			scopeKeys[key] = struct{}{}
		}
	}
	orderedKeys := make([]string, 0, len(scopeKeys))
	for key := range scopeKeys {
		orderedKeys = append(orderedKeys, key)
	}
	sort.Strings(orderedKeys)

	byScope, err := goPackageScopedSymbolIDs(ctx, neighborQuerier{s}, repoID, orderedKeys, scopedNames)
	if err != nil {
		return nil, nil, err
	}
	var ids []int64
	for _, name := range scopedNames {
		// Only the scopes that actually wrote this name may answer it, so one
		// seed's package never answers another's call.
		for _, key := range orderedKeys {
			if _, ok := bareScopes[name][key]; !ok {
				continue
			}
			ids = mergeIDsPreservingOrder(ids, byScope[key][name])
		}
	}
	return unscoped, ids, nil
}

// goPackageScopedSymbolIDs resolves bare Go call names inside a fixed set of
// package scopes: the only lookup a bare Go spelling is entitled to.
//
// Result is keyed by scope key then by name, so a caller holding several seeds
// in several packages never merges one package's answer into another's.
// Ambiguity inside one package is left to the caller (the resolver refuses it;
// the query surfaces keep the pre-existing "name evidence lists everything it
// matched" behaviour, now confined to the calling package).
func goPackageScopedSymbolIDs(ctx context.Context, q queryContexter, repoID int64, scopeKeys, names []string) (map[string]map[string][]int64, error) {
	out := map[string]map[string][]int64{}
	if len(scopeKeys) == 0 || len(names) == 0 {
		return out, nil
	}
	scopeSet := make(map[string]struct{}, len(scopeKeys))
	for _, key := range scopeKeys {
		if key != goPackageScopeUnknown {
			scopeSet[key] = struct{}{}
		}
	}
	if len(scopeSet) == 0 {
		return out, nil
	}
	for start := 0; start < len(names); start += nameLookupChunk {
		end := min(start+nameLookupChunk, len(names))
		chunk := names[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, name := range chunk {
			args = append(args, name)
		}
		// The scope filter is applied in Go rather than as a second IN list:
		// the name predicate is the selective, indexed one
		// (idx_symbols_repo_name), and a package holds few symbols of any one
		// name, so this reads a handful of rows per name.
		rows, err := q.QueryContext(ctx, `
			SELECT s.id, s.name, s.qualified_name, f.path
			FROM symbols s
			JOIN files f ON f.id = s.file_id
			WHERE s.repo_id = ? AND s.language = 'go'
			  AND s.name IN (`+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+`)
			ORDER BY s.id ASC
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var name, qualifiedName, filePath string
			if err := rows.Scan(&id, &name, &qualifiedName, &filePath); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if !goPackageLevelDeclaration(qualifiedName, name) {
				continue
			}
			key := goPackageScopeKey(filePath, qualifiedName)
			if _, ok := scopeSet[key]; !ok {
				continue
			}
			if out[key] == nil {
				out[key] = map[string][]int64{}
			}
			out[key][name] = append(out[key][name], id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

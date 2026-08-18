package store

import (
	"context"
	"database/sql"
	"slices"
)

// resolverQualifiedLookupSQL derives a small lookup relation from unresolved
// spellings. The symbol side remains an equality on qualified_name, so SQLite
// can use idx_symbols_repo_qname before applying the C++ evidence filters.
const resolverQualifiedLookupSQL = `
		WITH distinct_names AS (
			SELECT DISTINCT dst_name
			FROM edges
			WHERE repo_id = ? AND dst_symbol_id IS NULL AND dst_name != ''
		), qualified_names AS (
			SELECT dst_name, dst_name AS lookup_name, 1 AS bare_name, 0 AS global_only
			FROM distinct_names
			WHERE dst_name NOT LIKE '::%'
			UNION ALL
			SELECT dst_name, substr(dst_name, 3), 0, 1
			FROM distinct_names
			WHERE dst_name LIKE '::%'
		)`

const resolverQualifiedLookupFilter = `
				AND NOT (s.language = 'cpp' AND n.bare_name = 1 AND s.qualified_name NOT LIKE '%::%')
				AND (n.global_only = 0 OR (s.language = 'cpp' AND s.qualified_name NOT LIKE '%::%'))`

// Resolver ambiguity determinism (P3).
//
// P2 established *which* candidates an implicit strategy may consider: only
// symbols whose persisted language equals the calling file's language (see
// resolver_language.go). P3 answers the next question: what happens when more
// than one such candidate survives.
//
// Before P3 every strategy collapsed its candidate group with MIN(s.id) and
// bound the winner. `symbols.id` is an autoincrement rowid assigned while
// concurrent workers insert parsed files, so that "winner" encoded insertion
// order, worker completion order and SQL row order -- none of which is evidence
// about the call. The same graph could therefore bind different destinations on
// two identical index runs.
//
// The rule, applied identically by every implicit strategy and by every
// resolver entrypoint (repo-wide, path-scoped, name-targeted, incremental):
//
//	A strategy binds an edge only when, at that strategy's own evidence level,
//	exactly one language-compatible candidate exists. Two or more equally valid
//	candidates leave the edge unresolved.
//
// Consequences:
//
//   - Ambiguity is name-local, not global. A *different* name that shares a
//     bare name with the call may still be resolvable: each strategy evaluates
//     uniqueness over its own candidate set, so a call to `pkgA.foo` still
//     resolves through qualified evidence even though a call to bare `foo`
//     would not. What a strategy may not do is bind a name that an earlier
//     strategy already found ambiguous -- see resolverAmbiguousNamesSQL.
//   - Ambiguity is a stable result, not a fallback. A strategy that finds
//     several candidates does not pick one to raise the resolution rate; a
//     wrong edge is worse than a missing edge, and an unresolved edge is
//     reproducible while an arbitrary one is not.
//   - Uniqueness has exactly one definition, shared by the SQL strategies and
//     the Go-side binder: a candidate group of size one binds, a larger group
//     binds nothing. No entrypoint can therefore pick a winner out of a
//     candidate set another entrypoint would have refused.
//     What the entrypoints still do *not* share is the strategy set, and that
//     gap predates P3. Measured on this repository (7758 call edges), a full
//     resolve and an incremental resolve of the same tree disagree on 697
//     edges, all in the same direction: the Go-side binder's bare-tail lookup
//     binds names (`f.file`, `s.UpsertRepo`) for which the repo-wide resolver
//     has no strategy at all. P3 narrowed that disagreement from 811 edges to
//     697 by removing the ones the repo-wide resolver had been binding
//     arbitrarily; closing the rest means giving one path the other's
//     strategies, which is a resolver redesign, not an ambiguity rule.
//   - Ranking evidence that would break a tie on purpose is out of scope here,
//     with one exception added in P7: a production caller facing one production
//     candidate and any number of test-only candidates binds the production one
//     (resolver_testfile.go). That is not a tie-break -- the candidates are not
//     tied, because which file declares a definition is real, reproducible
//     evidence that separates them. Builtin/stdlib/external classification and
//     edge-confidence ranking remain out of scope, and no other tie is broken.
//
// Nothing about the *candidate* definition changed in P3: strategies match the
// same columns with the same predicates as before, so this is a refusal to
// guess, not a narrowing of what counts as a match.
//
// The measured cost of that refusal on this repository is 114 of 1300 resolved
// call edges (-8.8%), spread over 12 bare names that genuinely have several
// same-language definitions (`New` with 8 candidates, `Run` with 6,
// `writeFile`, `qualifiedSuffix`, ... with 2 each). No edge changed target and
// none was gained: every lost edge is one the resolver had been pointing at a
// definition chosen by insertion order. Read a drop in the resolved-edge count
// after this change as removed guesses, not as lost capability.

// The SQL predicate that enforces the rule above lives in resolver_testfile.go
// as resolverCandidateHavingSQL, because from P7 on "usable candidate group" has
// two readings -- one per caller kind -- and both are computed by the same
// aggregate. It is still the case that a group reaching a caller has exactly one
// member *of the kind that caller may bind*, which is why the MIN(id) aggregates
// there are identity rather than a tie-break: each minimises over a single row.

// resolverBareNameKindsSQL restricts bare-name candidates to symbol kinds a
// call edge can denote. It is the candidate set the repo-wide bare-name
// strategy has always used; it is named here only because the ambiguity veto
// below has to count exactly the same symbols.
var resolverBareNameKindsSQL = sqlQuotedList(resolverBareNameKinds)

// resolverBareNameKinds is that list, and the only place it is written down.
// The SQL spelling above is rendered from it, and the Go-side predicate below
// reads it directly, so the two pipelines cannot drift apart.
var resolverBareNameKinds = []string{"function", "method", "class", "type", "struct", "interface"}

// resolverBareNameKindBindable reports whether the bare-name STRATEGY may write
// a symbol of this kind as a destination. It is the Go twin of
// resolverBareNameKindsSQL and must answer identically;
// resolver_kind_parity_test.go pins the two against each other.
func resolverBareNameKindBindable(kind string) bool {
	return slices.Contains(resolverBareNameKinds, kind)
}

// resolverBareNameLevelKindsSQL is the bare-name LEVEL's population: every
// symbol some strategy at that level could bind -- the kinds `exact_name` binds
// (resolverBareNameKindsSQL) plus the container-bearing symbols of any kind that
// `receiver_method` binds. It is deliberately wider than
// resolverBareNameKindsSQL, and the difference is not an accident to be tidied
// away -- see the veto commentary below for why a candidate may make a level
// undecidable without being a destination `exact_name` could have chosen.
//
// The Go-side binder loads exactly this population and marks the narrower half
// bindable, so the two pipelines agree on what "ambiguous" means at this level
// without the binder gaining destinations it has no strategy for.
//
// `alias` is the table qualifier the surrounding query uses for `symbols`
// (`"s."`, or empty when the columns are unqualified).
func resolverBareNameLevelKindsSQL(alias string) string {
	return `(` + alias + `kind IN ` + resolverBareNameKindsSQL +
		` OR ` + alias + `container_name != '')`
}

// Cross-strategy ambiguity veto.
//
// Per-strategy uniqueness alone is not enough, because the repo-wide resolver
// runs several strategies in sequence over the same call name. Without a veto a
// name with three same-language definitions is refused by the bare-name
// strategy and then bound anyway by a later strategy that happens to match only
// one of them -- for example the receiver strategy, which sees only the
// definitions that have a container. Nothing about the *call* said "this is a
// method", so that binding is chosen by which definition survived a filter, not
// by evidence.
//
// So before any strategy binds, the names that are already ambiguous at the two
// broadest evidence levels -- exact qualified name and bare name -- are
// recorded per language, and every strategy's UPDATE skips edges whose
// (dst_name, source language) is recorded. Suffix strategies still evaluate
// their own uniqueness on top of this; the veto only prevents a narrower
// strategy from resurrecting a name that a broader one already found
// undecidable.
//
// This is deliberately conservative in one direction: a call whose bare name is
// ambiguous stays unresolved even if a suffix strategy could name one
// definition. Suffix evidence comes from the destination's shape, not from the
// call site, so it does not tell those definitions apart on the caller's
// behalf. A missing edge is recoverable; a confidently wrong one is not.
//
// P7 makes the veto per caller kind rather than global, because "undecidable at
// this evidence level" now has two readings (resolver_testfile.go):
//
//	a test caller is vetoed when the level had candidates but not exactly one
//	a production caller is vetoed when the level had candidates but not exactly
//	one *production* candidate
//
// Both readings are the same sentence -- "this level had matches yet none this
// caller may bind" -- so neither weakens the other. Concretely: production A +
// test B no longer vetoes a production caller (production A is uniquely usable,
// so the broad level decided it) while it still vetoes a test caller (which has
// two equally valid candidates and nothing to separate them). And a level whose
// only match is a test definition now *starts* vetoing production callers, which
// is what stops a narrower strategy from quietly retargeting an explicit
// reference to a test symbol at some unrelated production symbol.
//
// One narrow gap comes with that relaxation, inherited from a kind-set mismatch
// that predates P7: the bare-name veto counts symbols whose kind is callable
// *or* which merely have a container, while the bare-name strategy binds only
// the callable kinds. So a level holding one production candidate of a
// non-callable, contained kind plus a test candidate now writes no production
// veto row, and a suffix strategy (which runs before the receiver strategy) may
// bind a *different* production symbol that the pre-P7 veto would have blocked.
// Reaching it needs a dotted `symbols.name`, so it is not observed on this
// repository; closing it means either giving the veto a second, suffix-only
// scope or reordering the strategies, both of which are resolver changes rather
// than ambiguity rules.
const (
	// resolverAmbiguousNamesTable holds (dst_name, dst_language, caller_is_test)
	// triples: the name was matched at a broad evidence level, and a caller of
	// that kind had no single candidate it was allowed to bind.
	resolverAmbiguousNamesTable = `tmp_resolver_ambiguous_names`

	// resolverAmbiguousNamesSQL excludes vetoed names from a resolver UPDATE. It
	// requires the same surroundings as resolverLanguageGateSQL: the update
	// target is `edges` and `files` is joined in as `f`.
	resolverAmbiguousNamesSQL = `NOT EXISTS (
		SELECT 1 FROM ` + resolverAmbiguousNamesTable + ` a
		WHERE a.dst_name = edges.dst_name AND a.dst_language = f.language
		AND a.caller_is_test = CASE WHEN ` + resolverCallerIsTestSQL + ` THEN 1 ELSE 0 END
	)`
)

// resolverBindableCandidateSQL is what a repo-wide strategy must satisfy to
// write a destination at all: the P2 language gate, the requirement that the
// candidate group actually holds an id this caller kind may bind (P7), the
// P22.6 Go bare-name package-scope rule, the P22.9 bare-name type-target
// scope rule, and the P22.15 rule that a bare C/C++ call may not claim a class
// member without class evidence. The exact-qualified strategy carries this
// without the veto below.
//
// The three scope rules sit here rather than beside the own-module veto because
// they are properties of the *chosen candidate*, not of the edge alone: a
// strategy that reaches a foreign-package candidate for a bare Go call, a class
// the calling file neither declares nor imports for any bare call, or another
// class's member for a bare C/C++ call, must bind nothing, whichever strategy
// it is. See go_package_scope.go, resolver_type_scope.go and cpp_class_scope.go.
var resolverBindableCandidateSQL = resolverLanguageGateSQL + `
		AND (` + resolverChosenCandidateSQL + `) IS NOT NULL
		AND NOT (f.language = 'cpp' AND (instr(edges.dst_name, '.') = 0 OR instr(edges.dst_name, '::') > 0))
		AND ` + resolverGoBareScopeSQL + `
		AND ` + resolverBareNameTypeScopeSQL + `
		AND ` + resolverCppBareNamespaceScopeSQL + `
		AND ` + resolverCppBareMemberScopeSQL

// resolverBindGateSQL is what every repo-wide strategy's UPDATE must
// satisfy before it may write a destination: the P2 language gate, the P7
// caller-kind candidate choice, the P22.6 Go package-scope rule, and the P3
// ambiguity veto. It is one value so a strategy cannot be added that applies
// one rule and forgets the other.
var resolverBindGateSQL = resolverBindableCandidateSQL + `
		AND ` + resolverAmbiguousNamesSQL + `
		AND NOT EXISTS (
			SELECT 1 FROM tmp_resolver_own_module_veto v
			WHERE v.edge_id = edges.id
		)`

// -- one-time convergence for databases written before P22.12 -----------------

// bareNameLevelRepairSettingKey records that a repository's persisted bindings
// have been re-decided against the P22.12 bare-name level.
//
// Two pre-P22.12 defects leave a lasting mark on a database, and neither heals
// on its own:
//
//   - WRONG bindings. The incremental binder counted only the callable half of
//     the bare-name level, so it bound names the repo-wide veto refuses. No
//     strategy reconsiders an already-bound edge, and invalidateNameEvidenceBindings
//     only reaches names some batch mentions, so an unchanged tree keeps them.
//   - MISSING bindings. An edge left unresolved because a declaration made its
//     name ambiguous stayed unresolved after that declaration was removed,
//     because the removal contributed no name to any batch. A no-change update
//     has no old-name event either, so the under-resolution is permanent.
//
// The repair is the smallest thing that answers both: drop this repository's
// `exact_name` bindings and run the repo-wide resolver once. Clearing is safe
// precisely because the re-resolve follows in the same call -- every binding the
// current rules still allow is rebuilt with the same strategy and confidence a
// fresh index would give it, and the resolve also decides the unresolved edges
// the second defect stranded. Restating the veto here to clear a narrower set
// would duplicate the rule, which is the drift resolver_testfile.go forbids.
//
// Keyed per repository (one database holds several) and written only after the
// resolve succeeds, so a failure re-runs rather than marking a repository
// converged that is not.
const bareNameLevelRepairSettingKey = "resolver.bare_name_level_repaired.v1"

// repairBareNameLevelBindings re-decides a repository's bare-name bindings. The
// once-per-repository guard lives in runResolverRepairOnce.
func (s *Store) repairBareNameLevelBindings(ctx context.Context, repoID int64) error {
	// One transaction for the pair. Clearing and re-resolving in two commits
	// would let a cancellation between them leave the repository with every
	// `exact_name` binding gone and nothing put back.
	clear := func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE edges
			SET `+resolverClearResolutionSQL+`
			WHERE repo_id = ? AND dst_symbol_id IS NOT NULL
			  AND resolution_strategy = ?
		`, repoID, ResolutionStrategyExactName)
		return err
	}
	_, err := s.resolveEdgesWithPreStep(ctx, repoID, clear)
	return err
}

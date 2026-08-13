package store

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
const resolverBareNameKindsSQL = `('function', 'method', 'class', 'type', 'struct', 'interface')`

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

	// resolverBindableCandidateSQL is what a repo-wide strategy must satisfy to
	// write a destination at all: the P2 language gate, plus the requirement
	// that the candidate group actually holds an id this caller kind may bind
	// (P7). The exact-qualified strategy carries this without the veto below.
	resolverBindableCandidateSQL = resolverLanguageGateSQL + `
		AND (` + resolverChosenCandidateSQL + `) IS NOT NULL`

	// resolverBindGateSQL is what every repo-wide strategy's UPDATE must
	// satisfy before it may write a destination: the P2 language gate, the P7
	// caller-kind candidate choice, and the P3 ambiguity veto. It is one
	// constant so a strategy cannot be added that applies one rule and forgets
	// the other.
	resolverBindGateSQL = resolverBindableCandidateSQL + `
		AND ` + resolverAmbiguousNamesSQL
)

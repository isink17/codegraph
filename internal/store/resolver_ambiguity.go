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
//   - Ranking evidence that would break a tie on purpose (test-shadow
//     demotion, builtin/stdlib classification, edge confidence) is out of scope
//     here. Until such a rule exists, no tie is broken at all.
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

// resolverUniqueCandidateSQL is the one SQL predicate that enforces the rule
// above. It is appended to the GROUP BY of every candidate relation and
// requires only that the surrounding statement groups candidate symbols by
// (matched name, language).
//
// With this clause a group reaching the caller always has exactly one member,
// which is why the accompanying MIN(id) aggregates are identity rather than a
// tie-break: they select the sole candidate's id, and there is no second row
// they could ever prefer it over.
const resolverUniqueCandidateSQL = `HAVING COUNT(*) = 1`

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
const (
	// resolverAmbiguousNamesTable holds (dst_name, dst_language) pairs that more
	// than one same-language symbol claims.
	resolverAmbiguousNamesTable = `tmp_resolver_ambiguous_names`

	// resolverAmbiguousNamesSQL excludes vetoed names from a resolver UPDATE. It
	// requires the same surroundings as resolverLanguageGateSQL: the update
	// target is `edges` and `files` is joined in as `f`.
	resolverAmbiguousNamesSQL = `NOT EXISTS (
		SELECT 1 FROM ` + resolverAmbiguousNamesTable + ` a
		WHERE a.dst_name = edges.dst_name AND a.dst_language = f.language
	)`

	// resolverBindGateSQL is what every repo-wide strategy's UPDATE must
	// satisfy before it may write a destination: the P2 language gate and the
	// P3 ambiguity veto. It is one constant so a strategy cannot be added that
	// applies one rule and forgets the other.
	resolverBindGateSQL = resolverLanguageGateSQL + `
		AND ` + resolverAmbiguousNamesSQL
)

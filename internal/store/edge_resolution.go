package store

import "strconv"

// Explainable edge resolution (P4).
//
// P2 decided which candidates a strategy may consider (language gate,
// resolver_language.go). P3 decided what happens when several survive (refuse to
// guess, resolver_ambiguity.go). P4 answers a third, purely descriptive
// question: once an edge *is* bound, what evidence bound it, and how strong was
// that evidence.
//
// P4 changes nothing about candidate selection. Every strategy matches the same
// columns with the same predicates, under the same gates, and binds exactly the
// edges it bound before. The only difference is that a bind now also persists
// two columns describing itself (migration 019). An edge that was unresolved
// stays unresolved and carries no metadata; an edge that was resolved stays
// resolved and gains an explanation.
//
// Two rules keep this honest:
//
//  1. Provenance is the strategy that actually performed the bind, never a
//     reconstruction. Several strategies can reach the same destination, so a
//     value inferred after the fact from graph shape would be a guess. Rows
//     written before migration 019 therefore keep '' (unknown) rather than a
//     backfilled value.
//  2. Confidence is a function of the strategy alone -- see
//     resolutionConfidenceFor. It is not influenced by language, repository
//     size, symbol popularity, or how many edges a name attracted. Two edges
//     bound by the same strategy always report the same confidence.

// Resolution strategy values. These are persisted in
// `edges.resolution_strategy` and surface in export/MCP payloads, so they are a
// stable contract: rename with a migration, not in place.
const (
	// ResolutionStrategyExactQualified: the edge's dst_name equalled a symbol's
	// full `qualified_name`, and exactly one same-language symbol claimed it.
	// (ResolveEdges Strategy 1; the qualified branch of the Go-side binder.)
	ResolutionStrategyExactQualified = "exact_qualified"

	// ResolutionStrategyExactName: the edge's dst_name equalled a symbol's bare
	// `name`, restricted to callable/type kinds (resolverBareNameKindsSQL), and
	// exactly one same-language symbol claimed it. (ResolveEdges Strategy 2.)
	ResolutionStrategyExactName = "exact_name"

	// ResolutionStrategyReceiverMethod: the edge's dst_name equalled the bare
	// `name` of exactly one same-language symbol that has a container
	// (a method on some receiver). The call site itself carried no receiver
	// evidence -- the container came from the destination.
	// (ResolveEdges Strategy 4.)
	ResolutionStrategyReceiverMethod = "receiver_method"

	// ResolutionStrategySlashSuffix: the edge's dst_name equalled the persisted
	// `symbols.qualified_suffix` (the part of qualified_name after the last '/')
	// of exactly one same-language symbol. (ResolveEdges Strategy 3a.)
	ResolutionStrategySlashSuffix = "slash_suffix"

	// ResolutionStrategyDotTail2: the edge's dst_name (exactly one dot, no
	// slash) equalled the persisted `symbols.dot_tail2` -- the last two
	// dot-separated segments of the destination's after-slash name -- for
	// exactly one same-language symbol. (ResolveEdges Strategy 3b.)
	ResolutionStrategyDotTail2 = "dot_tail2"

	// ResolutionStrategyDotTail3: the edge's dst_name (exactly two dots, no
	// slash) equalled the persisted `symbols.dot_tail3` for exactly one
	// same-language symbol. Schema-backed prelude of Strategy 3c, and stricter
	// than the LIKE it stands in for.
	ResolutionStrategyDotTail3 = "dot_tail3"

	// ResolutionStrategyDotSuffix: the edge's dst_name matched
	// `qualified_name LIKE '%.' || dst_name` for exactly one same-language
	// symbol. This is the loosest predicate the resolver owns: SQLite's LIKE is
	// ASCII case-insensitive and treats '_' and '%' inside dst_name as
	// wildcards. (ResolveEdges Strategy 3c fallback.)
	ResolutionStrategyDotSuffix = "dot_suffix"

	// ResolutionStrategyBareTail: the Go-side binder's fallback, used by the
	// path-scoped and name-targeted entrypoints only. The last dot-separated
	// segment of dst_name (the whole dst_name when it has no dot) equalled the
	// bare `name` of exactly one same-language symbol.
	//
	// This is deliberately NOT reported as exact_name even when dst_name has no
	// dot: unlike Strategy 2 the lookup applies no symbol-kind restriction, so
	// it can bind a kind the repo-wide bare-name strategy would never have
	// considered. Reporting the weaker strategy under the stronger name would
	// misstate the evidence. The resulting full-vs-incremental provenance
	// difference is the pre-existing strategy-set gap documented in
	// resolver_ambiguity.go, now visible instead of silent.
	ResolutionStrategyBareTail = "bare_tail"

	// ResolutionStrategyCrossLanguageSharedName: an explicit cross_language_ref
	// edge created because two symbols in different languages share a bare name
	// (length > 3, not in a stop-list). No call site asserted this link.
	ResolutionStrategyCrossLanguageSharedName = "cross_language_shared_name"

	// ResolutionStrategyCrossLanguageImportPath: an explicit cross_language_ref
	// edge created because a file's import path matched another file's
	// extension-stripped path in a different language, then every exported
	// symbol pair across those two files was linked (capped at 50). No call
	// site asserted this link either.
	ResolutionStrategyCrossLanguageImportPath = "cross_language_import_path"
)

// Resolution confidence tiers, persisted in `edges.resolution_confidence`.
//
// Three coarse tiers, not a score. The resolver has no calibrated probability
// to report and inventing one (0.87) would read as measured when it is not.
// What it does have is a partial order over the *kind* of evidence a bind used,
// and that is exactly what these tiers encode:
//
//	high    The edge's dst_name was matched in full against a symbol identity
//	        column (`qualified_name` or `name`) that the destination genuinely
//	        owns, uniquely within the calling language. Nothing about the name
//	        was discarded to make the match.
//
//	medium  The match required either truncating the destination's identity to
//	        a suffix/tail (slash_suffix, dot_tail2, dot_tail3, bare_tail) or
//	        accepting an owner the call site never named (receiver_method). Still
//	        a unique same-language candidate under an exact equality predicate,
//	        but part of the identity was dropped to reach it.
//
//	low     The match came from a fuzzy or non-call predicate: the LIKE-based
//	        dot_suffix fallback (case-insensitive, wildcard-permeable), or an
//	        explicit cross-language link derived from name/import-path
//	        coincidence rather than from any call site.
//
// No tier means "wrong": every bind still had to be the unique language-compatible
// candidate at its strategy's evidence level. The tier says how much of the
// destination's identity the evidence actually pinned down.
const (
	ResolutionConfidenceHigh   = "high"
	ResolutionConfidenceMedium = "medium"
	ResolutionConfidenceLow    = "low"
)

// resolutionConfidenceByStrategy is the single mapping. Every strategy constant
// above must appear here; resolutionConfidenceFor panics on an unregistered
// value so a new strategy cannot ship without a deliberate confidence decision.
var resolutionConfidenceByStrategy = map[string]string{
	ResolutionStrategyExactQualified: ResolutionConfidenceHigh,
	ResolutionStrategyExactName:      ResolutionConfidenceHigh,

	ResolutionStrategyReceiverMethod: ResolutionConfidenceMedium,
	ResolutionStrategySlashSuffix:    ResolutionConfidenceMedium,
	ResolutionStrategyDotTail2:       ResolutionConfidenceMedium,
	ResolutionStrategyDotTail3:       ResolutionConfidenceMedium,
	ResolutionStrategyBareTail:       ResolutionConfidenceMedium,

	ResolutionStrategyDotSuffix:               ResolutionConfidenceLow,
	ResolutionStrategyCrossLanguageSharedName: ResolutionConfidenceLow,
	ResolutionStrategyCrossLanguageImportPath: ResolutionConfidenceLow,
}

// resolutionConfidenceFor returns the confidence tier for a strategy.
//
// It panics on an unknown strategy rather than returning a default. Every call
// site passes one of the compile-time constants above, so a panic here can only
// mean a strategy was added without registering it -- which the resolver tests
// catch before any database is written.
func resolutionConfidenceFor(strategy string) string {
	confidence, ok := resolutionConfidenceByStrategy[strategy]
	if !ok {
		panic("store: unregistered edge resolution strategy " + strategy)
	}
	return confidence
}

// binderStrategies lists the strategies the Go-side binder (resolveEdgeTargets)
// can produce, in the order it tries them. The index doubles as the compact
// `strategy_rank` the binder writes per row, so the strategy and confidence
// strings stay out of its per-row payload; binderSetResolvedSQL decodes the rank
// back into them.
//
// Order is part of the meaning of `strategy_rank` only for the lifetime of a
// single transaction (the temp table is recreated per call and dropped before
// commit), so it can be reordered freely -- but every entry must still be a
// registered strategy.
var binderStrategies = []string{
	ResolutionStrategyExactQualified,
	ResolutionStrategyBareTail,
}

// binderStrategyRank returns the index of a binder strategy in binderStrategies.
// It panics on anything else: the binder has exactly these two evidence levels,
// and a third appearing here without a rank would silently lose its provenance.
func binderStrategyRank(strategy string) int {
	for rank, candidate := range binderStrategies {
		if candidate == strategy {
			return rank
		}
	}
	panic("store: strategy " + strategy + " is not produced by the Go-side binder")
}

// binderStrategyRankLiteral is binderStrategyRank rendered for direct
// interpolation into statement text.
func binderStrategyRankLiteral(strategy string) string {
	return strconv.Itoa(binderStrategyRank(strategy))
}

// binderSetResolvedSQL is the Go-side binder's counterpart to
// resolverSetResolvedSQL: one SET clause that writes the destination and its
// explanation together, decoding the compact `strategy_rank` back into the
// strategy and confidence strings.
//
// The CASE chains are built from binderStrategies, so adding a binder strategy
// extends the decode automatically and cannot leave a rank unmapped. Decoding
// here rather than storing the strings per row keeps the binder's per-row
// payload at two bound ids plus a one-character rank -- close to what binding an
// edge cost with no provenance at all.
//
// The decode is per row, not per statement: one UPDATE handles a temp table
// holding a mix of ranks.
//
// It assumes the surroundings the binder already has: the update target is
// `edges` and `tmp_edge_resolution` is joined in as `t`.
func binderSetResolvedSQL() string {
	strategyCase := "CASE t.strategy_rank"
	confidenceCase := "CASE t.strategy_rank"
	for rank, strategy := range binderStrategies {
		when := " WHEN " + strconv.Itoa(rank) + " THEN '"
		strategyCase += when + strategy + "'"
		confidenceCase += when + resolutionConfidenceFor(strategy) + "'"
	}
	// No ELSE: every rank the binder writes comes from binderStrategyRank, so an
	// unmapped rank is impossible. Were one to appear, SQLite's NULL result would
	// violate the NOT NULL column and fail loudly rather than store a wrong
	// explanation.
	strategyCase += " END"
	confidenceCase += " END"
	return `SET dst_symbol_id = t.dst_symbol_id,
			resolution_strategy = ` + strategyCase + `,
			resolution_confidence = ` + confidenceCase
}

// resolverSetResolvedSQL is the SET clause every repo-wide strategy UPDATE must
// use. Binding a destination and recording why are one statement, so a strategy
// cannot persist a `dst_symbol_id` it has no explanation for, and two strategies
// cannot disagree about the confidence attached to the same evidence level.
//
// It assumes the surroundings every repo-wide strategy already has: the update
// target is `edges` and the candidate relation is joined in as `r` exposing the
// two nullable candidate ids resolverCandidateAggregatesSQL computes. Which of
// them is written is decided in exactly one place --
// resolverChosenCandidateSQL -- so no strategy can apply the P7 caller-kind rule
// differently from the rest.
//
// The interpolated values are compile-time constants from this file, never
// caller input.
func resolverSetResolvedSQL(strategy string) string {
	return `SET dst_symbol_id = ` + resolverChosenCandidateSQL + `,
			resolution_strategy = '` + strategy + `',
			resolution_confidence = '` + resolutionConfidenceFor(strategy) + `'`
}

// resolverClearResolutionSQL is the assignment list that unbinds an edge. Every
// path that drops a destination must use it, so an edge can never keep the
// provenance of a binding that no longer exists. It is a bare assignment list
// (no `SET`) so callers can compose it.
const resolverClearResolutionSQL = `dst_symbol_id = NULL,
			resolution_strategy = '',
			resolution_confidence = ''`

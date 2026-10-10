package store

import (
	"context"
	"maps"
	"slices"
	"strings"
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
				AND NOT (n.bare_name = 1 AND s.kind = 'enum')
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
//     gap predates P3. The 697-edge figure is a historical P3 measurement, not
//     a current claim; current parity measurements belong in the phase report.
//     P22.38 closes the documented dot-tail/bare-level population gap without
//     giving the binder a new destination strategy.
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
	return `(NOT (` + alias + `language = 'cpp' AND ` + alias + `visibility = 'hidden_friend') AND (` + alias + `kind IN ` + resolverBareNameKindsSQL +
		` OR ` + alias + `container_name != ''))`
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
// The Go-side binder loads this broad population for dotted fallback spellings
// too (P22.38). It uses the same caller-kind counts as this SQL veto while
// retaining dot_tail2/dot_tail3 as the only destination lookup.
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

// JVM scope owns every edge from a file with persisted scope evidence, even
// when it refuses a target. Read that ownership directly: incremental suffix
// passes do not run the scope resolvers that populate transaction-local vetoes.
const resolverJVMScopeVetoSQL = `(f.language IN ('java','kotlin') AND EXISTS (
	SELECT 1 FROM file_scope_evidence fs
	WHERE fs.repo_id = edges.repo_id AND fs.file_id = edges.file_id
))`

// resolverRuleID names one rule of the repo-wide bind gate. The same identity
// labels the rule's Go-side counterpart, so the SQL strategies, the Go binder
// and their parity tests talk about one rule rather than two spellings of it.
type resolverRuleID string

// resolverRuleStage places a rule relative to candidate selection. Stages are
// labels, not an execution order: every rule is one conjunct of the same
// WHERE clause, and moving a rule across selection (for example turning a
// chosen-candidate restriction into a population filter) changes which
// candidates count toward uniqueness, so it is a behaviour change.
type resolverRuleStage uint8

const (
	// resolverStagePopulation decides which candidates exist for the caller.
	resolverStagePopulation resolverRuleStage = iota + 1
	// resolverStageOwnership withholds the edge from every generic strategy
	// because a language pass owns it, whether that pass bound it or not.
	resolverStageOwnership
	// resolverStageChosenCandidate judges the candidate a strategy selected.
	resolverStageChosenCandidate
	// resolverStageBroadAmbiguity refuses names a broad evidence level found
	// undecidable for this caller kind.
	resolverStageBroadAmbiguity
	// resolverStageOwnModule refuses edges an own-module import claimed.
	resolverStageOwnModule
)

// resolverRuleDisposition is what a refusal by the rule means for the edge.
type resolverRuleDisposition uint8

const (
	// resolverDispositionIneligible: no candidate this caller may bind.
	resolverDispositionIneligible resolverRuleDisposition = iota + 1
	// resolverDispositionOwned: another pass decides the edge, or nothing does.
	resolverDispositionOwned
	// resolverDispositionAmbiguous: several equally valid candidates.
	resolverDispositionAmbiguous
)

// resolverGateRule is one conjunct of the repo-wide bind gate.
type resolverGateRule struct {
	id          resolverRuleID
	stage       resolverRuleStage
	disposition resolverRuleDisposition
	// languages are the caller languages the rule can refuse; nil means every
	// language.
	languages []string
	sql       string
	// owns is the Go binder's twin of an edge-local ownership rule: it reports
	// exactly the edges `NOT (sql)` selects. It is nil when the Go side decides
	// the rule from loaded facts rather than from the edge alone.
	owns func(edgeTarget) bool
	// refuses is the Go binder's twin of a chosen-candidate restriction: it
	// reports whether the rule refuses the candidate the binder chose for an
	// edge, reading the facts the binder loaded for the batch. It is nil for
	// every other rule.
	refuses func(binderChosenCandidate) bool
	// withholds is the Go binder's twin of a rule decided from facts the
	// binder loads for the batch rather than from the edge alone: it reports
	// whether the rule keeps the edge from every generic lookup. Within the
	// twin's stated domain it reports exactly the edges `NOT (sql)` selects;
	// outside it, it reports nothing. It is nil for every other rule.
	withholds func(edgeTarget, *binderCandidateFacts) bool
}

// binderChosenCandidate is one edge and the candidate the binder chose for
// it, with the batch facts the chosen-candidate restrictions read.
type binderChosenCandidate struct {
	target edgeTarget
	dstID  int64
	facts  *binderCandidateFacts
}

// binderCandidateFacts are the facts the binder loads once per batch for the
// chosen-candidate restrictions and the fact-decided twins. A nil map reads as "no fact", which every
// restriction answers as its SQL twin answers an empty temp table.
type binderCandidateFacts struct {
	byQualified, byShort symbolCandidates
	importScope          map[int64]map[int64]struct{}
	// cppMemberTargets and cppNamespaceTargets map candidate symbol ids to
	// their class and namespace scope; cppCallerClasses and
	// cppCallerNamespaces map edge ids to the calling symbol's.
	cppMemberTargets, cppCallerClasses       map[int64]string
	cppNamespaceTargets, cppCallerNamespaces map[int64]string
	// ownModuleVeto holds the edges an own-module import mapped, and
	// goLocalClaims the Go selector calls a locally bound qualifier claims.
	ownModuleVeto, goLocalClaims map[int64]struct{}
	// bareLevel is the bare-name level loaded for dotted fallback spellings,
	// and testFileIDs the repository's test files.
	bareLevel   symbolCandidates
	testFileIDs map[int64]struct{}
}

// The Go twins of the rules the binder decides from loaded facts.

// goLocalQualifierWithholds reads the claims goLocalQualifierClaims loads, the
// Go twin of resolverGoLocalQualifierSQL.
func goLocalQualifierWithholds(t edgeTarget, f *binderCandidateFacts) bool {
	_, claimed := f.goLocalClaims[t.edgeID]
	return claimed
}

// ownModuleImportWithholds reads the edges resolveOwnModuleImports mapped to
// an own-module package, the set it also writes to the SQL veto table.
func ownModuleImportWithholds(t edgeTarget, f *binderCandidateFacts) bool {
	_, vetoed := f.ownModuleVeto[t.edgeID]
	return vetoed
}

// broadDotTailAmbiguityWithholds is the bare-level half of broad_ambiguity
// for dotted spellings. Its domain is a dot-tail spelling whose qualified name
// matched nothing in the caller's language; there the SQL veto row for the
// spelling can only come from the bare-name level. Every other spelling meets
// the rule through candidate selection instead (candidateGroup.chosen), which
// reads the same caller-kind counts but is a choice, not a refusal.
func broadDotTailAmbiguityWithholds(t edgeTarget, f *binderCandidateFacts) bool {
	if _, column := binderFallbackForTarget(t); column != "dot_tail2" && column != "dot_tail3" {
		return false
	}
	key := symbolLangKey{name: t.dstName, language: t.srcLanguage}
	if _, matched := f.byQualified.groups[key]; matched {
		return false
	}
	broad, found := f.bareLevel.groups[key]
	_, callerIsTest := f.testFileIDs[t.srcFileID]
	return found && broad.levelUndecidedFor(callerIsTest)
}

// The chosen-candidate restrictions' Go twins. Each guards on the caller
// language and the bare spelling first, exactly as its SQL twin does, so a
// qualified spelling never consults the loaded facts.

// bareTypeScopeRefuses is gated on the spelling rather than on the strategy,
// so both levels a bare name can reach (a qualified_name that happens to be
// bare, and the bare-name lookup) are covered. goBareCallName is the Go twin of
// the SQL guard's sqlNotBareName and is not Go-specific despite the name.
func bareTypeScopeRefuses(c binderChosenCandidate) bool {
	t := c.target
	return typeScopeGatedLanguage(t.srcLanguage) && goBareCallName(t.dstName) &&
		(c.facts.byQualified.typeTargetOutOfScope(c.dstID, t.srcFileID, c.facts.importScope) ||
			c.facts.byShort.typeTargetOutOfScope(c.dstID, t.srcFileID, c.facts.importScope))
}

func cppBareNamespaceScopeRefuses(c binderChosenCandidate) bool {
	t := c.target
	return bareNameScopeAllKinds(t.srcLanguage) && goBareCallName(t.dstName) &&
		cppNamespaceTargetOutOfScope(t.edgeID, c.dstID, c.facts.cppNamespaceTargets, c.facts.cppCallerNamespaces)
}

func cppBareMemberScopeRefuses(c binderChosenCandidate) bool {
	t := c.target
	return bareNameScopeAllKinds(t.srcLanguage) && goBareCallName(t.dstName) &&
		cppMemberTargetOutOfClassScope(t.edgeID, c.dstID, c.facts.cppMemberTargets, c.facts.cppCallerClasses)
}

// resolverBindableCandidateRules is what a repo-wide strategy must satisfy to
// write a destination at all. The exact-qualified strategy carries this
// without the broad-level vetoes in resolverBindGateRules.
//
// Order is part of the contract only through the composed SQL text, which is
// pinned byte for byte; it is not a precedence.
var resolverBindableCandidateRules = []resolverGateRule{
	{id: "language_gate", stage: resolverStagePopulation, disposition: resolverDispositionIneligible,
		sql: resolverLanguageGateSQL},
	{id: "caller_kind_candidate", stage: resolverStageChosenCandidate, disposition: resolverDispositionIneligible,
		sql: `(` + resolverChosenCandidateSQL + `) IS NOT NULL`},
	{id: ruleCppEvidenceOwnership, stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"cpp"}, sql: cppScopeVetoSQL, owns: cppScopeOwned},
	{id: ruleGoBarePackageScope, stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"go"}, sql: resolverGoBareScopeSQL, owns: goBareScopeOwned},
	{id: ruleGoLocalQualifier, stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"go"}, sql: resolverGoLocalQualifierSQL, withholds: goLocalQualifierWithholds},
	// own_module_import binds exact_qualified too: an import-qualified spelling
	// of a package outside this repository's modules can equal a local
	// package's qualified name (`errors.Is`).
	{id: ruleOwnModuleImport, stage: resolverStageOwnModule, disposition: resolverDispositionOwned,
		languages: []string{"go"}, sql: `NOT EXISTS (
			SELECT 1 FROM tmp_resolver_own_module_veto v
			WHERE v.edge_id = edges.id
		)`, withholds: ownModuleImportWithholds},
	{id: "bare_type_scope", stage: resolverStageChosenCandidate, disposition: resolverDispositionIneligible,
		languages: slices.Sorted(maps.Keys(typeScopeGatedLanguages)), sql: resolverBareNameTypeScopeSQL,
		refuses: bareTypeScopeRefuses},
	{id: "cpp_bare_namespace_scope", stage: resolverStageChosenCandidate, disposition: resolverDispositionIneligible,
		languages: []string{"cpp"}, sql: resolverCppBareNamespaceScopeSQL,
		refuses: cppBareNamespaceScopeRefuses},
	{id: "cpp_bare_member_scope", stage: resolverStageChosenCandidate, disposition: resolverDispositionIneligible,
		languages: []string{"cpp"}, sql: resolverCppBareMemberScopeSQL,
		refuses: cppBareMemberScopeRefuses},
	{id: ruleRubyOwnership, stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"ruby"}, sql: rubyScopeVetoSQL, owns: rubyScopeOwned},
	{id: ruleLuaOwnership, stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"lua"}, sql: luaScopeVetoSQL, owns: luaScopeOwned},
	{id: ruleScalaOwnership, stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"scala"}, sql: scalaScopeVetoSQL, owns: scalaScopeOwned},
	{id: ruleDartOwnership, stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"dart"}, sql: dartScopeVetoSQL, owns: dartScopeOwned},
	{id: ruleHCLOwnership, stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"hcl"}, sql: hclScopeVetoSQL, owns: hclScopeOwned},
	{id: "jvm_scope_ownership", stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"java", "kotlin"}, sql: `NOT ` + resolverJVMScopeVetoSQL},
	{id: "csharp_scope_ownership", stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"csharp"}, sql: `NOT EXISTS (SELECT 1 FROM ` + csharpScopeVeto + ` csv WHERE csv.edge_id = edges.id)`},
	{id: ruleTypeScriptOwnership, stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"typescript"}, sql: typescriptScopeVetoSQL, owns: typescriptScopeOwned},
	{id: "python_scope_claims", stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"python"}, sql: `NOT EXISTS (SELECT 1 FROM ` + pyScopeVeto + ` psv WHERE psv.edge_id = edges.id)`},
	{id: rulePHPOwnership, stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"php"}, sql: phpScopeVetoSQL, owns: phpScopeOwned},
	{id: ruleSwiftOwnership, stage: resolverStageOwnership, disposition: resolverDispositionOwned,
		languages: []string{"swift"}, sql: swiftScopeVetoSQL, owns: swiftScopeOwned},
}

// resolverBindGateRules is what every repo-wide strategy's UPDATE other than
// exact-qualified must satisfy: the bindable-candidate rules plus the
// broad-level ambiguity veto. It is one list so a
// strategy cannot be added that applies one rule and forgets another.
var resolverBindGateRules = append(slices.Clip(resolverBindableCandidateRules),
	resolverGateRule{id: ruleBroadAmbiguity, stage: resolverStageBroadAmbiguity, disposition: resolverDispositionAmbiguous,
		sql: resolverAmbiguousNamesSQL, withholds: broadDotTailAmbiguityWithholds},
)

// The edge-local ownership rules: each carries the Go twin the binder uses to
// withhold the edges it owns from the generic lookups.
const (
	ruleCppEvidenceOwnership resolverRuleID = "cpp_evidence_ownership"
	ruleGoBarePackageScope   resolverRuleID = "go_bare_package_scope"
	ruleRubyOwnership        resolverRuleID = "ruby_ownership"
	ruleLuaOwnership         resolverRuleID = "lua_ownership"
	ruleScalaOwnership       resolverRuleID = "scala_ownership"
	ruleDartOwnership        resolverRuleID = "dart_ownership"
	ruleHCLOwnership         resolverRuleID = "hcl_ownership"
	rulePHPOwnership         resolverRuleID = "php_ownership"
	ruleSwiftOwnership       resolverRuleID = "swift_ownership"
	ruleTypeScriptOwnership  resolverRuleID = "typescript_scope_ownership"
)

// The rules whose Go twin reads facts the binder loads for the batch.
const (
	ruleGoLocalQualifier resolverRuleID = "go_local_qualifier"
	ruleBroadAmbiguity   resolverRuleID = "broad_ambiguity"
	ruleOwnModuleImport  resolverRuleID = "own_module_import"
)

// The Go binder (resolveEdgeTargets) routes owned edges through these
// predicates, taken from the inventory rather than named directly, so a
// rule's SQL veto and the binder's withholding are one rule. Each language
// keeps its own pass and its own outcome accounting; only which edges a pass
// owns is read from here. The map is what the binder uses, keyed by rule, so
// a route cannot be listed without its predicate; the inventory test
// requires it to cover every rule with an edge-local twin.
var binderOwnershipRoutes = map[resolverRuleID]func(edgeTarget) bool{
	ruleCppEvidenceOwnership: resolverRuleOwns(ruleCppEvidenceOwnership),
	ruleGoBarePackageScope:   resolverRuleOwns(ruleGoBarePackageScope),
	ruleRubyOwnership:        resolverRuleOwns(ruleRubyOwnership),
	ruleLuaOwnership:         resolverRuleOwns(ruleLuaOwnership),
	ruleScalaOwnership:       resolverRuleOwns(ruleScalaOwnership),
	ruleDartOwnership:        resolverRuleOwns(ruleDartOwnership),
	ruleHCLOwnership:         resolverRuleOwns(ruleHCLOwnership),
	rulePHPOwnership:         resolverRuleOwns(rulePHPOwnership),
	ruleSwiftOwnership:       resolverRuleOwns(ruleSwiftOwnership),
	ruleTypeScriptOwnership:  resolverRuleOwns(ruleTypeScriptOwnership),
}

var (
	binderOwnsCpp        = binderOwnershipRoutes[ruleCppEvidenceOwnership]
	binderOwnsGoBare     = binderOwnershipRoutes[ruleGoBarePackageScope]
	binderOwnsRuby       = binderOwnershipRoutes[ruleRubyOwnership]
	binderOwnsLua        = binderOwnershipRoutes[ruleLuaOwnership]
	binderOwnsScala      = binderOwnershipRoutes[ruleScalaOwnership]
	binderOwnsDart       = binderOwnershipRoutes[ruleDartOwnership]
	binderOwnsHCL        = binderOwnershipRoutes[ruleHCLOwnership]
	binderOwnsPHP        = binderOwnershipRoutes[rulePHPOwnership]
	binderOwnsSwift      = binderOwnershipRoutes[ruleSwiftOwnership]
	binderOwnsTypeScript = binderOwnershipRoutes[ruleTypeScriptOwnership]
)

// binderFactRoutes are the fact-decided twins the binder applies, keyed by
// rule like binderOwnershipRoutes. Each is applied at its own point in the
// binder, because each refusal is accounted differently: a go-local claim is
// counted as unresolved unless its own pass bound it, an own-module veto is
// not counted at all (the own-module pass decides the edge), and a broad
// ambiguity is counted as ambiguity or test shadow.
var binderFactRoutes = map[resolverRuleID]func(edgeTarget, *binderCandidateFacts) bool{
	ruleGoLocalQualifier: resolverRuleWithholds(ruleGoLocalQualifier),
	ruleBroadAmbiguity:   resolverRuleWithholds(ruleBroadAmbiguity),
	ruleOwnModuleImport:  resolverRuleWithholds(ruleOwnModuleImport),
}

var (
	binderWithholdsGoLocal      = binderFactRoutes[ruleGoLocalQualifier]
	binderWithholdsBroadDotTail = binderFactRoutes[ruleBroadAmbiguity]
	binderWithholdsOwnModule    = binderFactRoutes[ruleOwnModuleImport]
)

// resolverRuleWithholds returns a fact-decided rule's Go twin. An id with no
// such rule is a programming error caught at package initialisation.
func resolverRuleWithholds(id resolverRuleID) func(edgeTarget, *binderCandidateFacts) bool {
	for _, rule := range resolverBindGateRules {
		if rule.id == id && rule.withholds != nil {
			return rule.withholds
		}
	}
	panic("store: no fact-decided rule " + string(id))
}

// edgeRefusalReasons is the decide-only reading of the inventory: for each of
// the given edges that is still unresolved, the ids of the rules whose Go twin
// withholds it from the generic strategies, in inventory order. Every id it
// reports is a rule whose SQL conjunct refuses the edge in the repo-wide bind
// gate; the twins are pinned to their SQL by TestResolverOwnershipRulesMatchGoTwins
// and TestResolverFactTwinsMatchSQL. It only reads.
//
// An edge with no entry is unknown, not unrefused. Only the twins that can be
// decided without writing are consulted: the edge-local ownership twins,
// go_local_qualifier and the dotted-spelling half of broad_ambiguity. Not
// consulted are the chosen-candidate restrictions (they judge the binder's
// choice, which this does not make), own_module_import (the pass that computes
// its veto also binds), and the rules with no Go twin. An edge can also stay
// unresolved with no rule refusing it: no candidate, or a language pass that
// owns the edge and proved nothing.
func (s *Store) edgeRefusalReasons(ctx context.Context, repoID int64, edgeIDs []int64) (map[int64][]resolverRuleID, error) {
	return edgeRefusalReasonsQuery(ctx, s.db, repoID, edgeIDs)
}

// edgeRefusalReasonsQuery is edgeRefusalReasons over any querier, so a caller
// can read the edges and every fact they are judged on from one snapshot.
func edgeRefusalReasonsQuery(ctx context.Context, q execQuerier, repoID int64, edgeIDs []int64) (map[int64][]resolverRuleID, error) {
	var targets []edgeTarget
	if err := sqliteBatchedIDQuery(ctx, q, edgeIDs,
		`SELECT e.id, e.dst_name, e.file_id, e.evidence, e.edge_kind, f.language
		FROM edges e JOIN files f ON f.id = e.file_id
		WHERE e.repo_id = ? AND e.dst_symbol_id IS NULL AND e.id IN (`,
		[]any{repoID}, func(scan func(...any) error) error {
			var t edgeTarget
			if err := scan(&t.edgeID, &t.dstName, &t.srcFileID, &t.evidence, &t.edgeKind, &t.srcLanguage); err != nil {
				return err
			}
			targets = append(targets, t)
			return nil
		}); err != nil {
		return nil, err
	}
	var facts binderCandidateFacts
	var err error
	if hasGoTargets(targets) {
		if facts.goLocalClaims, err = goLocalQualifierClaims(ctx, q, repoID); err != nil {
			return nil, err
		}
	}
	if facts.testFileIDs, err = testFileIDsForRepo(ctx, q, repoID); err != nil {
		return nil, err
	}
	var dotted []string
	for _, t := range targets {
		if _, column := binderFallbackForTarget(t); column == "dot_tail2" || column == "dot_tail3" {
			dotted = append(dotted, t.dstName)
		}
	}
	// A name loaded twice in two chunks would count its candidates twice.
	dotted = slices.Compact(slices.Sorted(slices.Values(dotted)))
	if facts.byQualified, err = resolveSymbolCandidatesQuery(ctx, q, repoID, "qualified_name", dotted, facts.testFileIDs); err != nil {
		return nil, err
	}
	if facts.bareLevel, err = resolveSymbolCandidatesQuery(ctx, q, repoID, "name", dotted, facts.testFileIDs); err != nil {
		return nil, err
	}
	out := map[int64][]resolverRuleID{}
	for _, t := range targets {
		for _, rule := range resolverBindGateRules {
			if !edgeRefusalEvaluates(rule) {
				continue
			}
			if (rule.owns != nil && rule.owns(t)) || (rule.withholds != nil && rule.withholds(t, &facts)) {
				out[t.edgeID] = append(out[t.edgeID], rule.id)
			}
		}
	}
	return out, nil
}

// edgeRefusalSkipped are rules with a Go twin that edgeRefusalReasons still
// does not consult: own_module_import's veto is computed by the pass that also
// binds, so it cannot be decided without writing.
var edgeRefusalSkipped = map[resolverRuleID]bool{ruleOwnModuleImport: true}

// edgeRefusalEvaluates reports whether edgeRefusalReasons consults the rule:
// it has a decide-only Go twin and is not skipped.
func edgeRefusalEvaluates(rule resolverGateRule) bool {
	return (rule.owns != nil || rule.withholds != nil) && !edgeRefusalSkipped[rule.id]
}

// binderCandidateRestrictions are the chosen-candidate restrictions the
// binder applies, in inventory order, taken from the rules that carry a Go
// twin. A restriction added to the inventory with a twin is applied by the
// binder with no second list to update.
var binderCandidateRestrictions = func() []resolverGateRule {
	var out []resolverGateRule
	for _, rule := range resolverBindGateRules {
		if rule.refuses != nil {
			out = append(out, rule)
		}
	}
	return out
}()

// binderRefusesChosen reports whether any chosen-candidate restriction refuses
// the candidate the binder chose. Every refusal leaves the edge unresolved:
// there is no weaker evidence level a refused candidate may fall back to.
func binderRefusesChosen(c binderChosenCandidate) bool {
	for _, rule := range binderCandidateRestrictions {
		if rule.refuses(c) {
			return true
		}
	}
	return false
}

// resolverRuleOwns returns an edge-local ownership rule's Go twin. An id with
// no such rule is a programming error caught at package initialisation.
func resolverRuleOwns(id resolverRuleID) func(edgeTarget) bool {
	for _, rule := range resolverBindGateRules {
		if rule.id == id && rule.owns != nil {
			return rule.owns
		}
	}
	panic("store: no edge-local ownership rule " + string(id))
}

var (
	resolverBindableCandidateSQL = composeResolverGate(resolverBindableCandidateRules)
	resolverBindGateSQL          = composeResolverGate(resolverBindGateRules)
)

// composeResolverGate joins rules into one WHERE-clause conjunction.
func composeResolverGate(rules []resolverGateRule) string {
	parts := make([]string, len(rules))
	for i, rule := range rules {
		parts[i] = rule.sql
	}
	return strings.Join(parts, "\n\t\tAND ")
}

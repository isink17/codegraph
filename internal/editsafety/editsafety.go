// Package editsafety assesses whether editing one symbol or one file is safe
// under explicitly named static-analysis rules over the indexed graph.
//
// The assessment is fail-closed. It first decides whether the graph can
// support an authoritative answer at all (freshness, then parser coverage of
// the target's language); if not, the status is "unknown" and there is no
// verdict, whatever evidence was found. Only an assessed result carries a
// verdict, and each verdict names the rule that decided it. No rule in this
// version proves isolation, so SAFE is never produced: an empty dependent list
// is not evidence that nothing depends on the target.
package editsafety

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/isink17/codegraph/internal/constraints"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/limits"
	"github.com/isink17/codegraph/internal/platform"
	"github.com/isink17/codegraph/internal/store"
)

// SchemaID versions the result document.
const SchemaID = "codegraph.safe_to_edit/v1"

// Assessment status.
const (
	StatusAssessed = "assessed"
	StatusUnknown  = "unknown"
)

// Verdicts, present only when the status is assessed. SAFE is part of the
// contract but no rule in this version can produce it.
const (
	VerdictSafe    = "SAFE"
	VerdictCaution = "CAUTION"
	VerdictUnsafe  = "UNSAFE"
)

// Reasons an assessment is unknown.
const (
	ReasonKnownStale               = "known_stale"
	ReasonFreshnessUnknown         = "freshness_unknown"
	ReasonInsufficientCoverage     = "insufficient_coverage"
	ReasonParserSemanticUpgrade    = "parser_semantic_upgrade_required"
	ReasonIsolationUnproven        = "isolation_unproven"
	ReasonConstraintsIndeterminate = "constraints_indeterminate"
)

// Rule ids. They are stable: a rule's meaning never changes under its id.
const (
	RuleFreshness           = "ES-FRESHNESS"
	RuleCoverage            = "ES-COVERAGE"
	RuleConstraintViolation = "ES-CONSTRAINT-VIOLATION"
	RuleKnownDependents     = "ES-KNOWN-DEPENDENTS"
	RuleUntestedDependents  = "ES-UNTESTED-DEPENDENTS"
	RuleUnresolvedCandidate = "ES-UNRESOLVED-CANDIDATES"
	RuleIsolation           = "ES-ISOLATION"
)

// Rule outcomes.
const (
	OutcomeFired        = "fired"
	OutcomeNotFired     = "not_fired"
	OutcomeNotEvaluated = "not_evaluated"
	OutcomeUnknown      = "unknown"
)

// Process exit codes (CLI only; the MCP tool returns the result as data).
const (
	ExitSafe                 = 0
	ExitReview               = 1
	ExitInvalid              = 2
	ExitKnownStale           = 3
	ExitUnknown              = 4
	ExitInsufficientCoverage = 5
)

// maxDependents bounds the dependents a result lists; the total is always
// reported.
const maxDependents = 50

// maxCandidates bounds the candidate ids an ambiguity error lists.
const maxCandidates = 20

// Errors that are not an assessment. Every one is invalid input or setup.
var (
	ErrInvalidTarget  = errors.New("invalid target")
	ErrTargetNotFound = errors.New("target not found")
	ErrAmbiguous      = errors.New("target is ambiguous")
	ErrNotIndexed     = errors.New("the repository is not indexed; run `codegraph index` first")
)

// Options selects exactly one target.
type Options struct {
	// RepoRoot is the repository root on the host, for the constraints
	// document.
	RepoRoot string
	// Symbol is an exact qualified name. It must match exactly one symbol;
	// it is never matched by short name, suffix or fuzzy search.
	Symbol string
	// SymbolID is an exact symbol id.
	SymbolID int64
	// File is a repository-relative path of an indexed file. Directories are
	// not supported.
	File string
}

// Result is the codegraph.safe_to_edit/v1 document. It holds no host path.
type Result struct {
	Schema string `json:"schema"`
	Status string `json:"status"`
	// Verdict is null unless Status is assessed.
	Verdict *string `json:"verdict"`
	// Reason is the deciding rule id when assessed, or why the status is
	// unknown.
	Reason      string    `json:"reason"`
	Target      Target    `json:"target"`
	Rules       []Rule    `json:"rules"`
	Evidence    Evidence  `json:"evidence"`
	Freshness   Freshness `json:"freshness"`
	Coverage    Coverage  `json:"coverage"`
	Limitations []string  `json:"limitations"`
}

// Target identifies what was assessed.
type Target struct {
	Kind    string         `json:"kind"` // "symbol" or "file"
	File    string         `json:"file"`
	Symbols []TargetSymbol `json:"symbols"`
}

// TargetSymbol is one assessed symbol.
type TargetSymbol struct {
	SymbolID      int64  `json:"symbol_id"`
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	Language      string `json:"language"`
	File          string `json:"file"`
	StartLine     int    `json:"start_line"`
	StableKey     string `json:"stable_key"`
}

// Rule is one rule's outcome.
type Rule struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

// Evidence is what the rules read.
type Evidence struct {
	DependentsTotal          int                         `json:"dependents_total"`
	DependentsFromTests      int                         `json:"dependents_from_tests"`
	Dependents               []store.EditSafetyDependent `json:"dependents"`
	DependentsTruncated      bool                        `json:"dependents_truncated"`
	UnresolvedCandidates     int                         `json:"unresolved_candidates"`
	ConstraintStatus         string                      `json:"constraint_status"`
	ConstraintViolations     []constraints.Dependency    `json:"constraint_violations"`
	ConstraintViolationRules []string                    `json:"constraint_violation_rules"`
}

// Freshness is the strict freshness verdict the assessment rests on.
type Freshness struct {
	Verdict      string               `json:"verdict"`
	Reasons      []string             `json:"reasons"`
	State        string               `json:"state"`
	StateReasons []string             `json:"state_reasons"`
	HeadNow      string               `json:"head_now"`
	LastFullScan *graph.FreshnessScan `json:"last_full_scan"`
}

// Coverage is the parser coverage of the target's languages.
type Coverage struct {
	Sufficient        bool                    `json:"sufficient"`
	RequiredLanguages []string                `json:"required_languages"`
	Limitations       []store.GraphLimitation `json:"limitations"`
}

// fixedLimitations hold for every result of this version.
var fixedLimitations = []string{
	"static graph evidence only: dynamic dispatch, reflection, function values, callbacks, code generation and build-time wiring are not recorded as dependents",
	"no isolation rule is implemented, so SAFE is never produced; an empty dependent list does not prove that nothing depends on the target",
	"freshness is checked against the last full scan and the current HEAD; uncommitted edits and unqueued watcher events are not checked",
	"dependents are read from resolved edges; edges in another language than the target's are trusted only as far as that language's coverage",
}

// Assess evaluates one target against the indexed graph of repoID. It issues
// reads only. A returned error is invalid input, a missing or ambiguous
// target, an unindexed repository, or an I/O or SQL failure; every graph state
// that cannot be vouched for is a Result with status unknown.
func Assess(ctx context.Context, st *store.Store, repoID int64, opts Options) (Result, error) {
	set := 0
	for _, given := range []bool{opts.Symbol != "", opts.SymbolID != 0, opts.File != ""} {
		if given {
			set++
		}
	}
	if set != 1 {
		return Result{}, fmt.Errorf("%w: give exactly one of symbol, symbol_id or file", ErrInvalidTarget)
	}
	if opts.SymbolID < 0 {
		return Result{}, fmt.Errorf("%w: symbol_id must be positive", ErrInvalidTarget)
	}

	// Freshness is read before any graph row and again after the last one,
	// so a scan landing in between is reported rather than mixed in.
	before, err := st.FreshnessStatus(ctx, repoID)
	if err != nil {
		return Result{}, err
	}
	info, err := st.ConstraintIndexInfo(ctx, repoID)
	if err != nil {
		return Result{}, err
	}
	if info.LastScanID == 0 {
		return Result{}, ErrNotIndexed
	}

	res := Result{
		Schema:      SchemaID,
		Limitations: slices.Clone(fixedLimitations),
		Evidence: Evidence{
			Dependents:               []store.EditSafetyDependent{},
			ConstraintViolations:     []constraints.Dependency{},
			ConstraintViolationRules: []string{},
		},
		Coverage: Coverage{RequiredLanguages: []string{}, Limitations: []store.GraphLimitation{}},
	}
	target, semanticBlocked, err := readTarget(ctx, st, repoID, opts, &res)
	if err != nil {
		return Result{}, err
	}

	limitations, err := st.GraphLimitations(ctx, repoID)
	if err != nil {
		return Result{}, err
	}
	res.Coverage.Sufficient = true
	for _, l := range limitations {
		if slices.Contains(res.Coverage.RequiredLanguages, l.Language) {
			res.Coverage.Sufficient = false
			res.Coverage.Limitations = append(res.Coverage.Limitations, l)
		} else {
			res.Limitations = append(res.Limitations, fmt.Sprintf("%s: %s", l.Language, l.Effect))
		}
	}
	if len(res.Coverage.RequiredLanguages) == 0 {
		// A file whose language is unknown has no coverage to vouch for.
		res.Coverage.Sufficient = false
	}

	constraintOutcome := OutcomeNotEvaluated
	constraintDetail := ""
	if !semanticBlocked {
		constraintOutcome, constraintDetail, err = evaluateConstraints(ctx, st, repoID, opts, target, &res)
		if err != nil {
			return Result{}, err
		}
	}

	after, err := st.FreshnessStatus(ctx, repoID)
	if err != nil {
		return Result{}, err
	}
	fresh := constraints.StrictFreshnessVerdict(info.DirtyFiles, info.LastScanID, before, after)
	res.Freshness = Freshness{
		Verdict:      fresh.Verdict,
		Reasons:      nonNil(fresh.Reasons),
		State:        fresh.State,
		StateReasons: nonNil(fresh.StateReasons),
		HeadNow:      fresh.HeadNow,
		LastFullScan: fresh.LastFullScan,
	}

	decide(&res, semanticBlocked, constraintOutcome, constraintDetail)
	return res, nil
}

// readTarget resolves the target exactly and fills Target and the dependent
// evidence. semanticBlocked reports that the graph needs a parser semantic
// upgrade before it can be read; the target is then identified but no edge
// evidence is read.
func readTarget(ctx context.Context, st *store.Store, repoID int64, opts Options, res *Result) (store.EditSafetyTarget, bool, error) {
	var (
		t   store.EditSafetyTarget
		err error
	)
	switch {
	case opts.File != "":
		logical, pathErr := platform.PublicRepositoryPath(opts.File)
		if pathErr != nil {
			return t, false, fmt.Errorf("%w: %q is not a repository-relative file path", ErrInvalidTarget, opts.File)
		}
		res.Target = Target{Kind: "file", File: logical, Symbols: []TargetSymbol{}}
		t, err = st.EditSafetyFile(ctx, repoID, logical)
		if semanticUpgrade(err) {
			res.Limitations = append(res.Limitations, "the graph needs a parser semantic upgrade, so the file was not read and its presence is not verified")
			return store.EditSafetyTarget{Dependents: []store.EditSafetyDependent{}}, true, nil
		}
		if err != nil {
			return t, false, err
		}
		if !t.FileFound {
			return t, false, fmt.Errorf("%w: %q is not an indexed file (directories are not supported)", ErrTargetNotFound, opts.File)
		}
		if t.FileLanguage != "" {
			res.Coverage.RequiredLanguages = []string{t.FileLanguage}
		}
	default:
		id := opts.SymbolID
		if opts.Symbol != "" {
			// maxCandidates+1 rows tell a unique name from an ambiguous one
			// and bound the candidate list in the error.
			exact, err := st.SymbolsByQualifiedName(ctx, repoID, opts.Symbol, maxCandidates+1)
			if err != nil {
				return t, false, err
			}
			switch len(exact) {
			case 0:
				return t, false, fmt.Errorf("%w: no symbol has the qualified name %q", ErrTargetNotFound, opts.Symbol)
			case 1:
				id = exact[0].ID
			default:
				ids := make([]string, 0, len(exact))
				for _, m := range exact[:min(len(exact), maxCandidates)] {
					ids = append(ids, fmt.Sprintf("%d (%s:%d)", m.ID, m.FilePath, m.Range.StartLine))
				}
				more := ""
				if len(exact) > maxCandidates {
					more = ", ..."
				}
				return t, false, fmt.Errorf("%w: several symbols are named %q; pass symbol_id, one of %s%s", ErrAmbiguous, opts.Symbol, strings.Join(ids, ", "), more)
			}
		}
		var ok bool
		t, ok, err = st.EditSafetySymbol(ctx, repoID, id)
		if err != nil && !semanticUpgrade(err) {
			return t, false, err
		}
		if err == nil && !ok {
			return t, false, fmt.Errorf("%w: no symbol has id %d", ErrTargetNotFound, id)
		}
		res.Target = Target{Kind: "symbol", Symbols: []TargetSymbol{}}
		if err != nil {
			// The edge read needs an upgraded graph. Identify the target
			// from the symbol table alone so the result still names it.
			syms, lookupErr := st.SymbolsForIDs(ctx, repoID, []int64{id})
			if lookupErr != nil {
				return t, false, lookupErr
			}
			sym, found := syms[id]
			if !found {
				return t, false, fmt.Errorf("%w: no symbol has id %d", ErrTargetNotFound, id)
			}
			t = store.EditSafetyTarget{Symbols: []graph.Symbol{sym}, Dependents: []store.EditSafetyDependent{}}
		}
		res.Target.File = t.Symbols[0].FilePath
		res.Coverage.RequiredLanguages = []string{t.Symbols[0].Language}
		if err != nil {
			fillTarget(res, t)
			return t, true, nil
		}
	}
	fillTarget(res, t)
	return t, false, nil
}

func fillTarget(res *Result, t store.EditSafetyTarget) {
	for _, s := range t.Symbols {
		res.Target.Symbols = append(res.Target.Symbols, TargetSymbol{
			SymbolID: s.ID, QualifiedName: s.QualifiedName, Kind: s.Kind, Language: s.Language,
			File: s.FilePath, StartLine: s.Range.StartLine, StableKey: s.StableKey,
		})
	}
	res.Evidence.DependentsTotal = len(t.Dependents)
	for _, d := range t.Dependents {
		if d.SourceIsTest {
			res.Evidence.DependentsFromTests++
		}
	}
	shown := t.Dependents
	if len(shown) > maxDependents {
		shown = shown[:maxDependents]
		res.Evidence.DependentsTruncated = true
	}
	res.Evidence.Dependents = append(res.Evidence.Dependents, shown...)
	res.Evidence.UnresolvedCandidates = t.UnresolvedCandidates
}

func semanticUpgrade(err error) bool {
	return errors.Is(err, store.ErrParserSemanticUpgradeRequired) || errors.Is(err, store.ErrParserSemanticIncomplete)
}

// evaluateConstraints reads the repo-root constraints document and reports
// whether any violation involves the target: a finding or cycle hop whose
// source or target is a target symbol or, for a file target, the file.
func evaluateConstraints(ctx context.Context, st *store.Store, repoID int64, opts Options, t store.EditSafetyTarget, res *Result) (string, string, error) {
	keys := map[string]bool{}
	for _, s := range t.Symbols {
		// An empty key identifies nothing; it must not match an endpoint
		// that also lacks one.
		if s.StableKey != "" {
			keys[s.StableKey] = true
		}
	}
	involves := func(d constraints.Dependency) bool {
		if keys[d.Source.StableKey] || keys[d.Target.StableKey] {
			return true
		}
		return opts.File != "" && (d.Source.Path == res.Target.File || d.Target.Path == res.Target.File)
	}
	open := func(context.Context) (*store.Store, int64, error) { return st, repoID, nil }
	rules := map[string]bool{}
	cyclesTruncated := false
	for offset := 0; ; offset += limits.MaxPage {
		cres, err := constraints.Check(ctx, constraints.Options{RepoRoot: opts.RepoRoot, Limit: limits.MaxPage, Offset: offset}, open)
		if err != nil {
			return "", "", err
		}
		res.Evidence.ConstraintStatus = cres.Status
		switch cres.Status {
		case constraints.StatusNotConfigured:
			return OutcomeNotEvaluated, "no constraints document at the repository root", nil
		case constraints.StatusConfigError:
			res.Limitations = append(res.Limitations, "the constraints document is invalid; architectural violations were not evaluated")
			return OutcomeUnknown, "the constraints document is invalid", nil
		case constraints.StatusNotIndexed:
			return OutcomeUnknown, "the repository is not indexed", nil
		}
		for _, f := range cres.Findings {
			if involves(f.Dependency) {
				res.Evidence.ConstraintViolations = append(res.Evidence.ConstraintViolations, f.Dependency)
				rules[f.RuleID] = true
			}
		}
		if offset == 0 {
			for _, c := range cres.Cycles {
				for _, h := range c.Hops {
					if involves(h.Dependency) {
						res.Evidence.ConstraintViolations = append(res.Evidence.ConstraintViolations, h.Dependency)
						rules[c.RuleID] = true
					}
				}
			}
			cyclesTruncated = cres.CyclesTruncated
		}
		if !cres.FindingsTruncated {
			break
		}
	}
	for r := range rules {
		res.Evidence.ConstraintViolationRules = append(res.Evidence.ConstraintViolationRules, r)
	}
	slices.Sort(res.Evidence.ConstraintViolationRules)
	if len(rules) > 0 {
		return OutcomeFired, fmt.Sprintf("the target takes part in %d violation(s) of rule(s) %s", len(res.Evidence.ConstraintViolations), strings.Join(res.Evidence.ConstraintViolationRules, ", ")), nil
	}
	if cyclesTruncated {
		res.Limitations = append(res.Limitations, "constraint cycles were truncated; a cycle through the target may not have been read")
		return OutcomeUnknown, "constraint cycles were truncated", nil
	}
	return OutcomeNotFired, "no constraint violation involves the target", nil
}

// decide applies the evaluation order: freshness, then coverage, then the
// verdict rules. Evidence is kept whatever the outcome, but only an adequate
// graph produces a verdict.
func decide(res *Result, semanticBlocked bool, constraintOutcome, constraintDetail string) {
	ev := res.Evidence
	rule := func(id, outcome, detail string) {
		res.Rules = append(res.Rules, Rule{ID: id, Outcome: outcome, Detail: detail})
	}

	freshOK := res.Freshness.Verdict == constraints.VerdictFullCoverageAtHead
	if freshOK {
		rule(RuleFreshness, OutcomeNotFired, "the newest full scan covers the current HEAD and nothing is queued")
	} else {
		rule(RuleFreshness, OutcomeFired, "freshness verdict "+res.Freshness.Verdict)
	}
	if res.Coverage.Sufficient {
		rule(RuleCoverage, OutcomeNotFired, "the target's language is call-capable through one complete parser profile")
	} else {
		rule(RuleCoverage, OutcomeFired, "the target's language has no complete call-capable parser coverage")
	}

	evidenceRead := !semanticBlocked
	switch {
	case !evidenceRead:
		rule(RuleConstraintViolation, OutcomeNotEvaluated, "the graph needs a parser semantic upgrade")
		rule(RuleKnownDependents, OutcomeNotEvaluated, "the graph needs a parser semantic upgrade")
		rule(RuleUntestedDependents, OutcomeNotEvaluated, "the graph needs a parser semantic upgrade")
		rule(RuleUnresolvedCandidate, OutcomeNotEvaluated, "the graph needs a parser semantic upgrade")
	default:
		rule(RuleConstraintViolation, constraintOutcome, constraintDetail)
		if ev.DependentsTotal > 0 {
			rule(RuleKnownDependents, OutcomeFired, fmt.Sprintf("%d resolved edge(s) from outside the target reach it", ev.DependentsTotal))
		} else {
			rule(RuleKnownDependents, OutcomeNotFired, "no resolved edge from outside the target reaches it")
		}
		switch {
		case ev.DependentsTotal == 0:
			rule(RuleUntestedDependents, OutcomeNotEvaluated, "no known dependents")
		case ev.DependentsFromTests == 0:
			rule(RuleUntestedDependents, OutcomeFired, "no known dependent is in a test file; a missing test link does not prove no test exists")
		default:
			rule(RuleUntestedDependents, OutcomeNotFired, fmt.Sprintf("%d known dependent(s) are in test files", ev.DependentsFromTests))
		}
		if ev.UnresolvedCandidates > 0 {
			rule(RuleUnresolvedCandidate, OutcomeFired, fmt.Sprintf("%d unresolved edge(s) carry a name that may reach the target", ev.UnresolvedCandidates))
		} else {
			rule(RuleUnresolvedCandidate, OutcomeNotFired, "no unresolved edge carries a target name")
		}
	}
	rule(RuleIsolation, OutcomeNotEvaluated, "no isolation rule is implemented in this version")

	unknown := func(reason string) {
		res.Status, res.Verdict, res.Reason = StatusUnknown, nil, reason
	}
	assessed := func(verdict, ruleID string) {
		v := verdict
		res.Status, res.Verdict, res.Reason = StatusAssessed, &v, ruleID
	}
	switch {
	case res.Freshness.Verdict == constraints.VerdictKnownStale:
		unknown(ReasonKnownStale)
	case res.Freshness.Verdict == constraints.VerdictInsufficientCoverage:
		unknown(ReasonInsufficientCoverage)
	case !freshOK:
		unknown(ReasonFreshnessUnknown)
	case semanticBlocked:
		unknown(ReasonParserSemanticUpgrade)
	case !res.Coverage.Sufficient:
		unknown(ReasonInsufficientCoverage)
	case constraintOutcome == OutcomeFired:
		assessed(VerdictUnsafe, RuleConstraintViolation)
	case constraintOutcome == OutcomeUnknown:
		// A violation may exist unseen: neither CAUTION nor UNSAFE is proven
		// to be the whole answer.
		unknown(ReasonConstraintsIndeterminate)
	case ev.DependentsTotal > 0 && ev.DependentsFromTests == 0:
		assessed(VerdictCaution, RuleUntestedDependents)
	case ev.DependentsTotal > 0:
		assessed(VerdictCaution, RuleKnownDependents)
	default:
		unknown(ReasonIsolationUnproven)
	}
}

// ExitCode maps a result to the CLI exit contract: 0 SAFE, 1 CAUTION or
// UNSAFE, 3 known stale, 5 insufficient coverage, 4 any other unknown. Exit
// 2 (invalid input, setup, not indexed) is an error, not a result.
func ExitCode(res Result) int {
	if res.Status == StatusAssessed && res.Verdict != nil {
		if *res.Verdict == VerdictSafe {
			return ExitSafe
		}
		return ExitReview
	}
	switch res.Reason {
	case ReasonKnownStale:
		return ExitKnownStale
	case ReasonInsufficientCoverage:
		return ExitInsufficientCoverage
	default:
		return ExitUnknown
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

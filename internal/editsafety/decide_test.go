package editsafety

import (
	"testing"

	"github.com/isink17/codegraph/internal/constraints"
	"github.com/isink17/codegraph/internal/store"
)

// TestDecideOrder pins the evaluation order: freshness, then the parser
// semantic state, then coverage, then the verdict rules. Evidence never
// produces a verdict on a graph that cannot be vouched for, and no input
// produces SAFE.
func TestDecideOrder(t *testing.T) {
	type in struct {
		fresh      string
		blocked    bool
		covered    bool
		constraint string
		dependents int
		fromTests  int
	}
	full := constraints.VerdictFullCoverageAtHead
	for _, tc := range []struct {
		name    string
		in      in
		status  string
		verdict string
		reason  string
	}{
		{"stale beats a violation", in{constraints.VerdictKnownStale, false, true, OutcomeFired, 3, 0}, StatusUnknown, "", ReasonKnownStale},
		{"insufficient freshness coverage", in{constraints.VerdictInsufficientCoverage, false, true, OutcomeFired, 3, 0}, StatusUnknown, "", ReasonInsufficientCoverage},
		{"freshness unknown", in{constraints.VerdictUnknown, false, true, OutcomeNotFired, 3, 0}, StatusUnknown, "", ReasonFreshnessUnknown},
		{"semantic upgrade", in{full, true, true, OutcomeNotEvaluated, 0, 0}, StatusUnknown, "", ReasonParserSemanticUpgrade},
		{"language coverage missing beats dependents", in{full, false, false, OutcomeNotFired, 3, 0}, StatusUnknown, "", ReasonInsufficientCoverage},
		{"language coverage missing beats a violation", in{full, false, false, OutcomeFired, 3, 0}, StatusUnknown, "", ReasonInsufficientCoverage},
		{"violation", in{full, false, true, OutcomeFired, 0, 0}, StatusAssessed, VerdictUnsafe, RuleConstraintViolation},
		{"indeterminate constraints withhold CAUTION", in{full, false, true, OutcomeUnknown, 2, 1}, StatusUnknown, "", ReasonConstraintsIndeterminate},
		{"untested dependents", in{full, false, true, OutcomeNotFired, 2, 0}, StatusAssessed, VerdictCaution, RuleUntestedDependents},
		{"tested dependents", in{full, false, true, OutcomeNotEvaluated, 2, 1}, StatusAssessed, VerdictCaution, RuleKnownDependents},
		// Every adequacy condition holds and nothing depends on the target:
		// still not SAFE, because no isolation rule is proven.
		{"no evidence of dependents", in{full, false, true, OutcomeNotFired, 0, 0}, StatusUnknown, "", ReasonIsolationUnproven},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Result{
				Freshness: Freshness{Verdict: tc.in.fresh},
				Coverage:  Coverage{Sufficient: tc.in.covered},
				Evidence:  Evidence{DependentsTotal: tc.in.dependents, DependentsFromTests: tc.in.fromTests, Dependents: []store.EditSafetyDependent{}},
			}
			decide(&res, tc.in.blocked, tc.in.constraint, "")
			got := ""
			if res.Verdict != nil {
				got = *res.Verdict
			}
			if res.Status != tc.status || got != tc.verdict || res.Reason != tc.reason {
				t.Fatalf("got %s/%q/%s, want %s/%q/%s", res.Status, got, res.Reason, tc.status, tc.verdict, tc.reason)
			}
			if got == VerdictSafe {
				t.Fatal("SAFE produced without an isolation rule")
			}
			if res.Status == StatusUnknown && res.Verdict != nil {
				t.Fatal("unknown status carries a verdict")
			}
			ids := map[string]bool{}
			for _, r := range res.Rules {
				ids[r.ID] = true
			}
			for _, id := range []string{RuleFreshness, RuleCoverage, RuleConstraintViolation, RuleKnownDependents, RuleUntestedDependents, RuleUnresolvedCandidate, RuleIsolation} {
				if !ids[id] {
					t.Fatalf("rule %s missing from %+v", id, res.Rules)
				}
			}
		})
	}
}

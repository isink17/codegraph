package audit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestFixtureIntegrity asserts the fixture still contains what its cases claim:
// every declared case has a source file, a unique dst_name, and every expected
// definition symbol is actually produced by the parsers.
//
// It deliberately asserts nothing about which destination the resolver picks.
func TestFixtureIntegrity(t *testing.T) {
	files := map[string]struct{}{}
	for _, path := range FixtureFilePaths() {
		files[path] = struct{}{}
	}

	seenName := map[string]string{}
	for _, c := range Cases() {
		if _, ok := files[c.SrcFile]; !ok {
			t.Errorf("case %s: src_file %q is not part of the fixture", c.ID, c.SrcFile)
		}
		if prev, ok := seenName[c.DstName]; ok {
			t.Errorf("case %s reuses dst_name %q already used by case %s; cases must not share a dst_name", c.ID, c.DstName, prev)
		}
		seenName[c.DstName] = c.ID

		switch c.Expect {
		case ExpectValid:
			if len(c.ValidTargets) == 0 {
				t.Errorf("case %s is expected_valid but declares no valid targets", c.ID)
			}
		case ExpectInvalid, ExpectUnresolved:
			if len(c.ValidTargets) != 0 {
				t.Errorf("case %s declares valid targets but is %s", c.ID, c.Expect)
			}
		case ExpectAmbiguous:
			if len(c.ValidTargets) != 0 {
				t.Errorf("case %s declares valid targets but is %s; an ambiguous case has no selectable target", c.ID, c.Expect)
			}
			if len(c.CompetingTargets) < 2 {
				t.Errorf("case %s is expected_ambiguous but declares %d competing target(s); ambiguity needs at least 2",
					c.ID, len(c.CompetingTargets))
			}
			// Without this, dropping one definition from the fixture would
			// leave the case passing as "expected unresolved" while no longer
			// being ambiguous, i.e. while no longer probing anything.
			for _, shadow := range c.ShadowTargets {
				if !containsString(c.CompetingTargets, shadow) {
					t.Errorf("case %s declares shadow target %q that is not among its competing targets %v",
						c.ID, shadow, c.CompetingTargets)
				}
			}
			for _, competing := range c.CompetingTargets {
				if !strings.HasSuffix(competing, "."+c.DstName) && competing != c.DstName {
					t.Errorf("case %s competing target %q does not define the called name %q",
						c.ID, competing, c.DstName)
				}
			}
		default:
			t.Errorf("case %s has unknown expectation %q", c.ID, c.Expect)
		}
	}

	for _, def := range ExpectedDefinitions() {
		if _, ok := files[def.File]; !ok {
			t.Errorf("expected definition %s/%s names missing fixture file %q", def.CaseID, def.Name, def.File)
		}
	}
}

// TestFixtureProducesExpectedDefinitions indexes the fixture once and asserts
// each expected definition symbol exists with the expected language. Without
// this, an adapter regex change could silently reduce a case to a no-op and the
// miswire count would improve for the wrong reason.
func TestFixtureProducesExpectedDefinitions(t *testing.T) {
	report := runAudit(t, Options{Repeats: 1})

	for _, c := range report.Cases {
		if c.Outcome == OutcomeNoEdge {
			t.Errorf("case %s produced no call edge; the fixture no longer exercises it", c.ID)
		}
	}
	if len(report.Unclassified) != 0 {
		t.Errorf("fixture produced %d unclassified call edges; every edge should belong to a declared case: %+v",
			len(report.Unclassified), report.Unclassified)
	}
	if got, want := report.Metrics.CallEdgesInspected, len(Cases()); got != want {
		t.Errorf("call edges inspected = %d, want %d (one per case)", got, want)
	}

	// Every declared definition must actually be produced by the adapters,
	// otherwise a case silently stops probing anything.
	byCase := map[string][]string{}
	for _, c := range report.Cases {
		byCase[c.ID] = c.CandidateTargets
	}
	for _, def := range ExpectedDefinitions() {
		want := "(" + def.Language + ") " + def.File
		found := false
		for _, cand := range byCase[def.CaseID] {
			if strings.Contains(cand, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("case %s: expected %s definition %q in %s was not indexed as a candidate; candidates=%v",
				def.CaseID, def.Language, def.Name, def.File, byCase[def.CaseID])
		}
	}
}

// TestClassificationRules pins the classification logic itself, independent of
// the current resolver. These are the assertions that must keep holding when P2
// changes resolution: the rules stay, the measured counts move.
func TestClassificationRules(t *testing.T) {
	resolved := func(qualified string) *EdgeObservation {
		return &EdgeObservation{Resolved: true, DstQualifiedName: qualified}
	}
	unresolved := &EdgeObservation{}

	invalid := Case{ID: "x", Expect: ExpectInvalid}
	valid := Case{ID: "y", Expect: ExpectValid, ValidTargets: []string{"pkg.good"}}
	honest := Case{ID: "z", Expect: ExpectUnresolved}
	ambiguous := Case{ID: "w", Expect: ExpectAmbiguous, CompetingTargets: []string{"pkg.one", "tests.one"}}

	tests := []struct {
		name string
		c    Case
		edge *EdgeObservation
		want Outcome
	}{
		{"invalid case resolved is miswired", invalid, resolved("other.thing"), OutcomeMiswired},
		{"invalid case unresolved is correct", invalid, unresolved, OutcomeCorrectlyUnresolved},
		{"valid case resolved to declared target", valid, resolved("pkg.good"), OutcomeExpectedValid},
		{"valid case resolved elsewhere is miswired", valid, resolved("pkg.bad"), OutcomeMiswired},
		{"valid case unresolved is a missing edge", valid, unresolved, OutcomeMissingEdge},
		{"honest negative unresolved is expected", honest, unresolved, OutcomeExpectedUnresolved},
		{"honest negative resolved is miswired", honest, resolved("anything"), OutcomeMiswired},
		{"ambiguous case unresolved is expected", ambiguous, unresolved, OutcomeExpectedUnresolved},
		{"ambiguous case resolved to a competing target is miswired", ambiguous, resolved("pkg.one"), OutcomeMiswired},
		{"ambiguous case resolved to the test definition is miswired", ambiguous, resolved("tests.one"), OutcomeMiswired},
		{"absent edge is reported", valid, nil, OutcomeNoEdge},
	}
	for _, tc := range tests {
		if got := caseOutcome(tc.c, tc.edge); got != tc.want {
			t.Errorf("%s: outcome = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCrossLanguageMiswireIsClassifiedNotAsserted documents the P1 contract:
// the harness must classify a cross-language name coincidence as miswired
// without asserting that the resolver produces it. If P2 fixes resolution this
// test still passes; only the recorded baseline changes.
func TestCrossLanguageMiswireIsClassifiedNotAsserted(t *testing.T) {
	report := runAudit(t, Options{Repeats: 1})

	caseA := findCase(t, report, "A")
	if caseA.Expectation != ExpectInvalid {
		t.Fatalf("case A expectation = %q, want %q", caseA.Expectation, ExpectInvalid)
	}
	switch caseA.Outcome {
	case OutcomeMiswired:
		if caseA.Selected == nil || !caseA.Selected.Resolved {
			t.Fatal("case A is miswired but no selected destination was recorded")
		}
		if caseA.Selected.DstLanguage == caseA.SrcLanguage {
			t.Errorf("case A miswire should cross a language boundary, got src=%s dst=%s",
				caseA.SrcLanguage, caseA.Selected.DstLanguage)
		}
	case OutcomeCorrectlyUnresolved:
		// A future resolver may decline this edge. That is the desired
		// direction and must not fail the suite.
	default:
		t.Errorf("case A outcome = %q, want miswired or correctly_unresolved", caseA.Outcome)
	}
}

// TestSameLanguageControlStillResolves guards against "fixing" correctness by
// refusing to resolve anything.
func TestSameLanguageControlStillResolves(t *testing.T) {
	report := runAudit(t, Options{Repeats: 1})

	caseB := findCase(t, report, "B")
	if caseB.Outcome != OutcomeExpectedValid {
		t.Errorf("case B outcome = %q, want %q; the legitimate same-language call must keep resolving",
			caseB.Outcome, OutcomeExpectedValid)
	}
	if caseB.Selected == nil || caseB.Selected.DstLanguage != "python" {
		t.Errorf("case B should resolve to a python definition, got %+v", caseB.Selected)
	}
}

// TestStableProjectionIsReproducible requires the deterministic-by-construction
// part of the report to be byte-identical across invocations.
//
// It deliberately does not compare the whole report. The resolver breaks
// same-name ties with MIN(id) / an unordered LIMIT 1 over symbol rows whose ids
// follow indexing worker completion order, so the *selected destination* is not
// reproducible on the current resolver. That is a measured defect, recorded in
// BASELINE.md, not something this test may paper over by asserting a fixed
// winner. What must be reproducible is the fixture, the edge counts, and the
// candidate ambiguity each case presents.
func TestStableProjectionIsReproducible(t *testing.T) {
	first := runAudit(t, Options{Repeats: 2})
	second := runAudit(t, Options{Repeats: 2})

	if got, want := projectionJSON(t, second), projectionJSON(t, first); got != want {
		t.Errorf("stable projection is not reproducible across invocations\nfirst:\n%s\nsecond:\n%s", want, got)
	}
}

// TestNondeterminismIsReportedNotHidden verifies the harness records a
// divergence whenever it observes one, so instability can never be silently
// smoothed into a clean-looking baseline.
func TestNondeterminismIsReportedNotHidden(t *testing.T) {
	report := runAudit(t, Options{Repeats: 3})
	det := report.Determinism

	if det.Repeats != 3 {
		t.Errorf("determinism repeats = %d, want 3", det.Repeats)
	}
	if !det.Sampled || det.Note == "" {
		t.Error("determinism verdict must be labelled as sampled and carry its caveat")
	}

	unstable := !det.MetricsStable || !det.CaseOutcomesStable || !det.SelectedTargetsStable
	if unstable && len(det.Divergences) == 0 {
		t.Error("instability was reported without recording any divergence")
	}
	if !unstable && len(det.Divergences) != 0 {
		t.Errorf("divergences recorded while everything was reported stable: %v", det.Divergences)
	}

	for _, c := range report.Cases {
		switch {
		case c.SelectedStable && len(c.SelectedVariants) != 0:
			t.Errorf("case %s is marked stable but lists variants %v", c.ID, c.SelectedVariants)
		case !c.SelectedStable && len(c.SelectedVariants) < 2:
			t.Errorf("case %s is marked unstable but lists variants %v", c.ID, c.SelectedVariants)
		case !c.SelectedStable && c.Selected != nil:
			t.Errorf("case %s is unstable so it must not report a single selected destination", c.ID)
		}
	}
}

// TestAmbiguityIsMeasured checks the harness records how many candidate
// definitions each case presented. This counter is derived from persisted
// symbols rather than from a tie-break, so it is the metric P2 can compare
// against without fighting resolver nondeterminism.
func TestAmbiguityIsMeasured(t *testing.T) {
	report := runAudit(t, Options{Repeats: 1})

	ambiguous := 0
	for _, c := range report.Cases {
		if c.CandidateCount != len(c.CandidateTargets) {
			t.Errorf("case %s: candidate count %d does not match listed targets %d",
				c.ID, c.CandidateCount, len(c.CandidateTargets))
		}
		if c.CandidateCount > 1 {
			ambiguous++
		}
	}
	if report.Metrics.AmbiguousCases != ambiguous {
		t.Errorf("metrics ambiguous_cases = %d, want %d", report.Metrics.AmbiguousCases, ambiguous)
	}

	// Case C exists precisely to present several same-named definitions.
	caseC := findCase(t, report, "C")
	if caseC.CandidateCount < 3 {
		t.Errorf("case C should present at least 3 candidate definitions, got %d: %v",
			caseC.CandidateCount, caseC.CandidateTargets)
	}
}

// TestAmbiguousCaseStaysUnresolvedDeterministically pins the P3 contract for
// case E: two same-language definitions compete, nothing distinguishes them, so
// every repeat must leave the edge unresolved. A run that binds either one --
// even the production definition -- is an arbitrary selection and fails here.
func TestAmbiguousCaseStaysUnresolvedDeterministically(t *testing.T) {
	report := runAudit(t, Options{Repeats: 5})

	caseE := findCase(t, report, "E")
	if caseE.Expectation != ExpectAmbiguous {
		t.Fatalf("case E expectation = %q, want %q", caseE.Expectation, ExpectAmbiguous)
	}
	if caseE.CandidateCount < 2 {
		t.Fatalf("case E should present competing candidates, got %d: %v", caseE.CandidateCount, caseE.CandidateTargets)
	}
	if caseE.Outcome != OutcomeExpectedUnresolved {
		t.Errorf("case E outcome = %q (variants %v), want %q; ambiguous candidates must not be tie-broken",
			caseE.Outcome, caseE.OutcomeVariants, OutcomeExpectedUnresolved)
	}
	if caseE.MiswiredAnyRun {
		t.Error("case E bound a destination in at least one run; ambiguity must not be resolved by insertion order")
	}
	if caseE.TestShadowBinding {
		t.Error("case E bound a test definition; no test-shadow ranking rule exists")
	}
	if !caseE.SelectedStable {
		t.Errorf("case E selected different destinations across runs: %v", caseE.SelectedVariants)
	}
}

// TestMetricsAreSelfConsistent checks the counters add up, so a baseline cannot
// record an arithmetically impossible measurement.
func TestMetricsAreSelfConsistent(t *testing.T) {
	report := runAudit(t, Options{Repeats: 1})
	m := report.Metrics

	if m.Resolved+m.Unresolved != m.CallEdgesInspected {
		t.Errorf("resolved(%d) + unresolved(%d) != inspected(%d)", m.Resolved, m.Unresolved, m.CallEdgesInspected)
	}
	if m.SameLanguageResolved+m.CrossLanguageResolved > m.Resolved {
		t.Errorf("same(%d) + cross(%d) exceeds resolved(%d)", m.SameLanguageResolved, m.CrossLanguageResolved, m.Resolved)
	}
	if m.SameLanguageResolved+m.CrossLanguageResolved+m.UnknownLanguageResolved != m.Resolved {
		t.Errorf("same(%d) + cross(%d) + unknown(%d) != resolved(%d)",
			m.SameLanguageResolved, m.CrossLanguageResolved, m.UnknownLanguageResolved, m.Resolved)
	}
	sum := m.ExpectedValid + m.ExpectedUnresolved + m.CorrectlyUnresolved +
		m.Miswired + m.MissingEdge + m.NoCallEdge + m.Nondeterministic
	if sum != m.Cases {
		t.Errorf("case outcome counts sum to %d, want %d", sum, m.Cases)
	}
	if m.MiswiredAnyRun < m.Miswired {
		t.Errorf("miswired_any_run(%d) must be at least miswired(%d)", m.MiswiredAnyRun, m.Miswired)
	}
	if m.Cases != len(Cases()) {
		t.Errorf("metrics cases = %d, want %d", m.Cases, len(Cases()))
	}
}

// TestReportRenders checks both output forms are produced and carry provenance.
func TestReportRenders(t *testing.T) {
	report := runAudit(t, Options{Repeats: 1})

	var jsonOut, textOut strings.Builder
	if err := report.WriteJSON(&jsonOut); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if err := report.WriteText(&textOut); err != nil {
		t.Fatalf("WriteText: %v", err)
	}

	for _, want := range []string{SchemaID, FixtureVersion, `"miswired"`, `"cross_language_resolved"`, `"sqlite_driver"`} {
		if !strings.Contains(jsonOut.String(), want) {
			t.Errorf("JSON report missing %q", want)
		}
	}
	for _, want := range []string{"resolver correctness audit", "case outcomes:", "determinism (sampled", "ambiguity:", "findings:"} {
		if !strings.Contains(textOut.String(), want) {
			t.Errorf("text report missing %q", want)
		}
	}
	if report.Context.SQLiteDriver == "" || report.Context.GoVersion == "" || len(report.Context.Adapters) == 0 {
		t.Errorf("report context is incomplete: %+v", report.Context)
	}
	if report.Fixture.FFIDeclared {
		t.Error("fixture must declare no FFI, otherwise cross-language edges are not automatically wrong")
	}
}

// TestIncrementalParityIsMeasured covers the optional parity pass. It asserts
// the comparison runs and is internally consistent, not which way it comes out,
// so P2 may legitimately make the two paths agree.
func TestIncrementalParityIsMeasured(t *testing.T) {
	report := runAudit(t, Options{Repeats: 1, IncludeIncrementalParity: true})

	inc := report.Incremental
	if inc == nil {
		t.Fatal("incremental parity was requested but not reported")
	}
	if inc.FullResolveMode == "" || inc.IncrementalResolveMode == "" {
		t.Errorf("both resolve modes must be recorded, got full=%q incremental=%q",
			inc.FullResolveMode, inc.IncrementalResolveMode)
	}
	if inc.Agrees != (len(inc.Divergences) == 0) {
		t.Errorf("agrees=%t contradicts %d recorded divergences", inc.Agrees, len(inc.Divergences))
	}
}

// TestFixtureDeclaresNoFFI checks the premise the whole fixture rests on. If a
// fixture file ever declared a foreign function interface, a cross-language edge
// could be legitimate and "miswired" would be the wrong classification.
func TestFixtureDeclaresNoFFI(t *testing.T) {
	if FixtureDeclaresFFI() {
		t.Error("a fixture source declares an FFI; cross-language edges would no longer be automatically wrong")
	}
}

// TestSuffixKeyHelpersMirrorTheStore pins the copied suffix-key logic. The
// harness models the resolver's candidate set with these, so a drift from the
// store would silently under-report ambiguity.
func TestSuffixKeyHelpersMirrorTheStore(t *testing.T) {
	tests := []struct {
		qname                    string
		wantSuffix, want2, want3 string
	}{
		{"a.b", "", "a.b", ""},
		{"pkg.Class.method", "", "Class.method", ""},
		{"a.b.c.d", "", "c.d", "b.c.d"},
		{"host/path.Class.method", "path.Class.method", "Class.method", ""},
		{"x.y/z.f", "z.f", "z.f", ""},
		{"single", "", "", ""},
	}
	for _, tc := range tests {
		if got := qualifiedSuffix(tc.qname); got != tc.wantSuffix {
			t.Errorf("qualifiedSuffix(%q) = %q, want %q", tc.qname, got, tc.wantSuffix)
		}
		if got := dotTail2(tc.qname); got != tc.want2 {
			t.Errorf("dotTail2(%q) = %q, want %q", tc.qname, got, tc.want2)
		}
		if got := dotTail3(tc.qname); got != tc.want3 {
			t.Errorf("dotTail3(%q) = %q, want %q", tc.qname, got, tc.want3)
		}
	}
}

// TestCandidateModelCoversSelectedDestinations asserts the harness never reports
// a selected destination it could not have predicted as a candidate, since that
// would mean the ambiguity numbers understate reality.
func TestCandidateModelCoversSelectedDestinations(t *testing.T) {
	report := runAudit(t, Options{Repeats: 2})

	for _, d := range report.Determinism.Divergences {
		if strings.Contains(d, "candidate model does not cover") {
			t.Errorf("harness candidate model missed a destination the resolver chose: %s", d)
		}
	}
	for _, c := range report.Cases {
		if c.Selected == nil || !c.Selected.Resolved {
			continue
		}
		if !containsString(c.CandidateTargets, c.Selected.target()) {
			t.Errorf("case %s selected %q which is absent from its candidate list %v",
				c.ID, c.Selected.target(), c.CandidateTargets)
		}
	}
}

func runAudit(t *testing.T, opts Options) *Report {
	t.Helper()
	report, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

func findCase(t *testing.T, report *Report, id string) CaseResult {
	t.Helper()
	for _, c := range report.Cases {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("case %s missing from report", id)
	return CaseResult{}
}

// projectionJSON renders the deterministic-by-construction projection.
func projectionJSON(t *testing.T, report *Report) string {
	t.Helper()
	raw, err := json.MarshalIndent(report.StableProjection(), "", "  ")
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	return string(raw)
}

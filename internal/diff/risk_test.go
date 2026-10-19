package diff

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func riskDecl(path, name string) store.SemanticDeclaration {
	return store.SemanticDeclaration{Path: path, Language: "go", Kind: "function", Name: name, QualifiedName: "p." + name, Signature: "func " + name + "()", StableKey: "func:p::" + name}
}

func endpoint(d store.SemanticDeclaration, state string) store.SemanticEndpoint {
	return store.SemanticEndpoint{Path: d.Path, Language: d.Language, Kind: d.Kind, QualifiedName: d.QualifiedName, Signature: d.Signature, StableKey: d.StableKey, State: state}
}

func riskCall(from, to store.SemanticDeclaration, line, col int, state string) store.SemanticEdge {
	return store.SemanticEdge{Source: endpoint(from, "resolved"), Target: endpoint(to, state), Path: from.Path, Kind: "calls", Line: line, Column: col}
}

func riskRevision(files []store.SemanticFile, decls []store.SemanticDeclaration, edges []store.SemanticEdge) Revision {
	r := coverageRevision(files, edges, store.GraphCapability{})
	r.GraphData.Declarations = decls
	r.GraphData.State.JVMScope.State = "complete"
	return r
}

func indexed(paths ...string) []store.SemanticFile {
	var out []store.SemanticFile
	for _, p := range paths {
		out = append(out, store.SemanticFile{Path: p, Language: "go", ParseState: store.ParseStateIndexed})
	}
	return out
}

func TestRiskCitesCallersOfRemovedDeclaration(t *testing.T) {
	gone, a, b := riskDecl("lib.go", "Gone"), riskDecl("a.go", "A"), riskDecl("b.go", "B")
	baseEdges := []store.SemanticEdge{riskCall(b, gone, 9, 2, "resolved"), riskCall(a, gone, 4, 2, "resolved")}
	headEdges := []store.SemanticEdge{riskCall(a, store.SemanticDeclaration{QualifiedName: "Gone"}, 4, 2, "unresolved")}
	base := riskRevision(indexed("a.go", "b.go", "lib.go"), []store.SemanticDeclaration{gone, a, b}, baseEdges)
	head := riskRevision(indexed("a.go", "b.go", "lib.go"), []store.SemanticDeclaration{a, b}, headEdges)
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	r := got.Diff.Risk
	// A still calls Gone, now unresolved; B no longer references it.
	if r.Assessment != AssessmentRiskEvidence || r.Score == nil || *r.Score != 20 || r.Level != RiskLevelLow {
		t.Fatalf("risk = %s %v %s", r.Assessment, r.Score, r.Level)
	}
	ev := component(t, r, RuleRemovedDeclarationDependent).Evidence
	if len(ev) != 1 || ev[0].Change != "declaration_removed" || ev[0].Declaration != declIdentity(gone) || ev[0].Dependent != declIdentity(a) || ev[0].File != "a.go" || !slices.Equal(ev[0].HeadStates, []string{"unresolved"}) || len(ev[0].Sites) != 1 || len(ev[0].HeadSites) != 1 {
		t.Fatalf("evidence = %+v", ev)
	}
	if r.Coverage.State != CoveragePartial || !strings.Contains(r.Coverage.Reason, "only resolved edges") {
		t.Fatalf("risk coverage = %+v", r.Coverage)
	}
	if len(r.RemovedDeclarationDependents) != 1 || r.RemovedDeclarationDependents[0].Declaration != declIdentity(gone) {
		t.Fatalf("risk = %+v", r)
	}
	calls := r.RemovedDeclarationDependents[0].Edges
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	var sources []string
	for _, c := range calls {
		sources = append(sources, c.Base.Source.QualifiedName)
		if c.Site != edgeSite(c.Base, true) {
			t.Fatalf("site %q does not cite the edge identity", c.Site)
		}
		switch c.Base.Source.QualifiedName {
		case "p.A":
			if c.Head == nil || c.Head.Target.State != "unresolved" {
				t.Fatalf("A's call should now be unresolved: %+v", c.Head)
			}
		case "p.B":
			if c.Head != nil {
				t.Fatalf("B's call is gone in head: %+v", c.Head)
			}
		}
	}
	slices.Sort(sources)
	if !slices.Equal(sources, []string{"p.A", "p.B"}) {
		t.Fatalf("sources = %v", sources)
	}
	// Shuffled input gives byte-identical output.
	slices.Reverse(base.GraphData.Edges)
	again, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if canonical(again.Diff.Risk) != canonical(r) {
		t.Fatalf("risk depends on input order")
	}
}

func TestRiskSkipsRemovedCallersAndIncompleteFiles(t *testing.T) {
	gone, dead, a := riskDecl("lib.go", "Gone"), riskDecl("dead.go", "Dead"), riskDecl("a.go", "A")
	baseEdges := []store.SemanticEdge{riskCall(dead, gone, 3, 2, "resolved"), riskCall(a, gone, 4, 2, "resolved")}
	files := indexed("dead.go", "lib.go")
	files = append(files, store.SemanticFile{Path: "a.go", Language: "go", ParseState: store.ParseStatePending})
	base := riskRevision(files, []store.SemanticDeclaration{gone, dead, a}, baseEdges)
	head := riskRevision(files, []store.SemanticDeclaration{a}, nil)
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	r := got.Diff.Risk
	// Dead was removed with Gone, and A's call lies in a file without a
	// current parse: no call is citable, so Gone gets no entry.
	if len(r.RemovedDeclarationDependents) != 0 {
		t.Fatalf("risk = %+v", r)
	}
	if got.Diff.Summary["symbols_removed"] != 2 {
		t.Fatalf("summary = %v", got.Diff.Summary)
	}
	if r.Coverage.State != CoveragePartial || !strings.Contains(r.Coverage.Reason, "without a current parse") {
		t.Fatalf("risk coverage = %+v", r.Coverage)
	}
	if r.Assessment != AssessmentLimitedEvidence || *r.Score != 0 || !hasUncertainty(r, "removed_declaration_without_resolved_dependents", declIdentity(gone)) || !hasUncertainty(r, "edges_coverage_partial", "") {
		t.Fatalf("risk = %+v", r)
	}
}

func TestRiskUnavailableWhenEdgesNotCompared(t *testing.T) {
	gone, a := riskDecl("lib.go", "Gone"), riskDecl("a.go", "A")
	base := riskRevision(indexed("a.go", "lib.go"), []store.SemanticDeclaration{gone, a}, []store.SemanticEdge{riskCall(a, gone, 4, 2, "resolved")})
	head := riskRevision(indexed("a.go", "lib.go"), []store.SemanticDeclaration{a}, nil)
	head.GraphData.State.ParseFailures = 1
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if r := got.Diff.Risk; r.Assessment != AssessmentInsufficientEvidence || r.Score != nil || r.Level != RiskLevelUnknown || r.Coverage.State != CoverageUnavailable || len(r.RemovedDeclarationDependents) != 0 || len(r.Components) != 4 {
		t.Fatalf("risk = %+v", r)
	}
}

// An incomplete file's column-less edges must not switch a complete file's
// same-line calls to line-only matching.
func TestIncompleteFileDoesNotDisableEdgeColumns(t *testing.T) {
	a, b, c, d := riskDecl("a.go", "A"), riskDecl("lib.go", "B"), riskDecl("lib.go", "C"), riskDecl("lib.go", "D")
	stale := coverageEdge("old.go", 0, "X")
	files := indexed("a.go", "lib.go")
	files = append(files, store.SemanticFile{Path: "old.go", Language: "go", ParseState: store.ParseStatePending})
	decls := []store.SemanticDeclaration{a, b, c, d}
	base := riskRevision(files, decls, []store.SemanticEdge{riskCall(a, b, 10, 5, "resolved"), riskCall(a, c, 10, 9, "resolved"), stale})
	head := riskRevision(files, decls, []store.SemanticEdge{riskCall(a, b, 10, 5, "resolved"), riskCall(a, d, 10, 9, "resolved"), stale})
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.Diff.Summary["edge_ambiguous"] != 0 || len(got.Diff.Edges.Retargeted) != 1 {
		t.Fatalf("summary=%v retargeted=%+v", got.Diff.Summary, got.Diff.Edges.Retargeted)
	}
	if cov := got.Diff.Coverage.Edges; cov.State != CoveragePartial || strings.Contains(cov.Reason, "call column") {
		t.Fatalf("edge coverage = %+v", cov)
	}
}

func TestRiskMarksAmbiguousHeadSite(t *testing.T) {
	gone, a, x, y := riskDecl("lib.go", "Gone"), riskDecl("a.go", "A"), riskDecl("lib.go", "X"), riskDecl("lib.go", "Y")
	base := riskRevision(indexed("a.go", "lib.go"), []store.SemanticDeclaration{gone, a, x, y}, []store.SemanticEdge{riskCall(a, gone, 4, 2, "resolved")})
	head := riskRevision(indexed("a.go", "lib.go"), []store.SemanticDeclaration{a, x, y}, []store.SemanticEdge{riskCall(a, x, 4, 2, "resolved"), riskCall(a, y, 4, 2, "resolved")})
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	// A no longer references Gone; the ambiguous site is disclosed, not scored.
	if rk := got.Diff.Risk; rk.Assessment != AssessmentLimitedEvidence || *rk.Score != 0 || !hasUncertainty(rk, "dependent_site_ambiguous", "") || !hasUncertainty(rk, "edge_ambiguous", "") {
		t.Fatalf("risk = %+v", rk)
	}
	r := got.Diff.Risk.RemovedDeclarationDependents
	if len(r) != 1 || len(r[0].Edges) != 1 || !r[0].Edges[0].HeadAmbiguous || r[0].Edges[0].Head != nil {
		t.Fatalf("risk = %+v", r)
	}
}

func component(t *testing.T, r Risk, rule string) RiskComponent {
	t.Helper()
	for _, c := range r.Components {
		if c.Rule == rule {
			return c
		}
	}
	t.Fatalf("no component %s in %+v", rule, r.Components)
	return RiskComponent{}
}

// hasUncertainty matches reason and, when given, identity.
func hasUncertainty(r Risk, reason, identity string) bool {
	for _, u := range r.Uncertainty {
		if u.Reason == reason && (identity == "" || u.Identity == identity) {
			return true
		}
	}
	return false
}

func compareRisk(t *testing.T, base, head Revision) Risk {
	t.Helper()
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return got.Diff.Risk
}

func TestRiskSelfDiffHasNoRiskEvidence(t *testing.T) {
	x, a := riskDecl("lib.go", "X"), riskDecl("a.go", "A")
	rev := riskRevision(indexed("a.go", "lib.go"), []store.SemanticDeclaration{x, a}, []store.SemanticEdge{riskCall(a, x, 4, 2, "resolved")})
	r := compareRisk(t, rev, rev)
	if r.Assessment != AssessmentNoRiskEvidence || r.Score == nil || *r.Score != 0 || r.Level != RiskLevelNone || len(r.Uncertainty) != 0 || r.Model != RiskModel || r.ScoreKind != RiskScoreKind {
		t.Fatalf("risk = %+v", r)
	}
}

// Only an unresolved edge names the removed declaration: no evidence, but the
// risk is never reported as absent.
func TestRiskUnresolvedDependentIsNotEvidence(t *testing.T) {
	gone, a := riskDecl("lib.go", "Gone"), riskDecl("a.go", "A")
	dynamic := riskCall(a, store.SemanticDeclaration{QualifiedName: "Gone"}, 4, 2, "unresolved")
	base := riskRevision(indexed("a.go", "lib.go"), []store.SemanticDeclaration{gone, a}, []store.SemanticEdge{dynamic})
	head := riskRevision(indexed("a.go", "lib.go"), []store.SemanticDeclaration{a}, []store.SemanticEdge{dynamic})
	r := compareRisk(t, base, head)
	if r.Assessment != AssessmentLimitedEvidence || *r.Score != 0 || !hasUncertainty(r, "removed_declaration_without_resolved_dependents", declIdentity(gone)) {
		t.Fatalf("risk = %+v", r)
	}
}

func TestRiskDependentReboundToAnotherDeclaration(t *testing.T) {
	gone, moved, a := riskDecl("lib.go", "Gone"), riskDecl("other/lib.go", "Gone"), riskDecl("a.go", "A")
	files := indexed("a.go", "lib.go", "other/lib.go")
	base := riskRevision(files, []store.SemanticDeclaration{gone, a}, []store.SemanticEdge{riskCall(a, gone, 4, 2, "resolved")})
	// The same name now binds to a declaration in another directory, on a shifted line.
	head := riskRevision(files, []store.SemanticDeclaration{moved, a}, []store.SemanticEdge{riskCall(a, moved, 6, 2, "resolved")})
	r := compareRisk(t, base, head)
	if component(t, r, RuleRemovedDeclarationDependent).Count != 0 || *r.Score != 5 || r.Level != RiskLevelLow {
		t.Fatalf("risk = %+v", r)
	}
	ev := component(t, r, RuleDependentRetargeted).Evidence
	if len(ev) != 1 || ev[0].Change != "binding_changed" || ev[0].Declaration != declIdentity(gone) || !slices.Equal(ev[0].HeadTargets, []string{declIdentity(moved)}) {
		t.Fatalf("evidence = %+v", ev)
	}
	// A renamed callee the dependent was updated to is not a rebinding.
	renamed := riskDecl("lib.go", "Renamed")
	head = riskRevision(files, []store.SemanticDeclaration{renamed, a}, []store.SemanticEdge{riskCall(a, renamed, 4, 2, "resolved")})
	if r := compareRisk(t, base, head); *r.Score != 0 {
		t.Fatalf("updated caller scored: %+v", r.Components)
	}
}

// An unrelated call landing on a shifted line and column is not a rebinding.
func TestRiskIgnoresSiteCoincidence(t *testing.T) {
	x, y, a := riskDecl("lib.go", "X"), riskDecl("lib.go", "Y"), riskDecl("a.go", "A")
	decls := []store.SemanticDeclaration{x, y, a}
	base := riskRevision(indexed("a.go", "lib.go"), decls, []store.SemanticEdge{riskCall(a, x, 4, 2, "resolved"), riskCall(a, y, 9, 2, "resolved")})
	head := riskRevision(indexed("a.go", "lib.go"), decls, []store.SemanticEdge{riskCall(a, y, 4, 2, "resolved"), riskCall(a, x, 12, 2, "resolved")})
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Diff.Edges.Retargeted) != 1 || *got.Diff.Risk.Score != 0 || got.Diff.Risk.Assessment != AssessmentNoRiskEvidence {
		t.Fatalf("retargeted=%d risk=%+v", len(got.Diff.Edges.Retargeted), got.Diff.Risk)
	}
}

func TestRiskSurvivingTargetChanges(t *testing.T) {
	x, y, a := riskDecl("lib.go", "X"), riskDecl("lib.go", "Y"), riskDecl("a.go", "A")
	decls := []store.SemanticDeclaration{x, y, a}
	base := riskRevision(indexed("a.go", "lib.go"), decls, []store.SemanticEdge{riskCall(a, x, 4, 2, "resolved"), riskCall(a, y, 5, 2, "resolved")})
	shadow := riskDecl("other/lib.go", "X")
	unresolved := riskCall(a, store.SemanticDeclaration{QualifiedName: "Y"}, 5, 2, "ambiguous")
	head := riskRevision(indexed("a.go", "lib.go", "other/lib.go"), append(decls, shadow), []store.SemanticEdge{riskCall(a, shadow, 4, 2, "resolved"), unresolved})
	r := compareRisk(t, base, head)
	lost, moved := component(t, r, RuleDependentResolutionLost), component(t, r, RuleDependentRetargeted)
	if lost.Count != 1 || lost.Evidence[0].Declaration != declIdentity(y) || lost.Evidence[0].Change != "resolution_lost" || !slices.Equal(lost.Evidence[0].HeadStates, []string{"ambiguous"}) || moved.Count != 1 || moved.Evidence[0].Declaration != declIdentity(x) || *r.Score != 15 {
		t.Fatalf("risk = %+v", r)
	}
}

func TestRiskChangedDeclarationWithDependents(t *testing.T) {
	x, a, b := riskDecl("lib.go", "X"), riskDecl("a.go", "A"), riskDecl("b.go", "B")
	one, two := 1, 2
	x2 := x
	x.ArityMin, x.ArityMax, x2.ArityMin, x2.ArityMax = &one, &one, &two, &two
	edges := []store.SemanticEdge{riskCall(a, x, 4, 2, "resolved"), riskCall(a, x, 6, 2, "resolved"), riskCall(b, x, 3, 2, "resolved")}
	base := riskRevision(indexed("a.go", "b.go", "lib.go"), []store.SemanticDeclaration{x, a, b}, edges)
	head := riskRevision(indexed("a.go", "b.go", "lib.go"), []store.SemanticDeclaration{x2, a, b}, edges)
	r := compareRisk(t, base, head)
	c := component(t, r, RuleChangedDeclarationDependent)
	// Two call sites from A are one dependent relationship.
	if c.Count != 2 || len(c.Evidence[0].Sites) != 2 || c.Evidence[0].Dependent != declIdentity(a) || *r.Score != 10 {
		t.Fatalf("component = %+v", c)
	}
}

func TestRiskDistinctDependentsAndCaps(t *testing.T) {
	gone := riskDecl("lib.go", "Gone")
	decls := []store.SemanticDeclaration{gone}
	var edges []store.SemanticEdge
	for i, name := range []string{"A", "B", "C", "D"} {
		d := riskDecl(strings.ToLower(name)+".go", name)
		decls = append(decls, d)
		edges = append(edges, riskCall(d, gone, 3, 2, "resolved"), riskCall(d, gone, 7+i, 2, "resolved"))
	}
	var still []store.SemanticEdge
	for _, d := range decls[1:] {
		still = append(still, riskCall(d, store.SemanticDeclaration{QualifiedName: "Gone"}, 3, 2, "unresolved"), riskCall(d, store.SemanticDeclaration{QualifiedName: "Gone"}, 20, 2, "unresolved"))
	}
	files := indexed("a.go", "b.go", "c.go", "d.go", "lib.go")
	r := compareRisk(t, riskRevision(files, decls, edges), riskRevision(files, decls[1:], still))
	c := component(t, r, RuleRemovedDeclarationDependent)
	if c.Count != 4 || c.Points != 60 || *r.Score != 60 || r.Level != RiskLevelHigh {
		t.Fatalf("component = %d/%d score %d", c.Count, c.Points, *r.Score)
	}
	for _, ev := range c.Evidence {
		if len(ev.Sites) != 2 {
			t.Fatalf("sites of one dependent were split: %+v", ev)
		}
	}
}

func TestRiskLevelBoundaries(t *testing.T) {
	for score, want := range map[int]string{0: RiskLevelNone, 1: RiskLevelLow, 24: RiskLevelLow, 25: RiskLevelMedium, 49: RiskLevelMedium, 50: RiskLevelHigh, 100: RiskLevelHigh} {
		if got := RiskLevel(score); got != want {
			t.Fatalf("RiskLevel(%d) = %s, want %s", score, got, want)
		}
	}
	max := 0
	for _, r := range riskRules {
		max += r.maxPoints
	}
	if max < RiskMaxScore {
		t.Fatalf("rule caps sum to %d; score can never reach %d", max, RiskMaxScore)
	}
}

// Declarations with the same qualified name in two directories keep separate
// identities: removing one does not cite callers of the other.
func TestRiskIdentityAcrossDirectories(t *testing.T) {
	one, two, a := riskDecl("one/x.go", "Same"), riskDecl("two/x.go", "Same"), riskDecl("a.go", "A")
	files := indexed("a.go", "one/x.go", "two/x.go")
	edges := []store.SemanticEdge{riskCall(a, two, 4, 2, "resolved")}
	r := compareRisk(t, riskRevision(files, []store.SemanticDeclaration{one, two, a}, edges), riskRevision(files, []store.SemanticDeclaration{two, a}, edges))
	if *r.Score != 0 || !hasUncertainty(r, "removed_declaration_without_resolved_dependents", declIdentity(one)) {
		t.Fatalf("risk = %+v", r)
	}
}

func TestRiskAmbiguousDeclarationIsUncertainty(t *testing.T) {
	x, x2, a := riskDecl("lib.go", "X"), riskDecl("lib.go", "X"), riskDecl("a.go", "A")
	x2.StartLine = 9
	files := indexed("a.go", "lib.go")
	r := compareRisk(t, riskRevision(files, []store.SemanticDeclaration{x, a}, nil), riskRevision(files, []store.SemanticDeclaration{x, x2, a}, nil))
	if r.Assessment != AssessmentLimitedEvidence || !hasUncertainty(r, "declaration_ambiguous", declIdentity(x)) {
		t.Fatalf("risk = %+v", r)
	}
}

func TestRiskIncompatibleSemanticsIsInsufficient(t *testing.T) {
	gone, a := riskDecl("lib.go", "Gone"), riskDecl("a.go", "A")
	base := riskRevision(indexed("a.go", "lib.go"), []store.SemanticDeclaration{gone, a}, []store.SemanticEdge{riskCall(a, gone, 4, 2, "resolved")})
	head := riskRevision(indexed("a.go", "lib.go"), []store.SemanticDeclaration{a}, nil)
	head.GraphData.State.ResolverPolicies = map[string]string{"go": "v2"}
	r := compareRisk(t, base, head)
	if r.Assessment != AssessmentInsufficientEvidence || r.Score != nil || !hasUncertainty(r, "comparison_limitation", "") {
		t.Fatalf("risk = %+v", r)
	}
}

// Risk covers the whole change set: every page carries the same assessment.
func TestRiskIsPageInvariant(t *testing.T) {
	gone, x, a, b := riskDecl("lib.go", "Gone"), riskDecl("lib.go", "X"), riskDecl("a.go", "A"), riskDecl("b.go", "B")
	base := riskRevision(indexed("a.go", "b.go", "lib.go"), []store.SemanticDeclaration{gone, x, a, b}, []store.SemanticEdge{riskCall(a, gone, 4, 2, "resolved"), riskCall(b, x, 3, 2, "resolved")})
	head := riskRevision(indexed("a.go", "b.go", "lib.go"), []store.SemanticDeclaration{x, a, b}, []store.SemanticEdge{riskCall(b, store.SemanticDeclaration{QualifiedName: "X"}, 3, 2, "unresolved")})
	full, err := Compare(base, head, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	want := canonical(full.Diff.Risk)
	for off := 0; off <= full.Diff.Total; off++ {
		page, err := Compare(base, head, off, 1)
		if err != nil {
			t.Fatal(err)
		}
		if canonical(page.Diff.Risk) != want {
			t.Fatalf("risk at offset %d differs", off)
		}
	}
	slices.Reverse(base.GraphData.Declarations)
	slices.Reverse(head.GraphData.Edges)
	if canonical(compareRisk(t, base, head)) != want {
		t.Fatal("risk depends on input order")
	}
}

func TestCompareCommitsRiskLifecycle(t *testing.T) {
	r := gittest.Init(t)
	r.Write("go.mod", "module example.com/m\n\ngo 1.22\n")
	r.Write("lib.go", "package main\n\nfunc Gone() {}\n")
	r.Write("util.go", "package main\n\nfunc Helper() {}\n")
	r.Write("main.go", "package main\n\nfunc main() {\n\tGone()\n\tHelper()\n}\n")
	base := r.Commit("", "base")
	r.Git("mv", "util.go", "helpers.go")
	head := r.Commit("", "rename only")
	r.Write("lib.go", "package main\n")
	broken := r.Commit("", "remove Gone")
	reg := parser.NewRegistry(goparser.New())

	self, err := CompareCommits(t.Context(), r.Dir, base, base, reg, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if rk := self.Diff.Risk; rk.Assessment != AssessmentNoRiskEvidence || *rk.Score != 0 {
		t.Fatalf("self-diff risk = %+v", rk)
	}
	// A provable same-package rename moves Helper; its caller binds to the same declaration.
	renamed, err := CompareCommits(t.Context(), r.Dir, base, head, reg, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if rk := renamed.Diff.Risk; rk.Assessment != AssessmentNoRiskEvidence || *rk.Score != 0 || len(rk.RemovedDeclarationDependents) != 1 || len(renamed.Diff.DeclarationPairs.Pairs) != 1 {
		t.Fatalf("rename risk = %+v pairs %+v", rk, renamed.Diff.DeclarationPairs)
	}
	removed, err := CompareCommits(t.Context(), r.Dir, head, broken, reg, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	rk := removed.Diff.Risk
	ev := component(t, rk, RuleRemovedDeclarationDependent).Evidence
	if rk.Assessment != AssessmentRiskEvidence || *rk.Score != 20 || len(ev) != 1 || !strings.Contains(ev[0].Declaration, "Gone") || !strings.Contains(ev[0].Dependent, `"main.go"`) || !slices.Equal(ev[0].HeadStates, []string{"unresolved"}) {
		t.Fatalf("removal risk = %+v", rk)
	}
}

// Risk read from an incrementally updated graph equals risk read from a
// fresh index of the same tree, and a no-op update changes nothing.
func TestRiskFreshIncrementalAndNoOpParity(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	graphOf := func(st *store.Store) Revision {
		repo, ok, err := st.FindRepo(t.Context(), root)
		if err != nil || !ok {
			t.Fatalf("repo lookup: %v %v", ok, err)
		}
		g, err := st.SemanticGraph(t.Context(), repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		return Revision{GraphData: g}
	}
	open := func() (*store.Store, *indexer.Indexer) {
		st, err := store.Open(filepath.Join(t.TempDir(), "g.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st, indexer.New(st, parser.NewRegistry(goparser.New()), nil)
	}
	opts := indexer.Options{RepoRoot: root, NoHistory: true}
	write("a.go", "package main\nfunc A() { B(); Gone() }\n")
	write("b.go", "package main\nfunc B() {}\n")
	write("gone.go", "package main\nfunc Gone() {}\n")
	incSt, inc := open()
	if _, err := inc.Index(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	base := graphOf(incSt)
	if err := os.Remove(filepath.Join(root, "gone.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := inc.Update(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	updated := compareRisk(t, base, graphOf(incSt))
	if _, err := inc.Update(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	noop := compareRisk(t, base, graphOf(incSt))
	freshSt, fresh := open()
	if _, err := fresh.Index(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	fromFresh := compareRisk(t, base, graphOf(freshSt))
	if updated.Score == nil || *updated.Score != 20 || canonical(updated) != canonical(fromFresh) || canonical(updated) != canonical(noop) {
		t.Fatalf("risk differs\nincremental %s\nno-op %s\nfresh %s", canonical(updated), canonical(noop), canonical(fromFresh))
	}
}

// Repository-wide limitations are cited only through the changed files they
// affect.
func TestRiskScopesRepositoryLimitationsToChangedFiles(t *testing.T) {
	a := riskDecl("a.go", "A")
	tree := func(readme, java string) []SourceFile {
		return []SourceFile{{Path: "README", ContentHash: readme}, {Path: "a.go", ContentHash: "1", Language: "go"}, {Path: "B.java", ContentHash: java, Language: "java"}}
	}
	rev := func(files []SourceFile) Revision {
		r := riskRevision(append(indexed("a.go"), store.SemanticFile{Path: "README", ParseState: store.ParseStateIndexed}), []store.SemanticDeclaration{a}, nil)
		r.GraphData.State.JVMScope.State = "unknown"
		r.TreeFiles = files
		return r
	}
	unchanged := compareRisk(t, rev(tree("r", "j")), rev(tree("r", "j")))
	if unchanged.Assessment != AssessmentNoRiskEvidence {
		t.Fatalf("untouched limitations limited the assessment: %+v", unchanged)
	}
	changed := compareRisk(t, rev(tree("r", "j")), rev(tree("r2", "j2")))
	if changed.Assessment != AssessmentLimitedEvidence || !hasUncertainty(changed, "changed_file_without_semantic_coverage", "README") || !hasUncertainty(changed, "changed_file_jvm_scope_unknown", "B.java") || len(changed.Uncertainty) != 2 {
		t.Fatalf("risk = %+v", changed)
	}
}

// Removing one overload rebinds its callers to a sibling the dependent already
// used; that rebinding is cited, not absorbed by the sibling's relationship.
func TestRiskRemovedOverloadReboundToSibling(t *testing.T) {
	narrow, wide, a := riskDecl("lib.go", "Foo"), riskDecl("lib.go", "Foo"), riskDecl("a.go", "A")
	narrow.Signature, wide.Signature = "func Foo(int)", "func Foo(int64)"
	files := indexed("a.go", "lib.go")
	base := riskRevision(files, []store.SemanticDeclaration{narrow, wide, a}, []store.SemanticEdge{riskCall(a, narrow, 4, 2, "resolved"), riskCall(a, wide, 5, 2, "resolved")})
	head := riskRevision(files, []store.SemanticDeclaration{wide, a}, []store.SemanticEdge{riskCall(a, wide, 4, 2, "resolved"), riskCall(a, wide, 5, 2, "resolved")})
	r := compareRisk(t, base, head)
	ev := component(t, r, RuleDependentRetargeted).Evidence
	if r.Assessment != AssessmentRiskEvidence || *r.Score != 5 || len(ev) != 1 || ev[0].Declaration != declIdentity(narrow) || !slices.Equal(ev[0].HeadTargets, []string{declIdentity(wide)}) {
		t.Fatalf("risk = %+v", r)
	}
}

// A dependent whose signature changed is the only declaration under its path,
// kind and qualified name on both sides: it is paired and scored.
func TestRiskDependentAcrossSignatureChange(t *testing.T) {
	gone, a := riskDecl("lib.go", "Gone"), riskDecl("a.go", "A")
	a2 := a
	a2.Signature = "func A(x int)"
	files := indexed("a.go", "lib.go")
	base := riskRevision(files, []store.SemanticDeclaration{gone, a}, []store.SemanticEdge{riskCall(a, gone, 4, 2, "resolved")})
	head := riskRevision(files, []store.SemanticDeclaration{a2}, []store.SemanticEdge{riskCall(a2, store.SemanticDeclaration{QualifiedName: "Gone"}, 4, 2, "unresolved")})
	r := compareRisk(t, base, head)
	ev := component(t, r, RuleRemovedDeclarationDependent).Evidence
	if r.Assessment != AssessmentRiskEvidence || *r.Score != 20 || len(ev) != 1 || ev[0].Dependent != declIdentity(a) || ev[0].DependentHead != declIdentity(a2) {
		t.Fatalf("risk = %+v", r)
	}
}

// Overloads of the dependent share its qualified name. Only declarations
// without a counterpart compete: a deleted overload is removed, one changed
// overload next to an unchanged sibling is paired, and two changed overloads
// are ambiguous.
func TestRiskDependentOverloads(t *testing.T) {
	gone, a, b := riskDecl("lib.go", "Gone"), riskDecl("a.go", "A"), riskDecl("a.go", "A")
	b.Signature = "func A(s string)"
	a2, b2 := a, b
	a2.Signature, b2.Signature = "func A(x int)", "func A(s []string)"
	unresolved := store.SemanticDeclaration{QualifiedName: "Gone"}
	files := indexed("a.go", "lib.go")
	base := riskRevision(files, []store.SemanticDeclaration{gone, a, b}, []store.SemanticEdge{riskCall(a, gone, 4, 2, "resolved"), riskCall(b, gone, 8, 2, "resolved")})

	deleted := compareRisk(t, base, riskRevision(files, []store.SemanticDeclaration{b}, []store.SemanticEdge{riskCall(b, unresolved, 8, 2, "unresolved")}))
	if ev := component(t, deleted, RuleRemovedDeclarationDependent).Evidence; *deleted.Score != 20 || len(ev) != 1 || ev[0].Dependent != declIdentity(b) || hasUncertainty(deleted, "dependent_identity_ambiguous", "") {
		t.Fatalf("deleted overload: %+v", deleted)
	}

	changed := compareRisk(t, base, riskRevision(files, []store.SemanticDeclaration{a2, b}, []store.SemanticEdge{riskCall(a2, unresolved, 4, 2, "unresolved"), riskCall(b, unresolved, 8, 2, "unresolved")}))
	ev := component(t, changed, RuleRemovedDeclarationDependent).Evidence
	if *changed.Score != 40 || len(ev) != 2 || hasUncertainty(changed, "dependent_identity_ambiguous", "") {
		t.Fatalf("changed overload: %+v", changed)
	}
	for _, e := range ev {
		if e.Dependent == declIdentity(a) && e.DependentHead != declIdentity(a2) || e.Dependent == declIdentity(b) && e.DependentHead != "" {
			t.Fatalf("evidence = %+v", e)
		}
	}

	both := compareRisk(t, base, riskRevision(files, []store.SemanticDeclaration{a2, b2}, []store.SemanticEdge{riskCall(a2, unresolved, 4, 2, "unresolved"), riskCall(b2, unresolved, 8, 2, "unresolved")}))
	if both.Assessment != AssessmentLimitedEvidence || *both.Score != 0 || !hasUncertainty(both, "dependent_identity_ambiguous", declIdentity(a)) || !hasUncertainty(both, "dependent_identity_ambiguous", declIdentity(b)) || !hasUncertainty(both, "removed_declaration_without_resolved_dependents", declIdentity(gone)) {
		t.Fatalf("both changed: %+v", both)
	}
}

// A dependent that dropped its call to a removed declaration while keeping its
// own call to a same-name sibling (an overload, or a method of another
// receiver) has not rebound anything. The graph cannot tell that from a
// dropped sibling call plus a rebinding, so it is uncertainty, not points.
func TestRiskDroppedCallKeepingSiblingIsNotRebinding(t *testing.T) {
	overload := func() (store.SemanticDeclaration, store.SemanticDeclaration) {
		narrow, wide := riskDecl("lib.go", "Foo"), riskDecl("lib.go", "Foo")
		narrow.Signature, wide.Signature = "func Foo(int)", "func Foo(int64)"
		return narrow, wide
	}
	receivers := func() (store.SemanticDeclaration, store.SemanticDeclaration) {
		one, two := riskDecl("lib.go", "Close"), riskDecl("lib.go", "Close")
		one.Kind, two.Kind = "method", "method"
		one.QualifiedName, two.QualifiedName = "p.File.Close", "p.Conn.Close"
		one.StableKey, two.StableKey = "method:p:File.Close", "method:p:Conn.Close"
		return one, two
	}
	for name, decls := range map[string]func() (store.SemanticDeclaration, store.SemanticDeclaration){"overload": overload, "receiver": receivers} {
		t.Run(name, func(t *testing.T) {
			gone, sibling := decls()
			a := riskDecl("a.go", "A")
			files := indexed("a.go", "lib.go")
			base := riskRevision(files, []store.SemanticDeclaration{gone, sibling, a}, []store.SemanticEdge{riskCall(a, gone, 4, 2, "resolved"), riskCall(a, sibling, 5, 2, "resolved")})
			// Deleting line 4 shifts the kept sibling call onto it.
			head := riskRevision(files, []store.SemanticDeclaration{sibling, a}, []store.SemanticEdge{riskCall(a, sibling, 4, 2, "resolved")})
			r := compareRisk(t, base, head)
			if r.Assessment != AssessmentLimitedEvidence || *r.Score != 0 || !hasUncertainty(r, "dependent_rebinding_unproven", "") {
				t.Fatalf("risk = %+v", r)
			}
		})
	}
}

// Fewer references to a surviving declaration than base sites, next to an
// unresolved one by the same name, may be a lost resolution: disclosed.
func TestRiskPartialBindingIsUncertainty(t *testing.T) {
	x, a := riskDecl("lib.go", "X"), riskDecl("a.go", "A")
	decls := []store.SemanticDeclaration{x, a}
	files := indexed("a.go", "lib.go")
	base := riskRevision(files, decls, []store.SemanticEdge{riskCall(a, x, 4, 2, "resolved"), riskCall(a, x, 6, 2, "resolved")})
	head := riskRevision(files, decls, []store.SemanticEdge{riskCall(a, x, 4, 2, "resolved"), riskCall(a, store.SemanticDeclaration{QualifiedName: "X"}, 6, 2, "unresolved")})
	if r := compareRisk(t, base, head); r.Assessment != AssessmentLimitedEvidence || *r.Score != 0 || !hasUncertainty(r, "dependent_binding_partial", "") {
		t.Fatalf("risk = %+v", r)
	}
	// One call now resolves to another declaration with the same name.
	shadow := riskDecl("other/lib.go", "X")
	moved := riskRevision(indexed("a.go", "lib.go", "other/lib.go"), append(decls, shadow), []store.SemanticEdge{riskCall(a, x, 4, 2, "resolved"), riskCall(a, shadow, 6, 2, "resolved")})
	if r := compareRisk(t, base, moved); r.Assessment != AssessmentLimitedEvidence || *r.Score != 0 || !hasUncertainty(r, "dependent_binding_partial", "") {
		t.Fatalf("risk = %+v", r)
	}
	// An unresolved reference that was already there next to an unchanged
	// resolved one is not a loss.
	dynamic := riskCall(a, store.SemanticDeclaration{QualifiedName: "X"}, 6, 2, "unresolved")
	same := riskRevision(files, decls, []store.SemanticEdge{riskCall(a, x, 4, 2, "resolved"), dynamic})
	if r := compareRisk(t, same, same); r.Assessment != AssessmentNoRiskEvidence {
		t.Fatalf("risk = %+v", r)
	}
}

// Risk rules and weights are part of the model id: a released model never
// changes what it cites or how many points it gives.
func TestRiskModelPinsRules(t *testing.T) {
	want := "removed_declaration_dependent 20 60;dependent_resolution_lost 10 30;dependent_retargeted 5 20;changed_declaration_dependent 5 20;"
	got := ""
	for _, r := range riskRules {
		got += r.rule + " " + strconv.Itoa(r.pointsEach) + " " + strconv.Itoa(r.maxPoints) + ";"
	}
	if RiskModel != "codegraph.change-risk/v1" || got != want {
		t.Fatalf("model %s rules %s: changing the rule table needs a new model id", RiskModel, got)
	}
}

// A caller whose file was renamed in the same change is paired through
// declaration_pairs, so its broken call to a removed declaration is scored.
func TestCompareCommitsRiskPairsMovedDependent(t *testing.T) {
	r := gittest.Init(t)
	r.Write("go.mod", "module example.com/m\n\ngo 1.22\n")
	r.Write("lib.go", "package main\n\nfunc Gone() {}\n")
	r.Write("use.go", "package main\n\nfunc Use() {\n\tGone()\n}\n")
	r.Write("main.go", "package main\n\nfunc main() { Use() }\n")
	base := r.Commit("", "base")
	r.Git("mv", "use.go", "caller.go")
	r.Write("lib.go", "package main\n")
	head := r.Commit("", "move caller, remove Gone")
	got, err := CompareCommits(t.Context(), r.Dir, base, head, parser.NewRegistry(goparser.New()), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	rk := got.Diff.Risk
	ev := component(t, rk, RuleRemovedDeclarationDependent).Evidence
	if *rk.Score != 20 || len(ev) != 1 || !strings.Contains(ev[0].Dependent, `"use.go"`) || !strings.Contains(ev[0].DependentHead, `"caller.go"`) {
		t.Fatalf("risk = %+v", rk)
	}
}

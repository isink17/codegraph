package diff

import (
	"slices"
	"strings"
	"testing"

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
	if r.Coverage.State != CoveragePartial || !strings.Contains(r.Coverage.Reason, "only resolved calls") {
		t.Fatalf("risk coverage = %+v", r.Coverage)
	}
	if len(r.RemovedDeclarationCallers) != 1 || r.RemovedDeclarationCallers[0].Declaration != declIdentity(gone) {
		t.Fatalf("risk = %+v", r)
	}
	calls := r.RemovedDeclarationCallers[0].Calls
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
	if len(r.RemovedDeclarationCallers) != 0 {
		t.Fatalf("risk = %+v", r)
	}
	if got.Diff.Summary["symbols_removed"] != 2 {
		t.Fatalf("summary = %v", got.Diff.Summary)
	}
	if r.Coverage.State != CoveragePartial || !strings.Contains(r.Coverage.Reason, "without a current parse") {
		t.Fatalf("risk coverage = %+v", r.Coverage)
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
	if r := got.Diff.Risk; r.Coverage.State != CoverageUnavailable || len(r.RemovedDeclarationCallers) != 0 {
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

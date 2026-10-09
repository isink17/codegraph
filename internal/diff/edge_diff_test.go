package diff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func edgeChange(t *testing.T, changes []Change, targetName string) (before, after store.SemanticEdge) {
	t.Helper()
	for _, c := range changes {
		var b, a store.SemanticEdge
		if c.Before != nil {
			_ = json.Unmarshal(c.Before, &b)
		}
		if c.After != nil {
			_ = json.Unmarshal(c.After, &a)
		}
		if strings.Contains(b.Target.QualifiedName, targetName) || strings.Contains(a.Target.QualifiedName, targetName) {
			return b, a
		}
	}
	t.Fatalf("no edge change mentions %q in %+v", targetName, changes)
	return
}

func TestCommitEdgeDeltasPairCallsByColumn(t *testing.T) {
	r := gittest.Init(t)
	r.Write("main.go", "package main\nfunc A() { B(); C(); Gone() }\nfunc B() {}\nfunc C() {}\nfunc D() {}\nfunc Gone() {}\n")
	base := r.Commit("", "base")
	// Same line: B unchanged, C retargets to D, Gone becomes unresolved,
	// and a new call to B appears on its own line.
	r.Write("main.go", "package main\nfunc A() { B(); D(); Lost() }\nfunc B() { C() }\nfunc C() {}\nfunc D() {}\n")
	head := r.Commit("", "head")
	res, err := CompareCommits(t.Context(), r.Dir, base, head, parser.NewRegistry(goparser.New()), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if res.Comparison.Status == "incompatible" || res.Diff.Coverage.Edges.State != CoverageComplete {
		t.Fatalf("edge section not comparable: %+v %+v", res.Comparison, res.Diff.Coverage)
	}
	if n := len(res.Diff.Edges.EvidenceChanged); n != 0 {
		t.Fatalf("same-line calls were ambiguous or drifted: %+v", res.Diff.Edges.EvidenceChanged)
	}
	if b, a := edgeChange(t, res.Diff.Edges.Retargeted, "D"); b.Target.QualifiedName == a.Target.QualifiedName || a.Target.State != "resolved" || b.Column == 0 || b.Column != a.Column {
		t.Fatalf("retarget = %+v -> %+v", b, a)
	}
	if b, a := edgeChange(t, res.Diff.Edges.ResolutionChanged, "Lost"); b.Target.State != "resolved" || a.Target.State != "unresolved" {
		t.Fatalf("resolution change = %+v -> %+v", b, a)
	}
	if _, a := edgeChange(t, res.Diff.Edges.Added, "C"); a.Source.QualifiedName == "" || !strings.HasSuffix(a.Source.QualifiedName, "B") {
		t.Fatalf("added edge = %+v", a)
	}
	for _, c := range allChanges(res) {
		if strings.HasPrefix(c.Kind, "edge_") && strings.Contains(string(c.Before)+string(c.After), `"qualified_name":"main.B","signature"`) && !strings.Contains(string(c.After), "main.C") {
			t.Fatalf("unchanged call to B reported: %+v", c)
		}
	}
	// Reverse direction: unresolved becomes resolved, the added edge is removed.
	rev, err := CompareCommits(t.Context(), r.Dir, head, base, parser.NewRegistry(goparser.New()), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if b, a := edgeChange(t, rev.Diff.Edges.ResolutionChanged, "Gone"); b.Target.State != "unresolved" || a.Target.State != "resolved" {
		t.Fatalf("reverse resolution change = %+v -> %+v", b, a)
	}
	edgeChange(t, rev.Diff.Edges.Removed, "C")
}

func TestIncompleteGraphCoverageIsNotComplete(t *testing.T) {
	r := gittest.Init(t)
	r.Write(".codegraph/config.json", `{"parse_error_policy":"best_effort"}`)
	r.Git("add", "-f", ".codegraph/config.json")
	r.Write("main.go", "package main\nfunc A() { B() }\nfunc B() {}\n")
	base := r.Commit("", "valid")
	r.Write("main.go", "package main\nfunc A( {\n")
	head := r.Commit("", "broken")
	res, err := CompareCommits(t.Context(), r.Dir, base, head, parser.NewRegistry(goparser.New()), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	cov := res.Diff.Coverage
	if cov.Files.State != CoverageComplete || cov.Symbols.State == CoverageComplete || cov.Edges.State != CoverageUnavailable || cov.TestLinks.State != CoverageUnavailable || cov.Edges.Reason == "" {
		t.Fatalf("incomplete graph claimed complete coverage: %+v", cov)
	}
	if len(res.Diff.Edges.Removed) != 0 {
		t.Fatalf("incomplete graph fabricated edge removals: %+v", res.Diff.Edges.Removed)
	}

	self, err := CompareCommits(t.Context(), r.Dir, base, base, parser.NewRegistry(goparser.New()), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if c := self.Diff.Coverage; c.Symbols.State != CoverageComplete || c.Edges.State != CoverageComplete || self.Diff.Total != 0 {
		t.Fatalf("clean self diff coverage = %+v total=%d", c, self.Diff.Total)
	}

	policy := Revision{GraphData: store.SemanticGraph{SchemaVersion: 1}}
	other := Revision{GraphData: store.SemanticGraph{SchemaVersion: 2}}
	got, err := Compare(policy, other, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.Diff.Coverage.Symbols.State != CoverageUnavailable || got.Diff.Coverage.Edges.State != CoverageUnavailable {
		t.Fatalf("schema mismatch coverage = %+v", got.Diff.Coverage)
	}
}

func TestSemanticGraphFreshAndIncrementalParity(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	project := func(db string, update bool) []byte {
		st, err := store.Open(filepath.Join(t.TempDir(), db))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		idx := indexer.New(st, parser.NewRegistry(goparser.New()), nil)
		if _, err := idx.Index(t.Context(), indexer.Options{RepoRoot: root, NoHistory: true}); err != nil {
			t.Fatal(err)
		}
		if update {
			write("a.go", "package main\nfunc A() { B(); C() }\nfunc C() {}\n")
			if err := os.Remove(filepath.Join(root, "gone.go")); err != nil {
				t.Fatal(err)
			}
			if _, err := idx.Update(t.Context(), indexer.Options{RepoRoot: root, NoHistory: true}); err != nil {
				t.Fatal(err)
			}
		}
		repo, ok, err := st.FindRepo(t.Context(), root)
		if err != nil || !ok {
			t.Fatalf("repo lookup: %v %v", ok, err)
		}
		g, err := st.SemanticGraph(t.Context(), repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(g)
		return out
	}
	write("a.go", "package main\nfunc A() { B() }\n")
	write("b.go", "package main\nfunc B() { Gone() }\n")
	write("gone.go", "package main\nfunc Gone() {}\n")
	incremental := project("incremental.sqlite", true)
	fresh := project("fresh.sqlite", false)
	if string(incremental) != string(fresh) {
		t.Fatalf("incremental projection differs from fresh\n%s\n%s", incremental, fresh)
	}
	if !strings.Contains(string(fresh), `"column":`) {
		t.Fatalf("projection lost call columns: %s", fresh)
	}
}

func coverageRevision(files []store.SemanticFile, edges []store.SemanticEdge, capability store.GraphCapability) Revision {
	return Revision{GraphData: store.SemanticGraph{Files: files, Edges: edges, State: store.SemanticGraphState{Capability: capability, ResolverPolicies: map[string]string{}, ParserSemantics: map[string]string{}}}}
}

func coverageEdge(path string, column int, target string) store.SemanticEdge {
	src := store.SemanticEndpoint{Path: path, Language: "go", Kind: "function", QualifiedName: "p.A", Signature: "func A()", StableKey: "func:p::A", State: "resolved"}
	return store.SemanticEdge{Source: src, Path: path, Kind: "calls", Line: 3, Column: column, Target: store.SemanticEndpoint{QualifiedName: target, State: "unresolved"}}
}

func TestEdgeColumnRecordedOnOneSideOnlyIsNotAChange(t *testing.T) {
	files := []store.SemanticFile{{Path: "a.go", Language: "go", ParseState: store.ParseStateIndexed}}
	base := coverageRevision(files, []store.SemanticEdge{coverageEdge("a.go", 0, "B")}, store.GraphCapability{})
	head := coverageRevision(files, []store.SemanticEdge{coverageEdge("a.go", 7, "B")}, store.GraphCapability{})
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.Diff.Total != 0 {
		t.Fatalf("column recorded on one side produced changes: %+v", got.Diff)
	}
	if got.Diff.Coverage.Edges.State != CoveragePartial || !strings.Contains(got.Diff.Coverage.Edges.Reason, "call column") {
		t.Fatalf("edge coverage = %+v", got.Diff.Coverage.Edges)
	}
}

func TestNonCurrentParseStateMakesEdgesPartial(t *testing.T) {
	baseFiles := []store.SemanticFile{{Path: "a.go", Language: "go", ParseState: store.ParseStateIndexed}}
	headFiles := []store.SemanticFile{{Path: "a.go", Language: "go", ParseState: store.ParseStatePending}}
	base := coverageRevision(baseFiles, []store.SemanticEdge{coverageEdge("a.go", 5, "B")}, store.GraphCapability{})
	head := coverageRevision(headFiles, nil, store.GraphCapability{})
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Diff.Edges.Removed) != 0 {
		t.Fatalf("edge of a non-current file reported removed: %+v", got.Diff.Edges.Removed)
	}
	if c := got.Diff.Coverage; c.Edges.State != CoveragePartial || c.TestLinks.State != CoveragePartial || c.Symbols.State != CoveragePartial {
		t.Fatalf("coverage = %+v", c)
	}
}

func TestLimitedCallCapabilityMakesEdgesPartial(t *testing.T) {
	files := []store.SemanticFile{{Path: "a.go", Language: "go", ParseState: store.ParseStateIndexed}}
	limited := store.GraphCapability{State: store.GraphSymbolsOnly, Languages: []store.LanguageCapability{{Language: "go", Capability: store.GraphSymbolsOnly}}}
	got, err := Compare(coverageRevision(files, nil, limited), coverageRevision(files, nil, limited), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Diff.Coverage.Edges; c.State != CoveragePartial || !strings.Contains(c.Reason, "capability") {
		t.Fatalf("edge coverage = %+v", c)
	}
}

package diff

import (
	"encoding/json"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

func TestPendingStateSuppressesDeclarationDeltas(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state store.SemanticGraphState
	}{
		{name: "pending", state: store.SemanticGraphState{Pending: "parser semantic transition pending"}},
		{name: "dirty", state: store.SemanticGraphState{DirtyFiles: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := Revision{GraphData: store.SemanticGraph{
				State: store.SemanticGraphState{ResolverPolicies: map[string]string{}, ParserSemantics: map[string]string{}},
			}}
			tc.state.ResolverPolicies = map[string]string{}
			tc.state.ParserSemantics = map[string]string{}
			head := Revision{GraphData: store.SemanticGraph{
				Declarations: []store.SemanticDeclaration{{StableKey: "new"}},
				State:        tc.state,
			}}
			got, err := Compare(base, head, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if got.Comparison.Status != "incompatible" || len(got.Diff.Symbols.Added) != 0 || len(got.Diff.Symbols.Removed) != 0 {
				t.Fatalf("incomplete graph emitted semantic deltas: %+v", got)
			}
		})
	}
}

func TestCompareUsesStableIdentitiesAndDeterministicPages(t *testing.T) {
	decl := func(key, name, visibility string) store.SemanticDeclaration {
		return store.SemanticDeclaration{Path: "a.go", Language: "go", Kind: "function", Name: name, QualifiedName: "p." + key, Signature: "func " + key + "()", StableKey: "func:p::" + key, Visibility: visibility}
	}
	source := store.SemanticEndpoint{Path: "a.go", Language: "go", Kind: "function", QualifiedName: "p.Call", Signature: "func Call()", StableKey: "func:p::Call", State: "resolved"}
	unresolved := store.SemanticEdge{Source: source, Path: "a.go", Kind: "calls", Line: 12, Target: store.SemanticEndpoint{State: "unresolved"}}
	resolved := unresolved
	resolved.Target = store.SemanticEndpoint{Path: "b.go", Language: "go", Kind: "function", QualifiedName: "p.Target", Signature: "func Target()", StableKey: "func:p::Target", State: "resolved"}
	base := Revision{GraphData: store.SemanticGraph{
		Declarations: []store.SemanticDeclaration{decl("removed", "Removed", "exported"), decl("changed", "Changed", "private"), decl("dup", "Dup", "private"), decl("dup", "Dup", "exported")},
		Edges:        []store.SemanticEdge{unresolved},
	}}
	head := Revision{GraphData: store.SemanticGraph{
		Declarations: []store.SemanticDeclaration{decl("added", "Added", "exported"), decl("changed", "Changed", "exported"), decl("dup", "Dup", "private")},
		Edges:        []store.SemanticEdge{resolved},
	}}
	first, err := Compare(base, head, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	firstAgain, err := Compare(base, head, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compare(base, head, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	third, err := Compare(base, head, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(first.Diff)
	encodedAgain, _ := json.Marshal(firstAgain.Diff)
	if string(encoded) != string(encodedAgain) {
		t.Fatalf("first page is nondeterministic: %s != %s", encoded, encodedAgain)
	}
	if first.Diff.Total != 5 || !first.Diff.Truncated || !second.Diff.Truncated || third.Diff.Truncated || first.Diff.Offset != 0 || second.Diff.Offset != 2 || third.Diff.Offset != 4 {
		t.Fatalf("paging summary mismatch: first=%+v second=%+v third=%+v", first.Diff, second.Diff, third.Diff)
	}
	all := append(append(append([]Change(nil), first.DiffChanges()...), second.DiffChanges()...), third.DiffChanges()...)
	kinds := map[string]int{}
	identities := map[string]bool{}
	for _, change := range all {
		kinds[change.Kind]++
		if identities[change.Identity+change.Kind] {
			t.Fatalf("duplicate change across pages: %+v", change)
		}
		identities[change.Identity+change.Kind] = true
	}
	for kind, count := range map[string]int{"declaration_added": 1, "declaration_removed": 1, "declaration_changed": 1, "declaration_ambiguous": 1, "edge_resolution_changed": 1} {
		if kinds[kind] != count {
			t.Errorf("%s count = %d, want %d (%v)", kind, kinds[kind], count, kinds)
		}
	}
}

func TestParserIdentityMismatchSuppressesSemanticDiff(t *testing.T) {
	for _, change := range []string{"profile", "semantic epoch", "call capability"} {
		t.Run(change, func(t *testing.T) {
			baseFile := store.SemanticFile{Path: "a.go", Language: "go", ContentHash: "same", ParserProfile: "treesitter:go:v1", ParserCallEdges: true, ParserSemanticEpoch: 1}
			headFile := baseFile
			switch change {
			case "profile":
				headFile.ParserProfile = "treesitter:go:v2"
			case "semantic epoch":
				headFile.ParserSemanticEpoch++
			case "call capability":
				headFile.ParserCallEdges = false
			}
			base := Revision{GraphData: store.SemanticGraph{Files: []store.SemanticFile{baseFile}}}
			head := Revision{GraphData: store.SemanticGraph{Files: []store.SemanticFile{headFile}, Declarations: []store.SemanticDeclaration{{StableKey: "new"}}}}
			got, err := Compare(base, head, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if got.Comparison.Status != "incompatible" || len(got.Diff.Symbols.Added)+len(got.Diff.Symbols.Removed)+len(got.Diff.Symbols.Changed) != 0 {
				t.Fatalf("parser identity mismatch emitted semantic deltas: %+v", got)
			}
		})
	}
}

func TestLanguageChangeIsReportedForSameSourceBytes(t *testing.T) {
	base := Revision{TreeFiles: []SourceFile{{Path: "a.go", ContentHash: "same", Language: "go"}}}
	head := Revision{TreeFiles: []SourceFile{{Path: "a.go", ContentHash: "same", Language: "unknown"}}}
	got, err := Compare(base, head, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Diff.Files.Modified) != 1 || got.Diff.Files.Modified[0].Identity != "a.go" {
		t.Fatalf("language change was not reported: %+v", got.Diff.Files)
	}
}

func (r Result) DiffChanges() []Change {
	var out []Change
	out = append(out, r.Diff.Files.Added...)
	out = append(out, r.Diff.Files.Removed...)
	out = append(out, r.Diff.Files.Modified...)
	out = append(out, r.Diff.Symbols.Added...)
	out = append(out, r.Diff.Symbols.Removed...)
	out = append(out, r.Diff.Symbols.Changed...)
	out = append(out, r.Diff.Edges.Added...)
	out = append(out, r.Diff.Edges.Removed...)
	out = append(out, r.Diff.Edges.Retargeted...)
	out = append(out, r.Diff.Edges.ResolutionChanged...)
	out = append(out, r.Diff.Edges.EvidenceChanged...)
	return out
}

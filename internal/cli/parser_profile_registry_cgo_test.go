//go:build cgo

package cli

import "testing"

// The cgo build parses every supported language with tree-sitter, and all of
// those adapters build a call graph.
func TestCgoRegistryProfiles(t *testing.T) {
	for _, lang := range newDefaultRegistry().SupportedLanguages() {
		want := "treesitter:" + lang.Language + ":v1"
		if lang.Language == "csharp" { want = "treesitter:csharp:v4" }
		if lang.ParserProfile != want {
			t.Fatalf("%s: parser profile = %q, want %q", lang.Language, lang.ParserProfile, want)
		}
		if !lang.CallEdges {
			t.Fatalf("%s: CallEdges = false, want true for a tree-sitter adapter", lang.Language)
		}
	}
	if degraded := newDefaultRegistry().DegradedLanguages(); len(degraded) != 0 {
		t.Fatalf("DegradedLanguages() = %v, want none in the cgo build", degraded)
	}
}

//go:build !cgo

package cli

import (
	"strings"
	"testing"
)

// The non-cgo build keeps a call graph only for Go (go/ast) and Python (the
// dedicated adapter). The remaining nine languages fall back to the
// symbols-only heuristic adapters, and that loss is what parser provenance
// exists to make visible and refuse to apply silently.
func TestNoCgoRegistryProfiles(t *testing.T) {
	want := map[string]struct {
		profile   string
		callEdges bool
	}{
		"go":         {"go-ast:go:v1", true},
		"python":     {"python-regex:python:v1", true},
		"java":       {"heuristic:java:v1", false},
		"kotlin":     {"heuristic:kotlin:v1", false},
		"csharp":     {"heuristic:csharp:v2", false},
		"typescript": {"heuristic:typescript:v1", false},
		"rust":       {"heuristic:rust:v1", false},
		"ruby":       {"heuristic:ruby:v1", false},
		"swift":      {"heuristic:swift:v1", false},
		"php":        {"heuristic:php:v1", false},
		"cpp":        {"heuristic:cpp:v1", false},
	}
	got := newDefaultRegistry().SupportedLanguages()
	if len(got) != len(want) {
		t.Fatalf("registry has %d languages, want %d", len(got), len(want))
	}
	for _, lang := range got {
		expected, known := want[lang.Language]
		if !known {
			t.Fatalf("unexpected language %q in the non-cgo registry", lang.Language)
		}
		if lang.ParserProfile != expected.profile || lang.CallEdges != expected.callEdges {
			t.Fatalf("%s: profile=%q callEdges=%v, want %q/%v",
				lang.Language, lang.ParserProfile, lang.CallEdges, expected.profile, expected.callEdges)
		}
	}
	degraded := strings.Join(newDefaultRegistry().DegradedLanguages(), ",")
	if degraded != "cpp,csharp,java,kotlin,php,ruby,rust,swift,typescript" {
		t.Fatalf("DegradedLanguages() = %q", degraded)
	}
}

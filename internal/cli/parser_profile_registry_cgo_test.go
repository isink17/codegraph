//go:build cgo

package cli

import (
	"strings"
	"testing"
)

// The cgo build parses every supported language with tree-sitter, and all of
// those adapters except Scala build a call graph.
func TestCgoRegistryProfiles(t *testing.T) {
	for _, lang := range newDefaultRegistry().SupportedLanguages() {
		want := "treesitter:" + lang.Language + ":v1"
		if lang.Language == "csharp" {
			want = "treesitter:csharp:v6"
		}
		if lang.Language == "kotlin" {
			want = "treesitter:kotlin:v12"
		}
		if lang.Language == "java" {
			want = "treesitter:java:v12"
		}
		if lang.Language == "python" {
			want = "treesitter:python:v9"
		}
		if lang.Language == "php" {
			want = "treesitter:php:v4"
		}
		if lang.Language == "ruby" {
			want = "treesitter:ruby:v6"
		}
		if lang.Language == "rust" {
			want = "treesitter:rust:v5"
		}
		if lang.Language == "typescript" {
			want = "treesitter:typescript:v2"
		}
		if lang.Language == "lua" {
			want = "treesitter:lua:v3"
		}
		if lang.Language == "scala" {
			want = "treesitter:scala:v2"
		}
		if lang.Language == "swift" {
			want = "treesitter:swift:v7"
		}
		if lang.ParserProfile != want {
			t.Fatalf("%s: parser profile = %q, want %q", lang.Language, lang.ParserProfile, want)
		}
		if lang.CallEdges == (lang.Language == "scala") {
			t.Fatalf("%s: CallEdges = %v; only Scala is symbols-only under tree-sitter", lang.Language, lang.CallEdges)
		}
	}
	if degraded := strings.Join(newDefaultRegistry().DegradedLanguages(), ","); degraded != "scala" {
		t.Fatalf("DegradedLanguages() = %v, want only scala in the cgo build", degraded)
	}
	for _, capability := range newDefaultRegistry().Capabilities() {
		if capability.NoCGOParser == nil || !*capability.NoCGOParser {
			t.Errorf("%s no_cgo_parser = %v, want true", capability.ID, capability.NoCGOParser)
		}
		wantCallGraph := capability.ID == "go" || capability.ID == "python"
		if capability.NoCGOCallGraph == nil || *capability.NoCGOCallGraph != wantCallGraph {
			t.Errorf("%s no_cgo_call_graph = %v, want %v", capability.ID, capability.NoCGOCallGraph, wantCallGraph)
		}
	}
}

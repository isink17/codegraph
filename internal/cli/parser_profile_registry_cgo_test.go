//go:build cgo

package cli

import (
	"maps"
	"slices"
	"testing"
)

// symbolsOnlyCgoLanguages are the tree-sitter adapters that deliberately build
// no call graph; every other cgo adapter does.
var symbolsOnlyCgoLanguages = map[string]bool{"dart": true, "scala": true}

// The cgo build parses every supported language with tree-sitter, and all of
// those adapters except symbolsOnlyCgoLanguages build a call graph.
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
			want = "treesitter:scala:v3"
		}
		if lang.Language == "swift" {
			want = "treesitter:swift:v7"
		}
		if lang.Language == "dart" {
			want = "treesitter:dart:v2"
		}
		if lang.ParserProfile != want {
			t.Fatalf("%s: parser profile = %q, want %q", lang.Language, lang.ParserProfile, want)
		}
		if lang.CallEdges == symbolsOnlyCgoLanguages[lang.Language] {
			t.Fatalf("%s: CallEdges = %v, want %v", lang.Language, lang.CallEdges, !symbolsOnlyCgoLanguages[lang.Language])
		}
	}
	want := slices.Sorted(maps.Keys(symbolsOnlyCgoLanguages))
	if degraded := newDefaultRegistry().DegradedLanguages(); !slices.Equal(degraded, want) {
		t.Fatalf("DegradedLanguages() = %v, want %v in the cgo build", degraded, want)
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

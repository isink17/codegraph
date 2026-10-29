//go:build cgo

package cli

import (
	"maps"
	"slices"
	"testing"
)

// symbolsOnlyCgoLanguages are the tree-sitter adapters that deliberately build
// no call graph; every other cgo adapter does.
var symbolsOnlyCgoLanguages = map[string]bool{}

// The cgo build parses every supported language with tree-sitter, and all of
// those adapters except symbolsOnlyCgoLanguages build a call graph.
func TestCgoRegistryProfiles(t *testing.T) {
	registry := newDefaultRegistry()
	for _, lang := range registry.SupportedLanguages() {
		want := "treesitter:" + lang.Language + ":v1"
		if lang.Language == "csharp" {
			want = "treesitter:csharp:v6"
		}
		if lang.Language == "kotlin" {
			want = "treesitter:kotlin:v15"
		}
		if lang.Language == "java" {
			want = "treesitter:java:v14"
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
		if lang.Language == "hcl" {
			want = "treesitter:hcl:v3"
		}
		if lang.Language == "lua" {
			want = "treesitter:lua:v4"
		}
		if lang.Language == "cpp" {
			want = "treesitter:cpp:v2"
		}
		if lang.Language == "go" {
			want = "treesitter:go:v6"
		}
		if lang.Language == "dart" {
			want = "treesitter:dart:v3"
		}
		if lang.Language == "scala" {
			want = "treesitter:scala:v4"
		}
		if lang.Language == "swift" {
			want = "treesitter:swift:v7"
		}
		if lang.ParserProfile != want {
			t.Fatalf("%s: parser profile = %q, want %q", lang.Language, lang.ParserProfile, want)
		}
		if lang.CallEdges == symbolsOnlyCgoLanguages[lang.Language] {
			t.Fatalf("%s: CallEdges = %v, want %v", lang.Language, lang.CallEdges, !symbolsOnlyCgoLanguages[lang.Language])
		}
	}
	want := slices.Sorted(maps.Keys(symbolsOnlyCgoLanguages))
	if degraded := registry.DegradedLanguages(); !slices.Equal(degraded, want) {
		t.Fatalf("DegradedLanguages() = %v, want %v in the cgo build", degraded, want)
	}
	availability := noCGOCapabilities()
	for _, capability := range registry.Capabilities() {
		available, ok := availability[capability.ID]
		if !ok {
			if capability.NoCGOParser != nil || capability.NoCGOCallGraph != nil {
				t.Errorf("%s unknown no-CGO availability became known", capability.ID)
			}
			continue
		}
		if capability.NoCGOParser == nil || *capability.NoCGOParser != available.Parser {
			t.Errorf("%s no_cgo_parser = %v, want %v", capability.ID, capability.NoCGOParser, available.Parser)
		}
		if capability.NoCGOCallGraph == nil || *capability.NoCGOCallGraph != (available.Parser && available.CallGraph) {
			t.Errorf("%s no_cgo_call_graph = %v, want %v", capability.ID, capability.NoCGOCallGraph, available.Parser && available.CallGraph)
		}
	}
}

//go:build !cgo

package cli

import (
	"slices"
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
		"go":         {"go-ast:go:v6", true},
		"python":     {"python-regex:python:v10", true},
		"java":       {"heuristic:java:v1", false},
		"lua":        {"heuristic:lua:v1", false},
		"scala":      {"heuristic:scala:v1", false},
		"hcl":        {"heuristic:hcl:v2", false},
		"dart":       {"heuristic:dart:v1", false},
		"kotlin":     {"heuristic:kotlin:v4", false},
		"csharp":     {"heuristic:csharp:v5", false},
		"typescript": {"heuristic:typescript:v1", false},
		"rust":       {"heuristic:rust:v1", false},
		"ruby":       {"heuristic:ruby:v1", false},
		"swift":      {"heuristic:swift:v1", false},
		"php":        {"heuristic:php:v1", false},
		"cpp":        {"heuristic:cpp:v1", false},
	}
	registry := newDefaultRegistry()
	if _, ok := noCGOCapabilities()["solidity"]; ok {
		t.Fatal("no-CGO availability includes unregistered Solidity")
	}
	if adapter := registry.AdapterFor("contracts/Token.sol"); adapter != nil {
		t.Fatalf("no-CGO .sol adapter = %v, want unavailable", adapter)
	}
	for _, capability := range registry.Capabilities() {
		if capability.ID == "solidity" {
			t.Fatalf("no-CGO registry advertised Solidity: %+v", capability)
		}
	}
	got := registry.SupportedLanguages()
	seen := make(map[string]bool, len(got))
	for _, lang := range got {
		expected, known := want[lang.Language]
		seen[lang.Language] = true
		if known && (lang.ParserProfile != expected.profile || lang.CallEdges != expected.callEdges) {
			t.Fatalf("%s: profile=%q callEdges=%v, want %q/%v",
				lang.Language, lang.ParserProfile, lang.CallEdges, expected.profile, expected.callEdges)
		}
	}
	for language := range want {
		if !seen[language] {
			t.Errorf("required non-cgo language %q is missing", language)
		}
	}
	wantDegraded := make([]string, 0, len(got))
	for _, language := range got {
		if !language.CallEdges {
			wantDegraded = append(wantDegraded, language.Language)
		}
	}
	if degraded := registry.DegradedLanguages(); !slices.Equal(degraded, wantDegraded) {
		t.Fatalf("DegradedLanguages() = %v, want %v", degraded, wantDegraded)
	}
	availability := noCGOCapabilities()
	for language, available := range availability {
		profile, registered := registry.ProfileForLanguage(language)
		if !registered || !available.Parser || available.CallGraph != profile.EmitsCallEdges {
			t.Errorf("no-CGO availability for %s = %+v, registered profile = %+v (%v)", language, available, profile, registered)
		}
	}
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
	fixtures := make(map[string]struct {
		file string
		src  string
	}, len(callFixtures)+len(noCgoCallFixtures))
	for language, fixture := range callFixtures {
		fixtures[language] = fixture
	}
	for language, fixture := range noCgoCallFixtures {
		fixtures[language] = fixture
	}
	for language := range want {
		if _, ok := fixtures[language]; !ok {
			t.Errorf("required no-CGO call-edge probe missing for %q", language)
		}
	}
	for language, fixture := range fixtures {
		profile, ok := registry.ProfileForLanguage(language)
		if !ok {
			t.Fatalf("no registered parser profile for call fixture %q", language)
		}
		adapter := registry.AdapterFor(fixture.file)
		if adapter == nil {
			t.Fatalf("no registered adapter for call fixture %q", language)
		}
		parsed, err := adapter.Parse(t.Context(), fixture.file, []byte(fixture.src))
		if err != nil {
			t.Fatalf("%s Parse() error = %v", language, err)
		}
		observed := false
		for _, edge := range parsed.Edges {
			if edge.Kind == "calls" {
				observed = true
				break
			}
		}
		if observed != profile.EmitsCallEdges {
			t.Errorf("%s profile call-edge claim = %v, parsed fixture emitted calls = %v", language, profile.EmitsCallEdges, observed)
		}
	}
}

// noCgoCallFixtures cover the fallback adapters that are not in callFixtures,
// which also exercises their symbols-only claim against an actual parse.
var noCgoCallFixtures = map[string]struct {
	file string
	src  string
}{
	"csharp": {"cg_probe.cs", "class CgProbe {\nvoid Callee() {}\nvoid Caller() { Callee(); }\n}"},
	"hcl":    {"cg_probe.tf", "locals {\n value = module.example.output\n}"},
	"kotlin": {"cg_probe.kt", "fun callee() {}\nfun caller() { callee() }\n"},
	"php":    {"cg_probe.php", "<?php\nfunction callee() {}\nfunction caller() { callee(); }\n"},
	"ruby":   {"cg_probe.rb", "def callee; end\ndef caller; callee; end\n"},
	"rust":   {"cg_probe.rs", "fn callee() {}\nfn caller() { callee(); }\n"},
	"swift":  {"cg_probe.swift", "func callee() {}\nfunc caller() { callee() }\n"},
}

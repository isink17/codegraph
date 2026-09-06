package cli

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// These tests run against the PRODUCTION default registry for whichever build
// mode is compiling them, which is the only registry whose provenance is ever
// persisted. Run the package under both CGO_ENABLED=0 and CGO_ENABLED=1.

func TestDefaultRegistryDeclaresAProfileForEveryLanguage(t *testing.T) {
	registry := newDefaultRegistry()
	if missing := registry.LanguagesMissingProfile(); len(missing) > 0 {
		t.Fatalf("production adapters without a parser profile: %v", missing)
	}
	seen := map[string]string{}
	for _, lang := range registry.SupportedLanguages() {
		if lang.ParserProfile == "" {
			t.Fatalf("language %q has no parser profile", lang.Language)
		}
		if prev, dup := seen[lang.ParserProfile]; dup {
			t.Fatalf("profile %q is shared by %q and %q; profile identity must be per implementation and language",
				lang.ParserProfile, prev, lang.Language)
		}
		seen[lang.ParserProfile] = lang.Language
	}
}

func TestDefaultRegistryProfilesAreDeterministic(t *testing.T) {
	first := newDefaultRegistry().SupportedLanguages()
	second := newDefaultRegistry().SupportedLanguages()
	if len(first) != len(second) {
		t.Fatalf("registry size changed between constructions: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Language != second[i].Language ||
			first[i].ParserProfile != second[i].ParserProfile ||
			first[i].CallEdges != second[i].CallEdges {
			t.Fatalf("profile %d not deterministic: %#v vs %#v", i, first[i], second[i])
		}
	}
}

// callFixtures are minimal programs in which one declaration calls another. The
// capability flag is a persisted claim, so it is checked against what the
// adapter actually produces rather than against a hand-written table.
var callFixtures = map[string]struct {
	file string
	src  string
}{
	"go":         {"cg_probe.go", "package p\n\nfunc callee() {}\n\nfunc caller() { callee() }\n"},
	"python":     {"cg_probe.py", "def callee():\n    pass\n\n\ndef caller():\n    callee()\n"},
	"java":       {"CgProbe.java", "class CgProbe {\n    void callee() {}\n    void caller() { this.callee(); }\n}\n"},
	"cpp":        {"cg_probe.cpp", "void cg_probe_callee() {}\nvoid cg_probe_caller() { cg_probe_callee(); }\n"},
	"typescript": {"cg_probe.ts", "function callee(): void {}\nfunction caller(): void { callee(); }\n"},
}

func TestDefaultRegistryCallEdgeClaimsAreTruthful(t *testing.T) {
	ctx := context.Background()
	registry := newDefaultRegistry()
	for language, fixture := range callFixtures {
		profile, ok := registry.ProfileForLanguage(language)
		if !ok {
			t.Fatalf("no adapter for %q in the default registry", language)
		}
		adapter := registry.AdapterFor(fixture.file)
		if adapter == nil {
			t.Fatalf("no adapter resolves %q", fixture.file)
		}
		parsed, err := adapter.Parse(ctx, fixture.file, []byte(fixture.src))
		if err != nil {
			t.Fatalf("%s: Parse() error = %v", language, err)
		}
		observed := false
		for _, edge := range parsed.Edges {
			if edge.Kind == "calls" {
				observed = true
				break
			}
		}
		if observed != profile.EmitsCallEdges {
			t.Fatalf("%s (%s): EmitsCallEdges = %v but the adapter %s produce a call edge for its fixture",
				language, profile.ID, profile.EmitsCallEdges, map[bool]string{true: "did", false: "did not"}[observed])
		}
	}
}

func TestDefaultRegistryDegradedLanguagesMatchProfiles(t *testing.T) {
	registry := newDefaultRegistry()
	want := []string{}
	for _, lang := range registry.SupportedLanguages() {
		if !lang.CallEdges {
			want = append(want, lang.Language)
		}
	}
	sort.Strings(want)
	got := registry.DegradedLanguages()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("DegradedLanguages() = %v, want %v", got, want)
	}
}

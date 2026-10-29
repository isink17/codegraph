package parser

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

type stubAdapter struct {
	language string
	ext      string
	profile  Profile
	declares bool
}

func (a *stubAdapter) Language() string { return a.language }
func (a *stubAdapter) Supports(path string) bool {
	return strings.HasSuffix(path, a.ext)
}
func (a *stubAdapter) Extensions() []string { return []string{a.ext} }
func (a *stubAdapter) Parse(context.Context, string, []byte) (graph.ParsedFile, error) {
	return graph.ParsedFile{Language: a.language}, nil
}

type profiledStub struct{ stubAdapter }

func (a *profiledStub) Profile() Profile { return a.profile }

func profiled(language, ext, id string) Adapter {
	return &profiledStub{stubAdapter{language: language, ext: ext, profile: Profile{ID: id, EmitsCallEdges: true}}}
}

// syntheticAdapter is a complete test-only adapter used to pin the public
// registry contract without registering a production language.
type syntheticAdapter struct{}

func (syntheticAdapter) Language() string { return "lp2synthetic" }
func (syntheticAdapter) Supports(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".syn")
}
func (syntheticAdapter) Extensions() []string { return []string{" SYN ", ".syn"} }
func (syntheticAdapter) Profile() Profile {
	return Profile{ID: "synthetic:lp2synthetic:v1", SemanticEpoch: 1}
}
func (a syntheticAdapter) Parse(_ context.Context, path string, _ []byte) (graph.ParsedFile, error) {
	return graph.ParsedFile{Language: a.Language(), Symbols: []graph.Symbol{{
		Language: a.Language(), Kind: "function", Name: "entry", QualifiedName: "entry",
		Range:     graph.Position{StartLine: 1, StartCol: 1, EndLine: 1, EndCol: 6},
		StableKey: "func:lp2synthetic:entry",
	}}, References: []graph.Reference{{Kind: "calls", Name: "external"}}}, nil
}

func TestSyntheticAdapterExercisesRegistryContract(t *testing.T) {
	registry := NewRegistry(syntheticAdapter{})
	adapter := registry.AdapterFor("EXAMPLE.SYN")
	if adapter == nil || adapter.Language() != "lp2synthetic" {
		t.Fatalf("AdapterFor() = %v, want synthetic adapter", adapter)
	}
	if got := registry.ExtensionCollisions(); len(got) != 0 {
		t.Fatalf("ExtensionCollisions() = %v", got)
	}
	profile, ok := registry.ProfileForLanguage(adapter.Language())
	if !ok || profile.ID != "synthetic:lp2synthetic:v1" || profile.SemanticEpoch != 1 || profile.EmitsCallEdges {
		t.Fatalf("profile = %+v, %v", profile, ok)
	}
	parsed, err := adapter.Parse(context.Background(), "EXAMPLE.SYN", nil)
	if err != nil || parsed.Language != adapter.Language() || len(parsed.Symbols) != 1 {
		t.Fatalf("Parse() = %+v, %v", parsed, err)
	}
	symbol := parsed.Symbols[0]
	if symbol.StableKey != "func:lp2synthetic:entry" || symbol.Range.StartLine < 1 || symbol.Range.StartCol < 1 || symbol.Range.EndLine < symbol.Range.StartLine || symbol.Range.EndCol < 1 {
		t.Fatalf("invalid normalized symbol: %+v", symbol)
	}
	if len(parsed.References) != 1 || parsed.References[0].SymbolID != nil {
		t.Fatalf("unresolved reference fabricated a target: %+v", parsed.References)
	}
	capabilities := registry.Capabilities()
	if len(capabilities) != 1 || capabilities[0].CallResolution != nil || capabilities[0].ImportResolution != nil || capabilities[0].TypeResolution != nil {
		t.Fatalf("unknown capability dimensions were asserted: %+v", capabilities)
	}
}

func TestRegistryAdapterWithoutProfileIsReported(t *testing.T) {
	registry := NewRegistry(&stubAdapter{language: "java", ext: ".java"})
	if got := registry.LanguagesMissingProfile(); len(got) != 1 || got[0] != "java" {
		t.Fatalf("LanguagesMissingProfile() = %v, want [java]", got)
	}
	if _, ok := registry.ProfileForLanguage("java"); ok {
		t.Fatal("ProfileForLanguage() returned a profile for an adapter that declares none")
	}
	capabilities := registry.Capabilities()
	if len(capabilities) != 1 || !capabilities[0].Parser || capabilities[0].ParserProfile != "" || capabilities[0].Symbols != nil {
		t.Fatalf("capability without parser profile = %+v, want parser available and profile-dependent dimensions unknown", capabilities)
	}
}

// Two adapters claiming one language with different identities cannot both be
// right: AdapterFor picks by extension, so a file could be parsed by one and
// stamped with the other's profile, and the language would reparse forever.
// The registry refuses to guess and reports the language as profile-less.
func TestRegistryConflictingProfilesForOneLanguageAreRejected(t *testing.T) {
	registry := NewRegistry(
		profiled("java", ".java", "treesitter:java:v2"),
		profiled("java", ".jav", "heuristic:java:v1"),
	)
	if got := registry.LanguagesMissingProfile(); len(got) != 1 || got[0] != "java" {
		t.Fatalf("LanguagesMissingProfile() = %v, want [java]", got)
	}
	if _, ok := registry.ProfileForLanguage("java"); ok {
		t.Fatal("ProfileForLanguage() picked a winner between conflicting profiles")
	}
}

func TestRegistryIdenticalProfilesForOneLanguageAreFine(t *testing.T) {
	registry := NewRegistry(
		profiled("cpp", ".cpp", "treesitter:cpp:v1"),
		profiled("cpp", ".hpp", "treesitter:cpp:v1"),
	)
	if got := registry.LanguagesMissingProfile(); len(got) != 0 {
		t.Fatalf("LanguagesMissingProfile() = %v, want none", got)
	}
	if profile, ok := registry.ProfileForLanguage("cpp"); !ok || profile.ID != "treesitter:cpp:v1" {
		t.Fatalf("ProfileForLanguage() = %#v, %v", profile, ok)
	}
	capabilities := registry.Capabilities()
	if len(capabilities) != 1 || !reflect.DeepEqual(capabilities[0].Extensions, []string{".cpp", ".hpp"}) {
		t.Fatalf("Capabilities() = %+v, want one cpp capability with both adapter extensions", capabilities)
	}
}

func TestRegistryAmbiguousExtensionFailsClosed(t *testing.T) {
	registry := NewRegistry(
		profiled("one", ".shared", "treesitter:one:v1"),
		profiled("two", ".shared", "treesitter:two:v1"),
		profiled("three", ".shared", "treesitter:three:v1"),
	)
	if got := registry.AdapterFor("file.shared"); got != nil {
		t.Fatalf("AdapterFor(.shared) = %T, want nil for ambiguous extension", got)
	}
	if got := registry.AdapterFor("another.shared"); got != nil {
		t.Fatalf("AdapterFor(cached ambiguous extension) = %T, want nil", got)
	}
	want := []ExtensionCollision{{Extension: ".shared", Languages: []string{"one", "three", "two"}}}
	got := registry.ExtensionCollisions()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtensionCollisions() = %+v, want %+v", got, want)
	}
	got[0].Languages[0] = "mutated"
	if again := registry.ExtensionCollisions(); !reflect.DeepEqual(again, want) {
		t.Fatalf("ExtensionCollisions() exposed mutable registry data: %+v", again)
	}
}

func TestLanguageCapabilitiesAreSortedAndExplicit(t *testing.T) {
	registry := NewRegistry(profiled("java", ".java", "treesitter:java:v12"))
	got := registry.Capabilities()
	if len(got) != 1 || got[0].ID != "java" || got[0].DisplayName != "Java" || !got[0].Parser {
		t.Fatalf("Capabilities() = %+v", got)
	}
	if got[0].Symbols == nil || !*got[0].Symbols || got[0].Calls == nil || !*got[0].Calls {
		t.Fatalf("parser capabilities = %+v", got[0])
	}
	if got[0].CallResolution != nil || got[0].TypeResolution != nil {
		t.Fatalf("unknown resolver dimensions must stay unknown: %+v", got[0])
	}
	*got[0].Calls = false
	if again := registry.Capabilities(); again[0].Calls == nil || !*again[0].Calls {
		t.Fatalf("Capabilities() exposed mutable registry state: %+v", again)
	}
}

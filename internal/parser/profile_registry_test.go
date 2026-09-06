package parser

import (
	"context"
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

func TestRegistryAdapterWithoutProfileIsReported(t *testing.T) {
	registry := NewRegistry(&stubAdapter{language: "java", ext: ".java"})
	if got := registry.LanguagesMissingProfile(); len(got) != 1 || got[0] != "java" {
		t.Fatalf("LanguagesMissingProfile() = %v, want [java]", got)
	}
	if _, ok := registry.ProfileForLanguage("java"); ok {
		t.Fatal("ProfileForLanguage() returned a profile for an adapter that declares none")
	}
}

// Two adapters claiming one language with different identities cannot both be
// right: AdapterFor picks by extension, so a file could be parsed by one and
// stamped with the other's profile, and the language would reparse forever.
// The registry refuses to guess and reports the language as profile-less.
func TestRegistryConflictingProfilesForOneLanguageAreRejected(t *testing.T) {
	registry := NewRegistry(
		profiled("java", ".java", "treesitter:java:v1"),
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
}

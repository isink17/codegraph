package cli

import (
	"testing"

	"github.com/isink17/codegraph/internal/classify"
)

func TestClassificationCapabilitiesMatchLiveParserRegistry(t *testing.T) {
	registry := newDefaultRegistry()
	registered := registry.SupportedLanguages()
	capabilities := classify.Capabilities()
	if len(registered) != len(capabilities) {
		t.Fatalf("live languages = %d, capabilities = %d", len(registered), len(capabilities))
	}
	seen := map[string]bool{}
	for _, language := range registered {
		if seen[language.Language] {
			t.Fatalf("live registry repeats %q", language.Language)
		}
		seen[language.Language] = true
	}
	for _, capability := range capabilities {
		if !seen[capability.Language] {
			t.Errorf("capability table contains non-live language %q", capability.Language)
		}
	}
	for _, language := range registered {
		found := false
		for _, capability := range capabilities {
			if capability.Language == language.Language {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("live language %q missing from capability table", language.Language)
		}
	}
}

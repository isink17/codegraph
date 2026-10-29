package cli

import (
	"testing"

	"github.com/isink17/codegraph/internal/classify"
)

func TestClassificationCapabilitiesMatchLiveParserRegistry(t *testing.T) {
	registry := newDefaultRegistry()
	registered := registry.SupportedLanguages()
	capabilities := classify.Capabilities()
	seen := map[string]bool{}
	for _, language := range registered {
		if seen[language.Language] {
			t.Fatalf("live registry repeats %q", language.Language)
		}
		seen[language.Language] = true
	}
	for language := range seen {
		if language == "solidity" {
			continue // syntax-only pilot records no graph edges to classify
		}
		found := false
		for _, capability := range capabilities {
			if capability.Language == language {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("live language %q missing from capability table", language)
		}
	}
}

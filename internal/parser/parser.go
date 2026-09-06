package parser

import (
	"context"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

type Adapter interface {
	Language() string
	Supports(path string) bool
	Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error)
}

type ExtensionProvider interface {
	Extensions() []string
}

type Registry struct {
	adapters       []Adapter
	adapterByExt   map[string]Adapter
	adapterByPath  map[string]Adapter
	profileByLang  map[string]Profile
	langsNoProfile []string
}

type LanguageSupport struct {
	Language   string   `json:"language"`
	Extensions []string `json:"extensions,omitempty"`
	// ParserProfile is the semantic identity of the adapter serving this
	// language in THIS binary (see Profile). Empty means the adapter declares
	// no profile, which production registries never do.
	ParserProfile string `json:"parser_profile,omitempty"`
	// CallEdges reports whether that adapter builds a call graph. False means
	// the language is indexed symbols-only by this binary.
	CallEdges bool `json:"call_edges"`
}

func NewRegistry(adapters ...Adapter) *Registry {
	byExt := map[string]Adapter{}
	for _, adapter := range adapters {
		provider, ok := adapter.(ExtensionProvider)
		if !ok {
			continue
		}
		for _, ext := range provider.Extensions() {
			normalized := strings.ToLower(strings.TrimSpace(ext))
			if normalized == "" {
				continue
			}
			if !strings.HasPrefix(normalized, ".") {
				normalized = "." + normalized
			}
			if _, exists := byExt[normalized]; !exists {
				byExt[normalized] = adapter
			}
		}
	}
	// Profiles are keyed by language, but AdapterFor resolves by extension. If
	// two adapters claimed one language with different profiles, a file could be
	// parsed by one and stamped with the other's identity -- provenance that
	// would never match and a language that reparses on every scan forever.
	// Rather than pick a winner, such a language is recorded as having no
	// profile at all, which LanguagesMissingProfile reports and the production
	// registry contract test fails on.
	byLang := map[string]Profile{}
	missingSet := map[string]struct{}{}
	for _, adapter := range adapters {
		language := adapter.Language()
		profile := Profile{}
		if provider, ok := adapter.(ProfileProvider); ok {
			profile = provider.Profile()
		}
		if !profile.Known() {
			delete(byLang, language)
			missingSet[language] = struct{}{}
			continue
		}
		if _, conflicted := missingSet[language]; conflicted {
			continue
		}
		if existing, seen := byLang[language]; seen {
			if existing.ID != profile.ID {
				delete(byLang, language)
				missingSet[language] = struct{}{}
			}
			continue
		}
		byLang[language] = profile
	}
	missing := make([]string, 0, len(missingSet))
	for language := range missingSet {
		missing = append(missing, language)
	}
	sort.Strings(missing)
	return &Registry{
		adapters:       adapters,
		adapterByExt:   byExt,
		adapterByPath:  map[string]Adapter{},
		profileByLang:  byLang,
		langsNoProfile: missing,
	}
}

// ProfileForLanguage returns the profile of the adapter serving `language` in
// this registry. The second result is false when no adapter serves the
// language or when its adapter declares no profile.
func (r *Registry) ProfileForLanguage(language string) (Profile, bool) {
	profile, ok := r.profileByLang[language]
	return profile, ok
}

// LanguageProfiles returns a copy of the language -> profile map. Callers hold
// it for the duration of a scan instead of asking per file.
func (r *Registry) LanguageProfiles() map[string]Profile {
	out := make(map[string]Profile, len(r.profileByLang))
	for language, profile := range r.profileByLang {
		out[language] = profile
	}
	return out
}

// LanguagesMissingProfile lists the languages whose adapter declares no stable
// profile. It is empty for every production default registry; the registry
// contract tests fail the build if it is not.
func (r *Registry) LanguagesMissingProfile() []string {
	return slices.Clone(r.langsNoProfile)
}

// DegradedLanguages lists the languages this binary indexes without a call
// graph. Fresh indexing with such an adapter is allowed; hiding it is not.
func (r *Registry) DegradedLanguages() []string {
	out := make([]string, 0, len(r.profileByLang))
	for language, profile := range r.profileByLang {
		if !profile.EmitsCallEdges {
			out = append(out, language)
		}
	}
	sort.Strings(out)
	return out
}

func (r *Registry) AdapterFor(path string) Adapter {
	if adapter, ok := r.adapterByPath[path]; ok {
		return adapter
	}
	ext := strings.ToLower(filepath.Ext(path))
	if adapter, ok := r.adapterByExt[ext]; ok {
		r.adapterByPath[path] = adapter
		return adapter
	}
	for _, adapter := range r.adapters {
		if adapter.Supports(path) {
			r.adapterByPath[path] = adapter
			return adapter
		}
	}
	r.adapterByPath[path] = nil
	return nil
}

func (r *Registry) SupportedLanguages() []LanguageSupport {
	out := make([]LanguageSupport, 0, len(r.adapters))
	for _, adapter := range r.adapters {
		item := LanguageSupport{Language: adapter.Language()}
		if profile, ok := r.profileByLang[item.Language]; ok {
			item.ParserProfile = profile.ID
			item.CallEdges = profile.EmitsCallEdges
		}
		if provider, ok := adapter.(ExtensionProvider); ok {
			extSet := map[string]struct{}{}
			for _, ext := range provider.Extensions() {
				normalized := strings.ToLower(strings.TrimSpace(ext))
				if normalized == "" {
					continue
				}
				if !strings.HasPrefix(normalized, ".") {
					normalized = "." + normalized
				}
				extSet[normalized] = struct{}{}
			}
			item.Extensions = make([]string, 0, len(extSet))
			for ext := range extSet {
				item.Extensions = append(item.Extensions, ext)
			}
			sort.Strings(item.Extensions)
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Language < out[j].Language
	})
	return out
}

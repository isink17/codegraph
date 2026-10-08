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
	adapters            []Adapter
	adapterByExt        map[string]Adapter
	adapterByPath       map[string]Adapter
	profileByLang       map[string]Profile
	langsNoProfile      []string
	capabilities        map[string]LanguageCapability
	extensionCollisions []ExtensionCollision
}

// ExtensionCollision lists languages that claim the same file extension.
type ExtensionCollision struct {
	Extension string   `json:"extension"`
	Languages []string `json:"languages"`
}

// NoCGOCapability records parser support available in a build without CGO.
type NoCGOCapability struct {
	Parser    bool
	CallGraph bool
}

// LanguageCapability describes the language support implemented by this
// registry. Unknown dimensions remain nil; a parser does not imply resolver
// or type-resolution support.
type LanguageCapability struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name"`
	Extensions       []string `json:"extensions,omitempty"`
	ParserProfile    string   `json:"parser_profile,omitempty"`
	Parser           bool     `json:"parser"`
	NoCGOParser      *bool    `json:"no_cgo_parser,omitempty"`
	NoCGOCallGraph   *bool    `json:"no_cgo_call_graph,omitempty"`
	Symbols          *bool    `json:"symbols,omitempty"`
	Calls            *bool    `json:"calls,omitempty"`
	CallResolution   *bool    `json:"call_resolution,omitempty"`
	ImportResolution *bool    `json:"import_resolution,omitempty"`
	TypeResolution   *bool    `json:"type_resolution,omitempty"`
}

var languageNames = map[string]string{
	"cpp": "C/C++", "csharp": "C#", "dart": "Dart", "go": "Go", "java": "Java",
	"kotlin": "Kotlin", "php": "PHP", "python": "Python", "ruby": "Ruby",
	"hcl":  "HCL/Terraform",
	"lua":  "Lua",
	"rust": "Rust", "scala": "Scala", "swift": "Swift", "typescript": "TypeScript/JavaScript",
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
	collisions := map[string]map[string]struct{}{}
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
			if existing, exists := byExt[normalized]; !exists {
				byExt[normalized] = adapter
			} else if languages, collided := collisions[normalized]; collided {
				languages[adapter.Language()] = struct{}{}
			} else if existing != nil && existing.Language() != adapter.Language() {
				collisions[normalized] = map[string]struct{}{
					existing.Language(): {}, adapter.Language(): {},
				}
				byExt[normalized] = nil
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
		// Older synthetic adapters used in tests may omit the new contract;
		// bind them to the current language generation just like the legacy
		// unknown-provenance upgrade path. Production defaults are checked
		// directly by the profile registry tests.
		if profile.Known() && profile.SemanticEpoch == 0 {
			profile.SemanticEpoch = SemanticEpochForProfile(profile.ID)
			if profile.SemanticEpoch == 0 && !isProductionProfileFamily(profile.ID) {
				profile.SemanticEpoch = 1
			}
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
			if existing.ID != profile.ID || (existing.SemanticEpoch > 0 && profile.SemanticEpoch > 0 && existing.SemanticEpoch != profile.SemanticEpoch) {
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
	r := &Registry{
		adapters:       adapters,
		adapterByExt:   byExt,
		adapterByPath:  map[string]Adapter{},
		profileByLang:  byLang,
		langsNoProfile: missing,
	}
	for extension, languages := range collisions {
		collision := ExtensionCollision{Extension: extension}
		for language := range languages {
			collision.Languages = append(collision.Languages, language)
		}
		sort.Strings(collision.Languages)
		r.extensionCollisions = append(r.extensionCollisions, collision)
	}
	sort.Slice(r.extensionCollisions, func(i, j int) bool {
		return r.extensionCollisions[i].Extension < r.extensionCollisions[j].Extension
	})
	r.capabilities = registryCapabilities(r.SupportedLanguages())
	return r
}

func registryCapabilities(languages []LanguageSupport) map[string]LanguageCapability {
	out := make(map[string]LanguageCapability, len(languages))
	for _, language := range languages {
		capability, exists := out[language.Language]
		if !exists {
			name := languageNames[language.Language]
			if name == "" {
				name = language.Language
			}
			capability = LanguageCapability{ID: language.Language, DisplayName: name, Parser: true}
		}
		extensions := make(map[string]struct{}, len(capability.Extensions)+len(language.Extensions))
		for _, extension := range capability.Extensions {
			extensions[extension] = struct{}{}
		}
		for _, extension := range language.Extensions {
			extensions[extension] = struct{}{}
		}
		capability.Extensions = capability.Extensions[:0]
		for extension := range extensions {
			capability.Extensions = append(capability.Extensions, extension)
		}
		sort.Strings(capability.Extensions)
		if language.ParserProfile != "" {
			capability.ParserProfile = language.ParserProfile
			capability.Symbols = boolRef(true)
			capability.Calls = boolRef(language.CallEdges)
		}
		out[language.Language] = capability
	}
	return out
}

func boolRef(value bool) *bool { return &value }

func isProductionProfileFamily(id string) bool {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) < 2 {
		return false
	}
	switch parts[0] {
	case "treesitter", "heuristic", "python-regex":
		return true
	}
	return false
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

// SemanticEpochs returns the language-level generations served by this registry.
func (r *Registry) SemanticEpochs() map[string]int {
	out := make(map[string]int, len(r.profileByLang))
	for language, profile := range r.profileByLang {
		out[language] = profile.SemanticEpoch
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
		if adapter == nil {
			r.adapterByPath[path] = nil
			return nil
		}
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

// Capabilities returns a deterministic snapshot of the registered language
// capability records.
func (r *Registry) Capabilities() []LanguageCapability {
	out := make([]LanguageCapability, 0, len(r.capabilities))
	for _, capability := range r.capabilities {
		capability.Extensions = slices.Clone(capability.Extensions)
		capability.NoCGOParser = cloneBool(capability.NoCGOParser)
		capability.NoCGOCallGraph = cloneBool(capability.NoCGOCallGraph)
		capability.Symbols = cloneBool(capability.Symbols)
		capability.Calls = cloneBool(capability.Calls)
		capability.CallResolution = cloneBool(capability.CallResolution)
		capability.ImportResolution = cloneBool(capability.ImportResolution)
		capability.TypeResolution = cloneBool(capability.TypeResolution)
		out = append(out, capability)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// SetNoCGOCapabilities completes build-independent availability metadata.
// Call it during registry construction, before sharing the registry.
func (r *Registry) SetNoCGOCapabilities(available map[string]NoCGOCapability) {
	for language, capability := range r.capabilities {
		value, ok := available[language]
		if !ok {
			continue
		}
		capability.NoCGOParser = boolRef(value.Parser)
		capability.NoCGOCallGraph = boolRef(value.Parser && value.CallGraph)
		r.capabilities[language] = capability
	}
}

// ExtensionCollisions returns deterministic collision diagnostics.
func (r *Registry) ExtensionCollisions() []ExtensionCollision {
	out := make([]ExtensionCollision, len(r.extensionCollisions))
	for i, collision := range r.extensionCollisions {
		out[i] = ExtensionCollision{Extension: collision.Extension, Languages: slices.Clone(collision.Languages)}
	}
	return out
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

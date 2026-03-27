package parser

import (
	"context"
	"path/filepath"
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
	adapters      []Adapter
	adapterByExt  map[string]Adapter
	adapterByPath map[string]Adapter
}

type LanguageSupport struct {
	Language   string   `json:"language"`
	Extensions []string `json:"extensions,omitempty"`
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
	return &Registry{
		adapters:      adapters,
		adapterByExt:  byExt,
		adapterByPath: map[string]Adapter{},
	}
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

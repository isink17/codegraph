package parser

import (
	"context"
	"path/filepath"
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

func (r *Registry) Languages() []string {
	out := make([]string, 0, len(r.adapters))
	for _, adapter := range r.adapters {
		out = append(out, adapter.Language())
	}
	return out
}

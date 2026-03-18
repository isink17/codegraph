package parser

import (
	"context"

	"github.com/example/localcodegraph/internal/graph"
)

type Adapter interface {
	Language() string
	Supports(path string) bool
	Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error)
}

type Registry struct {
	adapters []Adapter
}

func NewRegistry(adapters ...Adapter) *Registry {
	return &Registry{adapters: adapters}
}

func (r *Registry) AdapterFor(path string) Adapter {
	for _, adapter := range r.adapters {
		if adapter.Supports(path) {
			return adapter
		}
	}
	return nil
}

func (r *Registry) Languages() []string {
	out := make([]string, 0, len(r.adapters))
	for _, adapter := range r.adapters {
		out = append(out, adapter.Language())
	}
	return out
}

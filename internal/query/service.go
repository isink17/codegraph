package query

import (
	"context"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

type Service struct {
	store *store.Store
}

func New(s *store.Store) *Service {
	return &Service{store: s}
}

func (s *Service) Stats(ctx context.Context, repoID int64) (graph.Stats, error) {
	return s.store.Stats(ctx, repoID)
}

func (s *Service) FindSymbol(ctx context.Context, repoID int64, query string, limit, offset int) ([]graph.Symbol, error) {
	return s.store.FindSymbol(ctx, repoID, query, limit, offset)
}

func (s *Service) SearchSymbols(ctx context.Context, repoID int64, query string, limit, offset int) ([]graph.Symbol, error) {
	return s.store.SearchSymbols(ctx, repoID, query, limit, offset)
}

func (s *Service) FindCallers(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error) {
	return s.store.FindCallers(ctx, repoID, symbol, symbolID, limit, offset)
}

func (s *Service) FindCallees(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error) {
	return s.store.FindCallees(ctx, repoID, symbol, symbolID, limit, offset)
}

func (s *Service) ImpactRadius(ctx context.Context, repoID int64, symbols []string, files []string, depth int) (map[string]any, error) {
	return s.store.ImpactRadius(ctx, repoID, symbols, files, depth)
}

func (s *Service) RelatedTests(ctx context.Context, repoID int64, symbol, file string, limit, offset int) ([]store.RelatedTest, error) {
	return s.store.RelatedTests(ctx, repoID, symbol, file, limit, offset)
}

func (s *Service) SemanticSearch(ctx context.Context, repoID int64, query string, limit, offset int) ([]map[string]any, error) {
	return s.store.SemanticSearch(ctx, repoID, query, limit, offset)
}

func (s *Service) GraphSnapshot(ctx context.Context, repoID int64, symbol string, depth int) ([]graph.Symbol, []store.ExportEdge, error) {
	return s.store.GraphSnapshot(ctx, repoID, symbol, depth)
}

func (s *Service) ExportSymbolsPage(ctx context.Context, repoID int64, limit, offset int) ([]graph.Symbol, error) {
	return s.store.ExportSymbolsPage(ctx, repoID, limit, offset)
}

func (s *Service) ExportEdgesPage(ctx context.Context, repoID int64, limit, offset int) ([]store.ExportEdge, error) {
	return s.store.ExportEdgesPage(ctx, repoID, limit, offset)
}

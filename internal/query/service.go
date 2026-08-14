package query

import (
	"context"
	"sort"

	"github.com/isink17/codegraph/internal/embedding"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

type Service struct {
	store    *store.Store
	embedder embedding.Embedder
	// ctxStore is the store surface ContextForTask expands the graph through. It
	// always points at store, and exists so a test can count the round trips a
	// context request makes: fixing seed parsing woke up caller/callee expansion
	// that had been dormant, and the guard is what keeps it from silently
	// regressing into a query storm.
	ctxStore contextStoreOps
}

func New(s *store.Store, embedder embedding.Embedder) *Service {
	if embedder == nil {
		embedder = embedding.NewNoop()
	}
	return &Service{store: s, embedder: embedder, ctxStore: s}
}

func (s *Service) Stats(ctx context.Context, repoID int64) (graph.Stats, error) {
	return s.store.Stats(ctx, repoID)
}

func (s *Service) ArchitectureOverview(ctx context.Context, repoID int64) (map[string]any, error) {
	return s.store.ArchitectureOverview(ctx, repoID)
}

func (s *Service) FindSymbol(ctx context.Context, repoID int64, query string, limit, offset int) ([]graph.Symbol, error) {
	return s.store.FindSymbol(ctx, repoID, query, limit, offset)
}

func (s *Service) FindSymbolExact(ctx context.Context, repoID int64, query string, limit, offset int) ([]graph.Symbol, error) {
	return s.store.FindSymbolExact(ctx, repoID, query, limit, offset)
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

func (s *Service) RelatedTestsForFiles(ctx context.Context, repoID int64, files []string, limit, offset int) ([]store.RelatedTest, error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 20
	}
	perFileLimit := min(max(50, limit+offset), 1000)

	seen := map[string]bool{}
	var all []store.RelatedTest
	for _, f := range files {
		tests, err := s.store.RelatedTests(ctx, repoID, "", f, perFileLimit, 0)
		if err != nil {
			return nil, err
		}
		for _, t := range tests {
			key := t.File + "::" + t.Symbol
			if !seen[key] {
				seen[key] = true
				all = append(all, t)
			}
		}
	}
	// Sort deterministically: score desc, then file, then symbol.
	sort.Slice(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		if all[i].File != all[j].File {
			return all[i].File < all[j].File
		}
		return all[i].Symbol < all[j].Symbol
	})
	// Apply limit/offset.
	if offset >= len(all) {
		return []store.RelatedTest{}, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], nil
}

// SemanticSearch performs hybrid search (vector + FTS) when embeddings are
// available, falling back to token-overlap search otherwise.
func (s *Service) SemanticSearch(ctx context.Context, repoID int64, query string, limit, offset int) ([]map[string]any, error) {
	if !embedding.IsNoop(s.embedder) {
		hasEmb, _ := s.store.HasEmbeddings(ctx, repoID)
		if hasEmb {
			queryVec, err := s.embedder.Embed(ctx, query)
			if err == nil && queryVec != nil {
				return s.store.HybridSearch(ctx, repoID, query, queryVec, limit, offset)
			}
			// Fall through to token-overlap on embedding error.
		}
	}
	return s.store.SemanticSearch(ctx, repoID, query, limit, offset)
}

func (s *Service) FindDeadCode(ctx context.Context, repoID int64, limit, offset int) ([]map[string]any, error) {
	return s.store.FindDeadCode(ctx, repoID, limit, offset)
}

func (s *Service) ListFiles(ctx context.Context, repoID int64, pathFilter string, limit, offset int) ([]map[string]any, error) {
	return s.store.ListFiles(ctx, repoID, pathFilter, limit, offset)
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

func (s *Service) ExportDOTNodeNamesPage(ctx context.Context, repoID int64, limit, offset int) ([]string, error) {
	return s.store.ExportDOTNodeNamesPage(ctx, repoID, limit, offset)
}

func (s *Service) TraceDependencies(ctx context.Context, repoID int64, symbol string, direction string, maxDepth int) ([]map[string]any, error) {
	return s.store.TraceDependencies(ctx, repoID, symbol, direction, maxDepth)
}

func (s *Service) BenchmarkTokens(ctx context.Context, repoID int64, task string) (map[string]any, error) {
	return s.store.BenchmarkTokens(ctx, repoID, task)
}

func (s *Service) ResolveCrossLanguageLinks(ctx context.Context, repoID int64) (int, error) {
	return s.store.ResolveCrossLanguageLinks(ctx, repoID)
}

func (s *Service) PageRank(ctx context.Context, repoID int64, limit int) ([]map[string]any, error) {
	return s.store.PageRank(ctx, repoID, limit)
}

func (s *Service) CouplingMetrics(ctx context.Context, repoID int64, limit int) ([]map[string]any, error) {
	return s.store.CouplingMetrics(ctx, repoID, limit)
}

func (s *Service) DetectCycles(ctx context.Context, repoID int64, limit int) ([]map[string]any, error) {
	return s.store.DetectCycles(ctx, repoID, limit)
}

func (s *Service) AllImports(ctx context.Context, repoID int64) (map[string][]string, error) {
	return s.store.AllImports(ctx, repoID)
}

func (s *Service) AllFilePaths(ctx context.Context, repoID int64) ([]string, error) {
	return s.store.AllFilePaths(ctx, repoID)
}

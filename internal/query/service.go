package query

import (
	"context"
	"fmt"
	"sort"

	"github.com/isink17/codegraph/internal/embedding"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

type Service struct {
	store    *store.Store
	embedder embedding.Embedder
}

func New(s *store.Store, embedder embedding.Embedder) *Service {
	if embedder == nil {
		embedder = embedding.NewNoop()
	}
	return &Service{store: s, embedder: embedder}
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

// ContextForTaskOptions controls the behaviour of ContextForTask.
type ContextForTaskOptions struct {
	MaxFiles       int
	MaxSymbols     int
	IncludeTests   bool
	IncludeCallers bool
}

// ContextForTask returns the most relevant files, symbols, and relationships
// for a natural-language task description.
func (s *Service) ContextForTask(ctx context.Context, repoID int64, task string, opts ContextForTaskOptions) (*graph.TaskContext, error) {
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 10
	}
	if opts.MaxSymbols <= 0 {
		opts.MaxSymbols = 30
	}

	// Step 1: seed symbols via semantic search.
	seedResults, err := s.SemanticSearch(ctx, repoID, task, opts.MaxSymbols, 0)
	if err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}

	// Collect file-grouped symbols with relevance tags.
	type taggedSymbol struct {
		sym       graph.Symbol
		relevance string
	}
	fileSymbols := map[string][]taggedSymbol{}
	fileLang := map[string]string{}
	fileHits := map[string]int{}

	addSymbol := func(sym graph.Symbol, relevance string) {
		path := sym.FilePath
		if path == "" {
			return
		}
		// Deduplicate by qualified name + relevance within a file.
		for _, existing := range fileSymbols[path] {
			if existing.sym.QualifiedName == sym.QualifiedName && existing.relevance == relevance {
				return
			}
		}
		fileSymbols[path] = append(fileSymbols[path], taggedSymbol{sym: sym, relevance: relevance})
		if sym.Language != "" {
			fileLang[path] = sym.Language
		}
		fileHits[path]++
	}

	// Parse seed results into Symbol structs and add them.
	seedSymbols := parseSeedSymbols(seedResults)
	for _, sym := range seedSymbols {
		addSymbol(sym, "direct_match")
	}

	// Step 2: expand callers/callees for seed symbols.
	if opts.IncludeCallers {
		for _, sym := range seedSymbols {
			callers, err := s.store.FindCallers(ctx, repoID, sym.Name, 0, 10, 0)
			if err == nil {
				for _, c := range callers {
					addSymbol(c, "caller")
				}
			}
			callees, err := s.store.FindCallees(ctx, repoID, sym.Name, 0, 10, 0)
			if err == nil {
				for _, c := range callees {
					addSymbol(c, "callee")
				}
			}
		}
	}

	// Step 3: find related tests for affected files.
	testFileSymbols := map[string][]taggedSymbol{}
	testFileLang := map[string]string{}
	if opts.IncludeTests {
		seen := map[string]bool{}
		for path := range fileSymbols {
			if seen[path] {
				continue
			}
			seen[path] = true
			tests, err := s.store.RelatedTests(ctx, repoID, "", path, 10, 0)
			if err == nil {
				for _, t := range tests {
					testSym := graph.Symbol{
						Name:     t.Symbol,
						FilePath: t.File,
						Kind:     "function",
					}
					tpath := t.File
					if tpath == "" {
						continue
					}
					dup := false
					for _, existing := range testFileSymbols[tpath] {
						if existing.sym.Name == testSym.Name {
							dup = true
							break
						}
					}
					if !dup {
						testFileSymbols[tpath] = append(testFileSymbols[tpath], taggedSymbol{sym: testSym, relevance: "test"})
						testFileLang[tpath] = testSym.Language
					}
				}
			}
		}
	}

	// Step 4: rank files by hit count, truncate.
	type rankedFile struct {
		path string
		hits int
	}
	ranked := make([]rankedFile, 0, len(fileHits))
	for p, h := range fileHits {
		ranked = append(ranked, rankedFile{path: p, hits: h})
	}
	// Sort by hits descending (insertion sort).
	for i := 1; i < len(ranked); i++ {
		for j := i; j > 0 && ranked[j].hits > ranked[j-1].hits; j-- {
			ranked[j], ranked[j-1] = ranked[j-1], ranked[j]
		}
	}
	if len(ranked) > opts.MaxFiles {
		ranked = ranked[:opts.MaxFiles]
	}

	result := &graph.TaskContext{Task: task}
	for i, rf := range ranked {
		syms := fileSymbols[rf.path]
		ctxSyms := make([]graph.TaskContextSymbol, 0, len(syms))
		for _, ts := range syms {
			ctxSyms = append(ctxSyms, graph.TaskContextSymbol{
				Name:          ts.sym.Name,
				Kind:          ts.sym.Kind,
				Signature:     ts.sym.Signature,
				DocSummary:    ts.sym.DocSummary,
				Relevance:     ts.relevance,
				QualifiedName: ts.sym.QualifiedName,
			})
		}
		score := 1.0 - float64(i)*0.05
		if score < 0.1 {
			score = 0.1
		}
		result.Files = append(result.Files, graph.TaskContextFile{
			Path:           rf.path,
			Language:       fileLang[rf.path],
			RelevanceScore: score,
			Symbols:        ctxSyms,
		})
	}

	// Add test files.
	for tpath, tSyms := range testFileSymbols {
		ctxSyms := make([]graph.TaskContextSymbol, 0, len(tSyms))
		for _, ts := range tSyms {
			ctxSyms = append(ctxSyms, graph.TaskContextSymbol{
				Name:          ts.sym.Name,
				Kind:          ts.sym.Kind,
				Relevance:     ts.relevance,
				QualifiedName: ts.sym.QualifiedName,
			})
		}
		result.TestFiles = append(result.TestFiles, graph.TaskContextFile{
			Path:    tpath,
			Symbols: ctxSyms,
		})
	}

	return result, nil
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

// parseSeedSymbols extracts Symbol structs from semantic search result maps.
func parseSeedSymbols(results []map[string]any) []graph.Symbol {
	symbols := make([]graph.Symbol, 0, len(results))
	for _, r := range results {
		sym := graph.Symbol{}
		if v, ok := r["name"].(string); ok {
			sym.Name = v
		}
		if v, ok := r["qualified_name"].(string); ok {
			sym.QualifiedName = v
		}
		if v, ok := r["kind"].(string); ok {
			sym.Kind = v
		}
		if v, ok := r["signature"].(string); ok {
			sym.Signature = v
		}
		if v, ok := r["doc_summary"].(string); ok {
			sym.DocSummary = v
		}
		if v, ok := r["language"].(string); ok {
			sym.Language = v
		}
		if v, ok := r["file"].(string); ok {
			sym.FilePath = v
		}
		if v, ok := r["symbol_id"].(float64); ok {
			sym.ID = int64(v)
		}
		if v, ok := r["symbol_id"].(int64); ok {
			sym.ID = v
		}
		if sym.Name != "" {
			symbols = append(symbols, sym)
		}
	}
	return symbols
}

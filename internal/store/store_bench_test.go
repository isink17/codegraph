package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func BenchmarkStoreSearchSymbols(b *testing.B) {
	ctx := context.Background()
	b.Logf("sqlite_driver=%s", store.SQLiteDriverName())
	s, repoID := setupStoreBenchData(b, ctx)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		items, err := s.SearchSymbols(ctx, repoID, "BenchFn", 20, 0)
		if err != nil {
			b.Fatalf("SearchSymbols() error = %v", err)
		}
		if len(items) == 0 {
			b.Fatalf("expected non-empty result")
		}
	}
}

func BenchmarkStoreSemanticSearch(b *testing.B) {
	ctx := context.Background()
	b.Logf("sqlite_driver=%s", store.SQLiteDriverName())
	s, repoID := setupStoreBenchData(b, ctx)

	// SemanticSearch may be disabled/unavailable depending on build/config (e.g. embeddings not enabled).
	// Skip rather than failing the whole benchmark suite.
	if items, err := s.SemanticSearch(ctx, repoID, "BenchFn", 20, 0); err != nil || len(items) == 0 {
		b.Skip("SemanticSearch unavailable or returned no results")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		items, err := s.SemanticSearch(ctx, repoID, "BenchFn", 20, 0)
		if err != nil {
			b.Fatalf("SemanticSearch() error = %v", err)
		}
		if len(items) == 0 {
			b.Fatalf("expected non-empty result")
		}
	}
}

func BenchmarkStoreReplaceFileGraphWriteHeavy(b *testing.B) {
	ctx := context.Background()
	profile := sqliteBenchProfile()
	b.Logf("sqlite_driver=%s", store.SQLiteDriverName())
	b.Logf("sqlite_profile=%s", profile)

	dbPath := filepath.Join(b.TempDir(), "store-write-heavy.sqlite")
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{PerformanceProfile: profile})
	if err != nil {
		b.Fatalf("store.OpenWithOptions() error = %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	repoRoot := b.TempDir()
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}

	parsed := heavyParsedFile(80, 3, 2)
	path := "bench/heavy.go"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.ReplaceFileGraph(ctx, repo.ID, 1, path, "go", 1024, int64(i), strconv.Itoa(i), parsed); err != nil {
			b.Fatalf("ReplaceFileGraph() error = %v", err)
		}
	}
}

// BenchmarkStoreReplaceFileGraphsBatchWriteHeavy stresses the multi-file batch
// path (`ReplaceFileGraphsBatchWithStats`) so per-batch hot-loop allocations
// are visible in isolation from single-file overhead.
func BenchmarkStoreReplaceFileGraphsBatchWriteHeavy(b *testing.B) {
	ctx := context.Background()
	profile := sqliteBenchProfile()
	b.Logf("sqlite_driver=%s", store.SQLiteDriverName())
	b.Logf("sqlite_profile=%s", profile)

	dbPath := filepath.Join(b.TempDir(), "store-batch-write-heavy.sqlite")
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{PerformanceProfile: profile})
	if err != nil {
		b.Fatalf("store.OpenWithOptions() error = %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	repoRoot := b.TempDir()
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}

	const (
		filesPerBatch  = 16
		symbolsPerFile = 40
		refsPerSymbol  = 2
		edgesPerSymbol = 2
	)

	parsed := heavyParsedFile(symbolsPerFile, refsPerSymbol, edgesPerSymbol)
	inputs := make([]store.ReplaceFileGraphInput, 0, filesPerBatch)
	for j := 0; j < filesPerBatch; j++ {
		inputs = append(inputs, store.ReplaceFileGraphInput{
			Path:        fmt.Sprintf("bench/batch_%d.go", j),
			Language:    "go",
			SizeBytes:   1024,
			MtimeUnixNS: 0,
			ContentHash: "",
			Parsed:      parsed,
		})
	}

	stats := &store.WriteStats{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Vary mtime/hash per iteration so the upsert always writes.
		hash := strconv.Itoa(i)
		for j := range inputs {
			inputs[j].MtimeUnixNS = int64(i)
			inputs[j].ContentHash = hash
		}
		if _, err := s.ReplaceFileGraphsBatchWithStats(ctx, repo.ID, 1, inputs, stats); err != nil {
			b.Fatalf("ReplaceFileGraphsBatchWithStats() error = %v", err)
		}
	}
}

// BenchmarkStoreReplaceFileGraph_EdgeHeavy isolates the per-edge insert
// cost path: 1 source symbol, many edges, no refs, no tokens. If a
// per-edge `dst_name`/args allocation hotspot exists, this surfaces it
// without the noise of the symbol-token / FTS / file-token paths.
func BenchmarkStoreReplaceFileGraph_EdgeHeavy(b *testing.B) {
	ctx := context.Background()
	profile := sqliteBenchProfile()
	b.Logf("sqlite_driver=%s", store.SQLiteDriverName())
	b.Logf("sqlite_profile=%s", profile)

	dbPath := filepath.Join(b.TempDir(), "store-edges.sqlite")
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{PerformanceProfile: profile})
	if err != nil {
		b.Fatalf("store.OpenWithOptions() error = %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	repoRoot := b.TempDir()
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}

	const numEdges = 5000
	parsed := graph.ParsedFile{Language: "go"}
	parsed.Symbols = []graph.Symbol{{
		Language:      "go",
		Kind:          "function",
		Name:          "Src",
		QualifiedName: "bench.Src",
		Range:         graph.Position{StartLine: 1, StartCol: 1, EndLine: 1, EndCol: 10},
		StableKey:     "go:bench.Src:0",
	}}
	parsed.Edges = make([]graph.Edge, 0, numEdges)
	for e := 0; e < numEdges; e++ {
		parsed.Edges = append(parsed.Edges, graph.Edge{
			DstName:  fmt.Sprintf("bench.Dst_%d", e),
			Kind:     "calls",
			Evidence: "bench",
			Line:     e + 1,
		})
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.ReplaceFileGraph(ctx, repo.ID, 1, "bench/edges.go", "go", 1024, int64(i), strconv.Itoa(i), parsed); err != nil {
			b.Fatalf("ReplaceFileGraph() error = %v", err)
		}
	}
}

// BenchmarkStoreReplaceFileGraph_TokenHeavy isolates the per-token
// (`execTokenTriplesInsert`) path: small symbol count, large per-symbol
// token cardinality. Surfaces alloc/CPU cost in token batching loops.
func BenchmarkStoreReplaceFileGraph_TokenHeavy(b *testing.B) {
	ctx := context.Background()
	profile := sqliteBenchProfile()
	b.Logf("sqlite_driver=%s", store.SQLiteDriverName())
	b.Logf("sqlite_profile=%s", profile)

	dbPath := filepath.Join(b.TempDir(), "store-tokens.sqlite")
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{PerformanceProfile: profile})
	if err != nil {
		b.Fatalf("store.OpenWithOptions() error = %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	repoRoot := b.TempDir()
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}

	// Long doc summary + signature drives the tokenizer to many tokens per
	// symbol. 200 symbols × ~25 tokens/symbol ≈ 5k symbol_tokens rows.
	docs := "this is a long descriptive doc summary covering multiple subjects and keywords for benchmark purposes including alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu"
	parsed := graph.ParsedFile{Language: "go"}
	const numSymbols = 200
	parsed.Symbols = make([]graph.Symbol, 0, numSymbols)
	for i := 0; i < numSymbols; i++ {
		name := fmt.Sprintf("TokenFn%d", i)
		qname := fmt.Sprintf("bench.%s", name)
		parsed.Symbols = append(parsed.Symbols, graph.Symbol{
			Language:      "go",
			Kind:          "function",
			Name:          name,
			QualifiedName: qname,
			Signature:     fmt.Sprintf("func %s(ctx context.Context, request *Request, options []Option) (*Response, error)", name),
			DocSummary:    docs,
			Range:         graph.Position{StartLine: i + 1, StartCol: 1, EndLine: i + 1, EndCol: 10},
			StableKey:     fmt.Sprintf("go:%s:%d", qname, i),
		})
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.ReplaceFileGraph(ctx, repo.ID, 1, "bench/tokens.go", "go", 1024, int64(i), strconv.Itoa(i), parsed); err != nil {
			b.Fatalf("ReplaceFileGraph() error = %v", err)
		}
	}
}

// BenchmarkStoreReplaceFileGraphsBatchWriteHeavy_FileScaling reveals
// per-batch / per-file overhead by holding total symbol count constant
// (~640 symbols) but varying file count (16 files × 40 syms vs
// 80 files × 8 syms). If per-file overhead (file upsert, deleteFileGraphs
// preamble, args slice setup) dominates over per-symbol cost, the
// many-files variant is meaningfully slower.
func BenchmarkStoreReplaceFileGraphsBatchWriteHeavy_FileScaling(b *testing.B) {
	cases := []struct {
		name     string
		numFiles int
		perFile  int
	}{
		{"16files_40syms", 16, 40},
		{"80files_8syms", 80, 8},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			ctx := context.Background()
			profile := sqliteBenchProfile()
			dbPath := filepath.Join(b.TempDir(), "store-batch-scaling.sqlite")
			s, err := store.OpenWithOptions(dbPath, store.OpenOptions{PerformanceProfile: profile})
			if err != nil {
				b.Fatalf("store.OpenWithOptions() error = %v", err)
			}
			b.Cleanup(func() { _ = s.Close() })

			repoRoot := b.TempDir()
			repo, err := s.UpsertRepo(ctx, repoRoot)
			if err != nil {
				b.Fatalf("UpsertRepo() error = %v", err)
			}

			parsed := heavyParsedFile(tc.perFile, 2, 2)
			inputs := make([]store.ReplaceFileGraphInput, 0, tc.numFiles)
			for j := 0; j < tc.numFiles; j++ {
				inputs = append(inputs, store.ReplaceFileGraphInput{
					Path:        fmt.Sprintf("bench/scale_%d.go", j),
					Language:    "go",
					SizeBytes:   1024,
					MtimeUnixNS: 0,
					ContentHash: "",
					Parsed:      parsed,
				})
			}
			stats := &store.WriteStats{}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				hash := strconv.Itoa(i)
				for j := range inputs {
					inputs[j].MtimeUnixNS = int64(i)
					inputs[j].ContentHash = hash
				}
				if _, err := s.ReplaceFileGraphsBatchWithStats(ctx, repo.ID, 1, inputs, stats); err != nil {
					b.Fatalf("ReplaceFileGraphsBatchWithStats() error = %v", err)
				}
			}
		})
	}
}

func setupStoreBenchData(b *testing.B, ctx context.Context) (*store.Store, int64) {
	b.Helper()
	repoRoot := b.TempDir()
	for i := 0; i < 120; i++ {
		name := fmt.Sprintf("file_%03d.go", i)
		content := fmt.Sprintf("package bench\n\nfunc BenchFn%d() int { return %d }\n", i, i)
		path := filepath.Join(repoRoot, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			b.Fatalf("WriteFile(%q) error = %v", path, err)
		}
	}

	dbPath := filepath.Join(b.TempDir(), "store-bench.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		b.Fatalf("store.Open() error = %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		b.Fatalf("Index() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}
	return s, repo.ID
}

func sqliteBenchProfile() string {
	// Keep benchmarks repeatable without requiring CLI wiring; override with CODEGRAPH_BENCH_SQLITE_PROFILE.
	if v := os.Getenv("CODEGRAPH_BENCH_SQLITE_PROFILE"); v != "" {
		return v
	}
	return "balanced"
}

func heavyParsedFile(symbols, refsPerSymbol, edgesPerSymbol int) graph.ParsedFile {
	out := graph.ParsedFile{
		Language: "go",
	}
	out.Symbols = make([]graph.Symbol, 0, symbols)
	out.References = make([]graph.Reference, 0, symbols*refsPerSymbol)
	out.Edges = make([]graph.Edge, 0, symbols*edgesPerSymbol)

	for i := 0; i < symbols; i++ {
		name := fmt.Sprintf("BenchHeavyFn%d", i)
		qname := fmt.Sprintf("bench.%s", name)
		stable := fmt.Sprintf("go:%s:%d", qname, i)
		out.Symbols = append(out.Symbols, graph.Symbol{
			Language:      "go",
			Kind:          "function",
			Name:          name,
			QualifiedName: qname,
			Range:         graph.Position{StartLine: i + 1, StartCol: 1, EndLine: i + 1, EndCol: 10},
			StableKey:     stable,
		})

		for r := 0; r < refsPerSymbol; r++ {
			out.References = append(out.References, graph.Reference{
				Kind: "call",
				Name: name,
				Range: graph.Position{
					StartLine: i + 1,
					StartCol:  1 + r,
					EndLine:   i + 1,
					EndCol:    2 + r,
				},
			})
		}

		for e := 0; e < edgesPerSymbol; e++ {
			dst := fmt.Sprintf("bench.BenchHeavyFn%d", (i+e+1)%symbols)
			out.Edges = append(out.Edges, graph.Edge{
				DstName:  dst,
				Kind:     "calls",
				Evidence: "bench",
				Line:     i + 1,
			})
		}
	}

	return out
}

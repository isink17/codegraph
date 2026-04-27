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

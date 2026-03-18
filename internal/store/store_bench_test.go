package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func BenchmarkStoreSearchSymbols(b *testing.B) {
	ctx := context.Background()
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
	s, repoID := setupStoreBenchData(b, ctx)

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

	idx := indexer.New(s, parser.NewRegistry(goparser.New()))
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		b.Fatalf("Index() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}
	return s, repo.ID
}

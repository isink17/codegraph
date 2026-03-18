package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func BenchmarkIndexerIndex(b *testing.B) {
	ctx := context.Background()
	repoRoot := b.TempDir()
	createGoFixtureRepo(b, repoRoot, 80)
	dbDir := b.TempDir()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dbPath := filepath.Join(dbDir, "bench-index.sqlite")
		_ = os.Remove(dbPath)
		s, err := store.Open(dbPath)
		if err != nil {
			b.Fatalf("store.Open() error = %v", err)
		}
		idx := New(s, parser.NewRegistry(goparser.New()))
		if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
			_ = s.Close()
			b.Fatalf("Index() error = %v", err)
		}
		_ = s.Close()
	}
}

func BenchmarkIndexerUpdateOneFile(b *testing.B) {
	ctx := context.Background()
	repoRoot := b.TempDir()
	createGoFixtureRepo(b, repoRoot, 80)
	dbPath := filepath.Join(b.TempDir(), "bench-update.sqlite")

	s, err := store.Open(dbPath)
	if err != nil {
		b.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := New(s, parser.NewRegistry(goparser.New()))
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		b.Fatalf("Index() error = %v", err)
	}

	target := filepath.Join(repoRoot, "file_000.go")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		content := fmt.Sprintf("package bench\n\nfunc BenchFn0() int { return %d }\n", i)
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			b.Fatalf("WriteFile() error = %v", err)
		}
		if _, err := idx.Update(ctx, Options{RepoRoot: repoRoot}); err != nil {
			b.Fatalf("Update() error = %v", err)
		}
	}
}

func createGoFixtureRepo(b *testing.B, repoRoot string, files int) {
	b.Helper()
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("file_%03d.go", i)
		content := fmt.Sprintf("package bench\n\nfunc BenchFn%d() int { return %d }\n", i, i)
		path := filepath.Join(repoRoot, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			b.Fatalf("WriteFile(%q) error = %v", path, err)
		}
	}
}

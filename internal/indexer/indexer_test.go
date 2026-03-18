package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func TestIndexAndIncrementalUpdate(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "main.go"), `package main

func helper() {}

func main() {
	helper()
}
`)

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(goparser.New()))

	summary, err := idx.Index(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if summary.FilesIndexed != 1 {
		t.Fatalf("FilesIndexed = %d, want 1", summary.FilesIndexed)
	}
	if summary.FilesChanged != 1 {
		t.Fatalf("FilesChanged = %d, want 1", summary.FilesChanged)
	}

	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	stats, err := s.Stats(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.Files != 1 {
		t.Fatalf("stats.Files = %d, want 1", stats.Files)
	}
	if stats.Symbols < 2 {
		t.Fatalf("stats.Symbols = %d, want at least 2", stats.Symbols)
	}

	updateSummary, err := idx.Update(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updateSummary.FilesSkipped != 1 {
		t.Fatalf("FilesSkipped = %d, want 1", updateSummary.FilesSkipped)
	}
	if updateSummary.FilesIndexed != 0 {
		t.Fatalf("FilesIndexed = %d, want 0", updateSummary.FilesIndexed)
	}

	time.Sleep(2 * time.Millisecond)
	writeFile(t, filepath.Join(repoRoot, "main.go"), `package main

func helper() {}

func main() {
	helper()
	helper()
}
`)

	modifiedSummary, err := idx.Update(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Update(modified) error = %v", err)
	}
	if modifiedSummary.FilesIndexed != 1 {
		t.Fatalf("FilesIndexed after modification = %d, want 1", modifiedSummary.FilesIndexed)
	}
	if modifiedSummary.FilesChanged != 1 {
		t.Fatalf("FilesChanged after modification = %d, want 1", modifiedSummary.FilesChanged)
	}

	if err := os.Remove(filepath.Join(repoRoot, "main.go")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	deletedSummary, err := idx.Update(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Update(delete) error = %v", err)
	}
	if deletedSummary.FilesDeleted != 1 {
		t.Fatalf("FilesDeleted = %d, want 1", deletedSummary.FilesDeleted)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

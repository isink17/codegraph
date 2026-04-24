package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func TestDirtyFilesQueueAndDrain(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}

	if ok, err := s.HasDirtyFiles(ctx, repo.ID); err != nil {
		t.Fatalf("HasDirtyFiles() error = %v", err)
	} else if ok {
		t.Fatalf("expected no dirty files at start")
	}

	if err := s.QueueDirtyFile(ctx, repo.ID, "a.go", "test"); err != nil {
		t.Fatalf("QueueDirtyFile(a.go) error = %v", err)
	}
	if err := s.QueueDirtyFile(ctx, repo.ID, "b.go", "test"); err != nil {
		t.Fatalf("QueueDirtyFile(b.go) error = %v", err)
	}
	if err := s.QueueDirtyFile(ctx, repo.ID, "a.go", "test2"); err != nil {
		t.Fatalf("QueueDirtyFile(a.go update) error = %v", err)
	}

	if ok, err := s.HasDirtyFiles(ctx, repo.ID); err != nil {
		t.Fatalf("HasDirtyFiles() error = %v", err)
	} else if !ok {
		t.Fatalf("expected dirty files after queueing")
	}

	claimedAt := "2026-01-01T00:00:00Z"
	paths, err := s.ClaimDirtyFiles(ctx, repo.ID, claimedAt, "test_inflight")
	if err != nil {
		t.Fatalf("ClaimDirtyFiles() error = %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d (%v)", len(paths), paths)
	}

	if ok, err := s.HasDirtyFiles(ctx, repo.ID); err != nil {
		t.Fatalf("HasDirtyFiles() error = %v", err)
	} else if !ok {
		t.Fatalf("expected dirty files after claim")
	}
	if err := s.DeleteClaimedDirtyFiles(ctx, repo.ID, paths, claimedAt); err != nil {
		t.Fatalf("DeleteClaimedDirtyFiles() error = %v", err)
	}
	if ok, err := s.HasDirtyFiles(ctx, repo.ID); err != nil {
		t.Fatalf("HasDirtyFiles() error = %v", err)
	} else if ok {
		t.Fatalf("expected no dirty files after delete")
	}

	if err := s.QueueDirtyFiles(ctx, repo.ID, []string{"c.go", "d.go"}, "batch"); err != nil {
		t.Fatalf("QueueDirtyFiles() error = %v", err)
	}

	claimedAt = "2026-01-01T00:00:01Z"
	paths, err = s.ClaimDirtyFiles(ctx, repo.ID, claimedAt, "batch_inflight")
	if err != nil {
		t.Fatalf("ClaimDirtyFiles() (2) error = %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths from batch, got %d (%v)", len(paths), paths)
	}
	if err := s.DeleteClaimedDirtyFiles(ctx, repo.ID, paths, claimedAt); err != nil {
		t.Fatalf("DeleteClaimedDirtyFiles() (2) error = %v", err)
	}
}

func TestListScansIncludesLanguageCoverage(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	scans, err := s.ListScans(ctx, repo.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListScans() error = %v", err)
	}
	if len(scans) == 0 {
		t.Fatalf("expected at least one scan record")
	}
	if len(scans[0].LanguageCoverage) == 0 {
		t.Fatalf("expected language coverage in latest scan record")
	}
}

package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestCanonicalRepositoryPathFormatMarker(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequireCanonicalRepositoryPaths(ctx, repo.ID); !errors.Is(err, ErrRepositoryPathFormatRebuild) {
		t.Fatalf("missing marker: %v", err)
	}
	if err := s.EnsureCanonicalRepositoryPaths(ctx, repo.ID, false); !errors.Is(err, ErrRepositoryPathFormatRebuild) {
		t.Fatalf("non-full initialize: %v", err)
	}
	if err := s.EnsureCanonicalRepositoryPaths(ctx, repo.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireCanonicalRepositoryPaths(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalRepositoryPathFormatConcurrentInitialize(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.EnsureCanonicalRepositoryPaths(ctx, repo.ID, true) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

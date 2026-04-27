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

func TestCrossFileResolution_PartialRunIntroducedSymbolResolvesOtherFile(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()

	if err := os.WriteFile(filepath.Join(repoRoot, "b.go"), []byte("package main\nfunc Bar() { Foo() }\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(b.go) error = %v", err)
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

	if err := os.WriteFile(filepath.Join(repoRoot, "a.go"), []byte("package main\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(a.go) error = %v", err)
	}
	summary, err := idx.Update(ctx, indexer.Options{RepoRoot: repoRoot, Paths: []string{"a.go"}})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if summary.ResolveMode != "paths+names" {
		t.Fatalf("ResolveMode = %q, want %q", summary.ResolveMode, "paths+names")
	}
	if summary.ResolveCrossFile == nil || summary.ResolveCrossFile.TargetsSelected == 0 {
		t.Fatalf("ResolveCrossFile stats missing or empty: %#v", summary.ResolveCrossFile)
	}

	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	unresolved, err := s.CountUnresolvedEdgesByDstName(ctx, repo.ID, "Foo")
	if err != nil {
		t.Fatalf("CountUnresolvedEdgesByDstName() error = %v", err)
	}
	if unresolved != 0 {
		t.Fatalf("unresolved edges for Foo = %d, want 0", unresolved)
	}
}

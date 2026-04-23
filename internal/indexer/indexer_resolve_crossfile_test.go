package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func TestUpdateResolvesUnresolvedEdgesInOtherFilesWhenNewSymbolIntroduced(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "codegraph.db")

	write := func(rel, content string) {
		t.Helper()
		abs := filepath.Join(repoRoot, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("a.go", "package p\n\nfunc A() { Foo() }\n")
	write("b.go", "package p\n\nfunc B() { Foo() }\n")

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	reg := parser.NewRegistry(golang.New())
	idx := New(s, reg, nil)

	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot, ScanKind: "index"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("upsert repo: %v", err)
	}

	// Introduce Foo in a.go and update only that path.
	write("a.go", "package p\n\nfunc Foo() {}\n\nfunc A() { Foo() }\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: repoRoot, ScanKind: "update", Paths: []string{"a.go"}}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Verify b.go's edge now resolves.
	edges, err := s.ExportEdgesPage(ctx, repo.ID, 10000, 0)
	if err != nil {
		t.Fatalf("export edges: %v", err)
	}
	foundB := false
	resolvedB := false
	for _, e := range edges {
		if e.FilePath != "b.go" {
			continue
		}
		foundB = true
		if e.DstSymbolID != nil {
			resolvedB = true
			break
		}
	}
	if !foundB {
		t.Fatalf("expected at least one edge from b.go")
	}
	if !resolvedB {
		t.Fatalf("expected unresolved edge in b.go to be resolved after updating only a.go")
	}
}

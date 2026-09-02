//go:build cgo

package query

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/embedding"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

func TestContextForTaskPreservesJavaOverloadIDs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Calculator.java"), []byte(`class Calculator {
  int add(int a, int b) { return a + b; }
  String add(String a, String b) { return a + b; }
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo, err := db.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	idx := indexer.New(db, parser.NewRegistry(tsparser.NewJava()), nil)
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}

	syms, err := db.ExportSymbolsPage(ctx, repo.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var overloads = make(map[int64]struct{})
	for _, sym := range syms {
		if sym.QualifiedName == "Calculator.add" {
			if sym.StableKey != "func:java:Calculator:add" {
				t.Fatalf("overload %d stable key = %q", sym.ID, sym.StableKey)
			}
			overloads[sym.ID] = struct{}{}
		}
	}
	if len(overloads) != 2 {
		t.Fatalf("persisted Calculator.add overloads = %d, want 2", len(overloads))
	}
	ranges := map[string]struct{}{}
	for _, sym := range syms {
		if _, ok := overloads[sym.ID]; ok {
			ranges[fmt.Sprintf("%d:%d-%d:%d", sym.Range.StartLine, sym.Range.StartCol, sym.Range.EndLine, sym.Range.EndCol)] = struct{}{}
		}
	}
	if len(ranges) != 2 {
		t.Fatalf("overload ranges = %v, want two distinct ranges", ranges)
	}

	svc := New(db, nil)
	raw, err := svc.SemanticSearch(ctx, repo.ID, "add", 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	searchIDs := map[int64]struct{}{}
	for _, hit := range parseSearchHits(raw) {
		if _, ok := overloads[hit.SymbolID]; ok {
			searchIDs[hit.SymbolID] = struct{}{}
		}
	}
	if len(searchIDs) != 2 {
		t.Fatalf("SemanticSearch overload IDs = %v, want both %v", searchIDs, overloads)
	}

	res, err := svc.ContextForTask(ctx, repo.ID, "add", ContextForTaskOptions{MaxSymbols: 30})
	if err != nil {
		t.Fatal(err)
	}
	contextIDs := map[int64]struct{}{}
	for _, file := range res.Files {
		for _, sym := range file.Symbols {
			if _, ok := overloads[sym.SymbolID]; ok {
				contextIDs[sym.SymbolID] = struct{}{}
			}
		}
	}
	if len(contextIDs) != 2 {
		t.Fatalf("ContextForTask overload IDs = %v, want both %v", contextIDs, overloads)
	}

	items := make([]store.SymbolEmbeddingUpsert, 0, len(syms))
	for _, sym := range syms {
		vec, err := (bagOfWordsEmbedder{}).Embed(ctx, embedding.FormatSymbolText(sym.Kind, sym.QualifiedName, sym.Signature, sym.DocSummary))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, store.SymbolEmbeddingUpsert{SymbolID: sym.ID, FileID: sym.FileID, Vector: vec})
	}
	if err := db.UpsertSymbolEmbeddings(ctx, repo.ID, "test-bow", items); err != nil {
		t.Fatal(err)
	}
	hybrid := New(db, bagOfWordsEmbedder{})
	if raw, err := hybrid.SemanticSearch(ctx, repo.ID, "add", 30, 0); err != nil {
		t.Fatal(err)
	} else {
		got := map[int64]struct{}{}
		for _, hit := range parseSearchHits(raw) {
			if _, ok := overloads[hit.SymbolID]; ok {
				got[hit.SymbolID] = struct{}{}
			}
		}
		if len(got) != 2 {
			t.Fatalf("hybrid SemanticSearch overload IDs = %v, want both %v", got, overloads)
		}
	}
}

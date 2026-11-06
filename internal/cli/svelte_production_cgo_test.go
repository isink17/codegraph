//go:build cgo

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/store"
)

func TestSvelteProductionIndexLifecycle(t *testing.T) {
	registry := newDefaultRegistry()
	if a := registry.AdapterFor("Tick.svelte"); a == nil || a.Language() != "svelte" {
		t.Fatal("production CGO registry does not recognize .svelte")
	}
	root := svelteProductionFixture(t)
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	app, _, repoID, err := openExistingApp(ctx, cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	snapshot := func() store.SemanticGraph {
		t.Helper()
		g, err := app.Store.SemanticGraph(ctx, repoID)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	before := snapshot()
	found := false
	for _, f := range before.Files {
		if f.Path != "Tick.svelte" {
			continue
		}
		found = true
		if f.Language != "svelte" || f.ParserProfile != "treesitter:svelte:v1" || f.ParserSemanticEpoch != 1 || f.ParserCallEdges {
			t.Fatalf("Svelte provenance = %+v", f)
		}
	}
	if !found {
		t.Fatal("production index omitted Svelte file")
	}
	items, err := app.Query.FindSymbolExactResult(ctx, repoID, "Tick.svelte#instance.handleKeydown", 10, 0)
	if err != nil || len(items.Matches) != 1 {
		t.Fatalf("retrievable Svelte symbol = %+v, %v", items, err)
	}
	sym := items.Matches[0]
	if sym.Language != "svelte" || sym.Range.StartLine != 6 || sym.Range.StartCol != 2 {
		t.Fatalf("source coordinates = %+v", sym)
	}
	imports, err := app.Store.AllImports(ctx, repoID)
	if err != nil || !reflect.DeepEqual(imports["Tick.svelte"], []string{"svelte"}) {
		t.Fatalf("imports = %v, %v", imports, err)
	}
	stats, err := app.Query.Stats(ctx, repoID)
	if err != nil || stats.References == 0 || len(before.Edges) != 0 {
		t.Fatalf("references/edges = %+v / %+v, %v", stats, before.Edges, err)
	}
	for _, replacement := range []string{"", "\n<!-- production incremental change -->\n"} {
		if replacement != "" {
			path := filepath.Join(root, "Tick.svelte")
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(src, replacement...), 0600); err != nil {
				t.Fatal(err)
			}
		}
		summary, err := app.Indexer.Update(ctx, indexer.Options{RepoRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if replacement == "" && summary.FilesIndexed != 0 {
			t.Fatalf("no-op indexed files: %+v", summary)
		}
		got := snapshot()
		if replacement == "" && !reflect.DeepEqual(before, got) {
			t.Fatal("no-op changed graph")
		}
		fresh, err := store.Open(filepath.Join(t.TempDir(), "fresh.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		freshIdx := indexer.New(fresh, newDefaultRegistry(), nil)
		if _, err := freshIdx.Index(ctx, indexer.Options{RepoRoot: root}); err != nil {
			fresh.Close()
			t.Fatal(err)
		}
		repo, err := fresh.UpsertRepo(ctx, root)
		if err != nil {
			fresh.Close()
			t.Fatal(err)
		}
		want, err := fresh.SemanticGraph(ctx, repo.ID)
		closeErr := fresh.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("fresh graph/close: %v / %v", err, closeErr)
		}
		if !reflect.DeepEqual(got, want) {
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(want)
			t.Fatalf("production incremental graph differs from fresh index\ngot: %s\nwant: %s", g, w)
		}
		if _, err := app.Indexer.Update(ctx, indexer.Options{RepoRoot: root}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, snapshot()) {
			t.Fatal("post-update no-op changed graph")
		}
	}
}

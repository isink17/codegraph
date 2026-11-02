//go:build cgo

package treesitter_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

func TestSvelteIndexUpdateAndNoOpParity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "Panel.svelte")
	open := func() (*store.Store, *indexer.Indexer) {
		t.Helper()
		s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		return s, indexer.New(s, parser.NewRegistry(treesitter.NewSvelte(), treesitter.NewTypeScript()), nil)
	}
	snapshot := func(s *store.Store) string {
		t.Helper()
		repo, err := s.UpsertRepo(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		g, err := s.SemanticGraph(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	s, idx := open()
	for step, src := range []string{
		`<script>import { tick } from 'svelte'; function old(){ tick(); }</script><button on:click={old}>old</button>`,
		`<script module lang="ts">export function helper(x: number){return x;}</script><script>import { onMount } from 'svelte'; function next(){ onMount(() => {}); }</script><button onclick={next}>next</button>`,
		`<script>function invalid(){}</script><script>function duplicate(){}</script>`,
		`<script>function restored(){}</script><div broken={unknown()`,
	} {
		if err := os.WriteFile(path, []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
		opts := indexer.Options{RepoRoot: root}
		if step == 0 {
			if _, err := idx.Index(ctx, opts); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := idx.Update(ctx, opts); err != nil {
				t.Fatal(err)
			}
		}
		got := snapshot(s)
		fresh, freshIdx := open()
		if _, err := freshIdx.Index(ctx, opts); err != nil {
			t.Fatal(err)
		}
		if want := snapshot(fresh); got != want {
			t.Fatalf("step %d: incremental graph differs\n%s\n%s", step, got, want)
		}
		summary, err := idx.Update(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		if summary.FilesIndexed != 0 || summary.FilesSkipped != 1 {
			t.Fatalf("step %d: no-op summary = %+v", step, summary)
		}
		if again := snapshot(s); again != got {
			t.Fatalf("step %d: no-op changed graph", step)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(ctx, indexer.Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	fresh, freshIdx := open()
	if _, err := freshIdx.Index(ctx, indexer.Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if snapshot(s) != snapshot(fresh) {
		t.Fatal("deletion differs from fresh empty graph")
	}
}

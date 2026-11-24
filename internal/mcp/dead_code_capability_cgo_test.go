//go:build cgo

package mcp

import (
	"context"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	heuristicparser "github.com/isink17/codegraph/internal/parser/heuristic"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// Java is call-capable with cgo and symbols-only without it. Upgrading a
// non-cgo graph classifies Java exactly as a fresh cgo index does; a non-cgo
// update over the cgo graph is refused, and the answer read from the persisted
// graph stays the call-capable one.
func TestFindDeadCodeAcrossCGOTransitions(t *testing.T) {
	withCGO := func() *parser.Registry { return parser.NewRegistry(goparser.New(), tsparser.NewJava()) }
	withoutCGO := func() *parser.Registry { return parser.NewRegistry(goparser.New(), heuristicparser.NewJava()) }
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "lib/lib.go", "package lib\n\nfunc Helper(n int) int { return n * 2 }\n\nfunc Run() int { return Helper(1) }\n")
	writeRepoFile(t, repoRoot, "src/Widget.java", "package demo;\n\npublic class Widget {\n    public int size() { return helper(); }\n    private int helper() { return 1; }\n}\n")
	answer := func(s *store.Store) store.DeadCodeResult {
		t.Helper()
		repo, err := s.UpsertRepo(ctx, repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.FindDeadCodeResult(ctx, repo.ID, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	names := func(r store.DeadCodeResult) []string {
		var out []string
		for _, item := range r.Items {
			out = append(out, item["language"].(string)+":"+item["name"].(string))
		}
		return out
	}

	s := openTestStore(t)
	t.Cleanup(func() { s.Close() })
	if _, err := indexer.New(s, withoutCGO(), nil).Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatal(err)
	}
	noCGO := answer(s)
	if got := names(noCGO); !reflect.DeepEqual(got, []string{"go:Run"}) ||
		!reflect.DeepEqual(noCGO.Skipped, []store.DeadCodeSkippedLanguage{{Language: "java", Capability: store.GraphSymbolsOnly, Reason: store.DeadCodeNoCallEdges}}) {
		t.Fatalf("non-cgo answer = %v skipped %+v", got, noCGO.Skipped)
	}

	if _, err := indexer.New(s, withCGO(), nil).Update(ctx, indexer.Options{RepoRoot: repoRoot, ScanKind: "update"}); err != nil {
		t.Fatal(err)
	}
	upgraded := answer(s)
	fresh := openTestStore(t)
	t.Cleanup(func() { fresh.Close() })
	if _, err := indexer.New(fresh, withCGO(), nil).Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatal(err)
	}
	if want := answer(fresh); !reflect.DeepEqual(upgraded, want) {
		t.Fatalf("upgraded = %+v, fresh cgo = %+v", upgraded, want)
	}
	if got := names(upgraded); !reflect.DeepEqual(got, []string{"go:Run", "java:Widget", "java:size"}) || upgraded.Skipped != nil {
		t.Fatalf("cgo answer = %v skipped %+v", got, upgraded.Skipped)
	}

	if _, err := indexer.New(s, withoutCGO(), nil).Update(ctx, indexer.Options{RepoRoot: repoRoot, ScanKind: "update"}); err == nil {
		t.Fatal("non-cgo update over a call-capable Java graph was not refused")
	}
	if after := answer(s); !reflect.DeepEqual(after, upgraded) {
		t.Fatalf("answer after refused downgrade = %+v, want %+v", after, upgraded)
	}
}

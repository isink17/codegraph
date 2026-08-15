package indexer

// Shared helpers for the source-attribution tests. They live in an untagged
// file so both the cgo (tree-sitter) and the non-cgo (heuristic/cgo-free)
// attribution tests can use them.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

// indexSource writes one fixture file, indexes it through the real parser and
// indexer, and returns the store plus repo id.
func indexSource(t *testing.T, adapter parser.Adapter, filename, source string) (*store.Store, int64) {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, filename), []byte(source), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", filename, err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })

	idx := New(s, parser.NewRegistry(adapter), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	return s, repo.ID
}

func qnames(symbols []graph.Symbol) []string {
	out := make([]string, 0, len(symbols))
	for _, sym := range symbols {
		out = append(out, sym.QualifiedName)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertCallees(t *testing.T, s *store.Store, repoID int64, symbol string, want ...string) {
	t.Helper()
	got, err := s.FindCallees(context.Background(), repoID, symbol, 0, 50, 0)
	if err != nil {
		t.Fatalf("FindCallees(%s) error = %v", symbol, err)
	}
	if names := qnames(got); !equalStrings(names, want) {
		t.Fatalf("FindCallees(%s) = %v, want %v", symbol, names, want)
	}
}

func assertCallers(t *testing.T, s *store.Store, repoID int64, symbol string, want ...string) {
	t.Helper()
	got, err := s.FindCallers(context.Background(), repoID, symbol, 0, 50, 0)
	if err != nil {
		t.Fatalf("FindCallers(%s) error = %v", symbol, err)
	}
	if names := qnames(got); !equalStrings(names, want) {
		t.Fatalf("FindCallers(%s) = %v, want %v", symbol, names, want)
	}
}

// assertNoSelfEdges fails if any symbol is recorded as calling itself. A
// fabricated self-edge is the signature of a source symbol being chosen by
// fallback rather than by containment.
func assertNoSelfEdges(t *testing.T, s *store.Store, repoID int64, symbols ...string) {
	t.Helper()
	for _, symbol := range symbols {
		callers, err := s.FindCallers(context.Background(), repoID, symbol, 0, 50, 0)
		if err != nil {
			t.Fatalf("FindCallers(%s) error = %v", symbol, err)
		}
		for _, c := range callers {
			if c.QualifiedName == symbol {
				t.Fatalf("%s is recorded as calling itself", symbol)
			}
		}
	}
}

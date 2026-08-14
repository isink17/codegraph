//go:build cgo

package mcp

import (
	"context"
	"io"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/query"
)

// TestSkeletonListsClassMembers covers the one structural case Go cannot show:
// a container whose members are nested inside its own source range. Go methods
// are siblings of the type they hang off, so only a language like Python
// exercises the containment rule the member lookup is built on.
//
// The tree-sitter registry is required for this, hence the cgo build tag: the
// regex heuristic registry records end_line == start_line and would produce no
// nesting to find.
func TestSkeletonListsClassMembers(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "shapes.py", `class Shape:
    """A shape."""

    def area(self):
        def rounded():
            return 1
        return rounded()

    def describe(self):
        return "shape"


def free_function():
    return 2
`)

	s := openTestStore(t)
	defer s.Close()

	idx := indexer.New(s, parser.NewRegistry(treesitter.NewPython()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	server := NewServer(repoRoot, repoRoot, repo.ID, s, idx, query.New(s, nil), io.Discard)

	data, _ := callToolJSON(t, server, ctx, "find_symbol", map[string]any{"query": "Shape", "detail": "skeleton"})
	shape := findRecord(t, recordsOf(t, data, "matches"), "Shape")

	rawMembers, ok := shape["members"].([]any)
	if !ok {
		t.Fatalf("skeleton of a class carries no members: %v", shape)
	}
	names := map[string]bool{}
	for _, m := range rawMembers {
		names[m.(map[string]any)["name"].(string)] = true
	}
	if !names["area"] || !names["describe"] {
		t.Fatalf("members = %v, want the class's methods", names)
	}
	// One level of structure: a function nested inside a method is a
	// grandchild, and a sibling after the class ends is outside the range.
	if names["rounded"] {
		t.Fatalf("members include a transitive descendant: %v", names)
	}
	if names["free_function"] {
		t.Fatalf("members include a symbol outside the container: %v", names)
	}
	if names["Shape"] {
		t.Fatalf("members include the container itself: %v", names)
	}

	// A skeleton describes shape only; the bodies of those methods must not
	// have come along.
	if _, hasSource := shape["source"]; hasSource {
		t.Fatalf("skeleton carried source: %v", shape)
	}
}

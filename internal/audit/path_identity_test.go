package audit

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

// TestLoadPreservesStoredPathIdentity pins that the harness reports a symbol
// and a call edge under the exact files.path bytes the store persisted. The
// path is a logical repository identity, not a native spelling, so a literal
// backslash is filename data: the slash sibling and the backslash sibling must
// stay two distinct files rather than collapsing into one spelling. The old
// filepath.ToSlash rewrite only differed on Windows, so this test guards
// against regression at runtime there (windows-latest in ci.yml) and is a
// no-op check on POSIX hosts.
func TestLoadPreservesStoredPathIdentity(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo, err := st.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if err := st.EnsureCanonicalRepositoryPaths(ctx, repo.ID, true); err != nil {
		t.Fatalf("EnsureCanonicalRepositoryPaths() error = %v", err)
	}
	scanID, _, err := st.BeginScan(ctx, repo.ID, "index")
	if err != nil {
		t.Fatalf("BeginScan() error = %v", err)
	}

	const slashPath, backslashPath = "pkg/x/y.go", `pkg/x\y.go`
	files := map[string]string{slashPath: "pkg/x/y.Run", backslashPath: `pkg/x\y.Run`}
	for path, qualified := range files {
		parsed := graph.ParsedFile{
			Language: "go",
			Symbols: []graph.Symbol{{
				Language: "go", Kind: "function", Name: "Run", QualifiedName: qualified,
				StableKey: "func:" + qualified, Range: graph.Position{StartLine: 1, EndLine: 5},
			}},
			Edges: []graph.Edge{{DstName: "Helper", Kind: "calls", Line: 3}},
		}
		if err := st.ReplaceFileGraph(ctx, repo.ID, scanID, path, "go", 1, 1, "hash-"+path, parsed); err != nil {
			t.Fatalf("ReplaceFileGraph(%q) error = %v", path, err)
		}
	}

	symbols, err := loadSymbols(ctx, st, repo.ID)
	if err != nil {
		t.Fatalf("loadSymbols() error = %v", err)
	}
	if len(symbols) != 2 {
		t.Fatalf("loadSymbols() returned %d symbols, want 2", len(symbols))
	}
	symbolFiles := map[string]string{}
	for _, sym := range symbols {
		symbolFiles[sym.QualifiedName] = sym.File
	}
	for path, qualified := range files {
		if got := symbolFiles[qualified]; got != path {
			t.Errorf("symbol %q File = %q, want exact stored path %q", qualified, got, path)
		}
	}

	edges, err := loadCallEdges(ctx, st, repo.ID, symbols)
	if err != nil {
		t.Fatalf("loadCallEdges() error = %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("loadCallEdges() returned %d edges, want 2", len(edges))
	}
	edgeFiles := map[string]string{}
	for _, edge := range edges {
		edgeFiles[edge.SrcQualifiedName] = edge.SrcFile
	}
	for path, qualified := range files {
		if got := edgeFiles[qualified]; got != path {
			t.Errorf("edge from %q SrcFile = %q, want exact stored path %q", qualified, got, path)
		}
	}
}

package watcher

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// A path-scoped watcher flush converges all files in its selected stale
// language, without broadening into unrelated languages or a watcher retry.
func TestWatcherPathScopedUpdateConvergesProfile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc helper() {}\n\nfunc main() { helper() }\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: root}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	// Rewind provenance to what a pre-036 database holds.
	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, false)
	if err != nil {
		t.Fatalf("BuildSQLiteDSN() error = %v", err)
	}
	raw, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := raw.Exec(`UPDATE files SET parser_profile = '', parser_call_edges = 0`); err != nil {
		t.Fatalf("rewind provenance: %v", err)
	}
	raw.Close()

	w := New(s, idx)
	if err := w.updateConverging(ctx, root, indexer.Options{RepoRoot: root, ScanKind: "watch", Paths: []string{"main.go"}}); err != nil {
		t.Fatalf("updateConverging() error = %v, want nil", err)
	}
	if got := w.Stats().ProfileConverges; got != 0 {
		t.Fatalf("ProfileConverges = %d, want no fallback convergence", got)
	}

	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	groups, err := s.FileParserProfileGroups(ctx, repo.ID)
	if err != nil {
		t.Fatalf("FileParserProfileGroups() error = %v", err)
	}
	if len(groups) != 1 || groups[0].Profile == "" {
		t.Fatalf("groups = %#v, want converged provenance", groups)
	}

	// Converged: the next path-scoped flush passes through untouched.
	if err := w.updateConverging(ctx, root, indexer.Options{RepoRoot: root, ScanKind: "watch", Paths: []string{"main.go"}}); err != nil {
		t.Fatalf("second updateConverging() error = %v", err)
	}
	if got := w.Stats().ProfileConverges; got != 0 {
		t.Fatalf("ProfileConverges = %d after convergence, want 0", got)
	}
}

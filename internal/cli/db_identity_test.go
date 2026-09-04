package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/store"
)

func TestV2RepoPathIgnoresLegacyDatabase(t *testing.T) {
	repoRoot := t.TempDir()
	legacy := filepath.Join(repoRoot, config.RepoArtifactsDir, "codegraph.sqlite")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical, err := store.CanonicalRepoPath(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	path, err := dbPathForRepo(config.Config{DBDir: config.RepoDBDir}, repoRoot, canonical)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(repoRoot, config.RepoArtifactsDir, store.RepoDatabaseFileName)
	if path != want {
		t.Fatalf("v2 path = %q, want %q", path, want)
	}
	if _, err := openIndexedRepoReadOnly(context.Background(), config.Config{DBDir: config.RepoDBDir}, repoRoot); err == nil {
		t.Fatal("legacy database treated as indexed v2 database")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy database removed: %v", err)
	}
}

func TestV2CustomDatabasePathIsVersioned(t *testing.T) {
	repoRoot := t.TempDir()
	canonical, err := store.CanonicalRepoPath(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := repoDBPathsForRepo(config.Config{DBDir: filepath.Join(t.TempDir(), "db")}, repoRoot, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) == "codegraph.sqlite" || filepath.Ext(paths[0]) != ".sqlite" {
		t.Fatalf("custom v2 path = %#v", paths)
	}
}

func TestV2RemovalLeavesLegacyArtifacts(t *testing.T) {
	repoRoot := t.TempDir()
	legacy := filepath.Join(repoRoot, config.RepoArtifactsDir, "codegraph.sqlite")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{legacy, legacy + "-wal", legacy + "-shm", legacy + "-journal"} {
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	canonical, err := store.CanonicalRepoPath(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := removeRepoDBFiles(config.Config{DBDir: config.RepoDBDir}, repoRoot, canonical); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{legacy, legacy + "-wal", legacy + "-shm", legacy + "-journal"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("legacy artifact removed: %s: %v", path, err)
		}
	}
}

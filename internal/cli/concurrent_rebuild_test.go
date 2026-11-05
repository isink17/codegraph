package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/store"
)

func TestRebuildRefusesBeforeRemovingActiveScanDatabase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cfg := config.Config{DBDir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc Original() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runIndex(ctx, cfg, io.Discard, "index", []string{root}, false); err != nil {
		t.Fatal(err)
	}
	canonical, err := store.CanonicalRepoPath(root)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := repoDBPathsForRepo(cfg, root, canonical)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release, err := s.AcquireRepositoryScan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	before, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	remove := repoDBRemove
	attempts := 0
	repoDBRemove = func(path string) error { attempts++; return remove(path) }
	defer func() { repoDBRemove = remove }()
	err = runIndex(ctx, cfg, io.Discard, "index", []string{root, "--rebuild"}, false)
	if !errors.Is(err, store.ErrScanActive) || attempts != 0 {
		t.Errorf("rebuild while owned: err=%v removal attempts=%d, want busy before any removal", err, attempts)
	}
	after, err := os.Stat(paths[0])
	if err != nil || !os.SameFile(before, after) {
		t.Errorf("active database was replaced: stat err=%v", err)
	}
	release()
	_ = s.Close()
	if err := runIndex(ctx, cfg, io.Discard, "index", []string{root, "--rebuild"}, false); err != nil {
		t.Fatalf("retry after release: %v", err)
	}
}

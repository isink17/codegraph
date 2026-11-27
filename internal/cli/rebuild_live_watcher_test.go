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

// A rebuild replaces the database file; a running watcher keeps the one it
// opened and would go on writing there. The rebuild must refuse before it
// removes anything, whether the watcher is still reconciling or watching, and
// proceed once the watcher has exited and left its lock files behind.
func TestRebuildRefusesWhileWatcherRuns(t *testing.T) {
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
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, canonical)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := s.OpenWatcherLock(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Close)
	before, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	remove := repoDBRemove
	attempts := 0
	repoDBRemove = func(path string) error { attempts++; return remove(path) }
	t.Cleanup(func() { repoDBRemove = remove })

	rebuild := func() error {
		return runIndex(ctx, cfg, io.Discard, "index", []string{root, "--rebuild"}, false)
	}
	if err := lock.Hold(); err != nil {
		t.Fatal(err)
	}
	if err := rebuild(); !errors.Is(err, store.ErrWatcherActive) || attempts != 0 {
		t.Fatalf("rebuild under a reconciling watcher: err=%v removal attempts=%d", err, attempts)
	}
	if err := lock.HoldLive(); err != nil {
		t.Fatal(err)
	}
	lock.Release()
	if err := rebuild(); !errors.Is(err, store.ErrWatcherActive) || attempts != 0 {
		t.Fatalf("rebuild under a watching watcher: err=%v removal attempts=%d", err, attempts)
	}
	after, err := os.Stat(paths[0])
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("database replaced under a live watcher: stat err=%v", err)
	}

	lock.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rebuild(); err != nil {
		t.Fatalf("rebuild after the watcher exited: %v", err)
	}
	if attempts == 0 {
		t.Fatal("rebuild after the watcher exited removed nothing")
	}
}

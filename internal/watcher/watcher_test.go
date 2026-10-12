package watcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/store"
)

func TestWatcherConfigPathUsesLogicalSeparators(t *testing.T) {
	if !isWatcherConfigPath(".codegraph/config.json") {
		t.Fatal("slash config path not recognized")
	}
	if isWatcherConfigPath(`.codegraph\config.json`) {
		t.Fatal("literal backslash config path recognized")
	}
}

func TestRelPathWithinRepoPreservesLogicalBackslash(t *testing.T) {
	if !isRelPathWithinRepo(`dir\file.go`) {
		t.Fatal("literal backslash logical path rejected")
	}
	if isRelPathWithinRepo("../file.go") || isRelPathWithinRepo("..") {
		t.Fatal("traversal path accepted")
	}
}

// Run must not return while the flush goroutine it started is still alive:
// the caller closes the store and indexer as soon as Run returns.
func TestRunStopsFlushLoopBeforeReturningError(t *testing.T) {
	ctx := context.Background() // never cancelled: only Run's own exit may stop the loop
	root := t.TempDir()
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	// An unmarked repository refuses dirty-queue writes, so the first indexable
	// event makes Run return an error while ctx is still live.
	runErr := make(chan error, 1)
	go func() { runErr <- New(s, nil).Run(ctx, root, repo.ID, time.Hour) }()
	file := filepath.Join(root, "main.go")
	var got error
	for got == nil {
		if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		select {
		case got = <-runErr:
		case <-time.After(20 * time.Millisecond): // rewrite until the watch is armed
		}
	}
	if !errors.Is(got, store.ErrRepositoryPathFormatRebuild) {
		t.Fatalf("Run err = %v, want ErrRepositoryPathFormatRebuild", got)
	}
	buf := make([]byte, 1<<20)
	if stacks := string(buf[:runtime.Stack(buf, true)]); strings.Contains(stacks, "watcher.(*Watcher).run.func") {
		t.Fatalf("flush goroutine outlived Run:\n%s", stacks)
	}
}

func openReadyTestStore(t *testing.T, root string) (*store.Store, int64) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return s, repo.ID
}

func TestRunWithReadyCallsReadyOnceAfterSetup(t *testing.T) {
	root := t.TempDir()
	s, repoID := openReadyTestStore(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := New(s, nil).RunWithReady(ctx, root, repoID, time.Hour, func() {
		calls++
		cancel()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunWithReady err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("ready calls = %d, want 1", calls)
	}
}

func TestRunWithReadySkipsReadyOnSetupError(t *testing.T) {
	root := t.TempDir()
	s, repoID := openReadyTestStore(t, root)
	missing := filepath.Join(root, "missing")
	calls := 0
	err := New(s, nil).RunWithReady(context.Background(), missing, repoID, time.Hour, func() { calls++ })
	if err == nil {
		t.Fatal("RunWithReady err = nil, want setup error")
	}
	if calls != 0 {
		t.Fatalf("ready calls = %d, want 0", calls)
	}
}

func TestRunWithReadySkipsReadyWhenCanceledBeforeReady(t *testing.T) {
	root := t.TempDir()
	s, repoID := openReadyTestStore(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := New(s, nil).RunWithReady(ctx, root, repoID, time.Hour, func() { calls++ })
	if err == nil {
		t.Fatal("RunWithReady err = nil, want cancellation error")
	}
	if calls != 0 {
		t.Fatalf("ready calls = %d, want 0", calls)
	}
}

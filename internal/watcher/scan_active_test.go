package watcher

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// waitTransient waits until the watcher records kind as its latest retryable
// failure, failing if it stops instead.
func (r *liveRepo) waitTransient(t *testing.T, kind string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for r.w.Stats().LastTransient != kind {
		select {
		case err := <-r.done:
			r.done <- err
			t.Fatalf("waiting for transient %s: watcher stopped: %v", kind, err)
		case <-deadline:
			t.Fatalf("transient %s never recorded, stats %+v", kind, r.w.Stats())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// An index or update running on the repository holds its writer lock; a
// flush that meets it is refused with ErrScanActive, which the lock's
// contract calls retryable. The watcher must retry, not stop.
func TestWatcherRetriesFlushRefusedByActiveScan(t *testing.T) {
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	r.waitState(t, StateWatching)
	other, err := store.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	release, err := other.AcquireRepositoryScan(context.Background(), r.root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.root, "b.go"), goFile)
	r.waitTransient(t, "scan_active")
	release()
	r.waitLive(t, "file queued while the scan ran", func(p []string) bool {
		return slices.Contains(p, "b.go")
	})
	r.waitState(t, StateWatching)
}

// A degraded watcher whose flush meets an external scan keeps its
// reconcile_pending marker and its liveness lock, so the scan does not make
// the graph read as consistent while the watcher still misses events.
func TestDegradedWatcherKeepsPendingWhenFlushMeetsActiveScan(t *testing.T) {
	var failing atomic.Bool
	addWatchHook = func(path string) error {
		if failing.Load() && strings.Contains(path, "newdir") {
			return fmt.Errorf("add watch %s: %w", path, syscall.ENOSPC)
		}
		return nil
	}
	var fw atomic.Pointer[fsnotify.Watcher]
	watcherCreatedHook = func(w *fsnotify.Watcher) { fw.CompareAndSwap(nil, w) }
	t.Cleanup(func() { addWatchHook, watcherCreatedHook = nil, nil })
	// No re-registration during the test: it would close the captured watcher.
	base, maxDelay := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = time.Hour, time.Hour
	t.Cleanup(func() { retryBaseDelay, retryMaxDelay = base, maxDelay })
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	failing.Store(true)
	newdir := filepath.Join(r.root, "newdir")
	writeFile(t, filepath.Join(newdir, "a.go"), goFile)
	r.waitState(t, StateRecovering)

	other, err := store.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	release, err := other.AcquireRepositoryScan(context.Background(), r.root)
	if err != nil {
		t.Fatal(err)
	}
	// Windows reports a file created in an unwatched directory as a write to
	// that directory; inotify and kqueue report nothing. Deliver it here so
	// the flush meets the held lock on every OS.
	fw.Load().Events <- fsnotify.Event{Name: newdir, Op: fsnotify.Write}
	r.waitTransient(t, "scan_active")
	release()
	if _, err := indexer.New(other, parser.NewRegistry(goparser.New()), nil).Update(context.Background(), indexer.Options{RepoRoot: r.root}); err != nil {
		t.Fatal(err)
	}
	f, err := other.FreshnessStatus(context.Background(), r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Verdict == "consistent_with_last_scan" || !slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("degraded watcher read as consistent after an external scan: verdict %s reasons %v", f.Verdict, f.Reasons)
	}
}

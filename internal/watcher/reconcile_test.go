package watcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

type liveRepo struct {
	root   string
	s      *store.Store
	repoID int64
	w      *Watcher
	done   chan error
	cancel context.CancelFunc
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// startLiveRepo indexes files under a fresh root and runs a real watcher on it
// until the test ends.
func startLiveRepo(t *testing.T, files map[string]string) *liveRepo {
	t.Helper()
	return startLiveRepoDebounce(t, files, 20*time.Millisecond)
}

func startLiveRepoDebounce(t *testing.T, files map[string]string, debounce time.Duration) *liveRepo {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		writeFile(t, filepath.Join(root, rel), body)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	ctx := context.Background()
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	r := &liveRepo{root: root, s: s, repoID: repo.ID}
	t.Cleanup(func() { _ = s.Close() })
	r.start(t, idx, debounce)
	return r
}

func (r *liveRepo) start(t *testing.T, idx *indexer.Indexer, debounce time.Duration) {
	t.Helper()
	r.w = New(r.s, idx)
	r.done = make(chan error, 1)
	runCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	ready := make(chan struct{})
	w, done := r.w, r.done
	go func() { done <- w.RunWithReady(runCtx, r.root, r.repoID, debounce, func() { close(ready) }) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-ready:
	case err := <-done:
		done <- err
		t.Fatalf("watcher stopped before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("watcher not ready")
	}
}

// waitLive polls the indexed paths until cond holds, failing on timeout or
// when the watcher stops.
func (r *liveRepo) waitLive(t *testing.T, what string, cond func([]string) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		paths, err := r.s.LiveFilePaths(context.Background(), r.repoID)
		if err != nil {
			t.Fatal(err)
		}
		if cond(paths) {
			return
		}
		select {
		case err := <-r.done:
			r.done <- err
			t.Fatalf("%s: watcher stopped: %v (indexed %v)", what, err, paths)
		case <-deadline:
			t.Fatalf("%s: timed out, indexed %v, stats %+v", what, paths, r.w.Stats())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

const goFile = "package p\n\nfunc F() {}\n"

// A directory moved into the repository arrives as one create event; the
// files already inside it produce none of their own.
func TestWatcherIndexesFilesOfMovedInDirectory(t *testing.T) {
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	staging := filepath.Join(t.TempDir(), "pkg")
	writeFile(t, filepath.Join(staging, "a.go"), goFile)
	writeFile(t, filepath.Join(staging, "sub", "b.go"), goFile)
	if err := os.Rename(staging, filepath.Join(r.root, "pkg")); err != nil {
		t.Skipf("cannot rename across temp dirs: %v", err)
	}
	r.waitLive(t, "moved-in files", func(p []string) bool {
		return slices.Contains(p, "pkg/a.go") && slices.Contains(p, "pkg/sub/b.go")
	})
}

// A directory moved out of the repository arrives as one rename event for the
// directory; its files must leave the graph too.
func TestWatcherRetiresFilesOfMovedOutDirectory(t *testing.T) {
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n", "pkg/a.go": goFile, "pkg/sub/b.go": goFile})
	if err := os.Rename(filepath.Join(r.root, "pkg"), filepath.Join(t.TempDir(), "gone")); err != nil {
		t.Skipf("cannot rename across temp dirs: %v", err)
	}
	r.waitLive(t, "moved-out files", func(p []string) bool {
		return slices.Equal(p, []string{"main.go"})
	})
}

func TestWatcherRetiresFilesOfRemovedDirectory(t *testing.T) {
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n", "pkg/a.go": goFile, "pkg/sub/b.go": goFile})
	if err := os.RemoveAll(filepath.Join(r.root, "pkg")); err != nil {
		t.Fatal(err)
	}
	r.waitLive(t, "removed files", func(p []string) bool {
		return slices.Equal(p, []string{"main.go"})
	})
}

// Dropped events cannot be replayed, so an overflow re-registers the watch
// tree and reconciles the repository through the incremental update instead
// of stopping the watcher.
func TestWatcherReconcilesAfterEventOverflow(t *testing.T) {
	type injected struct {
		fsw  *fsnotify.Watcher
		errs chan error
	}
	inject := make(chan injected, 4)
	// Errors is swapped for a test-owned channel: sending on fsnotify's own
	// channel races with its shutdown.
	watcherCreatedHook = func(w *fsnotify.Watcher) {
		errs := make(chan error, 1)
		w.Errors = errs
		inject <- injected{w, errs}
	}
	t.Cleanup(func() { watcherCreatedHook = nil })
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	first := <-inject
	// Hide a new directory and a file from the watcher, standing in for the
	// dropped events.
	if err := first.fsw.Remove(r.root); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.root, "newdir", "m.go"), goFile)
	if err := first.fsw.Add(r.root); err != nil {
		t.Fatal(err)
	}
	first.errs <- fsnotify.ErrEventOverflow
	deadline := time.After(5 * time.Second)
	for {
		f, err := r.s.FreshnessStatus(context.Background(), r.repoID)
		if err != nil {
			t.Fatal(err)
		}
		if f.LastCompletedScan != nil && f.LastCompletedScan.Kind == "watch_config" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no forced full scan: %+v stats %+v", f.LastCompletedScan, r.w.Stats())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if got := r.w.Stats().Overflows; got != 1 {
		t.Fatalf("Overflows = %d, want 1", got)
	}
	r.waitLive(t, "reconciled file", func(p []string) bool {
		return slices.Contains(p, "newdir/m.go")
	})
	// The directory created during the dropped events is watched again.
	writeFile(t, filepath.Join(r.root, "newdir", "x.go"), goFile)
	r.waitLive(t, "file in re-registered directory", func(p []string) bool {
		return slices.Contains(p, "newdir/x.go")
	})
}

// A directory removed between its create event and its registration is not
// an error: nothing is left in it to watch.
func TestWatcherSurvivesDirectoryVanishingBeforeRegistration(t *testing.T) {
	createdDirHook = func(path string) {
		if filepath.Base(path) == "tmpdir" {
			_ = os.RemoveAll(path)
		}
	}
	t.Cleanup(func() { createdDirHook = nil })
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	if err := os.Mkdir(filepath.Join(r.root, "tmpdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.root, "after.go"), goFile)
	r.waitLive(t, "file after vanished dir", func(p []string) bool {
		return slices.Contains(p, "after.go")
	})
	select {
	case err := <-r.done:
		r.done <- err
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("watcher stopped: %v", err)
		}
	default:
	}
}

// Events are persisted to the dirty queue before any flush, so a watcher
// stopped before its debounce fires leaves them for the next one.
func TestWatcherStopBeforeFlushResumesOnRestart(t *testing.T) {
	r := startLiveRepoDebounce(t, map[string]string{"main.go": "package main\n"}, time.Hour)
	writeFile(t, filepath.Join(r.root, "late.go"), goFile)
	deadline := time.After(5 * time.Second)
	for {
		f, err := r.s.FreshnessStatus(context.Background(), r.repoID)
		if err != nil {
			t.Fatal(err)
		}
		if f.DirtyQueue.Queued > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("event never queued")
		case <-time.After(20 * time.Millisecond):
		}
	}
	r.cancel()
	err := <-r.done
	r.done <- err
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stop: %v", err)
	}
	r.start(t, indexer.New(r.s, parser.NewRegistry(goparser.New()), nil), time.Hour)
	r.waitLive(t, "resumed file", func(p []string) bool { return slices.Contains(p, "late.go") })
}

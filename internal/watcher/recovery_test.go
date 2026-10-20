package watcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/indexer"
)

// shortRetries shrinks the retry backoff for one test. Call it before the
// watcher starts: the delays are read by its goroutines.
func shortRetries(t *testing.T) {
	t.Helper()
	base, maxDelay := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = 10*time.Millisecond, 80*time.Millisecond
	t.Cleanup(func() { retryBaseDelay, retryMaxDelay = base, maxDelay })
}

func (r *liveRepo) waitState(t *testing.T, want string) WatchStats {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		st := r.w.Stats()
		if st.State == want {
			return st
		}
		select {
		case err := <-r.done:
			r.done <- err
			t.Fatalf("waiting for %s: watcher stopped: %v", want, err)
		case <-deadline:
			t.Fatalf("state %q never reached, stats %+v", want, st)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (r *liveRepo) freshness(t *testing.T) graph.Freshness {
	t.Helper()
	f, err := r.s.FreshnessStatus(context.Background(), r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (r *liveRepo) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	err := <-r.done
	r.done <- err
	return err
}

func (r *liveRepo) hasSymbol(t *testing.T, name string) bool {
	t.Helper()
	syms, err := r.s.FindSymbolExact(context.Background(), r.repoID, name, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(syms) > 0
}

// The OS running out of watches while a new directory registers degrades the
// watcher instead of stopping it; once watches are available again it
// re-registers the tree and reconciles what it missed.
func TestWatcherRecoversFromRegistrationFailure(t *testing.T) {
	var failing atomic.Bool
	var attempts atomic.Int64
	addWatchHook = func(path string) error {
		if failing.Load() && strings.Contains(path, "newdir") {
			attempts.Add(1)
			return fmt.Errorf("add watch %s: %w", path, syscall.ENOSPC)
		}
		return nil
	}
	t.Cleanup(func() { addWatchHook = nil })
	shortRetries(t)
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	if st := r.w.Stats(); st.State != StateWatching || st.Reconciles != 1 {
		t.Fatalf("after ready: %+v, want watching after one reconciliation", st)
	}
	failing.Store(true)
	writeFile(t, filepath.Join(r.root, "newdir", "a.go"), goFile)
	st := r.waitState(t, StateRecovering)
	if st.LastTransient != "enospc" {
		t.Fatalf("LastTransient = %q, want enospc", st.LastTransient)
	}
	// Retries back off: at most one attempt per capped delay, not a tight loop.
	time.Sleep(400 * time.Millisecond)
	if n := attempts.Load(); n > 12 {
		t.Fatalf("%d registration attempts in 400ms with an 80ms cap", n)
	}
	if st := r.w.Stats(); st.State != StateRecovering {
		t.Fatalf("state %q while registration still fails", st.State)
	}
	failing.Store(false)
	r.waitLive(t, "file of directory registered during the failure", func(p []string) bool {
		return slices.Contains(p, "newdir/a.go")
	})
	st = r.waitState(t, StateWatching)
	if st.Recoveries < 1 || st.Reconciles < 2 {
		t.Fatalf("stats %+v, want a recovery and a second reconciliation", st)
	}
	writeFile(t, filepath.Join(r.root, "newdir", "b.go"), goFile)
	r.waitLive(t, "file in re-registered directory", func(p []string) bool {
		return slices.Contains(p, "newdir/b.go")
	})
}

// A failed flush keeps its paths in the persisted dirty queue, reports the
// watcher recovering and the graph stale, and retries until it succeeds.
func TestWatcherRetriesTransientFlushFailure(t *testing.T) {
	for _, tc := range []struct {
		kind string
		err  error
	}{
		{"emfile", fmt.Errorf("open: %w", syscall.EMFILE)},
		{"sqlite_busy", errors.New("database is locked (5) (SQLITE_BUSY)")},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			var failing atomic.Bool
			updateHook = func(indexer.Options) error {
				if failing.Load() {
					return tc.err
				}
				return nil
			}
			t.Cleanup(func() { updateHook = nil })
			shortRetries(t)
			r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
			failing.Store(true)
			writeFile(t, filepath.Join(r.root, "late.go"), goFile)
			st := r.waitState(t, StateRecovering)
			if st.LastTransient != tc.kind {
				t.Fatalf("LastTransient = %q, want %s", st.LastTransient, tc.kind)
			}
			f := r.freshness(t)
			if f.DirtyQueue.Queued+f.DirtyQueue.InFlight == 0 || f.Verdict == graph.FreshnessVerdictConsistentWithLastScan {
				t.Fatalf("while failing: queue %+v verdict %s, want the path kept and no consistent verdict", f.DirtyQueue, f.Verdict)
			}
			failing.Store(false)
			r.waitLive(t, "retried file", func(p []string) bool { return slices.Contains(p, "late.go") })
			r.waitState(t, StateWatching)
			f = r.freshness(t)
			if f.DirtyQueue.Queued+f.DirtyQueue.InFlight != 0 || f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
				t.Fatalf("after recovery: queue %+v verdict %s reasons %v", f.DirtyQueue, f.Verdict, f.Reasons)
			}
		})
	}
}

// Changes made while no watcher ran raise no events; the start-up
// reconciliation finds them before the watcher reports ready.
func TestWatcherReconcilesOfflineChanges(t *testing.T) {
	r := startLiveRepo(t, map[string]string{
		"main.go":      "package main\n",
		"edit.go":      goFile,
		"gone.go":      goFile,
		"old/c.go":     goFile,
		"skip/s.go":    goFile,
		"keep/deep.go": goFile,
	})
	if err := r.stop(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop: %v", err)
	}
	writeFile(t, filepath.Join(r.root, "edit.go"), "package p\n\nfunc Edited() {}\n")
	writeFile(t, filepath.Join(r.root, "new.go"), goFile)
	if err := os.Remove(filepath.Join(r.root, "gone.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(r.root, "old"), filepath.Join(r.root, "renamed")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.root, ".codegraph", "config.json"), `{"exclude":["skip/**"]}`)

	r.start(t, newTestIndexer(r.s), 20*time.Millisecond)
	// Ready waits for the reconciliation, so no polling is needed.
	paths, err := r.s.LiveFilePaths(context.Background(), r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"edit.go", "keep/deep.go", "main.go", "new.go", "renamed/c.go"}
	if !slices.Equal(paths, want) {
		t.Fatalf("live paths %v, want %v", paths, want)
	}
	if !r.hasSymbol(t, "Edited") {
		t.Fatal("offline edit not reconciled")
	}
	if st := r.w.Stats(); st.State != StateWatching || st.Reconciles != 1 {
		t.Fatalf("stats %+v", st)
	}
	if f := r.freshness(t); f.Verdict != graph.FreshnessVerdictConsistentWithLastScan || f.LastCompletedScan.Scope != "full" {
		t.Fatalf("verdict %s scan %+v", f.Verdict, f.LastCompletedScan)
	}
}

// A reconciliation interrupted by shutdown leaves no consistent verdict, and
// the next start reconciles again.
func TestWatcherInterruptedReconciliationIsRedone(t *testing.T) {
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n", "edit.go": goFile})
	if err := r.stop(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop: %v", err)
	}
	writeFile(t, filepath.Join(r.root, "edit.go"), "package p\n\nfunc Edited() {}\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updateHook = func(indexer.Options) error {
		cancel()
		return nil
	}
	t.Cleanup(func() { updateHook = nil })
	readyCalled := false
	err := New(r.s, newTestIndexer(r.s)).RunWithReady(ctx, r.root, r.repoID, time.Hour, func() { readyCalled = true })
	updateHook = nil
	if !errors.Is(err, context.Canceled) || readyCalled {
		t.Fatalf("interrupted run: err %v ready %v", err, readyCalled)
	}
	if f := r.freshness(t); f.Verdict == graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("interrupted reconciliation reads %s (reasons %v)", f.Verdict, f.Reasons)
	}

	r.start(t, newTestIndexer(r.s), time.Hour)
	if !r.hasSymbol(t, "Edited") {
		t.Fatal("restart did not reconcile")
	}
	if f := r.freshness(t); f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("after restart: %s (reasons %v)", f.Verdict, f.Reasons)
	}
}

// A configuration that stays malformed is an error after bounded retries,
// not silently the old configuration.
func TestWatcherStopsOnUnreadableConfig(t *testing.T) {
	shortRetries(t)
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	writeFile(t, filepath.Join(r.root, ".codegraph", "config.json"), `{"exclude":`)
	select {
	case err := <-r.done:
		r.done <- err
		if err == nil || !strings.Contains(err.Error(), "reload repository config") {
			t.Fatalf("err = %v, want a config reload error", err)
		}
		if st := r.w.Stats(); st.TransientErrors < maxConfigRetries || st.LastTransient != "config_invalid" {
			t.Fatalf("stopped without retrying: %+v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("watcher kept running on an unreadable config: %+v", r.w.Stats())
	}
}

// A config file caught mid-write is retried; once it parses, its rules apply.
func TestWatcherRetriesTruncatedConfig(t *testing.T) {
	shortRetries(t)
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n", "skip/s.go": goFile})
	writeFile(t, filepath.Join(r.root, ".codegraph", "config.json"), `{"exclude":`)
	st := r.waitState(t, StateRecovering)
	if st.LastTransient != "config_invalid" {
		t.Fatalf("LastTransient = %q, want config_invalid", st.LastTransient)
	}
	writeFile(t, filepath.Join(r.root, ".codegraph", "config.json"), `{"exclude":["skip/**"]}`)
	r.waitLive(t, "excluded file retired", func(p []string) bool { return slices.Equal(p, []string{"main.go"}) })
	r.waitState(t, StateWatching)
}

// An event whose dirty-queue write fails stays covered: the reconciliation
// pass that follows finds the change.
func TestWatcherReconcilesAfterQueueWriteFailure(t *testing.T) {
	var failures atomic.Int64
	failures.Store(-1)
	queueHook = func() error {
		if failures.Add(-1) >= 0 {
			return errors.New("database is locked (5) (SQLITE_BUSY)")
		}
		return nil
	}
	t.Cleanup(func() { queueHook = nil })
	shortRetries(t)
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	failures.Store(3) // every attempt of the next write
	writeFile(t, filepath.Join(r.root, "lost.go"), goFile)
	r.waitLive(t, "file whose queue write failed", func(p []string) bool { return slices.Contains(p, "lost.go") })
	st := r.waitState(t, StateWatching)
	if st.QueueErrors != 1 || st.LastTransient != "sqlite_busy" || st.Reconciles < 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestHealthReconciledIgnoresPassOlderThanFailure(t *testing.T) {
	var h health
	_ = h.reset(nil)
	h.setRegistered()
	since := h.failureCount()
	h.degrade("sqlite_busy", false)
	h.reconciled(since)
	if state, _ := h.snapshot(); state != StateRecovering {
		t.Fatalf("a pass begun before the failure set %s", state)
	}
	h.reconciled(h.failureCount())
	if state, _ := h.snapshot(); state != StateWatching {
		t.Fatalf("a pass begun after the failure left %s", state)
	}
}

// A tree that could not be fully registered at start is not reported
// watching, even though the start-up pass completed.
func TestWatcherStartsRecoveringWhenRegistrationFails(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	addWatchHook = func(path string) error {
		if failing.Load() && filepath.Base(path) == "sub" {
			return fmt.Errorf("add watch: %w", syscall.EMFILE)
		}
		return nil
	}
	t.Cleanup(func() { addWatchHook = nil })
	shortRetries(t)
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n", "sub/a.go": goFile})
	if st := r.w.Stats(); st.State != StateRecovering || st.LastTransient != "emfile" {
		t.Fatalf("after ready: %+v, want recovering", st)
	}
	failing.Store(false)
	r.waitState(t, StateWatching)
	writeFile(t, filepath.Join(r.root, "sub", "b.go"), goFile)
	r.waitLive(t, "file in directory registered by recovery", func(p []string) bool {
		return slices.Contains(p, "sub/b.go")
	})
}

// A failed start-up pass is retried without waiting for an event.
func TestWatcherRetriesFailedStartupReconciliation(t *testing.T) {
	var calls atomic.Int64
	updateHook = func(indexer.Options) error {
		if calls.Add(1) == 1 {
			return fmt.Errorf("walk: %w", syscall.ENFILE)
		}
		return nil
	}
	t.Cleanup(func() { updateHook = nil })
	shortRetries(t)
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	st := r.waitState(t, StateWatching)
	if st.Reconciles != 1 || st.TransientErrors != 1 || st.LastTransient != "enfile" {
		t.Fatalf("stats %+v", st)
	}
	if f := r.freshness(t); f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("verdict %s reasons %v", f.Verdict, f.Reasons)
	}
}

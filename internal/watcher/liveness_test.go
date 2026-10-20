package watcher

import (
	"context"
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
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// externalFreshness reads freshness through another store on the database,
// as another process would.
func (r *liveRepo) externalFreshness(t *testing.T) graph.Freshness {
	t.Helper()
	reader, err := store.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	f, err := reader.FreshnessStatus(context.Background(), r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (r *liveRepo) waitLiveWatcher(t *testing.T, want string) graph.Freshness {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f := r.externalFreshness(t)
		if f.LiveWatcher == want {
			return f
		}
		if time.Now().After(deadline) {
			t.Fatalf("live_watcher %s never became %s (stats %+v)", f.LiveWatcher, want, r.w.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Other processes see a watcher's whole life: watching once it reconciled,
// unverified while it recovers from a failure, watching again after an
// external full scan and after recovery, and nothing once it stops; a
// restart is seen again.
func TestWatcherLivenessLifecycle(t *testing.T) {
	var failing atomic.Bool
	addWatchHook = func(path string) error {
		if failing.Load() && strings.Contains(path, "newdir") {
			return fmt.Errorf("add watch %s: %w", path, syscall.ENOSPC)
		}
		return nil
	}
	t.Cleanup(func() { addWatchHook = nil })
	base, maxDelay := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = 50*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { retryBaseDelay, retryMaxDelay = base, maxDelay })
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	r.waitState(t, StateWatching)
	if f := r.waitLiveWatcher(t, store.LiveWatcherWatching); f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("watching: verdict %s reasons %v", f.Verdict, f.Reasons)
	}

	// An external full scan while it watches changes nothing about it.
	other, err := store.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if _, err := indexer.New(other, parser.NewRegistry(goparser.New()), nil).Update(context.Background(), indexer.Options{RepoRoot: r.root}); err != nil {
		t.Fatal(err)
	}
	if f := r.externalFreshness(t); f.LiveWatcher != store.LiveWatcherWatching || f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("after external scan: %s %s %v", f.LiveWatcher, f.Verdict, f.Reasons)
	}

	failing.Store(true)
	writeFile(t, filepath.Join(r.root, "newdir", "a.go"), goFile)
	r.waitState(t, StateRecovering)
	if f := r.waitLiveWatcher(t, store.LiveWatcherUnverified); !slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("recovering: %v", f.Reasons)
	}
	failing.Store(false)
	r.waitState(t, StateWatching)
	if f := r.waitLiveWatcher(t, store.LiveWatcherWatching); slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("recovered: %v", f.Reasons)
	}

	if err := r.stop(t); err != nil && err != context.Canceled {
		t.Fatal(err)
	}
	if f := r.externalFreshness(t); f.LiveWatcher != store.LiveWatcherNone {
		t.Fatalf("stopped: %s", f.LiveWatcher)
	}
	r.start(t, indexer.New(r.s, parser.NewRegistry(goparser.New()), nil), 20*time.Millisecond)
	r.waitState(t, StateWatching)
	r.waitLiveWatcher(t, store.LiveWatcherWatching)
}

// A second watcher of one repository runs and flushes without the live lock
// the first holds; once the first stops, other processes still see it, as
// unverified with reconcile_pending, never as no watcher.
func TestWatcherLivenessSecondWatcherAfterFirstStops(t *testing.T) {
	first := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	first.waitState(t, StateWatching)
	first.waitLiveWatcher(t, store.LiveWatcherWatching)
	s, err := store.Open(first.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	second := &liveRepo{root: first.root, dbPath: first.dbPath, s: s, repoID: first.repoID}
	second.start(t, indexer.New(s, parser.NewRegistry(goparser.New()), nil), 20*time.Millisecond)
	second.waitState(t, StateWatching)

	if err := first.stop(t); err != nil && err != context.Canceled {
		t.Fatal(err)
	}
	f := second.externalFreshness(t)
	if f.LiveWatcher != store.LiveWatcherUnverified || !slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("first stopped, second runs: live_watcher %s reasons %v", f.LiveWatcher, f.Reasons)
	}
	writeFile(t, filepath.Join(first.root, "added.go"), goFile)
	second.waitLive(t, "second flushes", func(paths []string) bool { return slices.Contains(paths, "added.go") })

	if err := second.stop(t); err != nil && err != context.Canceled {
		t.Fatal(err)
	}
	if f := second.externalFreshness(t); f.LiveWatcher != store.LiveWatcherNone {
		t.Fatalf("both stopped: %s", f.LiveWatcher)
	}
}

// Two repositories in one database each report only their own watcher.
func TestWatcherLivenessIsPerRepository(t *testing.T) {
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	r.waitState(t, StateWatching)
	otherRoot := t.TempDir()
	writeFile(t, filepath.Join(otherRoot, "x.go"), goFile)
	if _, err := indexer.New(r.s, parser.NewRegistry(goparser.New()), nil).Index(context.Background(), indexer.Options{RepoRoot: otherRoot}); err != nil {
		t.Fatal(err)
	}
	repo, err := r.s.UpsertRepo(context.Background(), otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	r.waitLiveWatcher(t, store.LiveWatcherWatching)
	f, err := r.s.FreshnessStatus(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.LiveWatcher != store.LiveWatcherNone || f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("unwatched repository: %s %s %v", f.LiveWatcher, f.Verdict, f.Reasons)
	}
}

// Cancelling a watcher during its start-up pass leaves no live claim, only
// the persisted marker.
func TestWatcherLivenessCancelledDuringReconciliation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	updateHook = func(indexer.Options) error {
		close(entered)
		<-release
		return context.Canceled
	}
	t.Cleanup(func() { updateHook = nil })
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(context.Background(), indexer.Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	r := &liveRepo{root: root, dbPath: dbPath, s: s, repoID: repo.ID, w: New(s, idx)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.w.Run(ctx, root, repo.ID, 20*time.Millisecond) }()
	<-entered
	f := r.externalFreshness(t)
	cancel()
	close(release)
	if err := <-done; err == nil {
		t.Fatal("cancelled watcher returned nil")
	}
	if f.LiveWatcher != store.LiveWatcherUnverified || !slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("reconciling: %s %v", f.LiveWatcher, f.Reasons)
	}
	if f := r.externalFreshness(t); f.LiveWatcher != store.LiveWatcherNone || !slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("cancelled: %s %v", f.LiveWatcher, f.Reasons)
	}
}

// While the config is malformed no flush runs: events are queued and wait
// for a valid config, which indexes them under its rules.
func TestWatcherConfigPauseHoldsFlushes(t *testing.T) {
	base, maxDelay := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = 3*time.Second, 3*time.Second
	t.Cleanup(func() { retryBaseDelay, retryMaxDelay = base, maxDelay })
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n", ".codegraph/config.json": "{}"})
	r.waitState(t, StateWatching)
	cfg := filepath.Join(r.root, ".codegraph", "config.json")
	writeFile(t, cfg, `{"exclude":`)
	if st := r.waitState(t, StateRecovering); st.LastTransient != "config_invalid" {
		t.Fatalf("LastTransient %q", st.LastTransient)
	}
	before := r.w.Stats()
	writeFile(t, filepath.Join(r.root, "a.go"), goFile)
	writeFile(t, filepath.Join(r.root, "b.go"), goFile)
	deadline := time.Now().Add(time.Second)
	for r.w.Stats().FlushSignals == before.FlushSignals && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // ten debounce windows
	if st := r.w.Stats(); st.FlushSignals == before.FlushSignals || st.FlushRuns != before.FlushRuns || st.FlushErrors != before.FlushErrors {
		t.Fatalf("flushed while paused: before %+v after %+v", before, st)
	}
	writeFile(t, cfg, `{}`)
	r.waitLive(t, "files queued during the pause", func(p []string) bool { return slices.Equal(p, []string{"a.go", "b.go", "main.go"}) })
	r.waitState(t, StateWatching)
}

// A valid config read from an event re-registers the whole tree, which is
// what a pending recovery attempt would do: the watcher reconciles without
// waiting for it, and the attempt never runs.
func TestWatcherValidConfigEventSupersedesRecovery(t *testing.T) {
	var failing atomic.Bool
	addWatchHook = func(path string) error {
		if failing.Load() && strings.Contains(path, "newdir") {
			return fmt.Errorf("add watch %s: %w", path, syscall.ENOSPC)
		}
		return nil
	}
	t.Cleanup(func() { addWatchHook = nil })
	base, maxDelay := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = 1500*time.Millisecond, 1500*time.Millisecond
	t.Cleanup(func() { retryBaseDelay, retryMaxDelay = base, maxDelay })
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n", ".codegraph/config.json": "{}"})
	r.waitState(t, StateWatching)
	failing.Store(true)
	writeFile(t, filepath.Join(r.root, "newdir", "a.go"), goFile)
	r.waitState(t, StateRecovering)
	failing.Store(false)
	start := time.Now()
	writeFile(t, filepath.Join(r.root, ".codegraph", "config.json"), `{"exclude":[]}`)
	r.waitState(t, StateWatching)
	if took := time.Since(start); took >= retryBaseDelay {
		t.Fatalf("watching only after %v, the recovery delay", took)
	}
	r.waitLive(t, "file of the directory registration missed", func(p []string) bool { return slices.Contains(p, "newdir/a.go") })
	time.Sleep(retryBaseDelay + 300*time.Millisecond)
	if st := r.w.Stats(); st.Recoveries != 0 || st.State != StateWatching {
		t.Fatalf("superseded recovery attempt ran: %+v", st)
	}
}

// A config caught mid-write by the start-up pass pauses the watcher like a
// later flush would, instead of stopping it before it is ready.
func TestWatcherStartupFlushRetriesMalformedConfig(t *testing.T) {
	shortRetries(t)
	var cfgPath string
	var calls atomic.Int64
	updateHook = func(indexer.Options) error {
		if calls.Add(1) == 1 {
			return os.WriteFile(cfgPath, []byte(`{"exclude":`), 0o644)
		}
		return nil
	}
	t.Cleanup(func() { updateHook = nil })
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(root, ".codegraph", "config.json")
	writeFile(t, cfgPath, `{}`)
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeFile(t, filepath.Join(root, "skip", "s.go"), goFile)
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(context.Background(), indexer.Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	r := &liveRepo{root: root, dbPath: dbPath, s: s, repoID: repo.ID}
	r.start(t, idx, 20*time.Millisecond)
	if st := r.w.Stats(); st.State == StateWatching || calls.Load() == 0 {
		t.Fatalf("after ready: %+v", st)
	}
	writeFile(t, cfgPath, `{"exclude":["skip/**"]}`)
	r.waitLive(t, "excluded file retired", func(p []string) bool { return slices.Equal(p, []string{"main.go"}) })
	r.waitState(t, StateWatching)
}

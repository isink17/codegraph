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

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// A flush that is first to read a config caught mid-write recovers like an
// event that read it: the watcher waits for a valid config instead of stopping.
func TestWatcherRetriesMalformedConfigSeenByFlush(t *testing.T) {
	shortRetries(t)
	var breakConfig atomic.Bool
	var cfgPath string
	updateHook = func(indexer.Options) error {
		if breakConfig.CompareAndSwap(true, false) {
			if err := os.WriteFile(cfgPath, []byte(`{"exclude":`), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	t.Cleanup(func() { updateHook = nil })
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n", "skip/s.go": goFile})
	cfgPath = filepath.Join(r.root, ".codegraph", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, StateWatching)
	breakConfig.Store(true)
	writeFile(t, filepath.Join(r.root, "new.go"), goFile)
	st := r.waitState(t, StateRecovering)
	if st.LastTransient != "config_invalid" {
		t.Fatalf("LastTransient = %q, want config_invalid", st.LastTransient)
	}
	if f := r.freshness(t); !slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("freshness during config recovery: %+v", f)
	}
	writeFile(t, cfgPath, `{"exclude":["skip/**"]}`)
	r.waitLive(t, "queued file indexed under the new config", func(p []string) bool {
		return slices.Equal(p, []string{"main.go", "new.go"})
	})
	r.waitState(t, StateWatching)
}

// Retries are spent by scheduled recovery attempts, not by the burst of events
// a single slow write raises.
func TestWatcherConfigRetriesCountAttemptsNotEvents(t *testing.T) {
	base, maxDelay := retryBaseDelay, retryMaxDelay
	retryBaseDelay, retryMaxDelay = 100*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { retryBaseDelay, retryMaxDelay = base, maxDelay })
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n", "skip/s.go": goFile})
	cfgPath := filepath.Join(r.root, ".codegraph", "config.json")
	for i := range 2 * maxConfigRetries {
		writeFile(t, cfgPath, `{"exclude":`+strings.Repeat(" ", i))
		time.Sleep(5 * time.Millisecond)
	}
	r.waitState(t, StateRecovering)
	writeFile(t, cfgPath, `{"exclude":["skip/**"]}`)
	r.waitLive(t, "excluded file retired", func(p []string) bool { return slices.Equal(p, []string{"main.go"}) })
	r.waitState(t, StateWatching)
}

// Valid JSON the config cannot hold is not a write in progress: the watcher
// stops at once instead of retrying.
func TestWatcherStopsAtOnceOnInvalidConfigValue(t *testing.T) {
	shortRetries(t)
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n"})
	writeFile(t, filepath.Join(r.root, ".codegraph", "config.json"), `{"exclude": 5}`)
	select {
	case err := <-r.done:
		r.done <- err
		if err == nil || !strings.Contains(err.Error(), "reload repository config") {
			t.Fatalf("err = %v, want a config reload error", err)
		}
		if st := r.w.Stats(); st.TransientErrors != 0 {
			t.Fatalf("invalid config value was retried: %+v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("watcher kept running on an invalid config: %+v", r.w.Stats())
	}
}

// A full scan by another process completes while this watcher still cannot
// register its tree. The scan covers what changed before it, not what the
// unregistered watcher keeps missing, so freshness must not read consistent.
func TestDegradedWatcherKeepsReconcilePendingAcrossExternalScan(t *testing.T) {
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
	failing.Store(true)
	writeFile(t, filepath.Join(r.root, "newdir", "a.go"), goFile)
	r.waitState(t, StateRecovering)

	other, err := store.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if _, err := indexer.New(other, parser.NewRegistry(goparser.New()), nil).Update(context.Background(), indexer.Options{RepoRoot: r.root}); err != nil {
		t.Fatal(err)
	}
	if st := r.w.Stats(); st.State != StateRecovering {
		t.Fatalf("watcher recovered during the test: %+v", st)
	}
	reader, err := store.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	f, err := reader.FreshnessStatus(context.Background(), r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Verdict == "consistent_with_last_scan" || !slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("degraded watcher read as consistent after an external scan: verdict %s reasons %v", f.Verdict, f.Reasons)
	}

	failing.Store(false)
	r.waitState(t, StateWatching)
	f, err = reader.FreshnessStatus(context.Background(), r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("reconcile_pending after the watcher recovered: %v", f.Reasons)
	}
}

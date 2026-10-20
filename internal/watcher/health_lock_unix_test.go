//go:build unix

package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/store"
)

// holdExclusive takes path exclusively through its own open file, as a
// status read in another process probing it does, until the returned
// function is called.
func holdExclusive(t *testing.T, path string) func() {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return func() { _ = f.Close() }
}

// While a status read holds the unverified lock, a failure waits to take
// it but never blocks the state other goroutines read; a reconciliation
// that lands meanwhile still leaves the lock released.
func TestHealthDegradeDoesNotBlockStateReads(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lock, err := s.OpenWatcherLock(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Close)
	var h health
	if err := h.reset(lock); err != nil {
		t.Fatal(err)
	}
	h.setRegistered()
	h.reconciled(h.failureCount())

	prober := holdExclusive(t, fmt.Sprintf("%s.watch-%d.lock", dbPath, repo.ID))
	done := make(chan struct{})
	go func() {
		h.degrade("sqlite_busy", false)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		start := time.Now()
		state, _ := h.snapshot()
		if wait := time.Since(start); wait > 100*time.Millisecond {
			t.Fatalf("snapshot waited %v for a lock hold", wait)
		}
		if state == StateRecovering {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("degrade never recorded")
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	h.failureCount()
	if wait := time.Since(start); wait > 100*time.Millisecond {
		t.Fatalf("failureCount waited %v for a lock hold", wait)
	}
	h.reconciled(h.failureCount())
	prober()
	<-done
	f, err := s.FreshnessStatus(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := h.snapshot(); state != StateWatching || f.LiveWatcher != store.LiveWatcherWatching {
		t.Fatalf("state %s, live_watcher %s", state, f.LiveWatcher)
	}
}

package store

import (
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func protocolStore(t *testing.T) (*Store, *Store, int64, *WatcherLock) {
	t.Helper()
	s, repoID := freshnessStore(t, t.TempDir())
	completeFullScan(t, s, repoID)
	lock, err := s.OpenWatcherLock(repoID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Close)
	return s, openSameStore(t, s), repoID, lock
}

func liveState(s *Store, repoID int64) string {
	live, _ := s.watcherStatus(repoID)
	return live
}

// Read after every step of a watcher's life, status never reads watching
// before the watcher reconciled or while it recovers or exits: the
// unverified lock is taken before the live one and released after it.
func TestWatcherLockProtocolStepOrder(t *testing.T) {
	_, reader, repoID, lock := protocolStore(t)
	var seen []string
	watcherLockStep = func() { seen = append(seen, liveState(reader, repoID)) }
	t.Cleanup(func() { watcherLockStep = func() {} })

	steps := []func() error{lock.Hold, lock.HoldLive, func() error { lock.Release(); return nil }, lock.Hold, func() error { lock.Close(); return nil }}
	for _, step := range steps {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		LiveWatcherUnverified, // starting
		LiveWatcherUnverified, // reconciling
		LiveWatcherWatching,   // reconciled
		LiveWatcherUnverified, // recovering
		LiveWatcherUnverified, // exiting: live lock released
		LiveWatcherNone,       // exited
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("status after each step %v, want %v", seen, want)
	}
}

// The live lock alone would read as watching, so it is refused before the
// unverified one is held.
func TestWatcherLockLiveNeedsUnverified(t *testing.T) {
	_, reader, repoID, lock := protocolStore(t)
	if err := lock.HoldLive(); err == nil {
		t.Fatal("live lock taken without the unverified one")
	}
	if got := liveState(reader, repoID); got != LiveWatcherNone {
		t.Fatalf("after a refused live hold: %s", got)
	}
}

// A watcher that exits between a status read's live probe and its probe of
// the unverified lock has released both; the read must not take the free
// unverified lock for a reconciled watcher.
func TestLiveWatcherExitBetweenProbes(t *testing.T) {
	for _, reconciled := range []bool{true, false} {
		_, reader, repoID, lock := protocolStore(t)
		if err := lock.Hold(); err != nil {
			t.Fatal(err)
		}
		if err := lock.HoldLive(); err != nil {
			t.Fatal(err)
		}
		if reconciled {
			lock.Release()
		}
		var exited atomic.Bool
		probeLockFile = func(f *os.File, exclusive bool) (bool, error) {
			if f.Name() == watchLockPath(reader.path, repoID) && !exited.Swap(true) {
				lock.Close()
			}
			return lockFile(f, exclusive)
		}
		got := liveState(reader, repoID)
		probeLockFile = lockFile
		if !exited.Load() || got != LiveWatcherNone {
			t.Fatalf("reconciled=%v: watcher exited between probes reads %s", reconciled, got)
		}
	}
}

// One status read's exclusive probe of the unverified lock must never be
// read as a watcher by another read in this process: reads take turns.
func TestLiveWatcherConcurrentReadsTakeTurns(t *testing.T) {
	old := scanLockProbeWait
	scanLockProbeWait = time.Millisecond
	t.Cleanup(func() { scanLockProbeWait = old; probeLockFile = lockFile })
	s, reader, repoID, lock := protocolStore(t)
	lock.Close()

	holding := make(chan struct{})
	secondDone := make(chan string, 1)
	var first atomic.Bool
	probeLockFile = func(f *os.File, exclusive bool) (bool, error) {
		ok, err := lockFile(f, exclusive)
		if ok && exclusive && f.Name() == watchLockPath(s.path, repoID) && !first.Swap(true) {
			close(holding)
			// Hold the probe until the other read finishes, or long enough
			// for it to exhaust its retries had it not waited its turn.
			select {
			case got := <-secondDone:
				secondDone <- got
			case <-time.After(200 * time.Millisecond):
			}
		}
		return ok, err
	}
	firstDone := make(chan string, 1)
	go func() { firstDone <- liveState(reader, repoID) }()
	<-holding
	go func() { secondDone <- liveState(s, repoID) }()
	if got := <-firstDone; got != LiveWatcherNone {
		t.Fatalf("first read: %s", got)
	}
	if got := <-secondDone; got != LiveWatcherNone {
		t.Fatalf("second read took the first's probe for a watcher: %s", got)
	}
}

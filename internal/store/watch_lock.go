package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// A watcher holds two OS locks per repository beside the database, both local
// to the host and both dropped by the operating system when the holder exits,
// however it exits:
//
//   - <db>.watch-<repo>.lock, shared, while it cannot vouch for its event
//     coverage (starting up, or recovering from a failure). Freshness reads
//     reconcile_pending while any holder lives, so a full scan by another
//     process cannot clear evidence of a failure that is still going on.
//   - <db>.watch-<repo>.live.lock, exclusive, for its whole run. Status reads
//     probe it shared, so they never mistake one another for a watcher.
//
// A watcher takes the first before the second and releases them in reverse,
// so a reader that finds the live lock held and then the other free has seen
// a watcher that reconciled, not one still starting. Nothing about a watcher
// is stored: a crashed watcher leaves only the persisted reconcile_pending
// marker, which the next full scan clears.

func watchLockPath(dbPath string, repoID int64) string {
	return fmt.Sprintf("%s.watch-%d.lock", dbPath, repoID)
}

func watchLivePath(dbPath string, repoID int64) string {
	return fmt.Sprintf("%s.watch-%d.live.lock", dbPath, repoID)
}

// Live watcher states reported in Freshness.LiveWatcher.
const (
	LiveWatcherNone       = "none"
	LiveWatcherWatching   = "watching"
	LiveWatcherUnverified = "unverified"
)

// watcherLockStep runs after each step of the protocol a WatcherLock takes;
// tests read status there.
var watcherLockStep = func() {}

// WatcherLock is one watcher's handle on those locks. It is not safe for
// concurrent use; a nil *WatcherLock holds nothing.
type WatcherLock struct {
	f        *os.File
	held     bool
	live     *os.File
	liveHeld bool
}

// OpenWatcherLock opens the watcher locks of repoID without taking them. A
// store with no database file returns nil.
func (s *Store) OpenWatcherLock(repoID int64) (*WatcherLock, error) {
	if s.path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(watchLockPath(s.path, repoID), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open watcher lock: %w", err)
	}
	live, err := os.OpenFile(watchLivePath(s.path, repoID), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("open watcher lock: %w", err)
	}
	return &WatcherLock{f: f, live: live}, nil
}

// holdLock takes f without waiting on a holder, retrying over the window a
// status read's probe can hold it.
func holdLock(f *os.File, exclusive bool) (bool, error) {
	for attempt := 1; ; attempt++ {
		ok, err := lockFile(f, exclusive)
		if err != nil || ok {
			return ok, err
		}
		if attempt == 10*scanLockProbeAttempts {
			return false, nil
		}
		time.Sleep(scanLockProbeWait)
	}
}

// Hold takes the unverified lock shared, waiting out status reads that probe
// it.
func (l *WatcherLock) Hold() error {
	if l == nil || l.held {
		return nil
	}
	ok, err := holdLock(l.f, false)
	if err != nil {
		return fmt.Errorf("hold watcher lock: %w", err)
	}
	if !ok {
		return errors.New("hold watcher lock: still probed after retries")
	}
	l.held = true
	watcherLockStep()
	return nil
}

// HoldLive takes the live lock for the rest of the watcher's run. Hold must
// have been called first. Another watcher of the same repository may hold it
// already; this one then runs unseen by status reads.
func (l *WatcherLock) HoldLive() error {
	if l == nil || l.liveHeld {
		return nil
	}
	if !l.held {
		// Taken first, the live lock alone would read as watching.
		return errors.New("hold watcher live lock: unverified lock not held")
	}
	ok, err := holdLock(l.live, true)
	if err != nil {
		return fmt.Errorf("hold watcher live lock: %w", err)
	}
	l.liveHeld = ok
	if ok {
		watcherLockStep()
	}
	return nil
}

func (l *WatcherLock) Release() {
	if l == nil || !l.held {
		return
	}
	_ = unlockFile(l.f)
	l.held = false
	watcherLockStep()
}

// Close releases the live lock before the unverified one.
func (l *WatcherLock) Close() {
	if l == nil {
		return
	}
	if l.liveHeld {
		_ = unlockFile(l.live)
		l.liveHeld = false
		watcherLockStep()
	}
	_ = l.live.Close()
	l.Release()
	_ = l.f.Close()
}

// probeLock opens path without creating it and tries attempts times, waiting
// scanLockProbeWait between them, to lock it without waiting on a holder;
// what it takes it releases at once.
func probeLock(path string, exclusive bool, attempts int) scanLockProbe {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return scanLockAbsent
	}
	if err != nil {
		return scanLockError
	}
	defer f.Close()
	for attempt := 1; ; attempt++ {
		ok, err := probeLockFile(f, exclusive)
		if err != nil {
			return scanLockError
		}
		if ok {
			_ = unlockFile(f)
			return scanLockFree
		}
		if attempt == attempts {
			return scanLockHeld
		}
		time.Sleep(scanLockProbeWait)
	}
}

// watcherStatus reports which watcher of repoID, if any, lives on this host,
// and whether a live one, or one that cannot be ruled out, has not verified
// its coverage. It never creates a lock file. Reads in this process take
// turns with each other and with the scan lock probe, so their own probes
// cannot make one another read a holder.
func (s *Store) watcherStatus(repoID int64) (live string, unverified bool) {
	db := s.lockDB()
	if db == "" {
		return LiveWatcherNone, false
	}
	scanLockProbeMu.Lock()
	defer scanLockProbeMu.Unlock()
	livePath := watchLivePath(db, repoID)
	// Readers probe the live lock shared and the watcher holds it
	// exclusively, so one attempt suffices; the unverified lock is probed
	// exclusively, and a concurrent status read holds it only for a moment.
	first := probeLock(livePath, false, 1)
	switch probeLock(watchLockPath(db, repoID), true, scanLockProbeAttempts) {
	case scanLockHeld:
		return LiveWatcherUnverified, true
	case scanLockError:
		return freshnessUnknown, true
	case scanLockAbsent:
		// A watcher creates both files before taking either, so the
		// unverified one was removed, and a watcher may still hold it.
		if first != scanLockAbsent {
			return freshnessUnknown, true
		}
	}
	if first != scanLockHeld {
		return liveProbeState(first), false
	}
	// Probed again: a watcher that exited after the first probe released
	// its unverified lock, which would otherwise read as reconciled.
	if again := probeLock(livePath, false, 1); again != scanLockHeld {
		return liveProbeState(again), false
	}
	return LiveWatcherWatching, false
}

func liveProbeState(p scanLockProbe) string {
	if p == scanLockError {
		return freshnessUnknown
	}
	return LiveWatcherNone
}

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// A watcher that cannot vouch for its event coverage (starting up, or
// recovering from a failure) holds a shared OS lock on
// <db>.watch-<repo>.lock. Freshness reads reconcile_pending while any holder
// lives, so a full scan by another process cannot clear evidence of a failure
// that is still going on. The operating system drops the lock when the holder
// exits, however it exits; a crashed watcher leaves only the persisted
// reconcile_pending marker, which the next full scan clears.

func watchLockPath(dbPath string, repoID int64) string {
	return fmt.Sprintf("%s.watch-%d.lock", dbPath, repoID)
}

// WatcherLock is one watcher's handle on that lock. It is not safe for
// concurrent use; a nil *WatcherLock holds nothing.
type WatcherLock struct {
	f    *os.File
	held bool
}

// OpenWatcherLock opens the watcher lock of repoID without taking it. A
// store with no database file returns nil.
func (s *Store) OpenWatcherLock(repoID int64) (*WatcherLock, error) {
	if s.path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(watchLockPath(s.path, repoID), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open watcher lock: %w", err)
	}
	return &WatcherLock{f: f}, nil
}

// Hold takes the lock shared, waiting out status reads that probe it.
func (l *WatcherLock) Hold() error {
	if l == nil || l.held {
		return nil
	}
	for attempt := 1; ; attempt++ {
		ok, err := lockFile(l.f, false)
		if err != nil {
			return fmt.Errorf("hold watcher lock: %w", err)
		}
		if ok {
			l.held = true
			return nil
		}
		if attempt == 10*scanLockProbeAttempts {
			return errors.New("hold watcher lock: still probed after retries")
		}
		time.Sleep(scanLockProbeWait)
	}
}

func (l *WatcherLock) Release() {
	if l == nil || !l.held {
		return
	}
	_ = unlockFile(l.f)
	l.held = false
}

func (l *WatcherLock) Close() {
	if l == nil {
		return
	}
	l.Release()
	_ = l.f.Close()
}

// watcherUnverified reports whether a live watcher of repoID holds its lock,
// or whether that cannot be ruled out. The lock file is never created here.
func (s *Store) watcherUnverified(repoID int64) bool {
	if s.path == "" {
		return false
	}
	f, err := os.Open(watchLockPath(s.path, repoID))
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer f.Close()
	// A concurrent status read holds it exclusively only for a moment.
	for attempt := 1; ; attempt++ {
		ok, err := probeLockFile(f, true)
		if err != nil {
			return true
		}
		if ok {
			_ = unlockFile(f)
			return false
		}
		if attempt == scanLockProbeAttempts {
			return true
		}
		time.Sleep(scanLockProbeWait)
	}
}

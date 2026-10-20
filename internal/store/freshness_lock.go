package store

import (
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"
)

type scanLockProbe int

const (
	// scanLockAbsent: no lock file (or no database file), so no lock-aware
	// scan has run and none can hold it.
	scanLockAbsent scanLockProbe = iota
	// scanLockError: the lock could not be probed; a writer may hold it.
	scanLockError
	scanLockHeld
	scanLockFree
)

// A status read in another process holds the lock exclusively only while it
// reads rows, so short retries tell it apart from a scan, which holds it
// throughout. Reads in this process are serialized instead.
var (
	scanLockProbeAttempts = 10
	scanLockProbeWait     = 10 * time.Millisecond
	// ponytail: one mutex for every database; per-path if status reads contend.
	scanLockProbeMu sync.Mutex
	// probeLockFile is lockFile; tests replace it to fail the lock call.
	probeLockFile = lockFile
)

// probeScanLock tries to take the scan lock exclusively without waiting on a
// scan. On scanLockFree the caller holds it until release is called, so no
// scan can begin while it reads; release is never nil and must be called.
// The lock file is never created: without one no lock-aware scan has run.
func (s *Store) probeScanLock() (scanLockProbe, func()) {
	scanLockProbeMu.Lock()
	noop := scanLockProbeMu.Unlock
	db := s.lockDB()
	if db == "" {
		return scanLockAbsent, noop
	}
	f, err := os.Open(scanLockPath(db))
	if errors.Is(err, fs.ErrNotExist) {
		return scanLockAbsent, noop
	}
	if err != nil {
		return scanLockError, noop
	}
	for attempt := 1; ; attempt++ {
		ok, err := probeLockFile(f, true)
		if err != nil {
			_ = f.Close()
			return scanLockError, noop
		}
		if ok {
			return scanLockFree, func() {
				releaseScanLockFile(f)
				scanLockProbeMu.Unlock()
			}
		}
		if attempt == scanLockProbeAttempts {
			_ = f.Close()
			return scanLockHeld, noop
		}
		time.Sleep(scanLockProbeWait)
	}
}

// lockDB is the live database whose locks this store probes: its own file,
// or the one a read-only store's snapshot was taken from.
func (s *Store) lockDB() string {
	if s.path != "" {
		return s.path
	}
	return s.liveDB
}

// liveChangedSinceSnapshot reports whether the database a read-only store's
// snapshot was taken from may have changed since: a file the snapshot copied
// changed size, modification time or identity, appeared, disappeared, or
// cannot be read. It is false for a store that serves its database directly.
func (s *Store) liveChangedSinceSnapshot() bool {
	if s.path != "" || s.liveDB == "" {
		return false
	}
	for _, p := range sqlitePersistentArtifactPaths(s.liveDB) {
		st, err := os.Stat(p)
		copied, ok := s.liveArtifacts[p]
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if ok {
				return true
			}
		case err != nil || !ok || !sqliteInspectionFileInfoEqual(copied.info, st):
			return true
		}
	}
	return false
}

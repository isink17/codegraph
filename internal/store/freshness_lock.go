package store

import (
	"os"
	"sync"
	"time"
)

type scanLockProbe int

const (
	scanLockUnknown scanLockProbe = iota
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
)

// probeScanLock tries to take the scan lock exclusively without waiting on a
// scan. On scanLockFree the caller holds it until release is called, so no
// scan can begin while it reads; release is never nil and must be called.
// The lock file is never created: without one no lock-aware scan has run.
func (s *Store) probeScanLock() (scanLockProbe, func()) {
	scanLockProbeMu.Lock()
	noop := scanLockProbeMu.Unlock
	if s.path == "" {
		return scanLockUnknown, noop
	}
	f, err := os.Open(scanLockPath(s.path))
	if err != nil {
		return scanLockUnknown, noop
	}
	for attempt := 1; ; attempt++ {
		ok, err := lockFile(f, true)
		if err != nil {
			_ = f.Close()
			return scanLockUnknown, noop
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

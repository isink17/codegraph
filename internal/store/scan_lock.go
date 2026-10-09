package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// A scan holds a shared OS lock on <db>.scan.lock from just before its row is
// inserted until the row is recorded finished. The operating system drops the
// lock when the holding process exits, however it exits, so a running row
// with no lock holder left belongs to a dead process. Recovery proves that by
// taking the lock exclusively; it cannot be granted while any scan, in this
// or any other process on the host, still holds its shared lock.

// ErrScanActive reports that a scan still holds the scan lock, so its
// running rows may belong to a live writer and are left untouched.
var ErrScanActive = errors.New("a scan of this database is still running")

// AbandonedScanError is the error_text recorded on a recovered scan row.
const AbandonedScanError = "abandoned: no live process held the scan lock"

const scanLockRetry = 50 * time.Millisecond

func scanLockPath(dbPath string) string { return dbPath + ".scan.lock" }

func openScanLock(dbPath string) (*os.File, error) {
	f, err := os.OpenFile(scanLockPath(dbPath), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open scan lock: %w", err)
	}
	return f, nil
}

// acquireScanLock takes the shared scan lock, waiting while a recovery holds
// it exclusively. It returns nil for a store with no database file.
func (s *Store) acquireScanLock(ctx context.Context) (*os.File, error) {
	if s.path == "" {
		return nil, nil
	}
	f, err := openScanLock(s.path)
	if err != nil {
		return nil, err
	}
	for {
		ok, err := lockFile(f, false)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock scan: %w", err)
		}
		if ok {
			return f, nil
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(scanLockRetry):
		}
	}
}

func releaseScanLockFile(f *os.File) {
	if f == nil {
		return
	}
	_ = unlockFile(f)
	_ = f.Close()
}

// holdScanLock keeps f until scanID is recorded finished. id 0 marks a row
// whose id is unknown; its lock is kept until Close.
func (s *Store) holdScanLock(scanID int64, f *os.File) {
	if f == nil {
		return
	}
	s.scanLockMu.Lock()
	defer s.scanLockMu.Unlock()
	if s.scanLocks == nil {
		s.scanLocks = map[int64]*os.File{}
	}
	if scanID == 0 {
		s.orphanScanLocks = append(s.orphanScanLocks, f)
		return
	}
	s.scanLocks[scanID] = f
}

func (s *Store) releaseScanLock(scanID int64) {
	s.scanLockMu.Lock()
	f := s.scanLocks[scanID]
	delete(s.scanLocks, scanID)
	s.scanLockMu.Unlock()
	releaseScanLockFile(f)
}

func (s *Store) releaseAllScanLocks() {
	s.scanLockMu.Lock()
	locks := s.scanLocks
	orphans := s.orphanScanLocks
	s.scanLocks, s.orphanScanLocks = nil, nil
	s.scanLockMu.Unlock()
	for _, f := range locks {
		releaseScanLockFile(f)
	}
	for _, f := range orphans {
		releaseScanLockFile(f)
	}
}

// RecoverAbandonedScans records every running scan row as failed when no
// process holds the scan lock, and returns their ids in ascending order. It
// returns ErrScanActive, changing nothing, while any scan holds the lock.
//
// A recovered row is failed, never completed: it certifies nothing, and strict
// freshness still requires a later completed full scan. Rows written by a
// binary that predates the scan lock cannot be told from a live writer of such
// a binary; stop those before recovering. The lock is advisory and local to
// the host: a writer on another machine sharing the database over a network
// file system is not seen.
func (s *Store) RecoverAbandonedScans(ctx context.Context) ([]int64, error) {
	if s.path == "" {
		return nil, errors.New("recover scans: store has no database file")
	}
	f, err := openScanLock(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ok, err := lockFile(f, true)
	if err != nil {
		return nil, fmt.Errorf("lock scan: %w", err)
	}
	if !ok {
		return nil, ErrScanActive
	}
	defer func() { _ = unlockFile(f) }()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM scans WHERE status = 'running' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE scans SET status = 'failed', finished_at = ?, error_text = ?, head_at_finish = ''
		WHERE status = 'running'
	`, time.Now().UTC().Format(time.RFC3339), AbandonedScanError); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

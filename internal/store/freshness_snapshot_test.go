package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func openSnapshot(t *testing.T, s *Store) *Store {
	t.Helper()
	ro, err := OpenReadOnly(s.path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	return ro
}

// A read-only store serves a snapshot, whose rows cannot show a scan that
// holds the lock but has not inserted its row; it probes the live database's
// lock instead, never the snapshot's.
func TestSnapshotFreshnessSeesPreInsertWriter(t *testing.T) {
	ctx := context.Background()
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	ro := openSnapshot(t, s)
	if f := freshness(t, ro, repoID); f.RunningScans.Liveness != "none" || f.Verdict != graph.FreshnessVerdictConsistentWithLastScan || f.LiveChangedSinceSnapshot {
		t.Fatalf("quiet snapshot: %+v", f)
	}
	writer := openSameStore(t, s)
	lock, err := writer.acquireScanLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := freshness(t, ro, repoID)
	releaseScanLockFile(lock)
	if f.RunningScans.Count != 0 || f.RunningScans.Liveness != "active" || f.Verdict != graph.FreshnessVerdictWriterActive {
		t.Fatalf("pre-insert writer through a snapshot: %+v", f)
	}
}

// A running row copied into a snapshot may belong to a scan that has finished
// since, so a free lock does not make it abandoned.
func TestSnapshotRunningRowIsNotAbandoned(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	scan(t, s, repoID, "update", "running", "")
	ro := openSnapshot(t, s)
	s.releaseAllScanLocks()
	f := freshness(t, ro, repoID)
	if f.RunningScans.Count != 1 || f.RunningScans.Liveness != "unknown" || f.Verdict != graph.FreshnessVerdictLivenessUnknown {
		t.Fatalf("snapshot running row under a free lock: %+v", f)
	}
	if live := freshness(t, s, repoID); live.RunningScans.Liveness != "abandoned" {
		t.Fatalf("live store under a free lock: %+v", live)
	}
}

// A scan that begins and finishes between the snapshot and the freshness
// read leaves no lock behind; the changed source files show it.
func TestSnapshotFreshnessSeesLiveDatabaseChange(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	ro := openSnapshot(t, s)
	scan(t, s, repoID, "update", "completed", "")
	f := freshness(t, ro, repoID)
	if !f.LiveChangedSinceSnapshot || f.RunningScans.Liveness != "none" {
		t.Fatalf("snapshot after a whole scan ran: %+v", f)
	}
	if live := freshness(t, s, repoID); live.LiveChangedSinceSnapshot {
		t.Fatalf("a live store reports a snapshot change: %+v", live)
	}
}

// SQLite creates an empty WAL when a connection opens the database and
// removes it when the last one closes; neither is a change.
func TestSnapshotFreshnessIgnoresEmptyWALComingAndGoing(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	wal := s.path + "-wal"
	if _, err := os.Stat(wal); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("closed database keeps a WAL: %v", err)
	}
	ro := openSnapshot(t, s)
	if err := os.WriteFile(wal, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if f := freshness(t, ro, repoID); f.LiveChangedSinceSnapshot {
		t.Fatal("an empty WAL appearing reads as a change")
	}
	if err := os.WriteFile(wal, []byte("frame"), 0o644); err != nil {
		t.Fatal(err)
	}
	if f := freshness(t, ro, repoID); !f.LiveChangedSinceSnapshot {
		t.Fatal("a WAL gaining content reads as unchanged")
	}
}

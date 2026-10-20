package store

import (
	"context"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/graph"
)

// A long scan that began before a short one completed is still writing; the
// graph must not read as consistent while it runs.
func TestFreshnessOlderLiveScanIsWriterActive(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	long := scan(t, s, repoID, "index", "running", "")
	scan(t, s, repoID, "update", "completed", "")
	f := freshness(t, s, repoID)
	if f.RunningScans.Liveness != "active" || f.Verdict != graph.FreshnessVerdictWriterActive {
		t.Fatalf("live older scan %d: %+v", long, f)
	}
	if f.State != graph.FreshnessKnownStale || !slices.Contains(f.Reasons, "scan_running_or_abandoned") {
		t.Fatalf("state=%s reasons=%v", f.State, f.Reasons)
	}
}

func TestFreshnessAbandonedScanNeedsRecovery(t *testing.T) {
	ctx := context.Background()
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	scan(t, s, repoID, "update", "running", "")
	s.releaseAllScanLocks() // what the OS does when the owning process dies

	f := freshness(t, s, repoID)
	if f.RunningScans.Liveness != "abandoned" || f.Verdict != graph.FreshnessVerdictAbandonedScan || f.State != graph.FreshnessKnownStale {
		t.Fatalf("abandoned: %+v", f)
	}
	// The probe released its lock: recovery and new scans proceed.
	if ids, err := openSameStore(t, s).RecoverAbandonedScans(ctx); err != nil || len(ids) != 1 {
		t.Fatalf("recover ids=%v err=%v", ids, err)
	}
	f = freshness(t, s, repoID)
	if f.RunningScans.Liveness != "none" || f.Verdict != graph.FreshnessVerdictStaleKnown {
		t.Fatalf("after recovery: %+v", f)
	}
	scan(t, s, repoID, "index", "completed", "")
	f = freshness(t, s, repoID)
	if f.Verdict != graph.FreshnessVerdictConsistentWithLastScan || f.State != graph.FreshnessNoKnownStaleness {
		t.Fatalf("after rescan: %+v", f)
	}
}

// Another process's live scan is seen through the shared OS lock.
func TestFreshnessSeesWriterInOtherStore(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	other := openSameStore(t, s)
	id, started, err := other.BeginScan(context.Background(), repoID, "update")
	if err != nil {
		t.Fatal(err)
	}
	if f := freshness(t, s, repoID); f.Verdict != graph.FreshnessVerdictWriterActive {
		t.Fatalf("other writer: %+v", f)
	}
	if err := other.CompleteScan(context.Background(), id, ScanSummary{}, started, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if f := freshness(t, s, repoID); f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("after other writer: %+v", f)
	}
}

// Concurrent status reads must not mistake each other's probe for a writer.
func TestFreshnessConcurrentProbesDoNotReportWriter(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	other := openSameStore(t, s)
	errs := make(chan error, 2)
	for _, st := range []*Store{s, other} {
		go func() {
			for range 50 {
				f, err := st.FreshnessStatus(context.Background(), repoID)
				if err == nil && f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
					err = &probeErr{f}
				}
				if err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

type probeErr struct{ f graph.Freshness }

func (e *probeErr) Error() string {
	return "verdict " + e.f.Verdict + " liveness " + e.f.RunningScans.Liveness
}

// A status read holds the scan lock only for its own row read.
func TestFreshnessProbeDoesNotBlockScans(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	freshness(t, s, repoID)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	id, started, err := s.BeginScan(ctx, repoID, "update")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteScan(ctx, id, ScanSummary{}, started, "completed", ""); err != nil {
		t.Fatal(err)
	}
}

// A status read's brief exclusive hold is not a running scan: recovery waits
// it out instead of refusing.
func TestRecoverAbandonedScansWaitsOutStatusRead(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "update", "running", "")
	s.releaseAllScanLocks()
	probe, release := s.probeScanLock()
	if probe != scanLockFree {
		release()
		t.Fatalf("probe = %v", probe)
	}
	go func() {
		time.Sleep(3 * scanLockProbeWait)
		release()
	}()
	ids, err := openSameStore(t, s).RecoverAbandonedScans(context.Background())
	if err != nil || len(ids) != 1 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}

// The scan lock is per database: another repository's live scan in the same
// database is not this repository's writer.
func TestFreshnessOtherRepositoryWriterIsNotAttributed(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	other, err := s.UpsertRepo(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scan(t, s, other.ID, "update", "running", "")
	f := freshness(t, s, repoID)
	if f.RunningScans.Liveness != "unknown" || f.Verdict != graph.FreshnessVerdictLivenessUnknown {
		t.Fatalf("held lock beside another repository's writer: %+v", f)
	}
	// This repository's own running row beside another repository's live
	// writer cannot be told live or abandoned.
	scan(t, s, repoID, "update", "running", "")
	f = freshness(t, s, repoID)
	if f.RunningScans.Liveness != "unknown" || f.Verdict != graph.FreshnessVerdictLivenessUnknown {
		t.Fatalf("ambiguous writer: %+v", f)
	}
}

// A scan takes the lock before it inserts its row, and that insert can wait
// on a busy database; the graph is not consistent meanwhile.
func TestFreshnessLockHeldBeforeRowIsWriterActive(t *testing.T) {
	ctx := context.Background()
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	writer := openSameStore(t, s)
	lock, err := writer.acquireScanLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := freshness(t, s, repoID)
	releaseScanLockFile(lock)
	if f.RunningScans.Count != 0 || f.RunningScans.Liveness != "active" || f.Verdict != graph.FreshnessVerdictWriterActive || f.State != graph.FreshnessKnownStale {
		t.Fatalf("pre-insert writer: %+v", f)
	}
	if f := freshness(t, s, repoID); f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("after release: %+v", f)
	}
}

// A lock that cannot be probed is unknown, not proof that no writer runs.
func TestFreshnessUnprobeableLockIsLivenessUnknown(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a permission-denied open")
	}
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	if err := os.Chmod(scanLockPath(s.path), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(scanLockPath(s.path), 0o644) })
	f := freshness(t, s, repoID)
	if f.RunningScans.Liveness != "unknown" || f.Verdict != graph.FreshnessVerdictLivenessUnknown {
		t.Fatalf("unprobeable lock: %+v", f)
	}
}

// Without a lock file no lock-aware scan has ever run, so no such writer
// can hold it: a graph with no running rows reads as before.
func TestFreshnessMissingLockFileWithoutRowsIsConsistent(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	if err := os.Remove(scanLockPath(s.path)); err != nil {
		t.Fatal(err)
	}
	f := freshness(t, s, repoID)
	if f.RunningScans.Liveness != "unknown" || f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("missing lock file: %+v", f)
	}
}

// Without a lock file a running row's writer cannot be told live or dead: a
// binary that predates the lock takes none.
func TestFreshnessMissingLockFileWithRunningRowIsLivenessUnknown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("an open lock file cannot be removed")
	}
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	scan(t, s, repoID, "update", "running", "")
	if err := os.Remove(scanLockPath(s.path)); err != nil {
		t.Fatal(err)
	}
	f := freshness(t, s, repoID)
	if f.RunningScans.Liveness != "unknown" || f.Verdict != graph.FreshnessVerdictLivenessUnknown {
		t.Fatalf("missing lock file with a running row: %+v", f)
	}
}

// A lock call that fails, rather than finding the lock held, proves nothing
// about a writer.
func TestFreshnessLockCallErrorIsLivenessUnknown(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	probeLockFile = func(*os.File, bool) (bool, error) { return false, os.ErrInvalid }
	t.Cleanup(func() { probeLockFile = lockFile })
	f := freshness(t, s, repoID)
	if f.RunningScans.Liveness != "unknown" || f.Verdict != graph.FreshnessVerdictLivenessUnknown {
		t.Fatalf("lock call error: %+v", f)
	}
}

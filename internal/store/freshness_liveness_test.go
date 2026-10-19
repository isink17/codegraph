package store

import (
	"context"
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
	if f.RunningScans.Liveness != "unknown" || f.Verdict != graph.FreshnessVerdictConsistentWithLastScan || f.State != graph.FreshnessNoKnownStaleness {
		t.Fatalf("other repo's writer attributed: %+v", f)
	}
	// This repository's own running row beside another repository's live
	// writer cannot be told live or abandoned.
	scan(t, s, repoID, "update", "running", "")
	f = freshness(t, s, repoID)
	if f.RunningScans.Liveness != "unknown" || f.Verdict != graph.FreshnessVerdictLivenessUnknown {
		t.Fatalf("ambiguous writer: %+v", f)
	}
}

package store

import (
	"context"
	"slices"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// A held watcher lock keeps reconcile_pending through a full scan by another
// handle, only for its own repository; releasing or closing it, as an exiting
// process does, lets the scan's evidence stand.
func TestWatcherLockKeepsReconcilePending(t *testing.T) {
	ctx := context.Background()
	s, repoID := freshnessStore(t, t.TempDir())
	other, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scan(t, s, other.ID, "index", "completed", "")
	full := func() {
		id, started, err := s.BeginScanWithScope(ctx, repoID, "index", ScanScopeFull, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CompleteScan(ctx, id, ScanSummary{RepoID: repoID, ScanID: id}, started, "completed", ""); err != nil {
			t.Fatal(err)
		}
	}
	full()
	reader, err := Open(s.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if f := freshness(t, reader, repoID); f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("no watcher: verdict %s reasons %v", f.Verdict, f.Reasons)
	}

	lock, err := s.OpenWatcherLock(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Hold(); err != nil {
		t.Fatal(err)
	}
	full()
	if f := freshness(t, reader, repoID); f.Verdict != graph.FreshnessVerdictStaleKnown || !slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("held: verdict %s reasons %v", f.Verdict, f.Reasons)
	}
	if f := freshness(t, reader, other.ID); slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("another repository's watcher lock leaked: %v", f.Reasons)
	}
	lock.Release()
	if f := freshness(t, reader, repoID); f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("released: verdict %s reasons %v", f.Verdict, f.Reasons)
	}
	if err := lock.Hold(); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if f := freshness(t, reader, repoID); f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("closed: verdict %s reasons %v", f.Verdict, f.Reasons)
	}
}

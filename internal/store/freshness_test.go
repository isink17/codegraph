package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/githistory"
	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/graph"
)

func freshnessStore(t *testing.T, root string) (*Store, int64) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), RepoDatabaseFileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return s, repo.ID
}

func freshness(t *testing.T, s *Store, repoID int64) graph.Freshness {
	t.Helper()
	f, err := s.FreshnessStatus(context.Background(), repoID)
	if err != nil {
		t.Fatal(err)
	}
	if f.State == "fresh" || slices.Contains(f.Reasons, "fresh") {
		t.Fatalf("freshness claims fresh: %+v", f)
	}
	if f.RunningScans.Liveness != "unknown" || f.Watcher != "not_in_this_process" || f.Filesystem != "not_checked" {
		t.Fatalf("fixed honesty fields changed: %+v", f)
	}
	return f
}

func scan(t *testing.T, s *Store, repoID int64, kind, status, errText string) int64 {
	t.Helper()
	ctx := context.Background()
	id, started, err := s.BeginScan(ctx, repoID, kind)
	if err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		if err := s.CompleteScan(ctx, id, ScanSummary{}, started, status, errText); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestFreshnessNoScans(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	f := freshness(t, s, repoID)
	if f.State != graph.FreshnessUnknown || !slices.Equal(f.Reasons, []string{"never_completed"}) {
		t.Fatalf("state=%s reasons=%v", f.State, f.Reasons)
	}
	if f.LastCompletedScan != nil || f.LatestScan != nil {
		t.Fatalf("scans = %+v %+v", f.LastCompletedScan, f.LatestScan)
	}
	if f.Worktree.CanonicalPath == "" || f.Worktree.HeadAtIndex != "unknown" || f.Worktree.HeadAtIndexSource != "none" {
		t.Fatalf("worktree = %+v", f.Worktree)
	}
}

func TestFreshnessCompletedThenFailed(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	ok := scan(t, s, repoID, "update", "completed", "")
	failed := scan(t, s, repoID, "index", "failed", "boom")
	f := freshness(t, s, repoID)
	if f.State != graph.FreshnessKnownStale || !slices.Equal(f.Reasons, []string{"latest_scan_failed"}) {
		t.Fatalf("state=%s reasons=%v", f.State, f.Reasons)
	}
	if f.LastCompletedScan.ID != ok || f.LastCompletedScan.Scope != "unknown" {
		t.Fatalf("last completed = %+v", f.LastCompletedScan)
	}
	if f.LatestScan.ID != failed || f.LatestScan.Status != "failed" || f.LatestScan.ErrorText != "boom" || f.LatestScan.FinishedAt == "" {
		t.Fatalf("latest = %+v", f.LatestScan)
	}
}

func TestFreshnessNeverCompletedWithFailure(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "failed", "boom")
	f := freshness(t, s, repoID)
	if f.State != graph.FreshnessKnownStale || !slices.Equal(f.Reasons, []string{"never_completed", "latest_scan_failed"}) {
		t.Fatalf("state=%s reasons=%v", f.State, f.Reasons)
	}
}

func TestFreshnessCompletedOnlyHasNoKnownStaleness(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "watch_config", "completed", "")
	f := freshness(t, s, repoID)
	if f.State != graph.FreshnessNoKnownStaleness || len(f.Reasons) != 0 || f.LastCompletedScan.Scope != "full" {
		t.Fatalf("got %+v", f)
	}
}

func TestFreshnessRunningScan(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	orphan := scan(t, s, repoID, "update", "running", "")
	scan(t, s, repoID, "index", "completed", "")
	f := freshness(t, s, repoID)
	// A running row older than the last completed scan was superseded.
	if f.RunningScans.Count != 1 || f.RunningScans.AfterLastCompleted != 0 || f.State != graph.FreshnessNoKnownStaleness {
		t.Fatalf("orphan %d: %+v", orphan, f)
	}
	running := scan(t, s, repoID, "update", "running", "")
	f = freshness(t, s, repoID)
	if f.RunningScans.Count != 2 || f.RunningScans.AfterLastCompleted != 1 || f.LatestScan.ID != running || f.LatestScan.Status != "running" {
		t.Fatalf("running = %+v latest = %+v", f.RunningScans, f.LatestScan)
	}
	if f.State != graph.FreshnessKnownStale || !slices.Equal(f.Reasons, []string{"scan_running_or_abandoned"}) {
		t.Fatalf("state=%s reasons=%v", f.State, f.Reasons)
	}
}

func TestFreshnessDirtyQueue(t *testing.T) {
	ctx := context.Background()
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	if err := s.EnsureCanonicalRepositoryPaths(ctx, repoID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDirtyFiles(ctx, repoID, []string{"a.go", "b.go"}, "watch"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimDirtyFiles(ctx, repoID, time.Now().UTC().Format(time.RFC3339Nano), "watch_inflight"); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDirtyFile(ctx, repoID, "c.go", "watch"); err != nil {
		t.Fatal(err)
	}
	f := freshness(t, s, repoID)
	if f.DirtyQueue.Queued != 1 || f.DirtyQueue.InFlight != 2 || f.DirtyQueue.OldestQueuedAt == "" {
		t.Fatalf("dirty = %+v", f.DirtyQueue)
	}
	if f.State != graph.FreshnessKnownStale || !slices.Equal(f.Reasons, []string{"dirty_queue_nonempty"}) {
		t.Fatalf("state=%s reasons=%v", f.State, f.Reasons)
	}
}

func TestFreshnessHeadNotARepository(t *testing.T) {
	gittest.Require(t)
	dir := t.TempDir()
	// Keep git from finding an enclosing repository above the temp dir.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	s, repoID := freshnessStore(t, dir)
	scan(t, s, repoID, "index", "completed", "")
	f := freshness(t, s, repoID)
	if f.Worktree.HeadNow != "unknown" || f.Worktree.HeadChanged != "unknown" || f.State != graph.FreshnessNoKnownStaleness {
		t.Fatalf("got %+v", f)
	}
}

func TestFreshnessHeadGitUnavailable(t *testing.T) {
	r := gittest.Init(t)
	head := r.Commit("", "one")
	s, repoID := freshnessStore(t, r.Dir)
	if err := s.ReplaceGitHistory(context.Background(), repoID, githistory.State{Status: githistory.StatusOK, Watermark: head}, []githistory.FileStats{}, nil, GitSymbolUpdate{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	f := freshness(t, s, repoID)
	if f.Worktree.HeadAtIndex != head || f.Worktree.HeadNow != "unknown" || f.Worktree.HeadChanged != "unknown" {
		t.Fatalf("worktree = %+v", f.Worktree)
	}
}

func TestFreshnessHeadComparison(t *testing.T) {
	ctx := context.Background()
	r := gittest.Init(t)
	first := r.Commit("", "one")
	s, repoID := freshnessStore(t, r.Dir)
	scan(t, s, repoID, "index", "completed", "")

	// History absent (disabled): no anchor, so the comparison is unknown.
	if err := s.ReplaceGitHistory(ctx, repoID, githistory.Absent(githistory.ReasonDisabled), []githistory.FileStats{}, nil, GitSymbolUpdate{All: true}); err != nil {
		t.Fatal(err)
	}
	f := freshness(t, s, repoID)
	if f.Worktree.HeadAtIndex != "unknown" || f.Worktree.HeadNow != first || f.Worktree.HeadChanged != "unknown" {
		t.Fatalf("absent history: %+v", f.Worktree)
	}

	if err := s.ReplaceGitHistory(ctx, repoID, githistory.State{Status: githistory.StatusOK, Watermark: first}, []githistory.FileStats{}, nil, GitSymbolUpdate{}); err != nil {
		t.Fatal(err)
	}
	f = freshness(t, s, repoID)
	if f.Worktree.HeadAtIndexSource != "git_history_watermark" || f.Worktree.HeadChanged != "no" || f.State != graph.FreshnessNoKnownStaleness {
		t.Fatalf("same head: %+v", f)
	}

	second := r.Commit("", "two")
	f = freshness(t, s, repoID)
	if f.Worktree.HeadNow != second || f.Worktree.HeadChanged != "yes" || f.State != graph.FreshnessKnownStale || !slices.Equal(f.Reasons, []string{"head_moved"}) {
		t.Fatalf("moved head: %+v", f)
	}
}

func TestFreshnessUnknownRepo(t *testing.T) {
	s, _ := freshnessStore(t, t.TempDir())
	if _, err := s.FreshnessStatus(context.Background(), 999); err == nil {
		t.Fatal("unknown repo returned no error")
	}
}

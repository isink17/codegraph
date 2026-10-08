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

func scopedScan(t *testing.T, s *Store, repoID int64, scope, head string) (int64, time.Time) {
	t.Helper()
	id, started, err := s.BeginScanWithScope(context.Background(), repoID, "index", scope, head)
	if err != nil {
		t.Fatal(err)
	}
	return id, started
}

func completeScan(t *testing.T, s *Store, id int64, started time.Time, status, head string) {
	t.Helper()
	if err := s.CompleteScan(context.Background(), id, ScanSummary{HeadAtFinish: head}, started, status, ""); err != nil {
		t.Fatal(err)
	}
}

// Two writers interleave: each sees the other and neither reads as an
// uncontested full scan. A scan started after both closed sees none.
func TestFreshnessCoverageOverlappingWriters(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	full, fullStarted := scopedScan(t, s, repoID, ScanScopeFull, "h1")
	paths, pathsStarted := scopedScan(t, s, repoID, ScanScopePaths, "h1")
	completeScan(t, s, paths, pathsStarted, "completed", "h1")
	completeScan(t, s, full, fullStarted, "completed", "h1")
	f := freshness(t, s, repoID)
	if f.Coverage.LastFullScan == nil || f.Coverage.LastFullScan.ID != full || f.Coverage.LastFullScan.Overlap != "yes" {
		t.Fatalf("full scan overlapped by a path scan: %+v", f.Coverage.LastFullScan)
	}
	if f.LatestScan.ID != paths || f.LatestScan.Overlap != "yes" {
		t.Fatalf("latest = %+v", f.LatestScan)
	}

	// The path scan started first and finished after a full scan began.
	paths2, paths2Started := scopedScan(t, s, repoID, ScanScopePaths, "h1")
	full2, full2Started := scopedScan(t, s, repoID, ScanScopeFull, "h1")
	completeScan(t, s, paths2, paths2Started, "completed", "h1")
	completeScan(t, s, full2, full2Started, "completed", "h1")
	if got := freshness(t, s, repoID).Coverage.LastFullScan; got.ID != full2 || got.Overlap != "yes" {
		t.Fatalf("full scan started during a path scan: %+v", got)
	}

	full3, full3Started := scopedScan(t, s, repoID, ScanScopeFull, "h1")
	if got := freshness(t, s, repoID).LatestScan; got.ID != full3 || got.Overlap != "unknown" {
		t.Fatalf("running scan overlap must stay unknown: %+v", got)
	}
	completeScan(t, s, full3, full3Started, "completed", "h1")
	if got := freshness(t, s, repoID).Coverage.LastFullScan; got.ID != full3 || got.Overlap != "none" {
		t.Fatalf("sequential full scan: %+v", got)
	}
}

// A row a crashed process left running cannot be told from a live writer, so
// every later scan reads as overlapped.
func TestFreshnessCoverageAbandonedRunningRowOverlaps(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scopedScan(t, s, repoID, ScanScopeFull, "h1")
	full, started := scopedScan(t, s, repoID, ScanScopeFull, "h1")
	completeScan(t, s, full, started, "completed", "h1")
	if got := freshness(t, s, repoID).Coverage.LastFullScan; got.ID != full || got.Overlap != "yes" {
		t.Fatalf("got %+v", got)
	}
}

// Rows written through BeginScan, as before scope recording, never qualify as
// a full scan; failures after the last full scan are counted.
func TestFreshnessCoverageUnrecordedRows(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	scan(t, s, repoID, "index", "completed", "")
	f := freshness(t, s, repoID)
	if !f.Coverage.Recorded || f.Coverage.LastFullScan != nil || f.LastCompletedScan.Scope != "unknown" || f.LastCompletedScan.HeadAtStart != "" {
		t.Fatalf("unrecorded row: %+v / %+v", f.Coverage, f.LastCompletedScan)
	}
	full, started := scopedScan(t, s, repoID, ScanScopeFull, "h1")
	completeScan(t, s, full, started, "completed", "h1")
	scan(t, s, repoID, "update", "failed", "boom")
	paths, pathsStarted := scopedScan(t, s, repoID, ScanScopePaths, "h1")
	completeScan(t, s, paths, pathsStarted, "failed", "")
	f = freshness(t, s, repoID)
	if f.Coverage.LastFullScan.ID != full || f.Coverage.FailedAfterLastFull != 2 {
		t.Fatalf("coverage = %+v", f.Coverage)
	}
}

// A database from before scope recording: a read-only handle (which never
// migrates) reports coverage as unrecorded and keeps the legacy scope rule;
// a writable open adds the columns and keeps the old rows unrecorded.
func TestFreshnessCoveragePreMigrationDatabase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	scan(t, s, repo.ID, "watch_config", "completed", "")
	scan(t, s, repo.ID, "index", "completed", "")
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE scans DROP COLUMN scope; ALTER TABLE scans DROP COLUMN head_at_start;
		ALTER TABLE scans DROP COLUMN head_at_finish; ALTER TABLE scans DROP COLUMN overlapping_scans;
		DELETE FROM schema_migrations WHERE version = 9`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f, err := ro.FreshnessStatus(ctx, repo.ID)
	_ = ro.Close()
	if err != nil {
		t.Fatal(err)
	}
	if f.Coverage.Recorded || f.Coverage.LastFullScan != nil || f.LastCompletedScan.Scope != "unknown" || f.LastCompletedScan.Overlap != "unknown" {
		t.Fatalf("pre-migration read: %+v / %+v", f.Coverage, f.LastCompletedScan)
	}
	if f.State != graph.FreshnessNoKnownStaleness {
		t.Fatalf("pre-migration state changed: %s %v", f.State, f.Reasons)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f = freshness(t, s, repo.ID)
	if !f.Coverage.Recorded || f.Coverage.LastFullScan != nil || f.LastCompletedScan.Scope != "unknown" || f.LastCompletedScan.Overlap != "unknown" {
		t.Fatalf("after upgrade: %+v / %+v", f.Coverage, f.LastCompletedScan)
	}
	full, started := scopedScan(t, s, repo.ID, ScanScopeFull, "h1")
	completeScan(t, s, full, started, "completed", "h1")
	if got := freshness(t, s, repo.ID).Coverage.LastFullScan; got == nil || got.ID != full || got.Overlap != "none" {
		t.Fatalf("first recorded full scan after upgrade: %+v", got)
	}
}

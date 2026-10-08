package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/isink17/codegraph/internal/githistory"
	"github.com/isink17/codegraph/internal/graph"
)

// freshnessHeadTimeout bounds the one git call FreshnessStatus makes.
var freshnessHeadTimeout = 2 * time.Second

const freshnessUnknown = "unknown"

// FreshnessStatus reports what the stored rows can say about the graph's
// staleness. It never writes and never claims "fresh". Every row is read in
// one read-only transaction, so the scan, queue and history fields describe
// one snapshot. HEAD is then read with one bounded git call outside that
// snapshot; a commit landing between the two is not detectable here.
func (s *Store) FreshnessStatus(ctx context.Context, repoID int64) (graph.Freshness, error) {
	f, err := s.freshnessRows(ctx, repoID)
	if err != nil {
		return graph.Freshness{}, err
	}
	f.Worktree.HeadNow = freshnessUnknown
	headCtx, cancel := context.WithTimeout(ctx, freshnessHeadTimeout)
	defer cancel()
	if head, err := githistory.Head(headCtx, f.Worktree.CanonicalPath); err == nil && head != "" {
		f.Worktree.HeadNow = head
	}
	f.Worktree.HeadChanged = freshnessUnknown
	if f.Worktree.HeadAtIndex != freshnessUnknown && f.Worktree.HeadNow != freshnessUnknown {
		f.Worktree.HeadChanged = "no"
		if f.Worktree.HeadAtIndex != f.Worktree.HeadNow {
			f.Worktree.HeadChanged = "yes"
		}
	}

	f.Reasons = []string{}
	if f.LastCompletedScan == nil {
		f.Reasons = append(f.Reasons, "never_completed")
	}
	if f.LatestScan != nil && f.LatestScan.Status == "failed" {
		f.Reasons = append(f.Reasons, "latest_scan_failed")
	}
	if f.RunningScans.AfterLastCompleted > 0 {
		f.Reasons = append(f.Reasons, "scan_running_or_abandoned")
	}
	if f.DirtyQueue.Queued+f.DirtyQueue.InFlight > 0 {
		f.Reasons = append(f.Reasons, "dirty_queue_nonempty")
	}
	if f.Worktree.HeadChanged == "yes" {
		f.Reasons = append(f.Reasons, "head_moved")
	}
	switch {
	case len(f.Reasons) > 0 && (f.LastCompletedScan != nil || len(f.Reasons) > 1):
		f.State = graph.FreshnessKnownStale
	case f.LastCompletedScan == nil:
		f.State = graph.FreshnessUnknown
	default:
		f.State = graph.FreshnessNoKnownStaleness
	}
	return f, nil
}

func (s *Store) freshnessRows(ctx context.Context, repoID int64) (graph.Freshness, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.Freshness{}, err
	}
	defer func() { _ = tx.Rollback() }()

	f := graph.Freshness{
		Watcher:    "not_in_this_process",
		Filesystem: "not_checked",
		RunningScans: graph.FreshnessRunningScans{
			Liveness: freshnessUnknown,
		},
		Worktree: graph.FreshnessWorktree{
			HeadAtIndex:       freshnessUnknown,
			HeadAtIndexSource: "none",
		},
	}
	if err := tx.QueryRowContext(ctx, `SELECT root_path, canonical_path FROM repos WHERE id = ?`, repoID).
		Scan(&f.Worktree.RepoRoot, &f.Worktree.CanonicalPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return graph.Freshness{}, fmt.Errorf("%w: repo %d", ErrRepoNotIndexed, repoID)
		}
		return graph.Freshness{}, err
	}

	// A read-only handle does not migrate, so a database from before scope
	// recording has none of its columns; it reads as unrecorded.
	var coverageColumns int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('scans') WHERE name IN ('scope', 'head_at_start', 'head_at_finish', 'overlapping_scans')`).Scan(&coverageColumns); err != nil {
		return graph.Freshness{}, err
	}
	f.Coverage.Recorded = coverageColumns == 4
	scanQuery := `SELECT id, scan_kind, status, started_at, COALESCE(finished_at, ''), error_text, '', '', '', NULL FROM scans WHERE repo_id = ?`
	if f.Coverage.Recorded {
		scanQuery = `SELECT id, scan_kind, status, started_at, COALESCE(finished_at, ''), error_text, scope, head_at_start, head_at_finish, overlapping_scans FROM scans WHERE repo_id = ?`
	}
	readScan := func(where string) (*graph.FreshnessScan, error) {
		var sc graph.FreshnessScan
		var overlap sql.NullInt64
		err := tx.QueryRowContext(ctx, scanQuery+where+` ORDER BY id DESC LIMIT 1`, repoID).
			Scan(&sc.ID, &sc.Kind, &sc.Status, &sc.StartedAt, &sc.FinishedAt, &sc.ErrorText, &sc.Scope, &sc.HeadAtStart, &sc.HeadAtFinish, &overlap)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if sc.Scope == "" {
			// Not recorded: watch_config runs never take paths; every other
			// kind may or may not.
			sc.Scope = freshnessUnknown
			if sc.Kind == "watch_config" {
				sc.Scope = "full"
			}
		}
		// A running row's count is final only once it closes.
		sc.Overlap = freshnessUnknown
		if overlap.Valid && sc.Status != "running" {
			sc.Overlap = "none"
			if overlap.Int64 > 0 {
				sc.Overlap = "yes"
			}
		}
		return &sc, nil
	}
	if f.LastCompletedScan, err = readScan(` AND status = 'completed'`); err != nil {
		return graph.Freshness{}, err
	}
	if f.LatestScan, err = readScan(``); err != nil {
		return graph.Freshness{}, err
	}
	if f.Coverage.Recorded {
		if f.Coverage.LastFullScan, err = readScan(` AND status = 'completed' AND scope = 'full'`); err != nil {
			return graph.Freshness{}, err
		}
		var lastFullID int64
		if f.Coverage.LastFullScan != nil {
			lastFullID = f.Coverage.LastFullScan.ID
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM scans WHERE repo_id = ? AND status = 'failed' AND id > ?`,
			repoID, lastFullID).Scan(&f.Coverage.FailedAfterLastFull); err != nil {
			return graph.Freshness{}, err
		}
	}
	var lastCompletedID int64
	if f.LastCompletedScan != nil {
		lastCompletedID = f.LastCompletedScan.ID
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(1), COALESCE(SUM(id > ?), 0) FROM scans WHERE repo_id = ? AND status = 'running'`,
		lastCompletedID, repoID).Scan(&f.RunningScans.Count, &f.RunningScans.AfterLastCompleted); err != nil {
		return graph.Freshness{}, err
	}

	var oldest sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(reason <> 'watch_inflight'), 0),
			COALESCE(SUM(reason = 'watch_inflight'), 0),
			MIN(CASE WHEN reason <> 'watch_inflight' THEN queued_at END)
		FROM dirty_files WHERE repo_id = ?`, repoID).
		Scan(&f.DirtyQueue.Queued, &f.DirtyQueue.InFlight, &oldest); err != nil {
		return graph.Freshness{}, err
	}
	f.DirtyQueue.OldestQueuedAt = oldest.String

	// A read-only handle on a database from before the history migration has
	// no table; that reads as no anchor, like an absent row.
	var tables int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='git_history_state'`).Scan(&tables); err != nil {
		return graph.Freshness{}, err
	}
	if tables > 0 {
		var status, watermark string
		err := tx.QueryRowContext(ctx, `SELECT status, watermark_sha FROM git_history_state WHERE repo_id = ?`, repoID).Scan(&status, &watermark)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return graph.Freshness{}, err
		}
		if err == nil && status != githistory.StatusAbsent && watermark != "" {
			f.Worktree.HeadAtIndex = watermark
			f.Worktree.HeadAtIndexSource = "git_history_watermark"
		}
	}
	return f, tx.Commit()
}

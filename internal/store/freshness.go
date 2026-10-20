package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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
// snapshot; a commit landing between the two is not detectable here. The scan
// lock is probed first and, when free, held across that read, so a running row
// it reads cannot belong to a live lock-aware scan.
func (s *Store) FreshnessStatus(ctx context.Context, repoID int64) (graph.Freshness, error) {
	probe, release := s.probeScanLock()
	f, otherRunning, reconcilePending, err := s.freshnessRows(ctx, repoID)
	release()
	if err != nil {
		return graph.Freshness{}, err
	}
	// A scan takes the lock before inserting its row, and the insert can
	// wait on a busy database, so a held lock counts as this repository's
	// writer even without a row. The lock is per database: beside another
	// repository's running rows the holder cannot be attributed.
	switch {
	case probe == scanLockHeld && otherRunning == 0:
		f.RunningScans.Liveness = "active"
	case probe == scanLockFree && f.RunningScans.Count > 0:
		f.RunningScans.Liveness = "abandoned"
	case probe == scanLockFree:
		f.RunningScans.Liveness = "none"
	}
	f.ResolverPolicy = freshnessUnknown
	if pending, err := s.PendingResolverPolicies(ctx, repoID); err == nil {
		f.ResolverPolicyPending = append([]string{}, pending...)
		f.ResolverPolicy = "converged"
		if len(pending) > 0 {
			f.ResolverPolicy = "pending"
		}
	}
	f.ParserProfile = freshnessUnknown
	f.ParserProfileCompatibility = graph.FreshnessParserCompatibility{State: freshnessUnknown, Reason: "provenance_unreadable", Languages: []graph.FreshnessParserLanguage{}}
	if groups, err := s.FileParserProfileGroups(ctx, repoID); err == nil {
		f.ParserProfileUnconverged = unconvergedParserProfiles(groups)
		f.ParserProfile = "converged"
		if len(f.ParserProfileUnconverged) > 0 {
			f.ParserProfile = "unconverged"
		}
		f.ParserProfileCompatibility.Reason = "no_parser_registry"
		if current := s.parserProfiles.Load(); current != nil {
			f.ParserProfileCompatibility = parserProfileCompatibility(groups, *current)
		}
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
	if f.RunningScans.AfterLastCompleted > 0 || f.RunningScans.Liveness == "active" {
		f.Reasons = append(f.Reasons, "scan_running_or_abandoned")
	}
	if f.DirtyQueue.Queued+f.DirtyQueue.InFlight > 0 {
		f.Reasons = append(f.Reasons, "dirty_queue_nonempty")
	}
	if f.Worktree.HeadChanged == "yes" {
		f.Reasons = append(f.Reasons, "head_moved")
	}
	var unverified bool
	f.LiveWatcher, unverified = s.watcherStatus(repoID)
	if reconcilePending || unverified {
		f.Reasons = append(f.Reasons, "reconcile_pending")
	}
	if len(f.ParserProfileUnconverged) > 0 {
		f.Reasons = append(f.Reasons, "parser_profile_unconverged")
	}
	if f.ParserProfileCompatibility.State == parserCompatObsolete {
		f.Reasons = append(f.Reasons, "parser_profile_obsolete")
	}
	switch {
	case len(f.Reasons) > 0 && (f.LastCompletedScan != nil || len(f.Reasons) > 1):
		f.State = graph.FreshnessKnownStale
	case f.LastCompletedScan == nil:
		f.State = graph.FreshnessUnknown
	default:
		f.State = graph.FreshnessNoKnownStaleness
	}
	f.Verdict = freshnessVerdict(f, probe == scanLockHeld || probe == scanLockError)
	return f, nil
}

// unconvergedParserProfiles returns the languages a scan left part-way
// through a parser-profile transition, or indexed before profiles were
// recorded: a scan converges every file of a language to one profile.
func unconvergedParserProfiles(groups []FileParserProfileGroup) []string {
	first := map[string]string{}
	out := []string{}
	for _, g := range groups {
		if g.Language == "" || g.Files == 0 {
			continue
		}
		seen, ok := first[g.Language]
		if !ok {
			first[g.Language] = g.Profile
		}
		if (g.Profile == "" || (ok && seen != g.Profile)) && !slices.Contains(out, g.Language) {
			out = append(out, g.Language)
		}
	}
	slices.Sort(out)
	return out
}

// lockUncertain is true when the lock is held or could not be probed.
func freshnessVerdict(f graph.Freshness, lockUncertain bool) string {
	switch {
	case f.LastCompletedScan == nil:
		return graph.FreshnessVerdictIndexIncomplete
	case f.RunningScans.Liveness == "active":
		return graph.FreshnessVerdictWriterActive
	case f.RunningScans.Liveness == "abandoned":
		return graph.FreshnessVerdictAbandonedScan
	case f.RunningScans.Count > 0 || lockUncertain:
		return graph.FreshnessVerdictLivenessUnknown
	case len(f.Reasons) > 0:
		return graph.FreshnessVerdictStaleKnown
	default:
		return graph.FreshnessVerdictConsistentWithLastScan
	}
}

func (s *Store) freshnessRows(ctx context.Context, repoID int64) (f graph.Freshness, otherRunning int64, reconcilePending bool, err error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.Freshness{}, 0, false, err
	}
	defer func() { _ = tx.Rollback() }()

	f = graph.Freshness{
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
			return graph.Freshness{}, 0, false, fmt.Errorf("%w: repo %d", ErrRepoNotIndexed, repoID)
		}
		return graph.Freshness{}, 0, false, err
	}

	// A read-only handle does not migrate, so a database from before scope
	// recording has none of its columns; it reads as unrecorded.
	var coverageColumns int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('scans') WHERE name IN ('scope', 'head_at_start', 'head_at_finish', 'overlapping_scans')`).Scan(&coverageColumns); err != nil {
		return graph.Freshness{}, 0, false, err
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
		return graph.Freshness{}, 0, false, err
	}
	if f.LatestScan, err = readScan(``); err != nil {
		return graph.Freshness{}, 0, false, err
	}
	if f.Coverage.Recorded {
		if f.Coverage.LastFullScan, err = readScan(` AND status = 'completed' AND scope = 'full'`); err != nil {
			return graph.Freshness{}, 0, false, err
		}
		// A failed scan counts when it started after the last full scan began
		// or closed at or after the moment it began: either way it was running
		// while the full scan read the tree, or after it, so its partial writes
		// may postdate what the full scan saw. Timestamps have one-second
		// resolution, so a failure closing in the second the full scan began
		// counts too.
		var lastFullID int64
		lastFullStarted := ""
		if f.Coverage.LastFullScan != nil {
			lastFullID = f.Coverage.LastFullScan.ID
			lastFullStarted = f.Coverage.LastFullScan.StartedAt
		}
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(1) FROM scans
			WHERE repo_id = ? AND status = 'failed' AND (id > ? OR (? <> '' AND COALESCE(finished_at, '') >= ?))`,
			repoID, lastFullID, lastFullStarted, lastFullStarted).Scan(&f.Coverage.FailedAfterLastFull); err != nil {
			return graph.Freshness{}, 0, false, err
		}
	}
	var lastCompletedID int64
	if f.LastCompletedScan != nil {
		lastCompletedID = f.LastCompletedScan.ID
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(1), COALESCE(SUM(id > ?), 0) FROM scans WHERE repo_id = ? AND status = 'running'`,
		lastCompletedID, repoID).Scan(&f.RunningScans.Count, &f.RunningScans.AfterLastCompleted); err != nil {
		return graph.Freshness{}, 0, false, err
	}

	var oldest sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(reason <> 'watch_inflight'), 0),
			COALESCE(SUM(reason = 'watch_inflight'), 0),
			MIN(CASE WHEN reason <> 'watch_inflight' THEN queued_at END)
		FROM dirty_files WHERE repo_id = ?`, repoID).
		Scan(&f.DirtyQueue.Queued, &f.DirtyQueue.InFlight, &oldest); err != nil {
		return graph.Freshness{}, 0, false, err
	}
	f.DirtyQueue.OldestQueuedAt = oldest.String

	// A read-only handle on a database from before the history migration has
	// no table; that reads as no anchor, like an absent row.
	var tables int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='git_history_state'`).Scan(&tables); err != nil {
		return graph.Freshness{}, 0, false, err
	}
	if tables > 0 {
		var status, watermark string
		err := tx.QueryRowContext(ctx, `SELECT status, watermark_sha FROM git_history_state WHERE repo_id = ?`, repoID).Scan(&status, &watermark)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return graph.Freshness{}, 0, false, err
		}
		if err == nil && status != githistory.StatusAbsent && watermark != "" {
			f.Worktree.HeadAtIndex = watermark
			f.Worktree.HeadAtIndexSource = "git_history_watermark"
		}
	}
	var marker int64
	err = tx.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM settings WHERE key = ?`, reconcilePendingKey(repoID)).Scan(&marker)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return graph.Freshness{}, 0, false, err
	}
	reconcilePending = err == nil && (f.Coverage.LastFullScan == nil || f.Coverage.LastFullScan.ID <= marker)
	// The scan lock is per database, which may hold other repositories.
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM scans WHERE repo_id <> ? AND status = 'running'`, repoID).Scan(&otherRunning); err != nil {
		return graph.Freshness{}, 0, false, err
	}
	return f, otherRunning, reconcilePending, tx.Commit()
}

func reconcilePendingKey(repoID int64) string {
	return fmt.Sprintf("reconcile_pending_after_scan:%d", repoID)
}

// MarkReconcilePending records that the graph may be missing changes no scan
// has seen, such as those made while a watcher was down or degraded. Freshness
// reports reconcile_pending until a full scan that begins after this call
// completes, whichever process runs it.
func (s *Store) MarkReconcilePending(ctx context.Context, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings(key, value) VALUES(?, (SELECT COALESCE(MAX(id), 0) FROM scans))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, reconcilePendingKey(repoID))
	return err
}

package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/isink17/codegraph/internal/githistory"
)

// GitHistoryState returns the stored repository-level history state.
func (s *Store) GitHistoryState(ctx context.Context, repoID int64) (githistory.State, bool, error) {
	var st githistory.State
	err := s.db.QueryRowContext(ctx, `
		SELECT status, absent_reason, watermark_sha, watermark_committer_time, window_limit, window_commits, algorithm
		FROM git_history_state WHERE repo_id=?`, repoID).Scan(
		&st.Status, &st.AbsentReason, &st.Watermark, &st.WatermarkTime, &st.WindowLimit, &st.WindowCommits, &st.Algorithm)
	if errors.Is(err, sql.ErrNoRows) {
		return githistory.State{}, false, nil
	}
	return st, err == nil, err
}

// ReplaceGitHistory stores one history evaluation atomically. files == nil
// keeps the stored per-file aggregates (the watermark did not move); any
// non-nil slice, empty included, replaces them. Worktree changes are always
// replaced.
func (s *Store) ReplaceGitHistory(ctx context.Context, repoID int64, state githistory.State, files []githistory.FileStats, changes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO git_history_state(repo_id, status, absent_reason, watermark_sha, watermark_committer_time, window_limit, window_commits, algorithm)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id) DO UPDATE SET status=excluded.status, absent_reason=excluded.absent_reason,
			watermark_sha=excluded.watermark_sha, watermark_committer_time=excluded.watermark_committer_time,
			window_limit=excluded.window_limit, window_commits=excluded.window_commits, algorithm=excluded.algorithm`,
		repoID, state.Status, state.AbsentReason, state.Watermark, state.WatermarkTime, state.WindowLimit, state.WindowCommits, state.Algorithm); err != nil {
		return err
	}
	if files != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM git_file_history WHERE repo_id=?`, repoID); err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO git_file_history(repo_id, path, commit_count, first_commit_sha, first_committer_time,
				last_commit_sha, last_committer_time, author_count, top_author, top_author_commits,
				lines_added, lines_deleted, revert_count)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, f := range files {
			if _, err := stmt.ExecContext(ctx, repoID, f.Path, f.Commits, f.FirstSHA, f.FirstTime, f.LastSHA, f.LastTime,
				f.Authors, f.TopAuthor, f.TopAuthorCommits, f.LinesAdded, f.LinesDeleted, f.Reverts); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM git_worktree_changes WHERE repo_id=?`, repoID); err != nil {
		return err
	}
	for _, p := range changes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO git_worktree_changes(repo_id, path) VALUES(?, ?)`, repoID, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GitCommitRef names one commit of the history window.
type GitCommitRef struct {
	SHA           string `json:"sha"`
	CommitterTime int64  `json:"committer_time"`
}

// GitFileHistory is the public per-file history row.
type GitFileHistory struct {
	Path string `json:"path"`
	// Indexed reports whether the path is a live file of the semantic graph.
	Indexed bool `json:"indexed"`
	// WorktreeDiffers reports that the working-tree content (what the graph
	// was built from) differs from the watermark commit, or is untracked.
	WorktreeDiffers  bool          `json:"worktree_differs"`
	CommitCount      int           `json:"commit_count"`
	FirstCommit      *GitCommitRef `json:"first_commit,omitempty"`
	LastCommit       *GitCommitRef `json:"last_commit,omitempty"`
	AuthorCount      int           `json:"author_count"`
	TopAuthor        string        `json:"top_author,omitempty"`
	TopAuthorCommits int           `json:"top_author_commits,omitempty"`
	LinesAdded       int64         `json:"lines_added"`
	LinesDeleted     int64         `json:"lines_deleted"`
	RevertCount      int           `json:"revert_count"`
}

// GitHistoryQuery selects rows: explicit Paths, or a page of every path with
// history or a worktree change, optionally under PathPrefix.
type GitHistoryQuery struct {
	Paths      []string
	PathPrefix string
	Limit      int
	Offset     int
}

// GitHistoryResult is the shared CLI and MCP answer.
type GitHistoryResult struct {
	History githistory.State `json:"history"`
	Files   []GitFileHistory `json:"files"`
	// Total counts the rows the listing could page through; explicit paths
	// report one row each.
	Total  int `json:"total"`
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

const gitFileHistorySelect = `
	SELECT p.path, COALESCE(h.commit_count, 0), COALESCE(h.first_commit_sha, ''), COALESCE(h.first_committer_time, 0),
		COALESCE(h.last_commit_sha, ''), COALESCE(h.last_committer_time, 0), COALESCE(h.author_count, 0),
		COALESCE(h.top_author, ''), COALESCE(h.top_author_commits, 0), COALESCE(h.lines_added, 0),
		COALESCE(h.lines_deleted, 0), COALESCE(h.revert_count, 0),
		w.path IS NOT NULL,
		EXISTS(SELECT 1 FROM files f WHERE f.repo_id=?1 AND f.path=p.path AND f.is_deleted=0)
	FROM paths p
	LEFT JOIN git_file_history h ON h.repo_id=?1 AND h.path=p.path
	LEFT JOIN git_worktree_changes w ON w.repo_id=?1 AND w.path=p.path`

// GitHistory answers file_history. Listing order is commit count descending,
// then path: a total order, since paths are unique.
func (s *Store) GitHistory(ctx context.Context, repoID int64, q GitHistoryQuery) (GitHistoryResult, error) {
	state, found, err := s.GitHistoryState(ctx, repoID)
	if err != nil {
		return GitHistoryResult{}, err
	}
	if !found {
		state = githistory.Absent(githistory.ReasonNotComputed)
	}
	out := GitHistoryResult{History: state, Files: []GitFileHistory{}}
	if state.Status == githistory.StatusAbsent {
		return out, nil
	}
	if len(q.Paths) > 0 {
		for _, p := range q.Paths {
			rows, err := s.db.QueryContext(ctx, `WITH paths(path) AS (SELECT ?2)`+gitFileHistorySelect, repoID, CanonicalRelPath(p))
			if err != nil {
				return GitHistoryResult{}, err
			}
			got, err := scanGitFileHistory(rows)
			if err != nil {
				return GitHistoryResult{}, err
			}
			out.Files = append(out.Files, got...)
		}
		out.Total = len(out.Files)
		return out, nil
	}
	const paths = `WITH paths(path) AS (
		SELECT path FROM git_file_history WHERE repo_id=?1
		UNION SELECT path FROM git_worktree_changes WHERE repo_id=?1)`
	const prefix = ` WHERE ?2 = '' OR substr(p.path, 1, length(?2)) = ?2`
	pathPrefix := CanonicalRelPath(q.PathPrefix)
	if err := s.db.QueryRowContext(ctx, paths+` SELECT COUNT(*) FROM paths p`+prefix, repoID, pathPrefix).Scan(&out.Total); err != nil {
		return GitHistoryResult{}, err
	}
	out.Limit, out.Offset = safeLimit(q.Limit), safeOffset(q.Offset)
	rows, err := s.db.QueryContext(ctx, paths+gitFileHistorySelect+prefix+`
		ORDER BY COALESCE(h.commit_count, 0) DESC, p.path ASC LIMIT ?3 OFFSET ?4`, repoID, pathPrefix, out.Limit, out.Offset)
	if err != nil {
		return GitHistoryResult{}, err
	}
	got, err := scanGitFileHistory(rows)
	if err != nil {
		return GitHistoryResult{}, err
	}
	out.Files = append(out.Files, got...)
	return out, nil
}

func scanGitFileHistory(rows *sql.Rows) ([]GitFileHistory, error) {
	defer rows.Close()
	var out []GitFileHistory
	for rows.Next() {
		var f GitFileHistory
		var first, last GitCommitRef
		if err := rows.Scan(&f.Path, &f.CommitCount, &first.SHA, &first.CommitterTime, &last.SHA, &last.CommitterTime,
			&f.AuthorCount, &f.TopAuthor, &f.TopAuthorCommits, &f.LinesAdded, &f.LinesDeleted, &f.RevertCount,
			&f.WorktreeDiffers, &f.Indexed); err != nil {
			return nil, err
		}
		if f.CommitCount > 0 {
			f.FirstCommit, f.LastCommit = &first, &last
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

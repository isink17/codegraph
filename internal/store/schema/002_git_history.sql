-- File-level Git history enrichment. Never read by symbol, edge or reference
-- resolution: these tables describe commits, not the semantic graph.

CREATE TABLE git_history_state (
    repo_id INTEGER PRIMARY KEY,
    status TEXT NOT NULL,
    absent_reason TEXT NOT NULL DEFAULT '',
    watermark_sha TEXT NOT NULL DEFAULT '',
    watermark_committer_time INTEGER NOT NULL DEFAULT 0,
    window_limit INTEGER NOT NULL DEFAULT 0,
    window_commits INTEGER NOT NULL DEFAULT 0,
    algorithm TEXT NOT NULL DEFAULT ''
);

CREATE TABLE git_file_history (
    repo_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    commit_count INTEGER NOT NULL,
    first_commit_sha TEXT NOT NULL,
    first_committer_time INTEGER NOT NULL,
    last_commit_sha TEXT NOT NULL,
    last_committer_time INTEGER NOT NULL,
    author_count INTEGER NOT NULL,
    top_author TEXT NOT NULL,
    top_author_commits INTEGER NOT NULL,
    lines_added INTEGER NOT NULL,
    lines_deleted INTEGER NOT NULL,
    revert_count INTEGER NOT NULL,
    PRIMARY KEY (repo_id, path)
);

-- Paths whose working-tree content differs from the watermark commit.
CREATE TABLE git_worktree_changes (
    repo_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    PRIMARY KEY (repo_id, path)
);

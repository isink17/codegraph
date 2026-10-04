-- Symbol-level Git history: the newest window commit git blame attributes to
-- the lines of each trusted symbol range at the watermark. Keyed by the range,
-- not by symbol rows, and never read by symbol, edge or reference resolution.
-- last_commit_sha is '' when every line was last touched before the window.

CREATE TABLE git_symbol_history (
    repo_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    start_line INTEGER NOT NULL,
    end_line INTEGER NOT NULL,
    last_commit_sha TEXT NOT NULL,
    last_committer_time INTEGER NOT NULL,
    last_author TEXT NOT NULL,
    PRIMARY KEY (repo_id, path, start_line, end_line)
);

-- Paths with a Git `filter` attribute (LFS, smudge/clean): their working-tree
-- lines need not be the blob lines blame reads, so they get no symbol rows.
CREATE TABLE git_filtered_paths (
    repo_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    PRIMARY KEY (repo_id, path)
);

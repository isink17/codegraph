CREATE TABLE IF NOT EXISTS session_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id    INTEGER NOT NULL,
    session_id TEXT    NOT NULL DEFAULT '',
    event_type TEXT    NOT NULL,  -- 'read', 'edit', 'decision', 'task', 'fact'
    key        TEXT    NOT NULL DEFAULT '',
    value      TEXT    NOT NULL DEFAULT '',
    metadata   TEXT    NOT NULL DEFAULT '{}',
    created_at TEXT    NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

CREATE INDEX IF NOT EXISTS idx_session_events_repo ON session_events(repo_id);
CREATE INDEX IF NOT EXISTS idx_session_events_session ON session_events(repo_id, session_id);
CREATE INDEX IF NOT EXISTS idx_session_events_type ON session_events(repo_id, event_type);
CREATE INDEX IF NOT EXISTS idx_session_events_key ON session_events(repo_id, key);

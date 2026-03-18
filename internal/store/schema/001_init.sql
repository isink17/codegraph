CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS repos (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    root_path TEXT NOT NULL,
    canonical_path TEXT NOT NULL UNIQUE,
    config_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS scans (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    scan_kind TEXT NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    status TEXT NOT NULL,
    files_seen INTEGER NOT NULL DEFAULT 0,
    files_changed INTEGER NOT NULL DEFAULT 0,
    files_deleted INTEGER NOT NULL DEFAULT 0,
    error_text TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    language TEXT NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    mtime_unix_ns INTEGER NOT NULL DEFAULT 0,
    content_sha256 TEXT NOT NULL DEFAULT '',
    parse_state TEXT NOT NULL DEFAULT 'pending',
    last_scan_id INTEGER NOT NULL DEFAULT 0,
    indexed_at TEXT NOT NULL DEFAULT '',
    is_deleted INTEGER NOT NULL DEFAULT 0,
    UNIQUE (repo_id, path),
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

CREATE INDEX IF NOT EXISTS idx_files_repo_path ON files(repo_id, path);

CREATE TABLE IF NOT EXISTS symbols (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    language TEXT NOT NULL,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    qualified_name TEXT NOT NULL,
    container_name TEXT NOT NULL DEFAULT '',
    signature TEXT NOT NULL DEFAULT '',
    visibility TEXT NOT NULL DEFAULT '',
    start_line INTEGER NOT NULL,
    start_col INTEGER NOT NULL,
    end_line INTEGER NOT NULL,
    end_col INTEGER NOT NULL,
    doc_summary TEXT NOT NULL DEFAULT '',
    stable_key TEXT NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE INDEX IF NOT EXISTS idx_symbols_repo_name ON symbols(repo_id, name);
CREATE INDEX IF NOT EXISTS idx_symbols_repo_qname ON symbols(repo_id, qualified_name);
CREATE INDEX IF NOT EXISTS idx_symbols_file_id ON symbols(file_id);
CREATE INDEX IF NOT EXISTS idx_symbols_repo_stable ON symbols(repo_id, stable_key);

CREATE VIRTUAL TABLE IF NOT EXISTS symbol_fts USING fts5(
    repo_id UNINDEXED,
    symbol_id UNINDEXED,
    name,
    qualified_name,
    signature,
    doc_summary
);

CREATE TABLE IF NOT EXISTS references_tbl (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    symbol_id INTEGER,
    ref_kind TEXT NOT NULL,
    name TEXT NOT NULL,
    qualified_name TEXT NOT NULL DEFAULT '',
    start_line INTEGER NOT NULL,
    start_col INTEGER NOT NULL,
    end_line INTEGER NOT NULL,
    end_col INTEGER NOT NULL,
    context_symbol_id INTEGER,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE INDEX IF NOT EXISTS idx_refs_repo_name ON references_tbl(repo_id, name);

CREATE TABLE IF NOT EXISTS edges (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    src_symbol_id INTEGER NOT NULL,
    dst_symbol_id INTEGER,
    dst_name TEXT NOT NULL DEFAULT '',
    edge_kind TEXT NOT NULL,
    evidence TEXT NOT NULL DEFAULT '',
    file_id INTEGER NOT NULL,
    line INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE INDEX IF NOT EXISTS idx_edges_repo_src ON edges(repo_id, src_symbol_id);
CREATE INDEX IF NOT EXISTS idx_edges_repo_dst ON edges(repo_id, dst_symbol_id);
CREATE INDEX IF NOT EXISTS idx_edges_repo_name ON edges(repo_id, dst_name);

CREATE TABLE IF NOT EXISTS file_imports (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    import_path TEXT NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE TABLE IF NOT EXISTS test_links (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    test_file_id INTEGER NOT NULL,
    test_symbol_id INTEGER,
    target_file_id INTEGER,
    target_symbol_id INTEGER,
    reason TEXT NOT NULL,
    score REAL NOT NULL DEFAULT 0,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (test_file_id) REFERENCES files(id)
);

CREATE TABLE IF NOT EXISTS file_tokens (
    file_id INTEGER NOT NULL,
    token TEXT NOT NULL,
    weight REAL NOT NULL,
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE INDEX IF NOT EXISTS idx_file_tokens_file_id ON file_tokens(file_id);
CREATE INDEX IF NOT EXISTS idx_file_tokens_token ON file_tokens(token);

CREATE TABLE IF NOT EXISTS symbol_tokens (
    symbol_id INTEGER NOT NULL,
    token TEXT NOT NULL,
    weight REAL NOT NULL,
    FOREIGN KEY (symbol_id) REFERENCES symbols(id)
);

CREATE INDEX IF NOT EXISTS idx_symbol_tokens_symbol_id ON symbol_tokens(symbol_id);
CREATE INDEX IF NOT EXISTS idx_symbol_tokens_token ON symbol_tokens(token);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS dirty_files (
    repo_id INTEGER NOT NULL,
    path TEXT NOT NULL,
    reason TEXT NOT NULL,
    queued_at TEXT NOT NULL,
    PRIMARY KEY (repo_id, path)
);

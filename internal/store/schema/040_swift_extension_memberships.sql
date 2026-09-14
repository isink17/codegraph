CREATE TABLE IF NOT EXISTS swift_extension_memberships (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    symbol_id INTEGER NOT NULL,
    target_qualified_name TEXT NOT NULL,
    extension_start_line INTEGER NOT NULL,
    extension_start_col INTEGER NOT NULL,
    extension_end_line INTEGER NOT NULL,
    extension_end_col INTEGER NOT NULL,
    is_generic INTEGER NOT NULL DEFAULT 0,
    is_constrained INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id),
    FOREIGN KEY (symbol_id) REFERENCES symbols(id)
);
CREATE INDEX IF NOT EXISTS idx_swift_extension_memberships_repo_file
    ON swift_extension_memberships(repo_id, file_id);
CREATE INDEX IF NOT EXISTS idx_swift_extension_memberships_symbol
    ON swift_extension_memberships(repo_id, symbol_id);

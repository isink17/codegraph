-- P22.21 Slice 1: syntax-proven scope evidence. No destination identity is
-- stored here; old raw file_imports remains for existing consumers.
CREATE TABLE IF NOT EXISTS file_scope_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    language TEXT NOT NULL,
    package_name TEXT NOT NULL DEFAULT '',
    UNIQUE(repo_id, file_id)
);

CREATE TABLE IF NOT EXISTS scope_import_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    language TEXT NOT NULL,
    source_specifier TEXT NOT NULL,
    imported_name TEXT NOT NULL DEFAULT '',
    local_name TEXT NOT NULL DEFAULT '',
    import_kind TEXT NOT NULL,
    wildcard INTEGER NOT NULL DEFAULT 0,
    is_static INTEGER NOT NULL DEFAULT 0,
    is_reexport INTEGER NOT NULL DEFAULT 0,
    is_namespace_export INTEGER NOT NULL DEFAULT 0,
    is_type_only INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_file_scope_evidence_repo_file
    ON file_scope_evidence(repo_id, file_id);
CREATE INDEX IF NOT EXISTS idx_scope_import_evidence_repo_source
    ON scope_import_evidence(repo_id, source_specifier);
CREATE INDEX IF NOT EXISTS idx_scope_import_evidence_repo_local
    ON scope_import_evidence(repo_id, local_name);
CREATE INDEX IF NOT EXISTS idx_scope_import_evidence_file
    ON scope_import_evidence(repo_id, file_id);

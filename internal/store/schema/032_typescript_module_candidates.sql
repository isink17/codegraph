-- P22.21: source-backed relative module candidates for bounded reverse lookup.
CREATE TABLE IF NOT EXISTS scope_module_candidate_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    source_file_id INTEGER NOT NULL,
    source_specifier TEXT NOT NULL,
    candidate_path TEXT NOT NULL,
    UNIQUE(repo_id, source_file_id, source_specifier, candidate_path)
);

CREATE INDEX IF NOT EXISTS idx_scope_module_candidates_repo_path
    ON scope_module_candidate_evidence(repo_id, candidate_path);
CREATE INDEX IF NOT EXISTS idx_scope_module_candidates_repo_source
    ON scope_module_candidate_evidence(repo_id, source_file_id);

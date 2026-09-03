-- P22.21: Rust module ownership for scoped imports and module declarations.
ALTER TABLE file_scope_evidence ADD COLUMN module_path TEXT NOT NULL DEFAULT '';
ALTER TABLE file_scope_evidence ADD COLUMN crate_root TEXT NOT NULL DEFAULT '';
ALTER TABLE scope_import_evidence ADD COLUMN owner_module TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_file_scope_evidence_repo_crate_file
    ON file_scope_evidence(repo_id, crate_root, file_id);

CREATE TABLE IF NOT EXISTS rust_module_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    owner_module TEXT NOT NULL,
    module_name TEXT NOT NULL,
    external_path TEXT NOT NULL DEFAULT '',
    is_inline INTEGER NOT NULL DEFAULT 0,
    visibility TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_rust_module_evidence_repo_owner
    ON rust_module_evidence(repo_id, file_id, owner_module);
CREATE INDEX IF NOT EXISTS idx_rust_module_evidence_repo_name
    ON rust_module_evidence(repo_id, module_name);

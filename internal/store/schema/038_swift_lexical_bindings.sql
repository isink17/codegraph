-- P22.60: range-bearing Swift lexical blockers. These facts identify a
-- spelling that is locally bound; they deliberately carry no destination.
CREATE TABLE IF NOT EXISTS swift_lexical_binding_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    binding_kind TEXT NOT NULL,
    owner_module TEXT NOT NULL DEFAULT '',
    scope_start_line INTEGER NOT NULL,
    scope_end_line INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_swift_lexical_binding_repo_file
    ON swift_lexical_binding_evidence(repo_id, file_id);
CREATE INDEX IF NOT EXISTS idx_swift_lexical_binding_repo_file_name
    ON swift_lexical_binding_evidence(repo_id, file_id, name);

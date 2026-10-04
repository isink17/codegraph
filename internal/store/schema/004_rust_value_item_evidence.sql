-- Module-level Rust declarations that bind no symbol but can still answer a
-- bare call: const, static and extern-block items (kind 'decl', the declared
-- name) and item-level macro invocations whose expansion is not read (kind
-- 'macro', the macro as written). owner_module is spelled as an import's owner.
-- Syntax facts only: they name no target.

CREATE TABLE rust_value_item_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    owner_module TEXT NOT NULL,
    name TEXT NOT NULL,
    kind TEXT NOT NULL
);

CREATE INDEX idx_rust_value_item_evidence_repo_file
    ON rust_value_item_evidence(repo_id, file_id);

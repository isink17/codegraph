-- A Go local initialized from a call records the called function (and the
-- import it was qualified through); the function's file records its single
-- named result type. Rows written before this migration read as "no
-- initializer" and "no recorded result", which binds nothing.
ALTER TABLE go_local_binding_evidence ADD COLUMN init_function TEXT NOT NULL DEFAULT '';
ALTER TABLE go_local_binding_evidence ADD COLUMN init_import_path TEXT NOT NULL DEFAULT '';

CREATE TABLE go_function_result_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    function_name TEXT NOT NULL,
    type_name TEXT NOT NULL,
    type_import_path TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE INDEX idx_go_function_result_evidence_repo_file
    ON go_function_result_evidence(repo_id, file_id);

CREATE INDEX idx_go_local_binding_evidence_init
    ON go_local_binding_evidence(repo_id, init_function) WHERE init_function <> '';

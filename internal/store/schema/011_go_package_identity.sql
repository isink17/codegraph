-- A Go file's package clause and build constraint, written by the parser. A Go
-- file without a row (indexed before this migration, or a header that did not
-- parse) has no package identity, never an empty one.
CREATE TABLE go_file_identity_evidence (
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    package_clause TEXT NOT NULL,
    build_constraint TEXT NOT NULL,
    PRIMARY KEY (repo_id, file_id),
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

-- Every go.mod and go.work of the repository as the last completed scan read
-- them. module_path is empty for a go.mod without a module directive. The
-- settings key go.module_evidence.<repo_id> marks that a scan recorded them;
-- without it, a repository's Go module identity is unknown.
CREATE TABLE go_module_evidence (
    repo_id INTEGER NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('module', 'workspace')),
    root TEXT NOT NULL,
    module_path TEXT NOT NULL DEFAULT '',
    local_replace INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_id, kind, root),
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

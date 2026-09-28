CREATE TABLE IF NOT EXISTS kotlin_jvm_name_evidence (
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    symbol_id INTEGER NOT NULL,
    is_known INTEGER NOT NULL CHECK (is_known IN (0, 1)),
    jvm_name TEXT,
    CHECK (
        (is_known = 1 AND jvm_name IS NOT NULL AND jvm_name != '')
        OR (is_known = 0 AND jvm_name IS NULL)
    ),
    PRIMARY KEY (repo_id, symbol_id),
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id),
    FOREIGN KEY (symbol_id) REFERENCES symbols(id)
);
CREATE INDEX IF NOT EXISTS idx_kotlin_jvm_name_repo_file
    ON kotlin_jvm_name_evidence(repo_id, file_id);

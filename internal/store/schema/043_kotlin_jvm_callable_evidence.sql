CREATE TABLE IF NOT EXISTS kotlin_jvm_callable_evidence (
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    symbol_id INTEGER NOT NULL,
    is_known INTEGER NOT NULL CHECK (is_known IN (0, 1)),
    jvm_arity_min INTEGER,
    jvm_arity_max INTEGER,
    CHECK (
        (is_known = 1 AND jvm_arity_min IS NOT NULL AND jvm_arity_max IS NOT NULL
            AND 0 <= jvm_arity_min AND jvm_arity_min <= jvm_arity_max)
        OR (is_known = 0 AND jvm_arity_min IS NULL AND jvm_arity_max IS NULL)
    ),
    PRIMARY KEY (repo_id, symbol_id),
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id),
    FOREIGN KEY (symbol_id) REFERENCES symbols(id)
);
CREATE INDEX IF NOT EXISTS idx_kotlin_jvm_callable_repo_file
    ON kotlin_jvm_callable_evidence(repo_id, file_id);

CREATE TABLE IF NOT EXISTS symbol_embeddings (
    symbol_id   INTEGER PRIMARY KEY,
    file_id     INTEGER NOT NULL,
    repo_id     INTEGER NOT NULL,
    embedding   BLOB    NOT NULL,
    dimensions  INTEGER NOT NULL,
    model_name  TEXT    NOT NULL DEFAULT '',
    updated_at  TEXT    NOT NULL,
    FOREIGN KEY (symbol_id) REFERENCES symbols(id),
    FOREIGN KEY (file_id)   REFERENCES files(id)
);

CREATE INDEX IF NOT EXISTS idx_symbol_embeddings_repo ON symbol_embeddings(repo_id);
CREATE INDEX IF NOT EXISTS idx_symbol_embeddings_file ON symbol_embeddings(file_id);

-- Speed up per-file graph deletion during re-index/update.
-- These paths are hot for "changed file" indexing where we delete old graph rows by file_id.

CREATE INDEX IF NOT EXISTS idx_edges_file_id
ON edges(file_id);

CREATE INDEX IF NOT EXISTS idx_refs_file_id
ON references_tbl(file_id);

CREATE INDEX IF NOT EXISTS idx_file_imports_file_id
ON file_imports(file_id);

CREATE INDEX IF NOT EXISTS idx_test_links_test_file_id
ON test_links(test_file_id);

-- These already exist in earlier migrations, but are essential for the batch delete path.
CREATE INDEX IF NOT EXISTS idx_symbols_file_id
ON symbols(file_id);

CREATE INDEX IF NOT EXISTS idx_file_tokens_file_id
ON file_tokens(file_id);

CREATE INDEX IF NOT EXISTS idx_symbol_embeddings_file
ON symbol_embeddings(file_id);

-- Speed up edge resolution strategies that filter by (repo_id, name) and then by kind.
CREATE INDEX IF NOT EXISTS idx_symbols_repo_name_kind
ON symbols(repo_id, name, kind);

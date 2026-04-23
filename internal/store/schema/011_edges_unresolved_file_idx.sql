-- Speeds up ResolveEdgesForPaths by restricting unresolved-edge lookups to a file_id subset.
-- The existing idx_edges_repo_dst_file helps, but a file_id-leading partial index performs
-- better for path-scoped updates where file_id filtering is the primary selector.
CREATE INDEX IF NOT EXISTS idx_edges_repo_file_unresolved
ON edges(repo_id, file_id)
WHERE dst_symbol_id IS NULL;


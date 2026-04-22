-- Supports ResolveEdges' multi-pass UPDATE strategies by tailoring the index to
-- the unresolved-edge working set (dst_symbol_id IS NULL).
CREATE INDEX IF NOT EXISTS idx_edges_repo_unresolved_name
ON edges(repo_id, dst_name)
WHERE dst_symbol_id IS NULL;

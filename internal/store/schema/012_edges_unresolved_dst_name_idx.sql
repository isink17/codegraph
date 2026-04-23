-- Helps targeted cross-file edge resolution (ResolveEdgesForNames) quickly find
-- exact dst_name matches without scanning all unresolved edges.
CREATE INDEX IF NOT EXISTS idx_edges_repo_unresolved_dst_name
ON edges(repo_id, dst_name)
WHERE dst_symbol_id IS NULL AND dst_name != '';


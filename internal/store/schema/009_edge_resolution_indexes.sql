-- Supports ResolveEdges' multi-pass UPDATE strategies by making unresolved dst_name lookups
-- (repo_id + dst_symbol_id IS NULL + dst_name) indexable.
CREATE INDEX IF NOT EXISTS idx_edges_repo_unresolved_name
ON edges(repo_id, dst_symbol_id, dst_name);


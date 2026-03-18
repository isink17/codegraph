CREATE INDEX IF NOT EXISTS idx_edges_repo_dst_file
ON edges(repo_id, dst_symbol_id, file_id);

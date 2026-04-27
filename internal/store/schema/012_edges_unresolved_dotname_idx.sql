-- Speeds up ResolveEdgesForNames' qualified dst_name scan by shrinking the
-- unresolved-edge working set to dotted (qualified-ish) names.
CREATE INDEX IF NOT EXISTS idx_edges_repo_unresolved_dotname
ON edges(repo_id, dst_name)
WHERE dst_symbol_id IS NULL AND instr(dst_name, '.') > 0;


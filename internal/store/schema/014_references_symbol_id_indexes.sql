-- Supports read-heavy queries that filter by symbol_id/context_symbol_id,
-- including FindDeadCode and cleanup paths.
CREATE INDEX IF NOT EXISTS idx_refs_repo_symbol_id
ON references_tbl(repo_id, symbol_id)
WHERE symbol_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_refs_repo_context_symbol_id
ON references_tbl(repo_id, context_symbol_id)
WHERE context_symbol_id IS NOT NULL;


-- Speeds up ResolveEdges strategies that filter by additional symbol predicates.
-- These are intentionally partial to keep the btree small on large repos.

-- Strategy 2: name match (unqualified) limited to callable/type-like kinds.
CREATE INDEX IF NOT EXISTS idx_symbols_repo_name_resolve_kind
ON symbols(repo_id, name)
WHERE kind IN ('function', 'method', 'class', 'type', 'struct', 'interface');

-- Strategy 4: method receiver match (container_name != '').
CREATE INDEX IF NOT EXISTS idx_symbols_repo_name_has_container
ON symbols(repo_id, name)
WHERE container_name != '';


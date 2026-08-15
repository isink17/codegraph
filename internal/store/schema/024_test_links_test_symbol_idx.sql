-- P22.2: RelatedTests gained a call-evidence branch that probes test_links by
-- the calling symbol (edges.src_symbol_id -> test_links.test_symbol_id). The
-- only existing index prefixed on repo_id alone for that probe
-- (idx_test_links_repo_test_file), which degrades to scanning every link row
-- per candidate edge. This makes the probe a point lookup.
CREATE INDEX IF NOT EXISTS idx_test_links_repo_test_symbol
ON test_links(repo_id, test_symbol_id);

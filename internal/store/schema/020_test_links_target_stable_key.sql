-- P10: deterministic two-pass indexing.
--
-- `target_stable_key` persists the *unresolved* target a parser derived for a
-- test link (`graph.TestLink.TargetStableKey`, e.g. `func:pkg::Helper`). Before
-- P10 this key was consumed inside the write transaction: the insert looked the
-- key up in `symbols` and stored the result, then threw the key away. A test
-- file written before its target file in the same indexing batch therefore
-- stored (NULL, NULL) permanently, because nothing remained to resolve later.
--
-- Persisting the key is what makes test-link resolution a Pass-2 operation, the
-- same shape `edges.dst_name` already gives regular edges: Pass 1 records the
-- fact, Pass 2 binds it against the completed symbol table.
--
-- Defaults to '' so rows written before this migration keep an honest "no
-- recorded key" value. There is deliberately no backfill: the key is a parser
-- output, not something recoverable from the stored graph. Re-running
-- `codegraph index`/`update` repopulates it.
--
-- The index supports the incremental Pass-2 scope
-- (`ResolveTestLinksForStableKeys`), which binds links whose target key is among
-- the symbols a changed batch introduced.

ALTER TABLE test_links ADD COLUMN target_stable_key TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_test_links_repo_target_key
ON test_links(repo_id, target_stable_key);

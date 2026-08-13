-- P4: explainable edge resolution.
--
-- `resolution_strategy` records *why* a destination was selected: the stable
-- name of the resolver strategy that bound `dst_symbol_id`. `resolution_confidence`
-- is a coarse tier derived from that strategy's evidence strength (see
-- edge_resolution.go for the exact mapping). Neither column participates in
-- candidate selection: P4 explains bindings, it does not change them.
--
-- Both default to '' so:
--   * unresolved edges (dst_symbol_id IS NULL) carry no resolution metadata, and
--   * rows written before this migration keep an honest "unknown" value.
--
-- There is deliberately no backfill. The strategy that bound a historical edge
-- is not recoverable from the current graph shape -- several strategies can
-- reach the same destination -- so inferring one would fabricate provenance.
-- Re-running `codegraph index`/`update` repopulates the columns from the actual
-- resolver run.
--
-- No index: nothing queries by strategy or confidence; these columns are only
-- ever read alongside an edge row already located by id/repo/src/dst.

ALTER TABLE edges ADD COLUMN resolution_strategy TEXT NOT NULL DEFAULT '';
ALTER TABLE edges ADD COLUMN resolution_confidence TEXT NOT NULL DEFAULT '';

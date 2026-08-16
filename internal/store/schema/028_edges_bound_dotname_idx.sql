-- P22.8: keep invalidateNameEvidenceBindings off a full table scan.
--
-- Every incremental update that reports a changed symbol name asks which BOUND
-- edges carry a qualified dst_name whose `.`-tail is one of those names.
-- Migration 012's partial index covers the UNRESOLVED population, which is the
-- opposite half of the table, so without this the query planner had nothing to
-- work with.
--
-- Measured on an indexed mitmproxy (56907 edges, 14440 of them bound, 4555 bound
-- and qualified), re-running ANALYZE each way:
--
--	index absent   `--SCAN edges                                   (56907 rows)
--	index present  `--SEARCH edges USING INDEX idx_edges_repo_dst_file
--	                                                               (14440 rows)
--
-- Note what that says: the planner does not name THIS index in the winning
-- plan. Its presence is nevertheless what moves the query off the scan, and the
-- effect is reproducible across repeated create/ANALYZE/drop/ANALYZE cycles on
-- two different repositories. The predicate is written to match the query's
-- exactly so it stays small and stays eligible; if a future SQLite picks it
-- directly the lookup is covering, since `id` comes from the rowid.
CREATE INDEX IF NOT EXISTS idx_edges_repo_bound_dotname
    ON edges(repo_id, dst_name)
    WHERE dst_symbol_id IS NOT NULL AND instr(dst_name, '.') > 0;

-- Case-insensitive prefilter index for the dot-suffix resolver strategy.
--
-- `resolveEdgesByDotSuffix` matches candidate symbols with
-- `qualified_name LIKE '%.' || dst_name`, which no index can serve, so the
-- strategy used to scan every symbol once per distinct unresolved name. This
-- index backs a *necessary* condition on `symbols.dot_tail2` (migration 017)
-- that narrows which symbols are worth testing for the dst_names that admit
-- one; the LIKE stays in the join and remains the authority on what actually
-- matches. See populateDotSuffixCandidates for the derivation, and for why
-- names carrying a LIKE wildcard in their last two segments keep the
-- unfiltered scan.
--
-- The comparison has to be NOCASE: SQLite's LIKE is ASCII case-insensitive, so
-- a BINARY equality would be a *stricter* condition and would drop real
-- matches. Migration 017's index is BINARY and cannot serve NOCASE, so it
-- cannot be reused here; it is kept as-is for the BINARY equality joins in the
-- other strategies.
--
-- Partial on `dot_tail2 != ''` for the same reason as 017: a symbol whose
-- after-slash portion has no '.' can never satisfy the LIKE pattern for a
-- wildcard-free multi-dot dst_name. The resolver repeats that predicate in the
-- query, which is what makes the partial index usable there.
--
-- One-time cost: on an existing database this builds one index over `symbols`
-- inside the migration transaction, in the same shape as 016/017/018.

CREATE INDEX IF NOT EXISTS idx_symbols_repo_dot_tail2_nocase
    ON symbols(repo_id, dot_tail2 COLLATE NOCASE)
    WHERE dot_tail2 != '';

-- Strategy 3a: indexed slash-suffix resolution.
--
-- `qualified_suffix` stores the substring of `qualified_name` after the last
-- '/' for slash-containing names, '' otherwise. This lets `resolveEdgesBySlashSuffix`
-- replace its full repo-wide symbols scan with an indexed equality JOIN
-- (tmp_needed.dst_name = symbols.qualified_suffix) under (repo_id, qualified_suffix).
--
-- Backfill uses a recursive CTE because SQLite has no `reverse()` /
-- last-occurrence builtin: each step strips one '/'-separated prefix until
-- none remain, and the terminal row (instr=0) is the after-last-slash value.
-- For non-slash qnames the column stays at its DEFAULT '' (and is excluded
-- from the partial index).

ALTER TABLE symbols ADD COLUMN qualified_suffix TEXT NOT NULL DEFAULT '';

WITH RECURSIVE strip(id, rest) AS (
    SELECT id, qualified_name FROM symbols WHERE instr(qualified_name, '/') > 0
    UNION ALL
    SELECT id, substr(rest, instr(rest, '/') + 1)
    FROM strip
    WHERE instr(rest, '/') > 0
)
UPDATE symbols
SET qualified_suffix = (
    SELECT s2.rest FROM strip s2
    WHERE s2.id = symbols.id AND instr(s2.rest, '/') = 0
)
WHERE instr(qualified_name, '/') > 0;

CREATE INDEX IF NOT EXISTS idx_symbols_repo_qsuffix
    ON symbols(repo_id, qualified_suffix)
    WHERE qualified_suffix != '';

-- Strategy 3c: indexed dot-suffix resolution (2-dot dst_names).
--
-- `dot_tail3` stores the last three dot-separated segments of `afterSlash`
-- when `afterSlash` has at least three dots (i.e., at least four
-- segments). It mirrors the predicate
-- `qualified_name LIKE '%.' || dst_name` in `resolveEdgesByDotSuffix`
-- restricted to 2-dot dst_names: those need exactly the last three segments
-- of `afterSlash` to match, AND there must be a preceding segment in
-- `afterSlash` (otherwise the leading wildcard `%` cannot bind any chars
-- before the leading dot of the LIKE pattern). When `afterSlash` has only
-- 2 or fewer dots, no multi-dot dst_name can ever match it via this
-- path — `dot_tail3` stays at its DEFAULT '' and is excluded from the
-- partial index.
--
-- Backfill walks two recursive CTEs. The first reuses migration 016's
-- `qualified_suffix` (or falls back to `qualified_name` for non-slash
-- names) so we don't restrip slashes. The second strips one
-- `<segment>.` prefix per step until the remaining tail has ≤1 dot
-- (i.e., we've stripped down to two segments); the terminal row's
-- `prev_rest` then holds the last three segments. Rows whose `afterSlash`
-- has fewer than three dots are filtered out by the seeding `multi_dot`
-- CTE so they never enter the recursion.
--
-- 2-dot dst_names are by far the dominant multi-dot case in real codebases
-- (e.g., `pkg.Sub.Func`); ≥3-dot dst_names are rare and continue to fall
-- through to the original LIKE-based path inside `resolveEdgesByDotSuffix`.

ALTER TABLE symbols ADD COLUMN dot_tail3 TEXT NOT NULL DEFAULT '';

WITH RECURSIVE
  after_slash(id, s) AS (
    SELECT id,
      CASE
        WHEN qualified_suffix != '' THEN qualified_suffix
        ELSE qualified_name
      END
    FROM symbols
  ),
  multi_dot(id, s) AS (
    SELECT id, s FROM after_slash
    WHERE length(s) - length(replace(s, '.', '')) >= 3
  ),
  strip_dot(id, rest, prev_rest) AS (
    SELECT id, s, '' FROM multi_dot
    UNION ALL
    SELECT id, substr(rest, instr(rest, '.') + 1), rest
    FROM strip_dot
    WHERE length(rest) - length(replace(rest, '.', '')) > 1
  )
UPDATE symbols
SET dot_tail3 = (
    SELECT prev_rest FROM strip_dot
    WHERE strip_dot.id = symbols.id
      AND length(strip_dot.rest) - length(replace(strip_dot.rest, '.', '')) <= 1
)
WHERE id IN (SELECT id FROM multi_dot);

CREATE INDEX IF NOT EXISTS idx_symbols_repo_dot_tail3
    ON symbols(repo_id, dot_tail3)
    WHERE dot_tail3 != '';

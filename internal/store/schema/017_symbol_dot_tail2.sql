-- Strategy 3b: indexed dot-tail2 resolution.
--
-- `dot_tail2` stores the last two dot-separated segments of `afterSlash`
-- (the substring after the last '/' for slash-containing names, the full
-- qualified_name for non-slash names). It mirrors the in-memory derivation
-- in `resolveEdgesBySlashSuffix`'s dot-tail2 branch, which previously
-- materialised every symbol row into Go and rebuilt this string per row
-- on every resolve pass. Persisting it lets the resolver replace that scan
-- with an indexed equality JOIN against `(repo_id, dot_tail2)`.
--
-- Backfill walks two recursive CTEs. The first reuses migration 016's
-- `qualified_suffix` (or falls back to `qualified_name` when no '/') so we
-- don't restrip slashes here. The second strips one '<segment>.' prefix
-- per step; the terminal row's `prev_rest` is exactly the last-2-segment
-- tail. Rows whose afterSlash has no '.' keep the column at its DEFAULT ''
-- and are excluded from the partial index.

ALTER TABLE symbols ADD COLUMN dot_tail2 TEXT NOT NULL DEFAULT '';

WITH RECURSIVE
  after_slash(id, s) AS (
    SELECT id,
      CASE
        WHEN qualified_suffix != '' THEN qualified_suffix
        ELSE qualified_name
      END
    FROM symbols
  ),
  strip_dot(id, rest, prev_rest) AS (
    SELECT id, s, '' FROM after_slash WHERE instr(s, '.') > 0
    UNION ALL
    SELECT id, substr(rest, instr(rest, '.') + 1), rest
    FROM strip_dot
    WHERE instr(rest, '.') > 0
  )
UPDATE symbols
SET dot_tail2 = (
    SELECT prev_rest FROM strip_dot
    WHERE strip_dot.id = symbols.id AND instr(strip_dot.rest, '.') = 0
)
WHERE id IN (SELECT id FROM after_slash WHERE instr(s, '.') > 0);

CREATE INDEX IF NOT EXISTS idx_symbols_repo_dot_tail2
    ON symbols(repo_id, dot_tail2)
    WHERE dot_tail2 != '';

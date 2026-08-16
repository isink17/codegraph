-- P22.6: retire pre-existing Go bare-name false positives.
--
-- A bare identifier in Go source names something in the calling file's own
-- package -- never "whatever package in the repository happens to declare that
-- name", and never a method, which Go spells through a receiver. The resolver
-- used to bind such a call to any unique repo-wide match, which wired a local
-- closure `countTags()` in package profile to an unrelated `graph.countTags`,
-- and a bare `write` to `store_test.reindexFixture.write`. No writer produces
-- those bindings any more; this clears the rows an existing database already
-- holds so it stops asserting them. Data repair only -- no schema change.
--
-- Scope is exactly the retired evidence class: a resolved edge in a Go source
-- file whose dst_name is bare (no '.', '/' or ':') and whose destination is
-- not a package-level declaration in the calling symbol's own Go package.
-- Package identity is (directory prefix, first dot-segment of qualified_name),
-- the same derivation go_package_scope.go applies -- so `pkg` and `pkg_test`
-- in one directory stay the different packages they are. Same-package
-- bindings, every non-Go language, and every qualifier-bearing spelling are
-- untouched, which makes the statement idempotent under the new rule.
--
-- Cleared edges are honestly unresolved, the same state a fresh full index
-- produces for them. Nothing is bound here: names that the new package-scoped
-- pass can answer bind the next time any resolve pass considers them (file
-- change, cross-file name pass, or reindex).
--
-- The assignment list mirrors resolverClearResolutionSQL: destination,
-- strategy, and confidence change together, never separately.

UPDATE edges
SET dst_symbol_id = NULL,
    resolution_strategy = '',
    resolution_confidence = ''
WHERE dst_symbol_id IS NOT NULL
  -- Cross-language links are inserted already bound by their own pass, from
  -- import-bridge evidence, and no resolver strategy ever considers them (every
  -- strategy matches `dst_symbol_id IS NULL`). Their dst_name is the target's
  -- bare name, so without this they would be cleared here and then deleted as
  -- obsolete by the next cross-language run.
  AND edge_kind <> 'cross_language_ref'
  AND instr(dst_name, '.') = 0
  AND instr(dst_name, '/') = 0
  AND instr(dst_name, ':') = 0
  AND EXISTS (
        SELECT 1 FROM files sf
        WHERE sf.id = edges.file_id AND sf.language = 'go'
      )
  AND NOT EXISTS (
        SELECT 1
        FROM symbols src
        JOIN files srcf ON srcf.id = src.file_id
        JOIN symbols cand ON cand.id = edges.dst_symbol_id
        JOIN files candf ON candf.id = cand.file_id
        WHERE src.id = edges.src_symbol_id
          AND src.language = 'go'
          AND cand.language = 'go'
          AND instr(cand.qualified_name, '.') > 1
          AND instr(src.qualified_name, '.') > 1
          AND cand.qualified_name =
              substr(cand.qualified_name, 1, instr(cand.qualified_name, '.') - 1)
              || '.' || cand.name
          AND rtrim(srcf.path, replace(replace(srcf.path, '/', ''), '\', ''))
              || substr(src.qualified_name, 1, instr(src.qualified_name, '.') - 1)
            = rtrim(candf.path, replace(replace(candf.path, '/', ''), '\', ''))
              || substr(cand.qualified_name, 1, instr(cand.qualified_name, '.') - 1)
      );

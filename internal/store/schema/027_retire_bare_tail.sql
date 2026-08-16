-- P22.8: retire the `bare_tail` evidence level from existing databases.
--
-- `bare_tail` was the Go-side binder's weakest fallback and existed only on the
-- incremental entry points, which is why a fresh full index and an incremental
-- update disagreed about the same tree. It has two populations, and they need
-- opposite repairs. Data repair only -- no schema change.
--
-- 1. UNSAFE: the destination was chosen by discarding evidence the edge carried.
--
--    a. An import-path spelling degraded to its last dot-segment:
--       `database/sql.Open` looked up `Open` and bound the project's own unique
--       `store.Open`. The import names the standard library; own-module import
--       paths are resolved from real evidence by `module_import`, and anything
--       else points outside the repository. Non-Go parsers also emit
--       slash-bearing spellings for ordinary expressions -- `(dir / "x").open` --
--       where the tail is not what the call names either. No writer produces
--       these any more (binderFallback).
--
--    b. A bare spelling bound a destination whose kind a call edge cannot
--       denote. The repo-wide bare-name strategy has always restricted
--       candidates to callable/type kinds; the binder did not, so it could bind
--       a value the full index would never have considered.
--
--    These rows are cleared. They become honestly unresolved -- the same state a
--    fresh full index produces for them -- and rebind if and only if some pass
--    later finds real evidence.
--
-- 2. SAFE, MISLABELLED: a bare spelling that bound a callable destination.
--
--    Both pipelines were looking at the same thing: equality on `symbols.name`,
--    binding only when the group held exactly one eligible candidate. If the
--    all-kinds group held exactly one symbol and that symbol IS a callable kind,
--    then the kind-restricted group holds exactly that one symbol too -- the
--    same destination the repo-wide strategy would have selected. So the
--    binding is kept and only its provenance is corrected; clearing it would
--    drop a correct edge until something happened to touch it.
--
--    Which label is correct depends on the SOURCE language. A bare spelling in
--    a Go file is answered by the package-scoped pass (P22.6), so a fresh index
--    calls it `go_package_scope`; every other language reaches the same
--    destination through the bare-name strategy and is `exact_name`. Both sit
--    in the `high` tier. Splitting them here is what keeps an upgraded database
--    byte-identical to a fresh index of the same tree rather than merely
--    equivalent.
--
--    This statement depends on migration 026 having run first, which it always
--    has: 026 already cleared every Go bare-name binding whose destination is
--    NOT a package-level declaration in the caller's own package, so the Go
--    rows still labelled `bare_tail` here are exactly the same-package ones
--    that `go_package_scope` would produce. Statement 1 above has already
--    removed every slash-bearing spelling, so no row reaching statement 2
--    carries a qualifier.
--
-- Both statements are idempotent: after the first run no `bare_tail` row is
-- left, so a re-run matches nothing. `cross_language_ref` edges are never
-- labelled `bare_tail` (their own pass writes their strategy), but the guard is
-- stated anyway so this file cannot become the exception that touches them.
--
-- The assignment lists mirror resolverClearResolutionSQL and
-- resolverSetResolvedSQL: destination, strategy and confidence always change
-- together, never separately.

-- 1. Clear the unsafe population.
UPDATE edges
SET dst_symbol_id = NULL,
    resolution_strategy = '',
    resolution_confidence = ''
WHERE resolution_strategy = 'bare_tail'
  AND dst_symbol_id IS NOT NULL
  AND edge_kind <> 'cross_language_ref'
  AND (
        instr(dst_name, '/') > 0
        OR NOT EXISTS (
              SELECT 1 FROM symbols cand
              WHERE cand.id = edges.dst_symbol_id
                AND cand.kind IN ('function', 'method', 'class', 'type', 'struct', 'interface')
           )
      );

-- 2. Relabel the safe population as what it always was, per source language.
UPDATE edges
SET resolution_strategy = CASE
        WHEN EXISTS (SELECT 1 FROM files sf WHERE sf.id = edges.file_id AND sf.language = 'go')
            THEN 'go_package_scope'
        ELSE 'exact_name'
    END,
    resolution_confidence = 'high'
WHERE resolution_strategy = 'bare_tail'
  AND dst_symbol_id IS NOT NULL
  AND edge_kind <> 'cross_language_ref';

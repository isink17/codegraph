-- P22.1: retire pre-existing member-call false positives.
--
-- The Go-side binder used to let a member/scope-qualified dst_name
-- (a '.' and no '/', e.g. `rows.Close`) fall back to its bare tail and bind
-- whatever unique same-language symbol carried that name -- which wired
-- external/stdlib receiver calls to unrelated project methods
-- (`strings.Builder.Grow` -> compactfmt.Document.Grow). The binder no longer
-- performs that bind; this clears the rows it already wrote so an existing
-- database stops asserting them. Data repair only -- no schema change.
--
-- Scope is exactly the retired evidence class: strategy `bare_tail` on a
-- dotted, slash-free dst_name. The new binder never produces that
-- combination (dotted slash-free names bind as dot_tail2/dot_tail3 or not at
-- all), so the statement is idempotent and touches nothing written under the
-- new rule. Import-path spellings (with '/') keep their bare_tail bindings --
-- that class is the deferred own-module import-mapping work.
--
-- Cleared edges are honestly unresolved, the same state a fresh full index
-- produces for them. The qualifier-confirmed subset re-binds via dot_tail2/3
-- the next time any resolve pass considers it (file change, cross-file name
-- pass, or reindex).
--
-- The assignment list mirrors resolverClearResolutionSQL: destination,
-- strategy, and confidence change together, never separately.

UPDATE edges
SET dst_symbol_id = NULL,
    resolution_strategy = '',
    resolution_confidence = ''
WHERE resolution_strategy = 'bare_tail'
  AND instr(dst_name, '.') > 0
  AND instr(dst_name, '/') = 0;

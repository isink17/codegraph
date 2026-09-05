-- P22.28: Go selector calls are interpreted differently, and nothing else
-- forces a reparse.
--
-- Before this migration a Go selector call `x.Method()` was rewritten to
-- `<import path>.Method` whenever `x` matched one of the file's import aliases,
-- with no check that `x` was actually the import binding. A parameter, receiver,
-- range variable or local named after an imported package therefore produced a
-- high-confidence `module_import` edge pointing into that package -- an edge no
-- Go compiler would agree with. The parsers now keep a locally bound qualifier
-- verbatim and record the file's lexical bindings as evidence.
--
-- A database written before this migration holds none of that evidence, and its
-- Go file contents are unchanged, so the content hash would never ask for those
-- files again. Dropping the stale bindings and clearing the hash makes the next
-- scan reparse every Go file and rebuild them.
--
-- The unbinding is deliberately wider than the miswire, in two directions.
--
-- It is not limited to the edges that were actually shadowed, because telling
-- those apart needs exactly the evidence this database does not yet have. And
-- it is not limited to `module_import`: the mis-rewritten `<import path>.Method`
-- spelling was offered to every strategy in turn, so when module_import did not
-- take it `exact_qualified`, `dot_tail3` or `dot_suffix` could, and those rows
-- are wrong for the same reason.
--
-- The correct edges are re-derived on the next scan from the same rules that
-- always produced them; the wrong ones are not. Until then the affected edges
-- read as unresolved, which is honest. Leaving a wrong high-confidence edge
-- queryable until a reparse happens to occur is the one outcome this migration
-- exists to prevent, and it is worth more than the recall this costs in the
-- window before the next scan.
CREATE TABLE IF NOT EXISTS go_local_binding_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL,
    file_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    scope_start_line INTEGER NOT NULL,
    scope_end_line INTEGER NOT NULL,
    type_name TEXT NOT NULL DEFAULT '',
    type_package TEXT NOT NULL DEFAULT '',
    type_import_path TEXT NOT NULL DEFAULT '',
    is_pointer INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (repo_id) REFERENCES repos(id),
    FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE INDEX IF NOT EXISTS idx_go_local_binding_repo_file
    ON go_local_binding_evidence(repo_id, file_id);

CREATE INDEX IF NOT EXISTS idx_go_local_binding_repo_file_name
    ON go_local_binding_evidence(repo_id, file_id, name);

DELETE FROM go_local_binding_evidence;

UPDATE edges
SET dst_symbol_id = NULL, resolution_strategy = '', resolution_confidence = ''
WHERE resolution_strategy IN (
        'module_import', 'go_receiver_scope',
        'exact_qualified', 'dot_tail2', 'dot_tail3', 'dot_suffix'
      )
  AND file_id IN (SELECT id FROM files WHERE language = 'go');

UPDATE files
SET content_sha256 = '', parse_state = 'pending'
WHERE language = 'go';

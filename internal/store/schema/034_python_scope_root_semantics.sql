-- P22.26-F2: Python scope evidence and resolution rules changed again.
--
-- Two persisted things are now different. Module candidate evidence is written
-- for the repository root alone, where before it was written for every ancestor
-- directory of the importing file, and a function's nested `def`/`class` names
-- are recorded as that function's own bindings. A database written before this
-- migration holds neither, and its file contents are unchanged, so the content
-- hash would never ask for those files again.
--
-- Dropping the stale evidence and clearing the hash makes the next scan reparse
-- every Python file and rebuild it. Bindings made from the old evidence are
-- cleared with it, so nothing survives that the new rules would not reach.
DELETE FROM scope_import_evidence WHERE language = 'python';

DELETE FROM scope_module_candidate_evidence
WHERE source_file_id IN (SELECT id FROM files WHERE language = 'python');

UPDATE edges
SET dst_symbol_id = NULL, resolution_strategy = '', resolution_confidence = ''
WHERE resolution_strategy IN ('python_import_scope', 'python_module_scope');

UPDATE files
SET content_sha256 = '', parse_state = 'pending'
WHERE language = 'python';

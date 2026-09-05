-- P22.26-F1: Python scope evidence changed shape, and nothing else forces a
-- reparse.
--
-- Three things about a Python file's persisted evidence are now different:
-- `import a.b` records the name it actually binds (`a`) rather than the dotted
-- path, every import carries the lexical scope it was written in, and each
-- scope's own bindings are recorded as negative evidence. A database written
-- before this migration holds none of that, and the resolver would silently
-- decide differently from a fresh index -- file contents are unchanged, so the
-- content hash would never ask for the file again.
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

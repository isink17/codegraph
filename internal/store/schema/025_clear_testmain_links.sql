-- P22.2: the producers no longer mint a test link for TestMain (the harness
-- hook tests nothing, and its guessed target key `func:<pkg>::Main` can bind a
-- real exported Main). Rows written by older producers survive in unchanged
-- files because unchanged content is never reparsed, so remove them here. The
-- join is by the persisted test symbol's name; rows whose test_symbol_id is
-- NULL cannot be identified and are left to the next reparse of their file.
DELETE FROM test_links
WHERE test_symbol_id IN (SELECT id FROM symbols WHERE name = 'TestMain');

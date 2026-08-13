-- P12: covering indexes for the hot read paths.
--
-- Each of the three changes below *replaces* an existing index with a wider
-- one that has the same leading columns and the same partiality. That keeps the
-- index count -- and therefore the per-row write cost of indexing -- flat, while
-- turning three measured table-lookup-per-row scans into covering index scans.
-- Every seek the dropped index could serve, its replacement serves from the
-- same prefix.
--
-- 1. edges: the FindCallers unresolved-name leg.
--
--    `FindCallers` unions bound edges with unresolved edges whose `dst_name`
--    spells the target. The `dst_name LIKE '%.'||short` half of that predicate
--    cannot seek, so the plan was
--        SEARCH e USING INDEX idx_edges_repo_src (repo_id=?)
--    i.e. walk every edge in the repository through a non-covering index and
--    fetch the row for each one, just to read `src_symbol_id`. On the 100k
--    fixture that is ~211k row fetches, and it cost ~32ms even when the answer
--    was three callers.
--
--    Adding `src_symbol_id` to the unresolved-name index makes the whole leg
--    covering, and the partial predicate restricts the walk to the unresolved
--    population (~31k rows in the fixture) instead of all edges.
--
--    `idx_edges_repo_unresolved_name` is (repo_id, dst_name) with the identical
--    WHERE clause, so it is a strict prefix of the replacement and its resolver
--    seeks are unaffected.
--
-- 2. symbols: the FindDeadCode ordering.
--
--    `FindDeadCode` orders by (f.path, s.start_line). SQLite drove it from
--    `idx_files_repo_path` -- already in path order -- but then had to sort each
--    file's symbols, because `idx_symbols_file_id` says nothing about
--    `start_line`:
--        SCAN f USING COVERING INDEX idx_files_repo_path
--        SEARCH s USING INDEX idx_symbols_file_id (file_id=?)
--        USE TEMP B-TREE FOR ORDER BY
--    The temp b-tree forces the query to evaluate the two NOT EXISTS
--    subqueries for all ~100k symbols before it can return the first of twenty
--    rows. Extending the index to (file_id, start_line) removes the sort, which
--    lets LIMIT stop the scan early.
--
--    `idx_symbols_file_id` is a strict prefix, and the per-file delete path
--    (`WHERE file_id IN (...)`) seeks on that same prefix.
--
-- 3. symbol_tokens: the token-overlap search.
--
--    `SemanticSearch` sums weights for matching tokens. `idx_symbol_tokens_token`
--    is (token) only, so every matching token row required a table fetch to read
--    `symbol_id` and `weight`. Both are now in the index, so the token scan is
--    covering.
--
-- No new index is introduced by this migration, and no index loses a seek it
-- previously served.
--
-- Cost note: the three CREATE INDEX statements run inside the migration's
-- single transaction, so on a multi-million-symbol database this is one
-- write-locked index build per statement, and the sorter runs with
-- temp_store = MEMORY on the default performance profile. It is a one-time
-- cost, in the same shape as migrations 016/017/018, but it is not free on a
-- very large existing graph.

DROP INDEX IF EXISTS idx_edges_repo_unresolved_name;

CREATE INDEX IF NOT EXISTS idx_edges_repo_unresolved_name_src
ON edges(repo_id, dst_name, src_symbol_id)
WHERE dst_symbol_id IS NULL;

DROP INDEX IF EXISTS idx_symbols_file_id;

CREATE INDEX IF NOT EXISTS idx_symbols_file_start
ON symbols(file_id, start_line);

DROP INDEX IF EXISTS idx_symbol_tokens_token;

CREATE INDEX IF NOT EXISTS idx_symbol_tokens_token_symbol
ON symbol_tokens(token, symbol_id, weight);

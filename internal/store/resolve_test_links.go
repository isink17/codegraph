package store

import (
	"context"
	"database/sql"
	"strings"
)

// Test-link target resolution (P10 Pass 2).
//
// A test link is a relationship, so it obeys the same staging rule every other
// relationship in CodeGraph obeys: Pass 1 persists the fact (which test symbol,
// which reason/score, and the parser's `target_stable_key`), Pass 2 binds it
// against the completed symbol table.
//
// Before P10 the binding happened inside the Pass-1 write transaction, which
// made it depend on whether the target file was persisted earlier in the same
// indexing batch. Because the indexer's file workers complete in a
// nondeterministic order, indexing `helper.go` and `helper_test.go` in one run
// bound the link only when `helper.go` won the race, and nothing ever backfilled
// the loser -- the key was not persisted. Persisting it (migration 020) and
// resolving here removes the order dependency entirely: a full index produces
// the same graph for every valid file processing order. (An incremental run is
// scoped to the changed batch, so it carries the same narrowing an incremental
// edge resolve already does -- see ResolveTestLinksForStableKeys.)
//
// Semantics, deliberately aligned with the edge resolver rather than reinvented:
//
//   - Language gate (P2): the target symbol's persisted language must equal the
//     *test file's* persisted language. A Go `func:pkg::Helper` and a Python
//     `func:pkg::Helper` are a name coincidence, not a relationship. Unknown
//     language on either side fails closed.
//   - Deterministic ambiguity refusal (P3): a key with more than one candidate
//     in the calling language binds nothing. Picking one would be a row-id
//     tie-break, which P3 forbids. Test links originate in test files by
//     construction, and P7 leaves a test caller on plain P2/P3 semantics -- so
//     "exactly one candidate" is also the P7-correct rule here, and no
//     production-over-test-shadow preference applies.
//   - Bind-only: these passes never clear an existing binding. Unbinding stale
//     targets stays where P6 put it, in the Pass-1 delete path
//     (deleteFileGraphsBatch), which nulls `target_symbol_id` while keeping
//     `target_file_id` so file-level RelatedTests survives; the purge path
//     (nullifyDeletedSymbolReferences) nulls both when the target file itself
//     goes away. Both keep the row and its key, so a subsequent Pass 2 rebinds
//     it if and only if the key resolves again.
//
// All three entry points are one set-based UPDATE per chunk. There is no
// per-link query and no symbol table materialised in Go.

// testLinkResolveChunkSize bounds how many scope values go into one statement.
//
// It is half the usual sqliteInClauseBatchSize because the scope predicate is
// rendered twice per statement (once for the candidate CTE, once for the
// UPDATE), so its arguments are bound twice: a chunk of N costs 2N+5 bound
// variables. 450 keeps the worst case under SQLITE_MAX_VARIABLE_NUMBER even on
// builds that still use the historical 999 limit.
const testLinkResolveChunkSize = sqliteInClauseBatchSize / 2

// testLinkScope narrows a resolve pass to part of the repo. `predicate` is
// rendered twice -- once for the candidate CTE (alias `tl`) and once for the
// UPDATE itself (alias `test_links`) -- so a scoped pass selects and writes the
// same set of rows. `args` is bound once per rendering, in that order.
//
// The zero value is the repo-wide scope.
type testLinkScope struct {
	predicate func(alias string) string
	args      []any
}

func (s testLinkScope) render(alias string) string {
	if s.predicate == nil {
		return ""
	}
	return s.predicate(alias)
}

// resolveTestLinksSQL binds unbound test links within a scope.
//
// The candidate relation is grouped by (target key, test-file language) and
// exposes an id pair that is NULL unless the group holds exactly one candidate,
// so the MIN()s are identity and never a tie-break. The UPDATE re-joins `files`
// to pin the candidate group to the link's own test-file language, which is what
// keeps a two-language collision on one stable key from binding either side.
func resolveTestLinksSQL(scope testLinkScope) string {
	return `
		WITH needed AS (
			SELECT DISTINCT tl.target_stable_key AS target_key, tf.language AS src_language
			FROM test_links tl
			JOIN files tf ON tf.id = tl.test_file_id
			WHERE tl.repo_id = ?
			  AND tl.target_symbol_id IS NULL
			  AND tl.target_stable_key != ''
			  AND tf.language != ''
			  ` + scope.render("tl") + `
		),
		candidates AS (
			SELECT n.target_key AS target_key, n.src_language AS src_language,
				CASE WHEN COUNT(*) = 1 THEN MIN(s.id) END AS target_symbol_id,
				CASE WHEN COUNT(*) = 1 THEN MIN(s.file_id) END AS target_file_id
			FROM needed n
			JOIN symbols s
			  ON s.repo_id = ?
			 AND s.stable_key = n.target_key
			 AND s.language = n.src_language
			GROUP BY n.target_key, n.src_language
		)
		UPDATE test_links
		SET target_symbol_id = c.target_symbol_id,
			target_file_id = c.target_file_id
		FROM candidates c, files f
		WHERE test_links.repo_id = ?
		  AND test_links.target_symbol_id IS NULL
		  AND test_links.target_stable_key = c.target_key
		  AND f.id = test_links.test_file_id
		  AND f.language = c.src_language
		  AND c.target_symbol_id IS NOT NULL
		  ` + scope.render("test_links") + `
	`
}

// ResolveTestLinks binds every unbound test link in the repo. This is the
// full-index Pass 2 counterpart to ResolveEdges.
func (s *Store) ResolveTestLinks(ctx context.Context, repoID int64) (int, error) {
	return s.execResolveTestLinks(ctx, repoID, testLinkScope{})
}

// ResolveTestLinksForPaths binds unbound test links declared by the given files.
// This is the incremental Pass 2 for a changed batch: a re-indexed test file's
// links are inserted unbound by Pass 1 and bound here, against the whole
// repository's symbols (changed batch included, since Pass 1 has completed).
func (s *Store) ResolveTestLinksForPaths(ctx context.Context, repoID int64, paths []string) (int, error) {
	return s.resolveTestLinksChunked(ctx, repoID, paths, func(chunk []string) testLinkScope {
		placeholders := sqlPlaceholders(len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, path := range chunk {
			args = append(args, path)
		}
		return testLinkScope{
			predicate: func(alias string) string {
				return ` AND ` + alias + `.test_file_id IN (
					SELECT id FROM files WHERE repo_id = ? AND path IN (` + placeholders + `)
				)`
			},
			args: args,
		}
	})
}

// ResolveTestLinksForStableKeys binds unbound test links anywhere in the repo
// whose target key is one of the given keys.
//
// This is the cross-file half of the incremental Pass 2, and the direct analogue
// of ResolveEdgesForNames: a changed batch can introduce the very definition a
// test file in an *unchanged* file has been waiting for. Scoping by the changed
// batch's symbol keys keeps a one-file edit off any repository-sized scan.
func (s *Store) ResolveTestLinksForStableKeys(ctx context.Context, repoID int64, keys []string) (int, error) {
	return s.resolveTestLinksChunked(ctx, repoID, keys, func(chunk []string) testLinkScope {
		placeholders := sqlPlaceholders(len(chunk))
		args := make([]any, 0, len(chunk))
		for _, key := range chunk {
			args = append(args, key)
		}
		return testLinkScope{
			predicate: func(alias string) string {
				return ` AND ` + alias + `.target_stable_key IN (` + placeholders + `)`
			},
			args: args,
		}
	})
}

// resolveTestLinksChunked splits the scope values into statement-sized chunks
// and runs them all in ONE transaction.
//
// One transaction, not one per chunk: a large incremental batch can produce
// hundreds of chunks, and committing each separately would mean hundreds of WAL
// commits on the resolve path and a partially-resolved graph if the run fails
// halfway. This matches the edge resolvers, which chunk their reads but write
// once.
func (s *Store) resolveTestLinksChunked(ctx context.Context, repoID int64, values []string, scopeFor func(chunk []string) testLinkScope) (int, error) {
	unique := dedupeNonEmpty(values)
	if len(unique) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	total := 0
	for start := 0; start < len(unique); start += testLinkResolveChunkSize {
		end := min(start+testLinkResolveChunkSize, len(unique))
		n, err := resolveTestLinksTx(ctx, tx, repoID, scopeFor(unique[start:end]))
		if err != nil {
			return 0, err
		}
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

// execResolveTestLinks runs one scoped resolve in its own transaction.
func (s *Store) execResolveTestLinks(ctx context.Context, repoID int64, scope testLinkScope) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	n, err := resolveTestLinksTx(ctx, tx, repoID, scope)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// resolveTestLinksTx binds the argument list in statement order: the `needed`
// CTE (repo id, scope args), the candidate CTE (repo id), then the UPDATE (repo
// id, scope args again -- the same predicate is rendered a second time there).
func resolveTestLinksTx(ctx context.Context, tx *sql.Tx, repoID int64, scope testLinkScope) (int, error) {
	args := make([]any, 0, 2*len(scope.args)+3)
	args = append(args, repoID)
	args = append(args, scope.args...)
	args = append(args, repoID, repoID)
	args = append(args, scope.args...)
	res, err := tx.ExecContext(ctx, resolveTestLinksSQL(scope), args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func dedupeNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

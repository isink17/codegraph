package store

import (
	"context"
	"database/sql"
	"path"
	"strings"
)

// Test-link target resolution (P10 Pass 2, canonicalised in P22.2).
//
// A test link is a relationship, so it obeys the same staging rule every other
// relationship in CodeGraph obeys: Pass 1 persists the fact (which test symbol,
// which reason/score, and the parser's `target_stable_key`), Pass 2 binds it
// against the completed symbol table.
//
// P22.2 makes Pass 2 a *canonical* resolution: after ResolveTestLinks runs,
// every row's target state is a pure function of the current tables --
//
//	symbol-bindable  exactly one production symbol matches the row's stable key
//	                 in the test file's language -> target_symbol_id set,
//	                 target_file_id follows the symbol, reason stays the
//	                 producer's 'test_name_match'.
//	file-bindable    no such symbol, but the test file has a conventional
//	                 production sibling (helper_test.go -> helper.go) ->
//	                 target_file_id set, target_symbol_id NULL, reason
//	                 'test_file_name_match'.
//	unbound          neither -> both targets NULL, reason back to the
//	                 producer's value.
//
// Because the final state does not depend on the previous state, a full index
// and any incremental update sequence that end at the same tree produce the
// same test-link semantics -- the parity the earlier scoped, bind-only passes
// could not give (a candidate added later never un-bound, a candidate deleted
// later never re-bound links outside the changed batch). The pass is a fixed
// number of set-based statements over indexed columns, so running it repo-wide
// on every update costs O(test_links), not O(test_links x symbols).
//
// Semantics, deliberately aligned with the edge resolver rather than reinvented:
//
//   - Language gate (P2): the target symbol's persisted language must equal the
//     *test file's* persisted language. A Go `func:pkg::Helper` and a Python
//     `func:pkg::Helper` are a name coincidence, not a relationship. Unknown
//     language on either side fails closed.
//   - Deterministic ambiguity refusal (P3): a key with more than one production
//     candidate in the calling language binds nothing. Picking one would be a
//     row-id tie-break, which P3 forbids. The strong file-level relation
//     survives the refusal (P22.2): an ambiguous symbol never costs the row its
//     sibling-file binding.
//   - Production candidates only (P7/P22.2): a test link *targets* the code
//     under test, so a same-named helper in another test file is never a
//     candidate -- it neither binds nor creates ambiguity. This is the
//     production-caller reading of P7: test files deliberately duplicate
//     production names, and letting a test helper steal the link would wire a
//     test to a test.
//   - Rows whose test file is not a test file by the shared policy
//     (IsTestFilePath) are deleted: Pass 1 no longer writes them, and rows left
//     behind by older producers (a production file exporting TestConnection)
//     are exactly the wrong-edge class P22.2 removes.
//   - Pre-020 rows with an empty target key carry no evidence in either
//     direction; their symbol binding (if any) is left untouched.

// testLinkResolveChunkSize bounds how many bound values go into one statement.
// Half the usual sqliteInClauseBatchSize because the sibling update binds two
// values per pair.
const testLinkResolveChunkSize = sqliteInClauseBatchSize / 2

// testLinkFileReason is the reason recorded on a row whose only binding
// evidence is the test file's conventional production sibling. The producers'
// symbol-guess rows keep their own reason ('test_name_match').
const testLinkFileReason = "test_file_name_match"

// TestLinkResolveStats reports what one canonical pass changed.
type TestLinkResolveStats struct {
	// SymbolsBound counts rows whose target_symbol_id was set this pass.
	SymbolsBound int
	// SymbolsUnbound counts previously bound rows whose target no longer
	// uniquely matches their key (deleted, moved, shadowed, or made ambiguous).
	SymbolsUnbound int
	// FilesBound counts rows whose target_file_id was set (or corrected) from
	// the test file's production sibling this pass.
	FilesBound int
	// RemovedNonTestRows counts rows deleted because their declaring file is
	// not a test file under the shared policy.
	RemovedNonTestRows int
}

// ResolveTestLinks brings every test link in the repo to its canonical target
// state. It is the only Pass-2 entry point: both a full index and an
// incremental update call it after Pass 1 completes.
func (s *Store) ResolveTestLinks(ctx context.Context, repoID int64) (TestLinkResolveStats, error) {
	var stats TestLinkResolveStats
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return stats, err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// Fresh test-file set for this transaction. The table is TEMP and the
	// connection is pooled, so a stale set from an earlier resolve on the same
	// connection must be dropped, exactly as prepareResolverTables does.
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+resolverTestFilesTable); err != nil {
		return stats, err
	}
	if err := s.recordResolverTestFiles(ctx, tx, repoID); err != nil {
		return stats, err
	}
	if err := recordTestLinkCandidates(ctx, tx, repoID); err != nil {
		return stats, err
	}

	if stats.RemovedNonTestRows, err = removeNonTestFileLinks(ctx, tx, repoID); err != nil {
		return stats, err
	}
	if stats.SymbolsUnbound, err = unbindStaleTestLinkTargets(ctx, tx, repoID); err != nil {
		return stats, err
	}
	if stats.SymbolsBound, err = bindTestLinkTargets(ctx, tx, repoID); err != nil {
		return stats, err
	}
	if stats.FilesBound, err = canonicalizeTestLinkFileTargets(ctx, tx, repoID); err != nil {
		return stats, err
	}

	if err := tx.Commit(); err != nil {
		return stats, err
	}
	return stats, nil
}

// removeNonTestFileLinks deletes rows declared by files the shared test-file
// policy rejects. Pass 1 gates inserts on the same policy, so on a healthy
// repository this deletes nothing; it exists for databases written by older
// producers, which linked any file whose functions matched the name prefix.
func removeNonTestFileLinks(ctx context.Context, tx *sql.Tx, repoID int64) (int, error) {
	res, err := tx.ExecContext(ctx, `
		DELETE FROM test_links
		WHERE repo_id = ?
		  AND test_file_id NOT IN (SELECT file_id FROM `+resolverTestFilesTable+`)
	`, repoID)
	if err != nil {
		return 0, err
	}
	return rowsAffected(res)
}

// testLinkCandidatesTable holds one row per (target key, test-file language)
// pair present in the repo's test links, with the ids of the sole production
// candidate -- or NULLs when the key has zero or several. It is a TEMP table
// keyed on the join columns rather than an inline CTE because both the verify
// and the bind UPDATE probe it once per test_links row: a materialised CTE has
// no index, which made those probes a full scan per row -- O(links x keys).
const testLinkCandidatesTable = `tmp_test_link_candidates`

// recordTestLinkCandidates rebuilds the candidate set for this transaction.
// The aggregate is grouped by (target key, test-file language) and exposes an
// id pair that is NULL unless the group holds exactly one production candidate,
// so the MIN()s are identity and never a tie-break. Test-file symbols are
// excluded before counting (P7 production-caller reading): they neither bind
// nor create ambiguity.
func recordTestLinkCandidates(ctx context.Context, tx *sql.Tx, repoID int64) error {
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+testLinkCandidatesTable); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE `+testLinkCandidatesTable+`(
			target_key TEXT NOT NULL,
			src_language TEXT NOT NULL,
			target_symbol_id INTEGER,
			target_file_id INTEGER,
			PRIMARY KEY(target_key, src_language)
		)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO `+testLinkCandidatesTable+`
		SELECT k.target_key, k.src_language,
			CASE WHEN COUNT(*) = 1 THEN MIN(s.id) END,
			CASE WHEN COUNT(*) = 1 THEN MIN(s.file_id) END
		FROM (
			SELECT DISTINCT tl.target_stable_key AS target_key, tf.language AS src_language
			FROM test_links tl
			JOIN files tf ON tf.id = tl.test_file_id
			WHERE tl.repo_id = ?
			  AND tl.target_stable_key != ''
			  AND tf.language != ''
		) k
		JOIN symbols s
		  ON s.repo_id = ?
		 AND s.stable_key = k.target_key
		 AND s.language = k.src_language
		LEFT JOIN `+resolverTestFilesTable+` stf ON stf.file_id = s.file_id
		WHERE stf.file_id IS NULL
		GROUP BY k.target_key, k.src_language
	`, repoID, repoID)
	return err
}

// unbindStaleTestLinkTargets clears target_symbol_id on rows whose key no
// longer resolves to exactly the symbol they are bound to: the target was
// deleted-and-recreated elsewhere, moved behind a new same-key candidate
// (ambiguity introduced later), or is a test-file symbol an older pass bound.
// P6's delete paths already null bindings whose symbol row disappeared; this
// extends the same invariant to logical staleness, which is what makes an
// update sequence converge to the full-index answer. Rows with an empty key
// (pre-020) carry no evidence and are left untouched.
func unbindStaleTestLinkTargets(ctx context.Context, tx *sql.Tx, repoID int64) (int, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE test_links
		SET target_symbol_id = NULL
		WHERE repo_id = ?
		  AND target_symbol_id IS NOT NULL
		  AND target_stable_key != ''
		  AND NOT EXISTS (
			SELECT 1
			FROM `+testLinkCandidatesTable+` c
			JOIN files f ON f.id = test_links.test_file_id
			WHERE c.target_key = test_links.target_stable_key
			  AND c.src_language = f.language
			  AND c.target_symbol_id = test_links.target_symbol_id
		  )
	`, repoID)
	if err != nil {
		return 0, err
	}
	return rowsAffected(res)
}

// bindTestLinkTargets binds unbound rows whose key names exactly one
// production symbol in the test file's language. The re-join on `files` pins
// the candidate group to the link's own test-file language, which is what keeps
// a two-language collision on one stable key from binding either side. A row
// that was file-bound by convention hands its reason back to the producer's
// value: symbol evidence supersedes filename evidence.
func bindTestLinkTargets(ctx context.Context, tx *sql.Tx, repoID int64) (int, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE test_links
		SET target_symbol_id = c.target_symbol_id,
			target_file_id = c.target_file_id,
			reason = CASE WHEN test_links.reason = '`+testLinkFileReason+`'
				THEN 'test_name_match' ELSE test_links.reason END
		FROM `+testLinkCandidatesTable+` c, files f
		WHERE test_links.repo_id = ?
		  AND test_links.target_symbol_id IS NULL
		  AND test_links.target_stable_key = c.target_key
		  AND f.id = test_links.test_file_id
		  AND f.language = c.src_language
		  AND c.target_symbol_id IS NOT NULL
	`, repoID)
	if err != nil {
		return 0, err
	}
	return rowsAffected(res)
}

// testLinkSiblingsTable maps a test file id to its live conventional
// production sibling's file id, for exactly the test files that still own a
// symbol-unbound link. A TEMP table keyed by test file id, so the two UPDATEs
// below probe it by primary key instead of rescanning an inline VALUES list per
// row.
const testLinkSiblingsTable = `tmp_test_link_siblings`

// canonicalizeTestLinkFileTargets gives every symbol-unbound row its canonical
// file-level target: the test file's conventional production sibling when one
// exists in the repo, NULL otherwise. This is what keeps "this test is related
// to this file, exact symbol unknown" alive through symbol ambiguity (the
// common case: Go test names describe behaviour, not a function), and what
// clears a stale target_file_id left behind when the once-bound symbol's
// evidence went away.
func canonicalizeTestLinkFileTargets(ctx context.Context, tx *sql.Tx, repoID int64) (int, error) {
	if err := recordTestLinkSiblings(ctx, tx, repoID); err != nil {
		return 0, err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE test_links
		SET target_file_id = sib.production_file_id,
			reason = '`+testLinkFileReason+`'
		FROM `+testLinkSiblingsTable+` sib
		WHERE test_links.repo_id = ?
		  AND test_links.test_file_id = sib.test_file_id
		  AND test_links.target_symbol_id IS NULL
		  AND (test_links.target_file_id IS NOT sib.production_file_id
			OR test_links.reason != '`+testLinkFileReason+`')
	`, repoID)
	if err != nil {
		return 0, err
	}
	bound, err := rowsAffected(res)
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE test_links
		SET target_file_id = NULL,
			reason = CASE WHEN reason = '`+testLinkFileReason+`'
				THEN 'test_name_match' ELSE reason END
		WHERE repo_id = ?
		  AND target_symbol_id IS NULL
		  AND (target_file_id IS NOT NULL OR reason = '`+testLinkFileReason+`')
		  AND test_file_id NOT IN (SELECT test_file_id FROM `+testLinkSiblingsTable+`)
	`, repoID); err != nil {
		return bound, err
	}
	return bound, nil
}

// recordTestLinkSiblings rebuilds the sibling map for this transaction: one
// scan of the affected test files, the path convention inverted in Go
// (productionSiblingPath), and one batched lookup of the sibling paths -- never
// a per-link query.
func recordTestLinkSiblings(ctx context.Context, tx *sql.Tx, repoID int64) error {
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+testLinkSiblingsTable); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TEMP TABLE `+testLinkSiblingsTable+`(
			test_file_id INTEGER PRIMARY KEY,
			production_file_id INTEGER NOT NULL
		)`); err != nil {
		return err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT f.id, f.path
		FROM test_links tl
		JOIN files f ON f.id = tl.test_file_id
		WHERE tl.repo_id = ? AND tl.target_symbol_id IS NULL
	`, repoID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type testFile struct {
		id      int64
		sibling string
	}
	files := make([]testFile, 0, 64)
	siblingPaths := make([]string, 0, 64)
	for rows.Next() {
		var id int64
		var filePath string
		if err := rows.Scan(&id, &filePath); err != nil {
			return err
		}
		sibling := productionSiblingPath(filePath)
		if sibling == "" {
			continue
		}
		files = append(files, testFile{id: id, sibling: sibling})
		siblingPaths = append(siblingPaths, sibling)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	siblingIDs, err := liveFileIDsByPath(ctx, tx, repoID, siblingPaths)
	if err != nil {
		return err
	}

	args := make([]any, 0, 2*testLinkResolveChunkSize)
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		placeholders := strings.TrimRight(strings.Repeat("(?, ?),", len(args)/2), ",")
		_, err := tx.ExecContext(ctx,
			`INSERT INTO `+testLinkSiblingsTable+`(test_file_id, production_file_id) VALUES `+placeholders, args...)
		args = args[:0]
		return err
	}
	for _, f := range files {
		id, ok := siblingIDs[f.sibling]
		if !ok {
			continue
		}
		args = append(args, f.id, id)
		if len(args) >= 2*testLinkResolveChunkSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// liveFileIDsByPath resolves stored paths to ids for non-deleted files, in
// chunks sized for the IN clause.
func liveFileIDsByPath(ctx context.Context, tx *sql.Tx, repoID int64, paths []string) (map[string]int64, error) {
	out := make(map[string]int64, len(paths))
	unique := dedupeNonEmpty(paths)
	for start := 0; start < len(unique); start += testLinkResolveChunkSize {
		end := min(start+testLinkResolveChunkSize, len(unique))
		chunk := unique[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id, path FROM files
			WHERE repo_id = ? AND is_deleted = 0 AND path IN (`+sqlPlaceholders(len(chunk))+`)
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var p string
			if err := rows.Scan(&id, &p); err != nil {
				rows.Close()
				return nil, err
			}
			out[p] = id
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// productionSiblingPath returns the conventional production counterpart of a
// test file path, or "" when the convention defines none. Only conventions
// whose inverse is unambiguous are inverted -- the same ones IsTestFilePath
// recognises for these languages:
//
//	Go:     helper_test.go -> helper.go
//	Python: test_utils.py -> utils.py, utils_test.py -> utils.py
//
// The sibling must itself classify as production (a test-directory segment or
// a doubly-affixed name keeps the result a test file, which is no sibling at
// all). Suffix-class languages whose producers emit no bindable evidence today
// (Java/Kotlin/C#/Swift FooTest, JS/TS foo.test.ts, ...) are deliberately not
// inverted here until their producers mint matchable keys.
func productionSiblingPath(testPath string) string {
	idx := strings.LastIndexAny(testPath, `/\`)
	base := testPath[idx+1:]
	extension := strings.ToLower(path.Ext(base))
	stem := base[:len(base)-len(path.Ext(base))]
	var siblingStem string
	switch extension {
	case ".go":
		trimmed := strings.TrimSuffix(stem, "_test")
		if trimmed == stem || trimmed == "" {
			return ""
		}
		siblingStem = trimmed
	case ".py", ".pyi":
		switch {
		case strings.HasPrefix(stem, "test_") && len(stem) > len("test_"):
			siblingStem = strings.TrimPrefix(stem, "test_")
		case strings.HasSuffix(stem, "_test") && len(stem) > len("_test"):
			siblingStem = strings.TrimSuffix(stem, "_test")
		default:
			return ""
		}
	default:
		return ""
	}
	sibling := testPath[:idx+1] + siblingStem + path.Ext(base)
	if IsTestFilePath(sibling) {
		return ""
	}
	return sibling
}

func rowsAffected(res sql.Result) (int, error) {
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

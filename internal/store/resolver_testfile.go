package store

import (
	"context"
	"database/sql"
	"path"
	"strings"
)

// Test-shadow aware resolver ranking (P7).
//
// P2 decided which candidates a strategy may consider (same persisted language).
// P3 decided what happens when several survive (refuse to guess). P4 recorded
// what evidence bound an edge. P7 answers a narrower question that P3 explicitly
// left open: when the candidates that survive are *one production definition and
// some number of test-only definitions*, does the caller still have to give up?
//
// The rule:
//
//	A call site in production code may bind only a production candidate. A call
//	site in a test file keeps the plain P2/P3 semantics.
//
// Spelled out, for a production caller at one strategy's evidence level:
//
//	1 production candidate + N test candidates -> binds the production candidate
//	2+ production candidates                   -> unresolved (test candidates
//	                                              never make one of them unique)
//	0 production candidates + N test candidates -> unresolved (production code is
//	                                              never wired into a test)
//
// and for a caller that is itself in a test file: nothing changes. Tests
// legitimately call both production code and test helpers, so "prefer
// production" would be a guess there, and "prefer test" equally so. A test
// caller resolves when exactly one candidate exists at the strategy's evidence
// level and stays unresolved otherwise, exactly as it did before P7.
//
// Why this is not a general ranking engine: the preference is applied *inside*
// one strategy's candidate set, after that strategy's own evidence has selected
// it. It filters candidates; it does not reorder evidence levels, invent a new
// matching strategy, or override stronger call-site evidence. A production call
// that names a test symbol by its full qualified name is not silently retargeted
// to some unrelated production symbol -- it stays unresolved, because the
// evidence level that matched (qualified name) had no production candidate at
// all. See the veto in resolver_ambiguity.go.
//
// Why it is honest rather than a tie-break: test-shadow evidence is a real
// property of the destination (which file declares it) combined with a real
// property of the call site (which file makes the call), not a row id. Two
// identical index runs classify the same files the same way, so the outcome is
// reproducible -- which is what P3 required of any rule allowed to break a tie.
//
// Provenance is deliberately untouched: an edge bound this way reports the
// strategy that actually matched its name (exact_name, exact_qualified, ...) and
// that strategy's confidence. "test_shadow" is not a matching strategy and
// inventing one would misdescribe how the destination was found.

// IsTestFilePath reports whether a repository-relative file path is a test file
// by an unambiguous, language-specific naming convention.
//
// This is the single production definition of "test file" in CodeGraph. Both the
// SQL strategies (via resolverTestFilesTable, populated from this function) and
// the Go-side binder consult it, so the two resolver paths cannot drift into
// different rules.
//
// It is deliberately narrow. Only conventions that a language's own tooling
// enforces or that are effectively universal in that ecosystem count:
//
//   - a path segment that *is* a test directory: test, tests, testdata,
//     __tests__ (compared case-insensitively, so Java's src/test/java and
//     Swift's Tests/ both match)
//   - Go: foo_test.go (the only form `go test` compiles)
//   - Python: test_foo.py, foo_test.py (pytest/unittest discovery)
//   - JS/TS: foo.test.ts, foo.spec.tsx (jest/vitest/jasmine defaults)
//   - Java/Kotlin/C#/Swift: FooTest, FooTests (JUnit/NUnit/XCTest)
//   - PHP: FooTest.php (PHPUnit)
//   - Ruby: foo_test.rb, foo_spec.rb, test_foo.rb (minitest/RSpec)
//   - C/C++/Rust: foo_test.cc and friends (googletest layout; Rust's real
//     convention is the tests/ directory, already covered above)
//
// What it deliberately does NOT do:
//
//   - classify by substring: a path containing "test" anywhere (latest/,
//     contest.go, TestingFramework.java) is not a test file
//   - classify mocks, fixtures or stubs: "mock" and "fixture" name no test
//     convention in any supported language and would demote real production
//     code (a Mock* factory shipped in production is production code)
//   - classify by symbol name: a production function called TestConnection is
//     production code
//   - infer anything about builtin, stdlib or external symbols; that
//     classification does not exist in CodeGraph and P7 does not add it
//
// A false positive here demotes real production code and can turn a resolvable
// edge into an unresolved one; a false negative only leaves P3's pre-existing
// ambiguity in place. The rules above are chosen to fail in the second
// direction.
func IsTestFilePath(filePath string) bool {
	normalized := strings.TrimSpace(filePath)
	if normalized == "" {
		return false
	}
	// Segment walk rather than strings.Split: this runs once per file per
	// resolve, and there is no reason for it to allocate. Both separators are
	// accepted because `files.path` is written with the host separator.
	start := 0
	for i := 0; i < len(normalized); i++ {
		if normalized[i] != '/' && normalized[i] != '\\' {
			continue
		}
		if isTestDirSegment(normalized[start:i]) {
			return true
		}
		start = i + 1
	}
	// Everything after the last separator is the file name, never a directory.
	return isTestFileName(normalized[start:])
}

// testDirSegments are directory names that *are* a test location rather than
// merely containing the word. Matched on the whole segment, case-insensitively,
// so Tests/ (Swift, C#) and src/test/java (Java, Kotlin) match while latest/ and
// contests/ do not.
//
// `spec/` is deliberately absent even though it is RSpec's convention: it is
// also a common production package name (OpenAPI/schema handling), and demoting
// a whole shipped package is a worse error than leaving Ruby's directory
// convention to the `_spec.rb` filename rule below.
var testDirSegments = []string{"test", "tests", "testdata", "__tests__"}

func isTestDirSegment(segment string) bool {
	for _, candidate := range testDirSegments {
		if strings.EqualFold(segment, candidate) {
			return true
		}
	}
	return false
}

// isTestFileName applies the per-language filename conventions. The extension
// selects the convention: `_test` means a test file in Go and C++ but nothing in
// Java, and `Test` means one in Java but nothing in Go.
func isTestFileName(base string) bool {
	extension := strings.ToLower(path.Ext(base))
	stem := base[:len(base)-len(path.Ext(base))]
	if stem == "" {
		return false
	}
	switch extension {
	case ".go":
		return strings.HasSuffix(stem, "_test")
	case ".py", ".pyi":
		return strings.HasPrefix(stem, "test_") || strings.HasSuffix(stem, "_test")
	case ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts":
		// The convention is a secondary extension, not a substring: foo.test.ts
		// is a test, foo_test.ts and latest.ts are not.
		return strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, ".spec")
	case ".java", ".kt", ".kts", ".cs", ".swift":
		// Suffix only. A `Test` *prefix* would also match TestingUtils.java, and
		// prefix-named test classes live under a test directory anyway, which the
		// segment rule already covers.
		return strings.HasSuffix(stem, "Test") || strings.HasSuffix(stem, "Tests")
	case ".php":
		return strings.HasSuffix(stem, "Test")
	case ".rb":
		return strings.HasPrefix(stem, "test_") ||
			strings.HasSuffix(stem, "_test") ||
			strings.HasSuffix(stem, "_spec")
	case ".rs", ".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp":
		return strings.HasSuffix(stem, "_test")
	}
	return false
}

// resolverTestFilesTable holds the `files.id` of every test file in the repo
// being resolved. It is the SQL side's only view of IsTestFilePath, populated
// once per resolve from Go so the classification cannot be restated -- and
// therefore cannot drift -- in SQL.
//
// One row per test file, keyed by file id, so the candidate aggregates below
// join against it by primary key. An *empty* table means "this repo has no test
// files", which reduces every rule here to the pre-P7 P2/P3 behaviour; that is
// the correct reading for a strategy invoked in isolation (benchmarks) which
// never populated it.
const resolverTestFilesTable = `tmp_resolver_test_files`

// resolverCallerTestJoinSQL brings the calling file's test-ness into a resolver
// UPDATE. It hangs off `files` (already joined as `f` for the language gate), so
// the lookup happens once per candidate row rather than once per edge per
// predicate -- a correlated EXISTS on `edges.file_id` would be evaluated again
// for the SET expression, the bindability check and the ambiguity veto.
const resolverCallerTestJoinSQL = `LEFT JOIN ` + resolverTestFilesTable + ` ctf ON ctf.file_id = f.id`

// resolverCallerIsTestSQL is true when the edge's own source file is a test
// file. It requires resolverCallerTestJoinSQL in the surrounding FROM clause.
const resolverCallerIsTestSQL = `ctf.file_id IS NOT NULL`

// resolverCandidateAggregatesSQL is the select-list fragment every candidate
// relation must expose, replacing the single `MIN(s.id) AS dst_symbol_id` that
// pre-P7 strategies computed. It requires the aggregate to group symbols aliased
// `s` by (matched name, language), with resolverTestFilesTable LEFT JOINed as
// `tf` on `s.file_id`.
//
// Two nullable ids, one per caller kind:
//
//	any_symbol_id         the sole candidate, or NULL when several exist. This
//	                      is what a test caller may bind: unchanged P3.
//	production_symbol_id  the sole candidate declared outside a test file, or
//	                      NULL when there are none or several. This is what a
//	                      production caller may bind.
//
// Both MIN()s are identity, never a tie-break: each is guarded by a COUNT over
// exactly the rows it minimises over, so it only ever sees one row. A symbol
// whose file row is missing has no `tf` match and therefore counts as
// production, which keeps it competing (and so keeps the edge unresolved)
// instead of letting a ghost row silently promote another candidate.
const resolverCandidateAggregatesSQL = `CASE WHEN COUNT(*) = 1 THEN MIN(s.id) END AS any_symbol_id,
				CASE WHEN SUM(CASE WHEN tf.file_id IS NULL THEN 1 ELSE 0 END) = 1
					THEN MIN(CASE WHEN tf.file_id IS NULL THEN s.id END) END AS production_symbol_id`

// resolverCandidateJoinSQL LEFT JOINs the test-file set onto the candidate
// symbols aliased `s`, supplying the `tf` alias resolverCandidateAggregatesSQL
// reads.
const resolverCandidateJoinSQL = `LEFT JOIN ` + resolverTestFilesTable + ` tf ON tf.file_id = s.file_id
			AND NOT (s.language = 'cpp' AND s.visibility = 'hidden_friend')`

// resolverCandidateHavingSQL keeps only candidate groups that some caller kind
// can actually use. It replaces P3's `HAVING COUNT(*) = 1`: a group of one is
// still usable (by anyone), and so now is a group with exactly one production
// definition (by a production caller). Groups that are undecidable for both
// caller kinds are dropped here rather than filtered later, so the relation
// stays small.
const resolverCandidateHavingSQL = `HAVING any_symbol_id IS NOT NULL OR production_symbol_id IS NOT NULL`

// resolverChosenCandidateSQL is the one expression that decides which of a
// candidate group's two ids an edge binds. It requires the update target to be
// `edges` and the candidate relation to be joined in as `r`.
//
// NULL means "this caller kind has no usable candidate here", which every
// strategy turns into "leave the edge unresolved" via
// resolverBindableCandidateSQL.
const resolverChosenCandidateSQL = `CASE WHEN ` + resolverCallerIsTestSQL + `
		THEN r.any_symbol_id ELSE r.production_symbol_id END`

// resolverCandidateColumnsDDL is the column list shared by the suffix
// strategies' precomputed candidate tables. They persist the same two nullable
// ids the inline strategies compute in place, so `r` exposes an identical shape
// everywhere and resolverChosenCandidateSQL is the only chooser in the resolver.
const resolverCandidateColumnsDDL = `dst_name TEXT NOT NULL,
		dst_language TEXT NOT NULL,
		any_symbol_id INTEGER,
		production_symbol_id INTEGER,
		PRIMARY KEY(dst_name, dst_language)`

// ensureResolverTestFilesTable makes the test-file set exist for the current
// transaction without populating it. Strategies that are callable on their own
// call this for the same reason they ensure the ambiguity veto table: an empty
// table is a correct, conservative reading (no test files -> pre-P7 semantics).
func ensureResolverTestFilesTable(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+resolverTestFilesTable+
		`(file_id INTEGER PRIMARY KEY)`)
	return err
}

// recordResolverTestFiles fills the test-file set for one repo.
//
// One scan of `files` (a few thousand rows on a large repo, never one query per
// edge or per candidate), the classification done in Go by IsTestFilePath, and
// one batched insert of the matching ids. Non-test files are not stored: absence
// from this table is what "production" means to every SQL fragment above.
func (s *Store) recordResolverTestFiles(ctx context.Context, tx *sql.Tx, repoID int64) error {
	if err := ensureResolverTestFilesTable(ctx, tx); err != nil {
		return err
	}
	testFileIDs, err := testFileIDsForRepo(ctx, tx, repoID)
	if err != nil {
		return err
	}
	return insertResolverTestFileIDs(ctx, tx, testFileIDs)
}

// testFileIDsForRepo returns the ids of every file in the repo that
// IsTestFilePath classifies as a test file.
//
// One scan of `files`, one classification per file. Both resolver paths use it,
// which is also why the Go-side binder never reads a candidate's path: it looks
// the candidate's file id up in this set instead, so classification cost scales
// with the file count rather than with the candidate count.
func testFileIDsForRepo(ctx context.Context, q queryContexter, repoID int64) (map[int64]struct{}, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, path FROM files WHERE repo_id = ?`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		var filePath string
		if err := rows.Scan(&id, &filePath); err != nil {
			return nil, err
		}
		if IsTestFilePath(filePath) {
			ids[id] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// insertResolverTestFileIDs fills resolverTestFilesTable. Insert order is
// irrelevant: the table is a set keyed by file id and every reader joins on it.
func insertResolverTestFileIDs(ctx context.Context, tx *sql.Tx, ids map[int64]struct{}) error {
	const maxPerInsert = 400
	var b strings.Builder
	args := make([]any, 0, maxPerInsert)
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, b.String(), args...)
		b.Reset()
		args = args[:0]
		return err
	}
	for id := range ids {
		if len(args) == 0 {
			b.WriteString(`INSERT OR IGNORE INTO ` + resolverTestFilesTable + `(file_id) VALUES (?)`)
		} else {
			b.WriteString(",(?)")
		}
		args = append(args, id)
		if len(args) >= maxPerInsert {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

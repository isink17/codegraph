package store

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
)

// Pass-2 test-link resolver semantics (P10).
//
// These exercise ResolveTestLinks* directly against hand-built rows so the
// rules can be stated without going through a parser: the language gate (P2),
// deterministic ambiguity refusal (P3), and the bind-only contract that leaves
// P6's stale-target unbinding as the only code that clears a target.

type testLinkFixture struct {
	t      *testing.T
	ctx    context.Context
	store  *Store
	repoID int64
}

func newTestLinkFixture(t *testing.T) *testLinkFixture {
	t.Helper()
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	return &testLinkFixture{t: t, ctx: ctx, store: s, repoID: repo.ID}
}

func (f *testLinkFixture) file(path, language string) int64 {
	f.t.Helper()
	id, err := insertTestFileLang(f.ctx, f.store, f.repoID, path, language)
	if err != nil {
		f.t.Fatalf("insert file %q: %v", path, err)
	}
	return id
}

// symbolWithKey inserts a function symbol with an explicit stable_key, which is
// what the Pass-2 resolver matches on.
func (f *testLinkFixture) symbolWithKey(fileID int64, name, language, stableKey string) int64 {
	f.t.Helper()
	res, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO symbols(
			repo_id, file_id, language, kind, name, qualified_name, container_name,
			start_line, start_col, end_line, end_col, stable_key, qualified_suffix, dot_tail2, dot_tail3
		)
		VALUES(?, ?, ?, 'function', ?, ?, '', 1, 1, 1, 1, ?, '', '', '')
	`, f.repoID, fileID, language, name, name, stableKey)
	if err != nil {
		f.t.Fatalf("insert symbol %q: %v", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		f.t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

// link inserts an unbound test link, exactly as Pass 1 writes it.
func (f *testLinkFixture) link(testFileID int64, targetKey string) int64 {
	f.t.Helper()
	res, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score, target_stable_key)
		VALUES(?, ?, NULL, NULL, NULL, 'test_name_match', 0.8, ?)
	`, f.repoID, testFileID, targetKey)
	if err != nil {
		f.t.Fatalf("insert test_link: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		f.t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

// target returns (target_symbol_id, target_file_id) for one link row, with 0
// standing in for NULL.
func (f *testLinkFixture) target(linkID int64) (int64, int64) {
	f.t.Helper()
	var symID, fileID int64
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT COALESCE(target_symbol_id, 0), COALESCE(target_file_id, 0) FROM test_links WHERE id = ?`,
		linkID).Scan(&symID, &fileID); err != nil {
		f.t.Fatalf("read test_link %d: %v", linkID, err)
	}
	return symID, fileID
}

func (f *testLinkFixture) resolveAll() int {
	f.t.Helper()
	n, err := f.store.ResolveTestLinks(f.ctx, f.repoID)
	if err != nil {
		f.t.Fatalf("ResolveTestLinks() error = %v", err)
	}
	return n
}

// TestResolveTestLinks_BindsUniqueSameLanguageTarget is the baseline: one
// candidate, same language, binds both target columns.
func TestResolveTestLinks_BindsUniqueSameLanguageTarget(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("helper.go", "go")
	targetSym := f.symbolWithKey(targetFile, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	if n := f.resolveAll(); n != 1 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 1", n)
	}
	gotSym, gotFile := f.target(link)
	if gotSym != targetSym || gotFile != targetFile {
		t.Fatalf("target = (sym %d, file %d), want (%d, %d)", gotSym, gotFile, targetSym, targetFile)
	}
}

// TestResolveTestLinks_AmbiguousTargetStaysUnresolved covers P3 for test links:
// two definitions share the stable key (two directories declaring the same Go
// package), so neither is evidence and the link stays unbound. Picking one
// would be a row-id tie-break.
func TestResolveTestLinks_AmbiguousTargetStaysUnresolved(t *testing.T) {
	f := newTestLinkFixture(t)
	a := f.file("a/helper.go", "go")
	f.symbolWithKey(a, "Helper", "go", "func:pkg::Helper")
	b := f.file("b/helper.go", "go")
	f.symbolWithKey(b, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("a/helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	if n := f.resolveAll(); n != 0 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 0 (ambiguous key)", n)
	}
	if gotSym, gotFile := f.target(link); gotSym != 0 || gotFile != 0 {
		t.Fatalf("target = (sym %d, file %d), want unbound", gotSym, gotFile)
	}
}

// TestResolveTestLinks_CrossLanguageTargetBlocked covers P2: the Go and Python
// test-link producers both mint `func:<module>::<name>` keys, so a coincidental
// collision must not wire a Go test to a Python function.
func TestResolveTestLinks_CrossLanguageTargetBlocked(t *testing.T) {
	f := newTestLinkFixture(t)
	pyFile := f.file("pkg/helper.py", "python")
	f.symbolWithKey(pyFile, "helper", "python", "func:pkg::Helper")
	testFile := f.file("helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	if n := f.resolveAll(); n != 0 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 0 (cross-language)", n)
	}
	if gotSym, gotFile := f.target(link); gotSym != 0 || gotFile != 0 {
		t.Fatalf("target = (sym %d, file %d), want unbound", gotSym, gotFile)
	}
}

// TestResolveTestLinks_UnknownLanguageFailsClosed: an unclassified test file
// carries no language evidence, so the gate refuses rather than guessing.
func TestResolveTestLinks_UnknownLanguageFailsClosed(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("helper.go", "go")
	f.symbolWithKey(targetFile, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("helper_test.unknown", "")
	link := f.link(testFile, "func:pkg::Helper")

	if n := f.resolveAll(); n != 0 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 0 (unknown source language)", n)
	}
	if gotSym, _ := f.target(link); gotSym != 0 {
		t.Fatalf("target symbol = %d, want unbound", gotSym)
	}
}

// TestResolveTestLinks_CrossLanguageCollisionDoesNotBindEitherSide: two
// languages own the same key and each has a test file. Each link must bind only
// its own language's definition -- the candidate group is keyed by (key,
// language), not by key alone.
func TestResolveTestLinks_CrossLanguageCollisionDoesNotBindEitherSide(t *testing.T) {
	f := newTestLinkFixture(t)
	goFile := f.file("helper.go", "go")
	goSym := f.symbolWithKey(goFile, "Helper", "go", "func:pkg::Helper")
	pyFile := f.file("pkg/helper.py", "python")
	pySym := f.symbolWithKey(pyFile, "helper", "python", "func:pkg::Helper")

	goTest := f.file("helper_test.go", "go")
	goLink := f.link(goTest, "func:pkg::Helper")
	pyTest := f.file("pkg/test_helper.py", "python")
	pyLink := f.link(pyTest, "func:pkg::Helper")

	if n := f.resolveAll(); n != 2 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 2", n)
	}
	if gotSym, gotFile := f.target(goLink); gotSym != goSym || gotFile != goFile {
		t.Fatalf("go link target = (sym %d, file %d), want (%d, %d)", gotSym, gotFile, goSym, goFile)
	}
	if gotSym, gotFile := f.target(pyLink); gotSym != pySym || gotFile != pyFile {
		t.Fatalf("python link target = (sym %d, file %d), want (%d, %d)", gotSym, gotFile, pySym, pyFile)
	}
}

// TestResolveTestLinks_IsBindOnly: an already-bound row is never rewritten by a
// resolve pass. Clearing a stale target stays the delete path's job (P6), which
// is what keeps target_file_id alive for file-level RelatedTests when only the
// symbol disappeared.
func TestResolveTestLinks_IsBindOnly(t *testing.T) {
	f := newTestLinkFixture(t)
	oldFile := f.file("old.go", "go")
	oldSym := f.symbolWithKey(oldFile, "Helper", "go", "func:other::Helper")
	newFile := f.file("helper.go", "go")
	f.symbolWithKey(newFile, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE test_links SET target_symbol_id = ?, target_file_id = ? WHERE id = ?`,
		oldSym, oldFile, link); err != nil {
		t.Fatalf("pre-bind test_link: %v", err)
	}

	if n := f.resolveAll(); n != 0 {
		t.Fatalf("ResolveTestLinks() rewrote %d bound rows, want 0", n)
	}
	if gotSym, gotFile := f.target(link); gotSym != oldSym || gotFile != oldFile {
		t.Fatalf("bound target changed to (sym %d, file %d), want (%d, %d)", gotSym, gotFile, oldSym, oldFile)
	}
}

// TestResolveTestLinks_EmptyKeyIsIgnored: rows written before migration 020 (or
// by a producer that minted no key) carry an empty key and must never match a symbol.
func TestResolveTestLinks_EmptyKeyIsIgnored(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("helper.go", "go")
	f.symbolWithKey(targetFile, "Helper", "go", "")
	testFile := f.file("helper_test.go", "go")
	link := f.link(testFile, "")

	if n := f.resolveAll(); n != 0 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 0 (empty key)", n)
	}
	if gotSym, _ := f.target(link); gotSym != 0 {
		t.Fatalf("target symbol = %d, want unbound", gotSym)
	}
}

// TestResolveTestLinks_ScopedByPathsOnlyTouchesThoseTestFiles guards the
// incremental scope: a changed-batch resolve must not sweep the repository.
func TestResolveTestLinks_ScopedByPathsOnlyTouchesThoseTestFiles(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("helper.go", "go")
	f.symbolWithKey(targetFile, "Helper", "go", "func:pkg::Helper")
	changed := f.file("helper_test.go", "go")
	changedLink := f.link(changed, "func:pkg::Helper")
	untouched := f.file("other_test.go", "go")
	untouchedLink := f.link(untouched, "func:pkg::Helper")

	n, err := f.store.ResolveTestLinksForPaths(f.ctx, f.repoID, []string{"helper_test.go"})
	if err != nil {
		t.Fatalf("ResolveTestLinksForPaths() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("ResolveTestLinksForPaths() bound %d rows, want 1", n)
	}
	if gotSym, _ := f.target(changedLink); gotSym == 0 {
		t.Fatalf("changed test file's link stayed unbound")
	}
	if gotSym, _ := f.target(untouchedLink); gotSym != 0 {
		t.Fatalf("out-of-scope link bound to %d, want untouched", gotSym)
	}
}

// TestResolveTestLinks_ScopedByStableKeysBindsOtherFiles is the cross-file half:
// a changed batch that introduces a definition must bind links declared by files
// it did not touch, and nothing else.
func TestResolveTestLinks_ScopedByStableKeysBindsOtherFiles(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("helper.go", "go")
	targetSym := f.symbolWithKey(targetFile, "Helper", "go", "func:pkg::Helper")
	otherTarget := f.file("worker.go", "go")
	f.symbolWithKey(otherTarget, "Worker", "go", "func:pkg::Worker")

	testFile := f.file("helper_test.go", "go")
	wanted := f.link(testFile, "func:pkg::Helper")
	otherLink := f.link(testFile, "func:pkg::Worker")

	n, err := f.store.ResolveTestLinksForStableKeys(f.ctx, f.repoID, []string{"func:pkg::Helper"})
	if err != nil {
		t.Fatalf("ResolveTestLinksForStableKeys() error = %v", err)
	}
	if n != 1 {
		t.Fatalf("ResolveTestLinksForStableKeys() bound %d rows, want 1", n)
	}
	if gotSym, gotFile := f.target(wanted); gotSym != targetSym || gotFile != targetFile {
		t.Fatalf("target = (sym %d, file %d), want (%d, %d)", gotSym, gotFile, targetSym, targetFile)
	}
	if gotSym, _ := f.target(otherLink); gotSym != 0 {
		t.Fatalf("out-of-scope key bound to %d, want untouched", gotSym)
	}
}

// TestResolveTestLinks_ScopesChunkAcrossStatementBoundary drives more scope
// values than testLinkResolveChunkSize so the chunking loop actually runs more
// than once. The scope predicate is bound twice per statement, so an off-by-one
// in the chunk size or in the argument order shows up here as a SQL error or a
// missed binding rather than silently on a large repo.
func TestResolveTestLinks_ScopesChunkAcrossStatementBoundary(t *testing.T) {
	f := newTestLinkFixture(t)
	const n = testLinkResolveChunkSize + 7

	keys := make([]string, 0, n)
	links := make([]int64, 0, n)
	for i := range n {
		key := "func:pkg::Helper" + itoa(i)
		targetFile := f.file("helper"+itoa(i)+".go", "go")
		f.symbolWithKey(targetFile, "Helper"+itoa(i), "go", key)
		testPath := "helper" + itoa(i) + "_test.go"
		testFile := f.file(testPath, "go")
		keys = append(keys, key)
		links = append(links, f.link(testFile, key))
	}

	got, err := f.store.ResolveTestLinksForStableKeys(f.ctx, f.repoID, keys)
	if err != nil {
		t.Fatalf("ResolveTestLinksForStableKeys() error = %v", err)
	}
	if got != n {
		t.Fatalf("ResolveTestLinksForStableKeys() bound %d rows, want %d", got, n)
	}
	for i, link := range links {
		if sym, _ := f.target(link); sym == 0 {
			t.Fatalf("link %d stayed unbound", i)
		}
	}

	// Same span through the path-scoped entry point, on a second set of links.
	more := make([]int64, 0, n)
	for i := range n {
		testFile := f.file("extra"+itoa(i)+"_test.go", "go")
		more = append(more, f.link(testFile, keys[i]))
	}
	extraPaths := make([]string, 0, n)
	for i := range n {
		extraPaths = append(extraPaths, "extra"+itoa(i)+"_test.go")
	}
	got, err = f.store.ResolveTestLinksForPaths(f.ctx, f.repoID, extraPaths)
	if err != nil {
		t.Fatalf("ResolveTestLinksForPaths() error = %v", err)
	}
	if got != n {
		t.Fatalf("ResolveTestLinksForPaths() bound %d rows, want %d", got, n)
	}
	for i, link := range more {
		if sym, _ := f.target(link); sym == 0 {
			t.Fatalf("path-scoped link %d stayed unbound", i)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// TestResolveTestLinks_OnlyTouchesOwnRepo guards the repo_id filter.
func TestResolveTestLinks_OnlyTouchesOwnRepo(t *testing.T) {
	f := newTestLinkFixture(t)
	other, err := f.store.UpsertRepo(f.ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	otherFileID, err := insertTestFileLang(f.ctx, f.store, other.ID, "helper.go", "go")
	if err != nil {
		t.Fatalf("insert other-repo file: %v", err)
	}
	otherTestFileID, err := insertTestFileLang(f.ctx, f.store, other.ID, "helper_test.go", "go")
	if err != nil {
		t.Fatalf("insert other-repo test file: %v", err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name,
			start_line, start_col, end_line, end_col, stable_key, qualified_suffix, dot_tail2, dot_tail3)
		VALUES(?, ?, 'go', 'function', 'Helper', 'Helper', '', 1, 1, 1, 1, 'func:pkg::Helper', '', '', '')
	`, other.ID, otherFileID); err != nil {
		t.Fatalf("insert other-repo symbol: %v", err)
	}
	var otherLink int64
	res, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score, target_stable_key)
		VALUES(?, ?, NULL, NULL, NULL, 'test_name_match', 0.8, 'func:pkg::Helper')
	`, other.ID, otherTestFileID)
	if err != nil {
		t.Fatalf("insert other-repo test_link: %v", err)
	}
	if otherLink, err = res.LastInsertId(); err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}

	targetFile := f.file("helper.go", "go")
	f.symbolWithKey(targetFile, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("helper_test.go", "go")
	mine := f.link(testFile, "func:pkg::Helper")

	if n := f.resolveAll(); n != 1 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 1 (own repo only)", n)
	}
	if gotSym, _ := f.target(mine); gotSym == 0 {
		t.Fatalf("own-repo link stayed unbound")
	}
	if gotSym, _ := f.target(otherLink); gotSym != 0 {
		t.Fatalf("other repo's link bound to %d, want untouched", gotSym)
	}
}

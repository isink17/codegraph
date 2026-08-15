package store

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
)

// Pass-2 test-link resolver semantics (P10, canonicalised in P22.2).
//
// These exercise ResolveTestLinks directly against hand-built rows so the rules
// can be stated without going through a parser: the language gate (P2),
// deterministic ambiguity refusal (P3), production-only candidates (P7), the
// sibling file-level fallback, and canonical convergence -- after a pass, a
// row's target state depends only on the current tables, never on the previous
// binding, which is what makes an update sequence equal a full index.

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

func (f *testLinkFixture) markFileDeleted(fileID int64) {
	f.t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE files SET is_deleted = 1, parse_state = 'deleted' WHERE id = ?`, fileID); err != nil {
		f.t.Fatalf("mark file %d deleted: %v", fileID, err)
	}
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

func (f *testLinkFixture) deleteSymbol(symbolID int64) {
	f.t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE id = ?`, symbolID); err != nil {
		f.t.Fatalf("delete symbol %d: %v", symbolID, err)
	}
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

func (f *testLinkFixture) reason(linkID int64) string {
	f.t.Helper()
	var reason string
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT reason FROM test_links WHERE id = ?`, linkID).Scan(&reason); err != nil {
		f.t.Fatalf("read test_link %d reason: %v", linkID, err)
	}
	return reason
}

func (f *testLinkFixture) rowCount() int {
	f.t.Helper()
	var n int
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT COUNT(*) FROM test_links WHERE repo_id = ?`, f.repoID).Scan(&n); err != nil {
		f.t.Fatalf("count test_links: %v", err)
	}
	return n
}

func (f *testLinkFixture) resolveAll() TestLinkResolveStats {
	f.t.Helper()
	stats, err := f.store.ResolveTestLinks(f.ctx, f.repoID)
	if err != nil {
		f.t.Fatalf("ResolveTestLinks() error = %v", err)
	}
	return stats
}

// TestResolveTestLinks_BindsUniqueSameLanguageTarget is the baseline: one
// production candidate, same language, binds both target columns and keeps the
// producer's reason.
func TestResolveTestLinks_BindsUniqueSameLanguageTarget(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("helper.go", "go")
	targetSym := f.symbolWithKey(targetFile, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	if stats := f.resolveAll(); stats.SymbolsBound != 1 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 1", stats.SymbolsBound)
	}
	gotSym, gotFile := f.target(link)
	if gotSym != targetSym || gotFile != targetFile {
		t.Fatalf("target = (sym %d, file %d), want (%d, %d)", gotSym, gotFile, targetSym, targetFile)
	}
	if reason := f.reason(link); reason != "test_name_match" {
		t.Fatalf("reason = %q, want test_name_match", reason)
	}
}

// TestResolveTestLinks_AmbiguousTargetKeepsFileRelation covers P3 plus the
// P22.2 file fallback: two production definitions share the stable key (two
// directories declaring the same Go package), so neither is symbol evidence --
// but the test file's own sibling still gives a correct file-level relation.
func TestResolveTestLinks_AmbiguousTargetKeepsFileRelation(t *testing.T) {
	f := newTestLinkFixture(t)
	a := f.file("a/helper.go", "go")
	f.symbolWithKey(a, "Helper", "go", "func:pkg::Helper")
	b := f.file("b/helper.go", "go")
	f.symbolWithKey(b, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("a/helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	stats := f.resolveAll()
	if stats.SymbolsBound != 0 {
		t.Fatalf("ResolveTestLinks() bound %d symbols, want 0 (ambiguous key)", stats.SymbolsBound)
	}
	if stats.FilesBound != 1 {
		t.Fatalf("ResolveTestLinks() file-bound %d rows, want 1 (sibling)", stats.FilesBound)
	}
	gotSym, gotFile := f.target(link)
	if gotSym != 0 {
		t.Fatalf("target symbol = %d, want unbound", gotSym)
	}
	if gotFile != a {
		t.Fatalf("target file = %d, want sibling %d", gotFile, a)
	}
	if reason := f.reason(link); reason != testLinkFileReason {
		t.Fatalf("reason = %q, want %q", reason, testLinkFileReason)
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

	if stats := f.resolveAll(); stats.SymbolsBound != 0 {
		t.Fatalf("ResolveTestLinks() bound %d symbols, want 0 (cross-language)", stats.SymbolsBound)
	}
	if gotSym, gotFile := f.target(link); gotSym != 0 || gotFile != 0 {
		t.Fatalf("target = (sym %d, file %d), want unbound", gotSym, gotFile)
	}
}

// TestResolveTestLinks_UnknownLanguageFailsClosed: an unclassified test file
// carries no language evidence, so the symbol gate refuses rather than
// guessing. The path convention still supplies the file-level relation -- that
// evidence is the file name itself.
func TestResolveTestLinks_UnknownLanguageFailsClosed(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("helper.go", "go")
	f.symbolWithKey(targetFile, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("helper_test.go", "")
	link := f.link(testFile, "func:pkg::Helper")

	if stats := f.resolveAll(); stats.SymbolsBound != 0 {
		t.Fatalf("ResolveTestLinks() bound %d symbols, want 0 (unknown source language)", stats.SymbolsBound)
	}
	gotSym, gotFile := f.target(link)
	if gotSym != 0 {
		t.Fatalf("target symbol = %d, want unbound", gotSym)
	}
	if gotFile != targetFile {
		t.Fatalf("target file = %d, want sibling %d", gotFile, targetFile)
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

	if stats := f.resolveAll(); stats.SymbolsBound != 2 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 2", stats.SymbolsBound)
	}
	if gotSym, gotFile := f.target(goLink); gotSym != goSym || gotFile != goFile {
		t.Fatalf("go link target = (sym %d, file %d), want (%d, %d)", gotSym, gotFile, goSym, goFile)
	}
	if gotSym, gotFile := f.target(pyLink); gotSym != pySym || gotFile != pyFile {
		t.Fatalf("python link target = (sym %d, file %d), want (%d, %d)", gotSym, gotFile, pySym, pyFile)
	}
}

// TestResolveTestLinks_RebindsStaleTarget replaces the old bind-only contract:
// a binding that no longer matches the row's key evidence is corrected to the
// current unique candidate. This is what makes an update sequence converge to
// the full-index answer instead of preserving whatever bound first.
func TestResolveTestLinks_RebindsStaleTarget(t *testing.T) {
	f := newTestLinkFixture(t)
	oldFile := f.file("old.go", "go")
	oldSym := f.symbolWithKey(oldFile, "Helper", "go", "func:other::Helper")
	newFile := f.file("helper.go", "go")
	newSym := f.symbolWithKey(newFile, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE test_links SET target_symbol_id = ?, target_file_id = ? WHERE id = ?`,
		oldSym, oldFile, link); err != nil {
		t.Fatalf("pre-bind test_link: %v", err)
	}

	stats := f.resolveAll()
	if stats.SymbolsUnbound != 1 || stats.SymbolsBound != 1 {
		t.Fatalf("ResolveTestLinks() = %+v, want 1 unbound + 1 bound", stats)
	}
	if gotSym, gotFile := f.target(link); gotSym != newSym || gotFile != newFile {
		t.Fatalf("target = (sym %d, file %d), want rebound (%d, %d)", gotSym, gotFile, newSym, newFile)
	}
}

// TestResolveTestLinks_AmbiguityIntroducedLaterUnbinds: a bound link whose key
// gains a second production candidate must return to unbound -- a fresh full
// index of the same tree would refuse to bind it, and update must agree. The
// sibling file relation survives the refusal.
func TestResolveTestLinks_AmbiguityIntroducedLaterUnbinds(t *testing.T) {
	f := newTestLinkFixture(t)
	a := f.file("a/helper.go", "go")
	f.symbolWithKey(a, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("a/helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	if stats := f.resolveAll(); stats.SymbolsBound != 1 {
		t.Fatalf("initial resolve bound %d, want 1", stats.SymbolsBound)
	}

	b := f.file("b/helper.go", "go")
	f.symbolWithKey(b, "Helper", "go", "func:pkg::Helper")

	stats := f.resolveAll()
	if stats.SymbolsUnbound != 1 {
		t.Fatalf("ResolveTestLinks() unbound %d rows, want 1 (ambiguity introduced)", stats.SymbolsUnbound)
	}
	gotSym, gotFile := f.target(link)
	if gotSym != 0 {
		t.Fatalf("target symbol = %d, want unbound after ambiguity", gotSym)
	}
	if gotFile != a {
		t.Fatalf("target file = %d, want sibling %d preserved", gotFile, a)
	}
}

// TestResolveTestLinks_AmbiguityRemovedRebinds: deleting one of two colliding
// candidates makes the survivor unique, and the next canonical pass binds it --
// including for links in files the deletion did not touch.
func TestResolveTestLinks_AmbiguityRemovedRebinds(t *testing.T) {
	f := newTestLinkFixture(t)
	a := f.file("a/helper.go", "go")
	aSym := f.symbolWithKey(a, "Helper", "go", "func:pkg::Helper")
	b := f.file("b/helper.go", "go")
	bSym := f.symbolWithKey(b, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("c/helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	if stats := f.resolveAll(); stats.SymbolsBound != 0 {
		t.Fatalf("initial resolve bound %d, want 0 (ambiguous)", stats.SymbolsBound)
	}

	f.deleteSymbol(bSym)

	if stats := f.resolveAll(); stats.SymbolsBound != 1 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 1 (ambiguity removed)", stats.SymbolsBound)
	}
	if gotSym, gotFile := f.target(link); gotSym != aSym || gotFile != a {
		t.Fatalf("target = (sym %d, file %d), want (%d, %d)", gotSym, gotFile, aSym, a)
	}
}

// TestResolveTestLinks_TestShadowNeverSteals covers P7 for test links: test
// files deliberately duplicate production names, and a link targets the code
// under test -- so a same-key helper in a test file neither binds nor makes the
// production candidate ambiguous.
func TestResolveTestLinks_TestShadowNeverSteals(t *testing.T) {
	f := newTestLinkFixture(t)
	prodFile := f.file("helper.go", "go")
	prodSym := f.symbolWithKey(prodFile, "Helper", "go", "func:pkg::Helper")
	shadowFile := f.file("shadow_test.go", "go")
	f.symbolWithKey(shadowFile, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	if stats := f.resolveAll(); stats.SymbolsBound != 1 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 1 (production wins)", stats.SymbolsBound)
	}
	if gotSym, gotFile := f.target(link); gotSym != prodSym || gotFile != prodFile {
		t.Fatalf("target = (sym %d, file %d), want production (%d, %d)", gotSym, gotFile, prodSym, prodFile)
	}
}

// TestResolveTestLinks_TestOnlyCandidateStaysUnbound: when the only definition
// of the guessed name lives in a test file, the link stays symbol-unbound --
// production code under test is never a test helper.
func TestResolveTestLinks_TestOnlyCandidateStaysUnbound(t *testing.T) {
	f := newTestLinkFixture(t)
	shadowFile := f.file("shadow_test.go", "go")
	f.symbolWithKey(shadowFile, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("other_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	if stats := f.resolveAll(); stats.SymbolsBound != 0 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 0 (test-only candidate)", stats.SymbolsBound)
	}
	if gotSym, _ := f.target(link); gotSym != 0 {
		t.Fatalf("target symbol = %d, want unbound", gotSym)
	}
}

// TestResolveTestLinks_NonTestFileRowsRemoved: rows declared by a file the
// shared policy rejects (a production file exporting TestConnection, written by
// an older producer) are deleted, never bound.
func TestResolveTestLinks_NonTestFileRowsRemoved(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("conn.go", "go")
	f.symbolWithKey(targetFile, "Connection", "go", "func:pkg::Connection")
	prodFile := f.file("health.go", "go")
	f.link(prodFile, "func:pkg::Connection")
	testFile := f.file("conn_test.go", "go")
	kept := f.link(testFile, "func:pkg::Connection")

	stats := f.resolveAll()
	if stats.RemovedNonTestRows != 1 {
		t.Fatalf("ResolveTestLinks() removed %d rows, want 1", stats.RemovedNonTestRows)
	}
	if got := f.rowCount(); got != 1 {
		t.Fatalf("test_links rows = %d, want 1", got)
	}
	if gotSym, _ := f.target(kept); gotSym == 0 {
		t.Fatalf("surviving test-file link stayed unbound")
	}
}

// TestResolveTestLinks_SiblingRemovedClearsFileTarget: the file-level relation
// is evidence-backed, so deleting the sibling clears it (and restores the
// producer reason) exactly as a fresh full index of the tree would.
func TestResolveTestLinks_SiblingRemovedClearsFileTarget(t *testing.T) {
	f := newTestLinkFixture(t)
	sibling := f.file("helper.go", "go")
	testFile := f.file("helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Missing")

	if stats := f.resolveAll(); stats.FilesBound != 1 {
		t.Fatalf("initial resolve file-bound %d, want 1", stats.FilesBound)
	}
	if _, gotFile := f.target(link); gotFile != sibling {
		t.Fatalf("target file = %d, want sibling %d", gotFile, sibling)
	}

	f.markFileDeleted(sibling)

	if stats := f.resolveAll(); stats.FilesBound != 0 {
		t.Fatalf("resolve after sibling deletion file-bound %d, want 0", stats.FilesBound)
	}
	if _, gotFile := f.target(link); gotFile != 0 {
		t.Fatalf("target file = %d, want cleared", gotFile)
	}
	if reason := f.reason(link); reason != "test_name_match" {
		t.Fatalf("reason = %q, want producer reason restored", reason)
	}
}

// TestResolveTestLinks_SymbolEvidenceSupersedesSibling: when the key resolves,
// the target file follows the symbol -- not the filename convention -- and the
// reason flips back to the producer's.
func TestResolveTestLinks_SymbolEvidenceSupersedesSibling(t *testing.T) {
	f := newTestLinkFixture(t)
	sibling := f.file("helper.go", "go")
	testFile := f.file("helper_test.go", "go")
	link := f.link(testFile, "func:pkg::Helper")

	if stats := f.resolveAll(); stats.FilesBound != 1 {
		t.Fatalf("initial resolve file-bound %d, want 1", stats.FilesBound)
	}
	if _, gotFile := f.target(link); gotFile != sibling {
		t.Fatalf("target file = %d, want sibling %d", gotFile, sibling)
	}

	elsewhere := f.file("impl/real.go", "go")
	realSym := f.symbolWithKey(elsewhere, "Helper", "go", "func:pkg::Helper")

	if stats := f.resolveAll(); stats.SymbolsBound != 1 {
		t.Fatalf("resolve after definition appeared bound %d, want 1", stats.SymbolsBound)
	}
	gotSym, gotFile := f.target(link)
	if gotSym != realSym || gotFile != elsewhere {
		t.Fatalf("target = (sym %d, file %d), want symbol evidence (%d, %d)", gotSym, gotFile, realSym, elsewhere)
	}
	if reason := f.reason(link); reason != "test_name_match" {
		t.Fatalf("reason = %q, want test_name_match", reason)
	}
}

// TestResolveTestLinks_EmptyKeyIsIgnored: rows written before migration 020 (or
// by a producer that minted no key) carry an empty key; they never match a
// symbol, and an existing binding on them is left untouched -- no evidence in
// either direction.
func TestResolveTestLinks_EmptyKeyIsIgnored(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("helper.go", "go")
	legacySym := f.symbolWithKey(targetFile, "Helper", "go", "")
	testFile := f.file("other_test.go", "go")
	link := f.link(testFile, "")
	bound := f.link(testFile, "")
	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE test_links SET target_symbol_id = ?, target_file_id = ? WHERE id = ?`,
		legacySym, targetFile, bound); err != nil {
		t.Fatalf("pre-bind legacy link: %v", err)
	}

	stats := f.resolveAll()
	if stats.SymbolsBound != 0 || stats.SymbolsUnbound != 0 {
		t.Fatalf("ResolveTestLinks() = %+v, want no symbol changes on empty keys", stats)
	}
	if gotSym, _ := f.target(link); gotSym != 0 {
		t.Fatalf("target symbol = %d, want unbound", gotSym)
	}
	if gotSym, gotFile := f.target(bound); gotSym != legacySym || gotFile != targetFile {
		t.Fatalf("legacy binding = (sym %d, file %d), want untouched (%d, %d)", gotSym, gotFile, legacySym, targetFile)
	}
}

// TestResolveTestLinks_SiblingChunksAcrossStatementBoundary drives more sibling
// pairs than testLinkResolveChunkSize so the chunking loop actually runs more
// than once; an off-by-one shows up here as a SQL error or a missed binding
// rather than silently on a large repo.
func TestResolveTestLinks_SiblingChunksAcrossStatementBoundary(t *testing.T) {
	f := newTestLinkFixture(t)
	const n = testLinkResolveChunkSize + 7

	links := make([]int64, 0, n)
	siblings := make([]int64, 0, n)
	for i := range n {
		sibling := f.file("pkg"+itoa(i)+"/helper.go", "go")
		testFile := f.file("pkg"+itoa(i)+"/helper_test.go", "go")
		siblings = append(siblings, sibling)
		links = append(links, f.link(testFile, "func:pkg"+itoa(i)+"::Missing"))
	}

	stats := f.resolveAll()
	if stats.FilesBound != n {
		t.Fatalf("ResolveTestLinks() file-bound %d rows, want %d", stats.FilesBound, n)
	}
	for i, link := range links {
		if _, gotFile := f.target(link); gotFile != siblings[i] {
			t.Fatalf("link %d file = %d, want %d", i, gotFile, siblings[i])
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// TestResolveTestLinks_Idempotent: a second pass over an unchanged tree changes
// nothing and reports zero work -- the canonical state is a fixed point.
func TestResolveTestLinks_Idempotent(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("helper.go", "go")
	f.symbolWithKey(targetFile, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("helper_test.go", "go")
	f.link(testFile, "func:pkg::Helper")
	f.link(testFile, "func:pkg::Missing")

	first := f.resolveAll()
	if first.SymbolsBound != 1 || first.FilesBound != 1 {
		t.Fatalf("first pass = %+v, want 1 symbol + 1 file bound", first)
	}
	second := f.resolveAll()
	if second != (TestLinkResolveStats{}) {
		t.Fatalf("second pass = %+v, want no work", second)
	}
}

// TestMigration025ClearsTestMainLinks: an existing database carries rows the
// retired TestMain linking wrote; re-applying migration 025 must delete exactly
// the rows whose test symbol is named TestMain and leave every other row alone.
func TestMigration025ClearsTestMainLinks(t *testing.T) {
	f := newTestLinkFixture(t)
	testFile := f.file("main_test.go", "go")
	mainSym := f.symbolWithKey(testFile, "TestMain", "go", "func:pkg::TestMain")
	helperSym := f.symbolWithKey(testFile, "TestHelper", "go", "func:pkg::TestHelper")
	insert := func(symID int64, key string) int64 {
		res, err := f.store.db.ExecContext(f.ctx, `
			INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score, target_stable_key)
			VALUES(?, ?, ?, NULL, NULL, 'test_name_match', 0.8, ?)
		`, f.repoID, testFile, symID, key)
		if err != nil {
			t.Fatalf("insert row: %v", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId: %v", err)
		}
		return id
	}
	legacy := insert(mainSym, "func:pkg::Main")
	kept := insert(helperSym, "func:pkg::Helper")

	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM schema_migrations WHERE version = 25`); err != nil {
		t.Fatalf("reset migration 25: %v", err)
	}
	if err := f.store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var n int
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT COUNT(*) FROM test_links WHERE id = ?`, legacy).Scan(&n); err != nil {
		t.Fatalf("count legacy row: %v", err)
	}
	if n != 0 {
		t.Fatalf("TestMain-minted row survived migration 025")
	}
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT COUNT(*) FROM test_links WHERE id = ?`, kept).Scan(&n); err != nil {
		t.Fatalf("count kept row: %v", err)
	}
	if n != 1 {
		t.Fatalf("unrelated row deleted by migration 025")
	}
}

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

	if stats := f.resolveAll(); stats.SymbolsBound != 1 {
		t.Fatalf("ResolveTestLinks() bound %d rows, want 1 (own repo only)", stats.SymbolsBound)
	}
	if gotSym, _ := f.target(mine); gotSym == 0 {
		t.Fatalf("own-repo link stayed unbound")
	}
	if gotSym, gotFile := f.target(otherLink); gotSym != 0 || gotFile != 0 {
		t.Fatalf("other repo's link changed to (sym %d, file %d), want untouched", gotSym, gotFile)
	}
}

// TestProductionSiblingPath pins the inverse-convention table.
func TestProductionSiblingPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"internal/store/store_test.go", "internal/store/store.go"},
		{"store_test.go", "store.go"},
		{`internal\store\store_test.go`, `internal\store\store.go`},
		{"pkg/test_utils.py", "pkg/utils.py"},
		{"pkg/utils_test.py", "pkg/utils.py"},
		{"pkg/utils.py", ""},        // not a test file
		{"pkg/store.go", ""},        // not a test file
		{"_test.go", ""},            // empty stem
		{"test_.py", ""},            // empty stem
		{"tests/test_utils.py", ""}, // sibling still under a test dir
		{"test_utils_test.py", ""},  // sibling would still be a test file
		{"pkg/helper.test.ts", ""},  // suffix-class languages not inverted
		{"pkg/HelperTest.java", ""}, // suffix-class languages not inverted
		{"pkg/helper_test.rs", ""},  // no bindable producer evidence today
		{"pkg/helper_spec.rb", ""},  // no bindable producer evidence today
	}
	for _, tc := range cases {
		if got := productionSiblingPath(tc.in); got != tc.want {
			t.Errorf("productionSiblingPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveTestLinksSiblingMatchesStoredSeparatorVariants(t *testing.T) {
	cases := []struct {
		name, testPath, targetPath, language string
	}{
		{"go slash", "a/shared_test.go", "a/shared.go", "go"},
		{"go native", `a\shared_test.go`, `a\shared.go`, "go"},
		{"go mixed", `a\shared_test.go`, "a/shared.go", "go"},
		{"python slash", "pkg/test_utils.py", "pkg/utils.py", "python"},
		{"python native", `pkg\test_utils.py`, `pkg\utils.py`, "python"},
		{"python mixed", `pkg\test_utils.py`, "pkg/utils.py", "python"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestLinkFixture(t)
			target := f.file(tc.targetPath, tc.language)
			testFile := f.file(tc.testPath, tc.language)
			link := f.link(testFile, "func:pkg::Shared")
			if got := f.resolveAll(); got.FilesBound != 1 {
				t.Fatalf("ResolveTestLinks() files bound = %d, want 1", got.FilesBound)
			}
			if gotSym, gotFile := f.target(link); gotSym != 0 || gotFile != target {
				t.Fatalf("target = (%d, %d), want file-only target (0, %d)", gotSym, gotFile, target)
			}
		})
	}
}

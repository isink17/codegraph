package store_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// The P6 invariant: any non-NULL test_links.target_symbol_id must reference an
// existing symbols row. The probe also covers the sibling id columns.
func (f *reindexFixture) assertNoDanglingTestLinks() {
	f.t.Helper()
	n, err := f.store.CountDanglingTestLinkRefsForTest(f.ctx, f.repoID)
	if err != nil {
		f.t.Fatalf("CountDanglingTestLinkRefsForTest() error = %v", err)
	}
	if n != 0 {
		f.t.Fatalf("dangling test_links id references = %d, want 0", n)
	}
}

// fork returns a fixture for a second repo backed by the same store/indexer,
// copying every field so new fixture fields cannot be silently dropped.
func (f *reindexFixture) fork(root string) *reindexFixture {
	g := *f
	g.repoRoot = root
	g.repoID = 0
	return &g
}

func (f *reindexFixture) testLinks() []store.TestLinkRow {
	f.t.Helper()
	links, err := f.store.TestLinksForTest(f.ctx, f.repoID)
	if err != nil {
		f.t.Fatalf("TestLinksForTest() error = %v", err)
	}
	return links
}

// testLink returns the single test_links row declared by testFile targeting
// targetFile.
func (f *reindexFixture) testLink(testFile, targetFile string) store.TestLinkRow {
	f.t.Helper()
	var found []store.TestLinkRow
	for _, l := range f.testLinks() {
		if l.TestFile == testFile && l.TargetFile == targetFile {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		f.t.Fatalf("test_links from %q to %q = %d, want 1 (%+v)", testFile, targetFile, len(found), found)
	}
	return found[0]
}

func assertTargetBound(t *testing.T, l store.TestLinkRow) int64 {
	t.Helper()
	if l.TargetSymbolID == nil {
		t.Fatalf("test_link %s -> %s target_symbol_id = NULL, want bound", l.TestFile, l.TargetFile)
	}
	return *l.TargetSymbolID
}

func assertTargetUnbound(t *testing.T, l store.TestLinkRow) {
	t.Helper()
	if l.TargetSymbolID != nil {
		t.Fatalf("test_link %s -> %s target_symbol_id = %d, want NULL",
			l.TestFile, l.TargetFile, *l.TargetSymbolID)
	}
}

const helperTestSrc = `package pkg

import "testing"

func TestHelper(t *testing.T) { Helper() }
`

// indexTargetsThenTests indexes the target files first and the test files in a
// second run.
//
// This used to be load-bearing: before P10, test links were bound during the
// write pass, so a test file indexed in the same batch as its target bound only
// if the target happened to be persisted first. Two-pass resolution removed that
// dependency (see two_pass_order_test.go), and the split is kept here only
// because these P6 regressions are about what happens on a *later* re-index, so
// they need the two runs to be distinct events.
func (f *reindexFixture) indexTargetsThenTests(targets, tests map[string]string) {
	f.t.Helper()
	for rel, src := range targets {
		f.write(rel, src)
	}
	f.index()
	for rel, src := range tests {
		f.write(rel, src)
	}
	f.update()
}

// TestReindexUnbindsTestLinkWhenTargetSymbolDisappears is the core P6
// regression: helper.go drops Helper while remaining indexed, so the test_link
// pointing at Helper must lose its symbol-level target instead of referencing a
// deleted symbols row. target_file_id must survive because helper.go survives.
func TestReindexUnbindsTestLinkWhenTargetSymbolDisappears(t *testing.T) {
	f := newReindexFixture(t)
	f.indexTargetsThenTests(
		map[string]string{"helper.go": "package pkg\n\nfunc Helper() {}\n"},
		map[string]string{"helper_test.go": helperTestSrc},
	)

	before := f.testLink("helper_test.go", "helper.go")
	targetID := assertTargetBound(t, before)
	f.assertNoDanglingTestLinks()

	// The symbol-level RelatedTests query must find the link while bound.
	related, err := f.store.RelatedTests(f.ctx, f.repoID, "Helper", "", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(symbol=Helper) error = %v", err)
	}
	if len(related) == 0 {
		t.Fatalf("RelatedTests(symbol=Helper) = 0 rows before removal, want >= 1")
	}

	// Re-index helper.go without Helper. The file itself is not deleted.
	f.write("helper.go", "package pkg\n\nfunc Other() {}\n")
	f.update()

	// Integrity probe first, so it is what fires when the invariant breaks.
	f.assertNoDanglingTestLinks()

	after := f.testLink("helper_test.go", "helper.go")
	assertTargetUnbound(t, after)
	if after.TargetFile != "helper.go" {
		t.Fatalf("target_file_id lost: target file = %q, want helper.go", after.TargetFile)
	}
	if after.Reason != before.Reason || after.Score != before.Score {
		t.Fatalf("test_link payload changed: %+v, want reason/score preserved from %+v", after, before)
	}
	if targetID == 0 {
		t.Fatalf("pre-removal target symbol id was 0")
	}

	// Symbol-level RelatedTests surfaces nothing once the target symbol is gone:
	// lookupSymbolID no longer finds it and reports ErrSymbolNotFound, which
	// RelatedTests propagates, so the (previously dangling) link was invisible
	// either way. This is the reported user-visible symptom, and it is why the
	// error tolerance below is intentional. The sentinel is CodeGraph's own --
	// it used to be a raw sql.ErrNoRows, which reached MCP clients verbatim.
	relatedSym, err := f.store.RelatedTests(f.ctx, f.repoID, "Helper", "", 10, 0)
	if err != nil && !errors.Is(err, store.ErrSymbolNotFound) {
		t.Fatalf("RelatedTests(symbol=Helper) post-removal error = %v", err)
	}
	if err != nil && errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("RelatedTests(symbol=Helper) leaked a driver error: %v", err)
	}
	if len(relatedSym) != 0 {
		t.Fatalf("RelatedTests(symbol=Helper) post-removal = %d rows, want 0: %+v", len(relatedSym), relatedSym)
	}

	// File-level RelatedTests must still work: the association survives.
	relatedFile, err := f.store.RelatedTests(f.ctx, f.repoID, "", "helper.go", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=helper.go) error = %v", err)
	}
	if len(relatedFile) == 0 {
		t.Fatalf("RelatedTests(file=helper.go) = 0 rows after symbol removal, want >= 1")
	}
}

// TestReindexRecreatedTargetHasNoStaleTestLinkID covers the replacement case:
// helper.go is re-indexed and still defines Helper, so its symbol row gets a
// new id. The test_links row must never keep the old id.
//
// P10 update: this test previously asserted the P6 limitation that the link
// stayed unbound until the *test* file was re-indexed, because the only code
// that could bind a target ran inside the test file's own insert. Pass 2 binds
// test links from the persisted `target_stable_key` after the batch completes,
// so re-indexing the target alone now rebinds the link to the new symbol row.
// The stale-id invariant the test was written for is unchanged and still
// asserted below; only the "stays NULL" clause was a statement about the old
// architecture.
func TestReindexRecreatedTargetHasNoStaleTestLinkID(t *testing.T) {
	f := newReindexFixture(t)
	f.indexTargetsThenTests(
		map[string]string{"helper.go": "package pkg\n\nfunc Helper() {}\n"},
		map[string]string{"helper_test.go": helperTestSrc},
	)

	oldID := assertTargetBound(t, f.testLink("helper_test.go", "helper.go"))

	// Helper survives by name, but its symbol row is replaced.
	f.write("helper.go", "package pkg\n\n// Helper does nothing.\nfunc Helper() {}\n")
	f.update()

	f.assertNoDanglingTestLinks()

	after := f.testLink("helper_test.go", "helper.go")
	if after.TargetSymbolID != nil && *after.TargetSymbolID == oldID {
		t.Fatalf("test_link kept stale symbol id %d after target row replacement", oldID)
	}
	if after.TargetFile != "helper.go" {
		t.Fatalf("target_file_id lost: target file = %q, want helper.go", after.TargetFile)
	}
	// P10: Pass 2 rebinds from the persisted target_stable_key, so re-indexing
	// only the target file is enough.
	rebound := assertTargetBound(t, after)
	if rebound == oldID {
		t.Fatalf("rebound target_symbol_id = %d, want a new symbol row", rebound)
	}

	// Re-indexing the test file keeps it bound to the same (current) symbol row.
	f.write("helper_test.go", helperTestSrc+"\n// touch\n")
	f.update()
	f.assertNoDanglingTestLinks()
	afterTestReindex := assertTargetBound(t, f.testLink("helper_test.go", "helper.go"))
	if afterTestReindex == oldID {
		t.Fatalf("target_symbol_id = %d, want a new symbol row", afterTestReindex)
	}
	if afterTestReindex != rebound {
		t.Fatalf("target_symbol_id = %d after test-file re-index, want %d (unchanged target row)",
			afterTestReindex, rebound)
	}
}

// TestReindexTargetAndTestFileInOneBatch covers the shape production actually
// produces: the target file and the test file change in the same
// ReplaceFileGraphsBatch transaction, so the DELETE of the test file's own
// test_links rows and the new unbind UPDATE interact inside one transaction.
func TestReindexTargetAndTestFileInOneBatch(t *testing.T) {
	f := newReindexFixture(t)
	f.indexTargetsThenTests(
		map[string]string{"helper.go": "package pkg\n\nfunc Helper() {}\n"},
		map[string]string{"helper_test.go": helperTestSrc},
	)
	assertTargetBound(t, f.testLink("helper_test.go", "helper.go"))

	// Both files re-indexed in the same batch: Helper disappears while the test
	// file keeps its TestHelper function.
	f.write("helper.go", "package pkg\n\nfunc Other() {}\n")
	f.write("helper_test.go", helperTestSrc+"\n// touch\n")
	f.update()

	f.assertNoDanglingTestLinks()

	// The row is rebuilt by the test file's own insert; the target no longer
	// exists at all, so both target columns come back empty.
	after := f.testLink("helper_test.go", "")
	assertTargetUnbound(t, after)
	if after.Reason != "test_name_match" {
		t.Fatalf("reason = %q, want test_name_match: %+v", after.Reason, after)
	}
}

// TestReindexUnbindsMultipleTestLinksTargetingOneSymbol covers several test
// files linking to the same removed target symbol.
func TestReindexUnbindsMultipleTestLinksTargetingOneSymbol(t *testing.T) {
	f := newReindexFixture(t)
	// Both test files link to the same target symbol (test_name_match trims the
	// "Test" prefix), producing two test_links rows pointing at Helper.
	f.indexTargetsThenTests(
		map[string]string{"helper.go": "package pkg\n\nfunc Helper() {}\n"},
		map[string]string{
			"helper_test.go": helperTestSrc,
			"extra_test.go":  helperTestSrc,
		},
	)

	assertTargetBound(t, f.testLink("helper_test.go", "helper.go"))
	assertTargetBound(t, f.testLink("extra_test.go", "helper.go"))

	f.write("helper.go", "package pkg\n\nfunc Other() {}\n")
	f.update()

	f.assertNoDanglingTestLinks()
	assertTargetUnbound(t, f.testLink("helper_test.go", "helper.go"))
	assertTargetUnbound(t, f.testLink("extra_test.go", "helper.go"))
}

// TestReindexUnbindsMultipleRemovedTargetsInOneBatch re-indexes two target
// files in a single replacement batch, each dropping its linked symbol.
func TestReindexUnbindsMultipleRemovedTargetsInOneBatch(t *testing.T) {
	f := newReindexFixture(t)
	f.indexTargetsThenTests(
		map[string]string{
			"helper.go": "package pkg\n\nfunc Helper() {}\n",
			"worker.go": "package pkg\n\nfunc Worker() {}\n",
		},
		map[string]string{
			"helper_test.go": helperTestSrc,
			"worker_test.go": "package pkg\n\nimport \"testing\"\n\nfunc TestWorker(t *testing.T) { Worker() }\n",
		},
	)

	assertTargetBound(t, f.testLink("helper_test.go", "helper.go"))
	assertTargetBound(t, f.testLink("worker_test.go", "worker.go"))

	f.write("helper.go", "package pkg\n\nfunc HelperGone() {}\n")
	f.write("worker.go", "package pkg\n\nfunc WorkerGone() {}\n")
	f.update()

	f.assertNoDanglingTestLinks()
	assertTargetUnbound(t, f.testLink("helper_test.go", "helper.go"))
	assertTargetUnbound(t, f.testLink("worker_test.go", "worker.go"))
}

// TestReindexTestLinkUnbindOnlyTouchesOwnRepo guards the repo_id filter on the
// unbind UPDATE.
func TestReindexTestLinkUnbindOnlyTouchesOwnRepo(t *testing.T) {
	targets := map[string]string{"helper.go": "package pkg\n\nfunc Helper() {}\n"}
	tests := map[string]string{"helper_test.go": helperTestSrc}

	a := newReindexFixture(t)
	a.indexTargetsThenTests(targets, tests)

	b := a.fork(t.TempDir())
	b.indexTargetsThenTests(targets, tests)
	if b.repoID == a.repoID {
		t.Fatalf("both fixtures resolved to repo %d, want distinct repos", a.repoID)
	}
	otherID := assertTargetBound(t, b.testLink("helper_test.go", "helper.go"))

	a.write("helper.go", "package pkg\n\nfunc Other() {}\n")
	a.update()

	a.assertNoDanglingTestLinks()
	assertTargetUnbound(t, a.testLink("helper_test.go", "helper.go"))

	b.assertNoDanglingTestLinks()
	stillBound := assertTargetBound(t, b.testLink("helper_test.go", "helper.go"))
	if stillBound != otherID {
		t.Fatalf("other repo test_link rebound %d -> %d, want untouched", otherID, stillBound)
	}
}

// TestDeletedFilePurgeStillDropsTestLinks guards the deleted-file purge path,
// which deletes the whole row (the target file itself is gone) rather than
// merely unbinding the symbol.
func TestDeletedFilePurgeStillDropsTestLinks(t *testing.T) {
	f := newReindexFixture(t)
	f.indexTargetsThenTests(
		map[string]string{"helper.go": "package pkg\n\nfunc Helper() {}\n"},
		map[string]string{"helper_test.go": helperTestSrc},
	)

	assertTargetBound(t, f.testLink("helper_test.go", "helper.go"))

	f.remove("helper.go")
	summary := f.update()
	if summary.FilesDeleted != 1 {
		t.Fatalf("FilesDeleted = %d, want 1", summary.FilesDeleted)
	}

	f.assertNoDanglingTestLinks()
	for _, l := range f.testLinks() {
		if l.TestFile == "helper_test.go" && l.TargetFile == "helper.go" {
			t.Fatalf("test_link to purged file survived: %+v", l)
		}
	}
	related, err := f.store.RelatedTests(f.ctx, f.repoID, "", "helper.go", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=helper.go) post-purge error = %v", err)
	}
	if len(related) != 0 {
		t.Fatalf("RelatedTests(file=helper.go) post-purge = %d rows, want 0", len(related))
	}
}

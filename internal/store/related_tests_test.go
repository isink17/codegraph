package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRelatedTests_FileScoped(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}

	targetA, err := insertTestFile(ctx, s, repo.ID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile(a.go) error = %v", err)
	}
	targetB, err := insertTestFile(ctx, s, repo.ID, "b.go")
	if err != nil {
		t.Fatalf("insertTestFile(b.go) error = %v", err)
	}
	testFile, err := insertTestFile(ctx, s, repo.ID, "a_test.go")
	if err != nil {
		t.Fatalf("insertTestFile(a_test.go) error = %v", err)
	}
	testSym, err := insertTestSymbol(ctx, s, repo.ID, testFile, "TestA", "TestA")
	if err != nil {
		t.Fatalf("insertTestSymbol(test) error = %v", err)
	}

	// Two links: one for a.go, one for b.go. Querying by file must return only the matching one.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score)
		VALUES(?, ?, ?, ?, NULL, 'name_match', 0.9)
	`, repo.ID, testFile, testSym, targetA); err != nil {
		t.Fatalf("insert test_link(a.go) error = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score)
		VALUES(?, ?, ?, ?, NULL, 'name_match', 0.9)
	`, repo.ID, testFile, testSym, targetB); err != nil {
		t.Fatalf("insert test_link(b.go) error = %v", err)
	}

	got, err := s.RelatedTests(ctx, repo.ID, "", "a.go", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=a.go) error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("RelatedTests(file=a.go) len=%d, want 1", len(got))
	}
	if got[0].File != "a_test.go" {
		t.Fatalf("RelatedTests(file=a.go) file=%q, want %q", got[0].File, "a_test.go")
	}
	if got[0].Symbol != "TestA" {
		t.Fatalf("RelatedTests(file=a.go) symbol=%q, want %q", got[0].Symbol, "TestA")
	}
}

func TestRelatedTests_FileScopedMatchesWindowsStoredPath(t *testing.T) {
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
	target, err := insertTestFile(ctx, s, repo.ID, `pkg\utils.py`)
	if err != nil {
		t.Fatalf("insertTestFile(target) error = %v", err)
	}
	testFile, err := insertTestFile(ctx, s, repo.ID, `pkg\test_utils.py`)
	if err != nil {
		t.Fatalf("insertTestFile(test) error = %v", err)
	}
	testSym, err := insertTestSymbol(ctx, s, repo.ID, testFile, "test_utils", "test_utils")
	if err != nil {
		t.Fatalf("insertTestSymbol() error = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score)
		VALUES(?, ?, ?, ?, NULL, 'test_file_name_match', 0.8)
	`, repo.ID, testFile, testSym, target); err != nil {
		t.Fatalf("insert test_link: %v", err)
	}

	got, err := s.RelatedTests(ctx, repo.ID, "", "pkg/utils.py", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests() error = %v", err)
	}
	if len(got) != 1 || got[0].File != "pkg/test_utils.py" {
		t.Fatalf("RelatedTests() = %+v, want canonical Python test path", got)
	}
}

// callEvidenceFixture builds the P22.2 end-to-end shape: a production type with
// two methods, and one test function per method, where each test's only
// relation to its method is a resolved call edge (Go test names rarely spell a
// symbol, so name-guess links stay unbound here -- exactly the real-repo case).
type callEvidenceFixture struct {
	*testLinkFixture
	prodFile    int64
	processSym  int64
	otherSym    int64
	processTest int64
	otherTest   int64
}

func (f *testLinkFixture) linkedTestSymbol(testFileID int64, name string) int64 {
	f.t.Helper()
	symID := f.symbolWithKey(testFileID, name, "go", "func:pkg::"+name)
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score, target_stable_key)
		VALUES(?, ?, ?, NULL, NULL, 'test_name_match', 0.8, ?)
	`, f.repoID, testFileID, symID, "func:pkg::"+name[len("Test"):]); err != nil {
		f.t.Fatalf("insert linked test symbol %q: %v", name, err)
	}
	return symID
}

func (f *testLinkFixture) callEdge(srcSymID, dstSymID, srcFileID int64) {
	f.t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, ?, 'x', 'calls', '', ?, 1)
	`, f.repoID, srcSymID, dstSymID, srcFileID); err != nil {
		f.t.Fatalf("insert call edge: %v", err)
	}
}

func newCallEvidenceFixture(t *testing.T) *callEvidenceFixture {
	f := newTestLinkFixture(t)
	prodFile := f.file("service.go", "go")
	processSym := f.symbolWithKey(prodFile, "Process", "go", "func:pkg:Service:Process")
	otherSym := f.symbolWithKey(prodFile, "Other", "go", "func:pkg:Service:Other")
	// Distinct qualified names, as the Go parsers persist them for methods.
	for sym, qname := range map[int64]string{processSym: "pkg.Service.Process", otherSym: "pkg.Service.Other"} {
		if _, err := f.store.db.ExecContext(f.ctx,
			`UPDATE symbols SET qualified_name = ? WHERE id = ?`, qname, sym); err != nil {
			t.Fatalf("set qualified name: %v", err)
		}
	}
	testFile := f.file("process_test.go", "go")
	processTest := f.linkedTestSymbol(testFile, "TestProcessHandlesInput")
	otherTest := f.linkedTestSymbol(testFile, "TestOtherRejectsInput")
	f.callEdge(processTest, processSym, testFile)
	f.callEdge(otherTest, otherSym, testFile)
	if stats := f.resolveAll(); stats.SymbolsBound != 0 {
		t.Fatalf("name-guess keys unexpectedly bound %d symbols", stats.SymbolsBound)
	}
	return &callEvidenceFixture{
		testLinkFixture: f,
		prodFile:        prodFile,
		processSym:      processSym,
		otherSym:        otherSym,
		processTest:     processTest,
		otherTest:       otherTest,
	}
}

// TestRelatedTests_SymbolCallEvidence: a symbol seed returns exactly the tests
// with a resolved call edge into it -- and never a same-file test of a
// different method. This is the precision half of the §17 contract.
func TestRelatedTests_SymbolCallEvidence(t *testing.T) {
	f := newCallEvidenceFixture(t)

	got, err := f.store.RelatedTests(f.ctx, f.repoID, "pkg.Service.Process", "", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(symbol=pkg.Service.Process) error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("RelatedTests(symbol) = %+v, want exactly the calling test", got)
	}
	if got[0].File != "process_test.go" || got[0].Symbol != "TestProcessHandlesInput" {
		t.Fatalf("RelatedTests(symbol) = %+v, want TestProcessHandlesInput", got[0])
	}
	if got[0].Reason != "test_calls" || got[0].Score != 0.9 {
		t.Fatalf("RelatedTests(symbol) provenance = %s/%.2f, want test_calls/0.90", got[0].Reason, got[0].Score)
	}
}

// TestRelatedTests_FileCallEvidence: a file seed reaches every test calling any
// symbol the file defines. Both methods live in service.go, so both tests are
// related at file level -- the documented coarser semantics.
func TestRelatedTests_FileCallEvidence(t *testing.T) {
	f := newCallEvidenceFixture(t)

	got, err := f.store.RelatedTests(f.ctx, f.repoID, "", "service.go", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=service.go) error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("RelatedTests(file) = %+v, want both calling tests", got)
	}
	for _, item := range got {
		if item.Reason != "test_calls" {
			t.Fatalf("RelatedTests(file) reason = %q, want test_calls: %+v", item.Reason, item)
		}
	}
}

// TestRelatedTests_DedupesLinkAndCallEvidence: when one test is related through
// both a persisted link row and a call edge, the row appears once with the
// strongest evidence.
func TestRelatedTests_DedupesLinkAndCallEvidence(t *testing.T) {
	f := newTestLinkFixture(t)
	prodFile := f.file("helper.go", "go")
	prodSym := f.symbolWithKey(prodFile, "Helper", "go", "func:pkg::Helper")
	testFile := f.file("helper_test.go", "go")
	testSym := f.linkedTestSymbol(testFile, "TestHelper")
	f.callEdge(testSym, prodSym, testFile)
	if stats := f.resolveAll(); stats.SymbolsBound != 1 {
		t.Fatalf("resolve bound %d, want 1", stats.SymbolsBound)
	}

	for _, seed := range []struct{ symbol, file string }{
		{symbol: "Helper"}, {file: "helper.go"},
	} {
		got, err := f.store.RelatedTests(f.ctx, f.repoID, seed.symbol, seed.file, 10, 0)
		if err != nil {
			t.Fatalf("RelatedTests(%+v) error = %v", seed, err)
		}
		if len(got) != 1 {
			t.Fatalf("RelatedTests(%+v) = %+v, want one deduplicated row", seed, got)
		}
		if got[0].Reason != "test_calls" || got[0].Score != 0.9 {
			t.Fatalf("RelatedTests(%+v) kept %s/%.2f, want strongest test_calls/0.90", seed, got[0].Reason, got[0].Score)
		}
	}
}

// TestRelatedTests_ScoreTieAcrossReasonsIsDeterministic: a name-guess row and a
// sibling-bound row can share one (path, ”) group at the same producer score
// (test_symbol_id NULL on both). The pick must be the ranked winner
// (test_name_match), never whichever row SQLite scanned last.
func TestRelatedTests_ScoreTieAcrossReasonsIsDeterministic(t *testing.T) {
	f := newTestLinkFixture(t)
	target := f.file("helper.go", "go")
	testFile := f.file("helper_test.go", "go")
	// Both rows have NULL test_symbol_id and target helper.go: one as a raw
	// name-guess row pre-bound at file level, one sibling-bound by the pass.
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score, target_stable_key)
		VALUES(?, ?, NULL, ?, NULL, 'test_name_match', 0.8, 'func:pkg::KeptByEmptyCandidacy'),
			  (?, ?, NULL, ?, NULL, 'test_file_name_match', 0.8, 'func:pkg::Missing')
	`, f.repoID, testFile, target, f.repoID, testFile, target); err != nil {
		t.Fatalf("insert tied rows: %v", err)
	}

	for range 5 {
		got, err := f.store.RelatedTests(f.ctx, f.repoID, "", "helper.go", 10, 0)
		if err != nil {
			t.Fatalf("RelatedTests() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("RelatedTests() = %+v, want one deduplicated row", got)
		}
		if got[0].Reason != "test_name_match" {
			t.Fatalf("tied-score reason = %q, want ranked winner test_name_match", got[0].Reason)
		}
	}
}

// TestRelatedTests_FileSeedExcludesOwnTests: seeding the file branch with a
// test file must not list that file's own tests as "related" merely because
// they call helpers defined in the same file.
func TestRelatedTests_FileSeedExcludesOwnTests(t *testing.T) {
	f := newTestLinkFixture(t)
	testFile := f.file("helper_test.go", "go")
	helperSym := f.symbolWithKey(testFile, "makeFixture", "go", "func:pkg::makeFixture")
	testSym := f.linkedTestSymbol(testFile, "TestSomething")
	f.callEdge(testSym, helperSym, testFile)

	got, err := f.store.RelatedTests(f.ctx, f.repoID, "", "helper_test.go", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=helper_test.go) error = %v", err)
	}
	for _, item := range got {
		if item.File == "helper_test.go" && item.Reason == "test_calls" {
			t.Fatalf("seed test file returned itself via call evidence: %+v", got)
		}
	}
}

// TestRelatedTests_FileScopedNonGoTestFile: the file branch no longer carries
// the Go-only `%_test.go` filter, so a Python test file's file-level link is
// visible too.
func TestRelatedTests_FileScopedNonGoTestFile(t *testing.T) {
	f := newTestLinkFixture(t)
	prodFile := f.file("pkg/utils.py", "python")
	testFile := f.file("pkg/test_utils.py", "python")
	link := f.link(testFile, "func:utils::missing")
	if stats := f.resolveAll(); stats.FilesBound != 1 {
		t.Fatalf("resolve file-bound %d, want 1", stats.FilesBound)
	}
	if _, gotFile := f.target(link); gotFile != prodFile {
		t.Fatalf("target file = %d, want %d", gotFile, prodFile)
	}

	got, err := f.store.RelatedTests(f.ctx, f.repoID, "", "pkg/utils.py", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=pkg/utils.py) error = %v", err)
	}
	if len(got) != 1 || got[0].File != "pkg/test_utils.py" {
		t.Fatalf("RelatedTests(file=pkg/utils.py) = %+v, want the python test file", got)
	}
}

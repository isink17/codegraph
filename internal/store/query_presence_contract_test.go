package store

import (
	"context"
	"runtime"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestQueryPresenceContractIsPageIndependent(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := testContext()
	fileID, err := insertTestFile(ctx, s, repoID, "presence.go")
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := insertTestSymbol(ctx, s, repoID, fileID, "Known", "pkg.Known")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO symbol_fts(repo_id, symbol_id, name, qualified_name, signature, doc_summary) VALUES(?, ?, ?, ?, '', '')`, repoID, targetID, "Known", "pkg.Known"); err != nil {
		t.Fatal(err)
	}
	callerID, err := insertTestSymbol(ctx, s, repoID, fileID, "Caller", "pkg.Caller")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestEdge(ctx, s, repoID, fileID, callerID, "MissingTarget"); err != nil {
		t.Fatal(err)
	}

	for name, got := range map[string]SymbolSearchResult{
		"find":   must(s.FindSymbolExactResult(ctx, repoID, "Known", 1, 100)),
		"search": must(s.SearchSymbolsResult(ctx, repoID, "Known", 1, 100)),
	} {
		if !got.Matched || len(got.Matches) != 0 {
			t.Fatalf("%s high-offset result = %+v, want matched=true and empty page", name, got)
		}
	}
	missingSearch := must(s.SearchSymbolsResult(ctx, repoID, "Absent", 1, 0))
	if missingSearch.Matched || len(missingSearch.Matches) != 0 {
		t.Fatalf("missing search = %+v", missingSearch)
	}

	callers := must(s.FindCallersResult(ctx, repoID, "Known", 0, 1, 100))
	if !callers.TargetFound || len(callers.Callers) != 0 {
		t.Fatalf("known callers page = %+v, want found-empty", callers)
	}
	unknown := must(s.FindCallersResult(ctx, repoID, "MissingTarget", 0, 10, 0))
	if unknown.TargetFound || len(unknown.Callers) != 0 || len(unknown.UnresolvedHints) == 0 {
		t.Fatalf("unknown callers = %+v, want separate hints", unknown)
	}
	stale := must(s.FindCallersResult(ctx, repoID, "Known", targetID+999999, 10, 0))
	if stale.TargetFound || len(stale.Callers) != 0 || len(stale.UnresolvedHints) != 0 {
		t.Fatalf("stale exact ID = %+v", stale)
	}
	foreignRepo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foreign := must(s.FindCalleesResult(ctx, foreignRepo.ID, "Known", targetID, 10, 0))
	if foreign.TargetFound || len(foreign.Callees) != 0 {
		t.Fatalf("foreign exact ID = %+v", foreign)
	}

	related := must(s.RelatedTestsResult(ctx, repoID, "Known", "", 10, 0))
	if !related.TargetFound || len(related.Tests) != 0 {
		t.Fatalf("known related tests = %+v, want found-empty", related)
	}
	missingRelated := must(s.RelatedTestsResult(ctx, repoID, "Absent", "", 10, 0))
	if missingRelated.TargetFound || len(missingRelated.Tests) != 0 {
		t.Fatalf("missing related tests = %+v", missingRelated)
	}
	existingFile := must(s.RelatedTestsResult(ctx, repoID, "", "presence.go", 10, 0))
	missingFile := must(s.RelatedTestsResult(ctx, repoID, "", "absent.go", 10, 0))
	if !existingFile.TargetFound || missingFile.TargetFound {
		t.Fatalf("file presence = existing:%+v missing:%+v", existingFile, missingFile)
	}

	impact := must(s.ImpactRadius(ctx, repoID, []string{"Known", "Absent"}, nil, 1, 1, 100))
	presence, ok := impact["seed_presence"].(ImpactSeedPresence)
	if !ok || presence.Requested != 2 || presence.Found != 1 || len(presence.Missing) != 1 || len(impact["symbols"].([]graph.Symbol)) != 0 {
		t.Fatalf("impact presence/result = %#v / %#v", impact["seed_presence"], impact["symbols"])
	}

	trace := must(s.TraceDependenciesResult(ctx, repoID, "Known", "downstream", 1, 1, 100))
	if !trace.TargetFound || len(trace.Dependencies) != 0 || trace.Total == 0 {
		t.Fatalf("trace high-offset result = %+v, want found with empty page", trace)
	}
	missingTrace := must(s.TraceDependenciesResult(ctx, repoID, "Absent", "downstream", 1, 10, 0))
	if missingTrace.TargetFound || len(missingTrace.Dependencies) != 0 || missingTrace.Total != 0 {
		t.Fatalf("missing trace = %+v", missingTrace)
	}
}

func TestRelatedTestFilesPresentPreservesRequestedOrder(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := testContext()
	for _, path := range []string{"a.go", "b.go"} {
		if _, err := insertTestFile(ctx, s, repoID, path); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.RelatedTestFilesPresent(ctx, repoID, []string{"b.go", "missing.go", "a.go", "b.go", ""})
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{true, false, true, true, false}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RelatedTestFilesPresent() = %v, want %v", got, want)
		}
	}
}

func TestRelatedTestsFilePresenceTracksActiveLifecycle(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("target.go", "go")
	testFile := f.file("target_test.go", "go")
	testSymbol := f.symbolWithKey(testFile, "TestTarget", "go", "func:TestTarget")
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score)
		VALUES(?, ?, ?, ?, NULL, 'test_file_name_match', 0.8)
	`, f.repoID, testFile, testSymbol, targetFile); err != nil {
		t.Fatal(err)
	}

	assertResult := func(wantFound bool, wantTests int) {
		t.Helper()
		result, err := f.store.RelatedTestsResult(f.ctx, f.repoID, "", "target.go", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if result.TargetFound != wantFound || len(result.Tests) != wantTests {
			t.Fatalf("RelatedTestsResult() = %+v, want found=%v tests=%d", result, wantFound, wantTests)
		}
	}
	assertResult(true, 1)

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted = 1 WHERE id = ?`, targetFile); err != nil {
		t.Fatal(err)
	}
	assertResult(false, 0)
	if tests, err := f.store.RelatedTests(f.ctx, f.repoID, "", "target.go", 10, 0); err != nil || len(tests) != 0 {
		t.Fatalf("RelatedTests() for deleted target = %+v, %v; want empty", tests, err)
	}

	f.file("active.go", "go")
	deleted := f.file("deleted.go", "go")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted = 1 WHERE id = ?`, deleted); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.RelatedTestFilesPresent(f.ctx, f.repoID, []string{
		"active.go", "deleted.go", "missing.go", "active.go", "deleted.go", "../invalid.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{true, false, false, true, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RelatedTestFilesPresent() = %v, want %v", got, want)
		}
	}
	for _, tc := range []struct {
		path string
		want bool
	}{{"active.go", true}, {"deleted.go", false}, {"missing.go", false}, {"../invalid.go", false}} {
		present, err := f.store.filePresent(f.ctx, f.repoID, tc.path)
		if err != nil || present != tc.want {
			t.Fatalf("filePresent(%q) = %v, %v; want %v", tc.path, present, err, tc.want)
		}
	}
	if runtime.GOOS != "windows" {
		backslash := f.file(`d/x\y.go`, "go")
		f.file("d/x/y.go", "go")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted = 1 WHERE id = ?`, backslash); err != nil {
			t.Fatal(err)
		}
		got, err := f.store.RelatedTestFilesPresent(f.ctx, f.repoID, []string{`d/x\y.go`, "d/x/y.go"})
		if err != nil || len(got) != 2 || got[0] || !got[1] {
			t.Fatalf("separator siblings after exact delete = %v, %v; want [false true]", got, err)
		}
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted = 0 WHERE id = ?`, targetFile); err != nil {
		t.Fatal(err)
	}
	assertResult(true, 1)
}

func TestFindCalleesPresenceSurvivesHighOffset(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := testContext()
	fileID, err := insertTestFile(ctx, s, repoID, "callee_presence.go")
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := insertTestSymbol(ctx, s, repoID, fileID, "Target", "pkg.Target")
	if err != nil {
		t.Fatal(err)
	}
	calleeID, err := insertTestSymbol(ctx, s, repoID, fileID, "Callee", "pkg.Callee")
	if err != nil {
		t.Fatal(err)
	}
	edgeID, err := insertTestEdge(ctx, s, repoID, fileID, targetID, "pkg.Callee")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, calleeID, edgeID); err != nil {
		t.Fatal(err)
	}

	control := must(s.FindCalleesResult(ctx, repoID, "Target", 0, 1, 0))
	if !control.TargetFound || len(control.Callees) != 1 || control.Callees[0].ID != calleeID {
		t.Fatalf("callee control = %+v, want found with one callee", control)
	}
	highOffset := must(s.FindCalleesResult(ctx, repoID, "Target", 0, 1, 100))
	if !highOffset.TargetFound || len(highOffset.Callees) != 0 {
		t.Fatalf("callee high-offset = %+v, want found-empty", highOffset)
	}
}

func TestRelatedTestsPresenceSurvivesHighOffset(t *testing.T) {
	f := newTestLinkFixture(t)
	targetFile := f.file("target.go", "go")
	targetID := f.symbolWithKey(targetFile, "Target", "go", "func:Target")
	testFile := f.file("target_test.go", "go")
	testID := f.symbolWithKey(testFile, "TestTarget", "go", "func:TestTarget")
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score)
		VALUES(?, ?, ?, ?, ?, 'test_name_match', 0.8)
	`, f.repoID, testFile, testID, targetFile, targetID); err != nil {
		t.Fatal(err)
	}

	control := must(f.store.RelatedTestsResult(f.ctx, f.repoID, "Target", "", 1, 0))
	if !control.TargetFound || len(control.Tests) != 1 || control.Tests[0].Symbol != "TestTarget" {
		t.Fatalf("related-test control = %+v, want found with one test", control)
	}
	highOffset := must(f.store.RelatedTestsResult(f.ctx, f.repoID, "Target", "", 1, 100))
	if !highOffset.TargetFound || len(highOffset.Tests) != 0 {
		t.Fatalf("related-test high-offset = %+v, want found-empty", highOffset)
	}

	fileControl := must(f.store.RelatedTestsResult(f.ctx, f.repoID, "", "target.go", 1, 0))
	if !fileControl.TargetFound || len(fileControl.Tests) != 1 {
		t.Fatalf("related-test file control = %+v, want found with one test", fileControl)
	}
	fileHighOffset := must(f.store.RelatedTestsResult(f.ctx, f.repoID, "", "target.go", 1, 100))
	if !fileHighOffset.TargetFound || len(fileHighOffset.Tests) != 0 {
		t.Fatalf("related-test file high-offset = %+v, want found-empty", fileHighOffset)
	}
}

func TestImpactSeedBatchKeepsQualifiedAndRepositoryResolution(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := testContext()
	fileA, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatal(err)
	}
	fileB, err := insertTestFile(ctx, s, repoID, "b.go")
	if err != nil {
		t.Fatal(err)
	}
	wantID, err := insertTestSymbol(ctx, s, repoID, fileA, "Shared", "pkg.one.Shared")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := insertTestSymbol(ctx, s, repoID, fileB, "Shared", "pkg.two.Shared")
	if err != nil {
		t.Fatal(err)
	}
	edgeID, err := insertTestEdge(ctx, s, repoID, fileA, wantID, "pkg.two.Shared")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, otherID, edgeID); err != nil {
		t.Fatal(err)
	}
	foreignRepo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foreignFile, err := insertTestFile(ctx, s, foreignRepo.ID, "foreign.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbol(ctx, s, foreignRepo.ID, foreignFile, "Shared", "foreign.Shared"); err != nil {
		t.Fatal(err)
	}

	qualified := must(s.ImpactRadius(ctx, repoID, []string{"pkg.one.Shared"}, nil, 1, 10, 0))
	qualifiedPresence := qualified["seed_presence"].(ImpactSeedPresence)
	if qualifiedPresence.Found != 1 || qualifiedPresence.Missing != nil {
		t.Fatalf("qualified seed presence = %+v", qualifiedPresence)
	}
	qualifiedSymbols := qualified["symbols"].([]graph.Symbol)
	if len(qualifiedSymbols) != 2 || !hasSymbolID(qualifiedSymbols, wantID) || !hasSymbolID(qualifiedSymbols, otherID) {
		t.Fatalf("qualified seed traversal = %+v, want seed %d and neighbor %d", qualifiedSymbols, wantID, otherID)
	}

	bare := must(s.ImpactRadius(ctx, repoID, []string{"Shared", "pkg.two.Shared", "Absent", "Shared"}, nil, 0, 10, 0))
	barePresence := bare["seed_presence"].(ImpactSeedPresence)
	if barePresence.Requested != 4 || barePresence.Found != 3 || len(barePresence.Missing) != 1 {
		t.Fatalf("batched seed presence = %+v", barePresence)
	}
	seen := map[int64]bool{}
	for _, sym := range bare["symbols"].([]graph.Symbol) {
		seen[sym.ID] = true
	}
	if !seen[wantID] || !seen[otherID] {
		t.Fatalf("batched seed symbols lost deterministic resolution: %+v", bare["symbols"])
	}
}

func testContext() context.Context { return context.Background() }

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

// Presence is reported next to the related-test result for the same request
// path, so it must address the row that lookup addresses. Both helpers go
// through platform.PublicRepositoryPath, exactly as Store.RelatedTests does.
// The former CanonicalRelPath(normalizeRepoRelPath(...)) pair trimmed
// whitespace the lookup keeps, so " a.go" was reported present while the tests
// for it came back empty.
func TestFilePresenceUsesThePublicRequestBoundary(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := testContext()
	stored := []string{"a.go", "d/x/y.go", `d/x\y.go`}
	for _, path := range stored {
		if _, err := insertTestFile(ctx, s, repoID, path); err != nil {
			t.Fatalf("insertTestFile(%q) error = %v", path, err)
		}
	}

	// Every stored identity resolves as itself, and the separator siblings stay
	// two files rather than one bucket.
	for _, path := range stored {
		got, err := s.RelatedTestFilesPresent(ctx, repoID, []string{path})
		if err != nil {
			t.Fatalf("RelatedTestFilesPresent(%q) error = %v", path, err)
		}
		if len(got) != 1 || !got[0] {
			t.Fatalf("stored path %q reported absent: %v", path, got)
		}
		present, err := s.filePresent(ctx, repoID, path)
		if err != nil {
			t.Fatalf("filePresent(%q) error = %v", path, err)
		}
		if !present {
			t.Fatalf("filePresent(%q) = false, want true", path)
		}
	}

	// Negative control. None of these name an indexed file: the whitespace
	// variants differ from "a.go" in bytes the lookup preserves, and the rest are
	// not valid public repository spellings at all. A vacuous version of this
	// assertion is ruled out by the positive rungs above, which share the same
	// call path.
	//
	// `d/x\y.go` versus `d/x/y.go` is asserted here only as the POSIX case, where
	// a backslash is filename data. On Windows platform.PublicRepositoryPath
	// deliberately accepts the host separator, so that pair is one request
	// spelling there; proving that needs a Windows run, not this test.
	for _, absent := range []string{" a.go", "a.go ", "/a.go", "../a.go", "d/x/../x/y.go.bak", ""} {
		got, err := s.RelatedTestFilesPresent(ctx, repoID, []string{absent})
		if err != nil {
			t.Fatalf("RelatedTestFilesPresent(%q) error = %v", absent, err)
		}
		if len(got) != 1 || got[0] {
			t.Fatalf("path %q reported present; it names no indexed file: %v", absent, got)
		}
		present, err := s.filePresent(ctx, repoID, absent)
		if err != nil {
			t.Fatalf("filePresent(%q) error = %v", absent, err)
		}
		if present {
			t.Fatalf("filePresent(%q) = true, want false", absent)
		}
	}
}

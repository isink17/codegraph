package store

import (
	"context"
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

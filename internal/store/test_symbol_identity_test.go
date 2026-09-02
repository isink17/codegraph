package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

func testSymbol(index int) *int { return &index }

func persistCollisionFixture(t *testing.T, symbols []graph.Symbol, links []graph.TestLink) []store.TestLinkRow {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ReplaceFileGraphsBatch(ctx, repo.ID, 1, []store.ReplaceFileGraphInput{{
		Path: "identity_test.go", Language: "go", ContentHash: "identity",
		Parsed: graph.ParsedFile{Language: "go", Symbols: symbols, TestLinks: links},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.TestLinksForTest(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func collisionSymbols() []graph.Symbol {
	return []graph.Symbol{
		{Language: "go", Kind: "function", Name: "TestA", QualifiedName: "pkg.TestA", StableKey: "same", Range: graph.Position{StartLine: 1, EndLine: 3}},
		{Language: "go", Kind: "function", Name: "TestB", QualifiedName: "pkg.TestB", StableKey: "same", Range: graph.Position{StartLine: 5, EndLine: 7}},
	}
}

func collisionLinks(order []int) []graph.TestLink {
	links := make([]graph.TestLink, 0, len(order))
	for _, index := range order {
		name := "A"
		if index == 1 {
			name = "B"
		}
		links = append(links, graph.TestLink{
			TestName: name, TestSymbolKey: "same", TestSymbolIndex: testSymbol(index),
			TargetStableKey: "target-" + name, Reason: "test_name_match",
		})
	}
	return links
}

func TestPersistedTestSymbolIdentitySurvivesStableKeyCollision(t *testing.T) {
	rows := persistCollisionFixture(t, collisionSymbols(), collisionLinks([]int{0, 1}))
	owners := map[string]string{}
	for _, row := range rows {
		owners[row.TargetStableKey] = row.TestSymbolName
	}
	if len(rows) != 2 || owners["target-A"] != "TestA" || owners["target-B"] != "TestB" {
		t.Fatalf("test link ownership = %+v, want target-A->TestA and target-B->TestB", rows)
	}
	if rows[0].TestSymbolID == nil || rows[1].TestSymbolID == nil || *rows[0].TestSymbolID == *rows[1].TestSymbolID {
		t.Fatalf("test link IDs = %+v, want two distinct persisted IDs", rows)
	}
}

func TestPersistedTestSymbolIdentityMissingReferenceFailsClosed(t *testing.T) {
	rows := persistCollisionFixture(t, collisionSymbols(), []graph.TestLink{{
		TestName: "A", TestSymbolKey: "same", TargetStableKey: "target-A", Reason: "test_name_match",
	}})
	if len(rows) != 1 || rows[0].TestSymbolID != nil {
		t.Fatalf("missing exact reference = %+v, want NULL test_symbol_id", rows)
	}
	rows = persistCollisionFixture(t, collisionSymbols(), []graph.TestLink{{
		TestName: "A", TestSymbolKey: "same", TestSymbolIndex: testSymbol(2), TargetStableKey: "target-A", Reason: "test_name_match",
	}})
	if len(rows) != 1 || rows[0].TestSymbolID != nil {
		t.Fatalf("invalid exact reference = %+v, want NULL test_symbol_id", rows)
	}
}

func TestPersistedTestSymbolIdentityFollowsReversedEmission(t *testing.T) {
	rows := persistCollisionFixture(t, collisionSymbols(), collisionLinks([]int{1, 0}))
	owners := map[string]string{}
	for _, row := range rows {
		owners[row.TargetStableKey] = row.TestSymbolName
	}
	if owners["target-A"] != "TestA" || owners["target-B"] != "TestB" {
		t.Fatalf("reversed emission ownership = %+v, want target-A->TestA and target-B->TestB", rows)
	}
}

func TestPersistedTestSymbolIdentityRejectsNonTestReference(t *testing.T) {
	symbols := append(collisionSymbols(), graph.Symbol{Language: "go", Kind: "type", Name: "NotATest", StableKey: "other"})
	rows := persistCollisionFixture(t, symbols, []graph.TestLink{{
		TestName: "A", TestSymbolKey: "other", TestSymbolIndex: testSymbol(2), TargetStableKey: "target-A", Reason: "test_name_match",
	}})
	if len(rows) != 1 || rows[0].TestSymbolID != nil {
		t.Fatalf("non-test reference = %+v, want NULL test_symbol_id", rows)
	}
}

func TestRelatedTestsUsesExactPersistedTestSymbolIdentity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	targets := graph.ParsedFile{Language: "go", Symbols: []graph.Symbol{
		{Language: "go", Kind: "function", Name: "TargetA", QualifiedName: "pkg.TargetA", StableKey: "func:pkg::TargetA", Range: graph.Position{StartLine: 1, EndLine: 3}},
		{Language: "go", Kind: "function", Name: "TargetB", QualifiedName: "pkg.TargetB", StableKey: "func:pkg::TargetB", Range: graph.Position{StartLine: 5, EndLine: 7}},
	}}
	tests := graph.ParsedFile{Language: "go", Symbols: collisionSymbols(),
		Edges: []graph.Edge{
			{DstName: "pkg.TargetA", Kind: "calls", Evidence: "A", Line: 2},
			{DstName: "pkg.TargetB", Kind: "calls", Evidence: "B", Line: 6},
		}, TestLinks: []graph.TestLink{
			{TestName: "A", TestSymbolKey: "same", TestSymbolIndex: testSymbol(0), TargetStableKey: "func:pkg::TargetA", Reason: "test_name_match"},
			{TestName: "B", TestSymbolKey: "same", TestSymbolIndex: testSymbol(1), TargetStableKey: "func:pkg::TargetB", Reason: "test_name_match"},
		}}
	_, err = s.ReplaceFileGraphsBatch(ctx, repo.ID, 1, []store.ReplaceFileGraphInput{
		{Path: "targets.go", Language: "go", ContentHash: "targets", Parsed: targets},
		{Path: "identity_test.go", Language: "go", ContentHash: "tests", Parsed: tests},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveTestLinks(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ target, want string }{{"pkg.TargetA", "pkg.TestA"}, {"pkg.TargetB", "pkg.TestB"}} {
		got, err := s.RelatedTests(ctx, repo.ID, tc.target, "", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Symbol != tc.want {
			t.Fatalf("RelatedTests(%q) = %+v, want only %s", tc.target, got, tc.want)
		}
	}
	// Replace the test file: TestA disappears and TestB is recreated at the
	// first parser position. The new row must not inherit TestA's identity.
	recreated := graph.ParsedFile{Language: "go", Symbols: []graph.Symbol{collisionSymbols()[1]}, TestLinks: []graph.TestLink{{
		TestName: "B", TestSymbolKey: "same", TestSymbolIndex: testSymbol(0),
		TargetStableKey: "func:pkg::TargetB", Reason: "test_name_match",
	}}}
	_, err = s.ReplaceFileGraphsBatch(ctx, repo.ID, 2, []store.ReplaceFileGraphInput{{
		Path: "identity_test.go", Language: "go", ContentHash: "recreated", Parsed: recreated,
	}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.TestLinksForTest(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TestSymbolName != "TestB" || rows[0].TestSymbolID == nil {
		t.Fatalf("recreated test link = %+v, want live TestB identity", rows)
	}
	fresh := persistCollisionFixture(t, []graph.Symbol{collisionSymbols()[1]}, recreated.TestLinks)
	if len(fresh) != 1 || fresh[0].TargetStableKey != rows[0].TargetStableKey || fresh[0].TestSymbolName != rows[0].TestSymbolName {
		t.Fatalf("incremental/fresh semantic identity diverged: recreated=%+v fresh=%+v", rows, fresh)
	}
}

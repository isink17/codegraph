package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// TestReplaceFileGraphsBatch_TempTablePathUnbindsTestLinks covers the
// >sqliteInClauseBatchSize branch of deleteFileGraphsBatch
// (deleteFileGraphsBatchFromTemp), which the chunked-IN regressions never
// reach. Driving the real indexer with 900+ files would be a pathological
// fixture, so this exercises the same store entry point
// (ReplaceFileGraphsBatch) with trivial synthetic parsed files.
func TestReplaceFileGraphsBatch_TempTablePathUnbindsTestLinks(t *testing.T) {
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
	scanID, _, err := s.BeginScan(ctx, repo.ID, "index")
	if err != nil {
		t.Fatalf("BeginScan() error = %v", err)
	}

	// The batch must exceed the IN-clause chunk size to take the temp-table path.
	const fileCount = sqliteInClauseBatchSize + 1
	withHelper := func(i int) ReplaceFileGraphInput {
		name := fmt.Sprintf("Helper%d", i)
		return ReplaceFileGraphInput{
			Path:     fmt.Sprintf("f%04d.go", i),
			Language: "go",
			Parsed: graph.ParsedFile{
				Language: "go",
				Symbols: []graph.Symbol{{
					Language:      "go",
					Kind:          "function",
					Name:          name,
					QualifiedName: "pkg." + name,
					StableKey:     "func:pkg::" + name,
					Range:         graph.Position{StartLine: 1, StartCol: 1, EndLine: 1, EndCol: 1},
				}},
			},
		}
	}

	first := make([]ReplaceFileGraphInput, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		first = append(first, withHelper(i))
	}
	fileIDs, err := s.ReplaceFileGraphsBatch(ctx, repo.ID, scanID, first)
	if err != nil {
		t.Fatalf("ReplaceFileGraphsBatch(initial) error = %v", err)
	}
	if len(fileIDs) != fileCount {
		t.Fatalf("file ids = %d, want %d", len(fileIDs), fileCount)
	}

	// A test file outside the re-indexed batch, linking to two of the symbols
	// that are about to disappear.
	testFile, err := insertTestFile(ctx, s, repo.ID, "outside_test.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}
	testSym, err := insertTestSymbol(ctx, s, repo.ID, testFile, "TestHelper0", "pkg.TestHelper0")
	if err != nil {
		t.Fatalf("insertTestSymbol() error = %v", err)
	}
	targetIDs := make([]int64, 0, 2)
	for _, i := range []int{0, fileCount - 1} {
		var symID int64
		if err := s.db.QueryRowContext(ctx,
			`SELECT id FROM symbols WHERE repo_id = ? AND stable_key = ?`,
			repo.ID, fmt.Sprintf("func:pkg::Helper%d", i)).Scan(&symID); err != nil {
			t.Fatalf("lookup Helper%d error = %v", i, err)
		}
		targetIDs = append(targetIDs, symID)
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score)
			VALUES(?, ?, ?, ?, ?, 'test_name_match', 0.8)
		`, repo.ID, testFile, testSym, fileIDs[i], symID); err != nil {
			t.Fatalf("insert test_link error = %v", err)
		}
	}

	// Re-index the whole batch with every Helper renamed away.
	second := make([]ReplaceFileGraphInput, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		in := withHelper(i)
		in.Parsed.Symbols[0].Name = fmt.Sprintf("Gone%d", i)
		in.Parsed.Symbols[0].QualifiedName = fmt.Sprintf("pkg.Gone%d", i)
		in.Parsed.Symbols[0].StableKey = fmt.Sprintf("func:pkg::Gone%d", i)
		second = append(second, in)
	}
	if _, err := s.ReplaceFileGraphsBatch(ctx, repo.ID, scanID, second); err != nil {
		t.Fatalf("ReplaceFileGraphsBatch(re-index) error = %v", err)
	}

	dangling, err := s.CountDanglingTestLinkRefsForTest(ctx, repo.ID)
	if err != nil {
		t.Fatalf("CountDanglingTestLinkRefsForTest() error = %v", err)
	}
	if dangling != 0 {
		t.Fatalf("dangling test_links.target_symbol_id after temp-table re-index = %d, want 0", dangling)
	}

	// Both rows survive with target_file_id intact and target_symbol_id cleared.
	links, err := s.TestLinksForTest(ctx, repo.ID)
	if err != nil {
		t.Fatalf("TestLinksForTest() error = %v", err)
	}
	if len(links) != len(targetIDs) {
		t.Fatalf("test_links = %d, want %d: %+v", len(links), len(targetIDs), links)
	}
	for _, l := range links {
		if l.TargetSymbolID != nil {
			t.Fatalf("target_symbol_id = %d, want NULL: %+v", *l.TargetSymbolID, l)
		}
		if l.TargetFile == "" {
			t.Fatalf("target_file_id cleared, want preserved: %+v", l)
		}
		if l.Reason != "test_name_match" || l.Score != 0.8 {
			t.Fatalf("reason/score changed: %+v", l)
		}
	}
}

package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestPurgeDeletes_TestLinksTargetingDeletedFile pins down the
// target_file_id ghost: when a target file is purged, the existing nullify
// pass only nulls test_links.target_symbol_id; without this fix the row
// survives with a stale target_file_id pointing at the soft-deleted file
// row, and RelatedTests(file=path) would surface it as a ghost association.
func TestPurgeDeletes_TestLinksTargetingDeletedFile(t *testing.T) {
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

	targetA, err := insertTestFile(ctx, s, repo.ID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile(a.go) error = %v", err)
	}
	targetSym, err := insertTestSymbol(ctx, s, repo.ID, targetA, "Helper", "pkg/a.Helper")
	if err != nil {
		t.Fatalf("insertTestSymbol(target) error = %v", err)
	}
	testFile, err := insertTestFile(ctx, s, repo.ID, "a_test.go")
	if err != nil {
		t.Fatalf("insertTestFile(a_test.go) error = %v", err)
	}
	testSym, err := insertTestSymbol(ctx, s, repo.ID, testFile, "TestA", "pkg/a.TestA")
	if err != nil {
		t.Fatalf("insertTestSymbol(test) error = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score)
		VALUES(?, ?, ?, ?, ?, 'name_match', 0.9)
	`, repo.ID, testFile, testSym, targetA, targetSym); err != nil {
		t.Fatalf("insert test_link error = %v", err)
	}

	// Mark the target file deleted, then purge. The test file is unrelated to
	// this scan and remains live (it has its own last_scan_id).
	if _, err := s.MarkFilesDeletedBatch(ctx, repo.ID, scanID, []string{"a.go"}); err != nil {
		t.Fatalf("MarkFilesDeletedBatch error = %v", err)
	}
	purged, err := s.PurgeDeletedFileGraphsForScan(ctx, repo.ID, scanID)
	if err != nil {
		t.Fatalf("PurgeDeletedFileGraphsForScan error = %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeDeletedFileGraphsForScan purged = %d, want 1", purged)
	}

	// The test_links row targeting a.go must be gone — not just nulled.
	var remaining int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM test_links WHERE repo_id = ? AND target_file_id = ?
	`, repo.ID, targetA).Scan(&remaining); err != nil {
		t.Fatalf("count test_links error = %v", err)
	}
	if remaining != 0 {
		t.Fatalf("test_links targeting deleted file remaining = %d, want 0", remaining)
	}

	// And RelatedTests(file=a.go) must surface zero ghosts. The path lookup
	// itself still resolves to the soft-deleted file row, which is exactly
	// the surface that previously leaked.
	got, err := s.RelatedTests(ctx, repo.ID, "", "a.go", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=a.go) error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("RelatedTests(file=a.go) after purge = %d entries, want 0: %+v", len(got), got)
	}
}

// TestPurgeNullifiesEdgeAndRefSymbolReferences covers the symmetric paths
// already wired up: edges.dst_symbol_id, references_tbl.symbol_id, and
// references_tbl.context_symbol_id must all be NULL (not dangling) after the
// target file's symbols are physically deleted.
func TestPurgeNullifiesEdgeAndRefSymbolReferences(t *testing.T) {
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

	defFile, err := insertTestFile(ctx, s, repo.ID, "def.go")
	if err != nil {
		t.Fatalf("insertTestFile(def.go) error = %v", err)
	}
	defSym, err := insertTestSymbol(ctx, s, repo.ID, defFile, "Helper", "pkg/def.Helper")
	if err != nil {
		t.Fatalf("insertTestSymbol(def) error = %v", err)
	}
	useFile, err := insertTestFile(ctx, s, repo.ID, "use.go")
	if err != nil {
		t.Fatalf("insertTestFile(use.go) error = %v", err)
	}
	useSym, err := insertTestSymbol(ctx, s, repo.ID, useFile, "Caller", "pkg/use.Caller")
	if err != nil {
		t.Fatalf("insertTestSymbol(use) error = %v", err)
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, file_id, line)
		VALUES(?, ?, ?, ?, 'call', ?, 1)
	`, repo.ID, useSym, defSym, "Helper", useFile); err != nil {
		t.Fatalf("insert edge error = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO references_tbl(repo_id, file_id, symbol_id, context_symbol_id, ref_kind, name, start_line, start_col, end_line, end_col)
		VALUES(?, ?, ?, ?, 'call', ?, 1, 1, 1, 1)
	`, repo.ID, useFile, defSym, defSym, "Helper"); err != nil {
		t.Fatalf("insert reference error = %v", err)
	}

	if _, err := s.MarkFilesDeletedBatch(ctx, repo.ID, scanID, []string{"def.go"}); err != nil {
		t.Fatalf("MarkFilesDeletedBatch error = %v", err)
	}
	if _, err := s.PurgeDeletedFileGraphsForScan(ctx, repo.ID, scanID); err != nil {
		t.Fatalf("PurgeDeletedFileGraphsForScan error = %v", err)
	}

	var dangling int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM edges WHERE repo_id = ? AND dst_symbol_id IS NOT NULL AND dst_symbol_id NOT IN (SELECT id FROM symbols)
	`, repo.ID).Scan(&dangling); err != nil {
		t.Fatalf("count dangling edges error = %v", err)
	}
	if dangling != 0 {
		t.Fatalf("dangling edges.dst_symbol_id = %d, want 0", dangling)
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM references_tbl WHERE repo_id = ? AND symbol_id IS NOT NULL AND symbol_id NOT IN (SELECT id FROM symbols)
	`, repo.ID).Scan(&dangling); err != nil {
		t.Fatalf("count dangling refs.symbol_id error = %v", err)
	}
	if dangling != 0 {
		t.Fatalf("dangling references_tbl.symbol_id = %d, want 0", dangling)
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM references_tbl WHERE repo_id = ? AND context_symbol_id IS NOT NULL AND context_symbol_id NOT IN (SELECT id FROM symbols)
	`, repo.ID).Scan(&dangling); err != nil {
		t.Fatalf("count dangling refs.context_symbol_id error = %v", err)
	}
	if dangling != 0 {
		t.Fatalf("dangling references_tbl.context_symbol_id = %d, want 0", dangling)
	}

	// Stats counters reflect physical row counts. After purge the dst-side
	// rows (the def file's symbol) are gone, but the use-file rows remain.
	stats, err := s.Stats(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.Files != 1 {
		t.Fatalf("Stats.Files post-purge = %d, want 1", stats.Files)
	}
	if stats.Symbols != 1 {
		t.Fatalf("Stats.Symbols post-purge = %d, want 1", stats.Symbols)
	}
	// The edge and reference rows survive (they belong to the live use file)
	// but are now unresolved (dst_symbol_id / symbol_id are NULL).
	if stats.Edges != 1 {
		t.Fatalf("Stats.Edges post-purge = %d, want 1", stats.Edges)
	}
	if stats.References != 1 {
		t.Fatalf("Stats.References post-purge = %d, want 1", stats.References)
	}
}

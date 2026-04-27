package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestResolveEdgesForNames_ExactName(t *testing.T) {
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

	fileA, err := insertTestFile(ctx, s, repo.ID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile(a.go) error = %v", err)
	}
	fileB, err := insertTestFile(ctx, s, repo.ID, "b.go")
	if err != nil {
		t.Fatalf("insertTestFile(b.go) error = %v", err)
	}

	dstID, err := insertTestSymbol(ctx, s, repo.ID, fileA, "Foo", "Foo")
	if err != nil {
		t.Fatalf("insertTestSymbol(dst) error = %v", err)
	}
	srcID, err := insertTestSymbol(ctx, s, repo.ID, fileB, "Bar", "Bar")
	if err != nil {
		t.Fatalf("insertTestSymbol(src) error = %v", err)
	}
	edgeID, err := insertTestEdge(ctx, s, repo.ID, fileB, srcID, "Foo")
	if err != nil {
		t.Fatalf("insertTestEdge() error = %v", err)
	}

	stats, err := s.ResolveEdgesForNamesWithStats(ctx, repo.ID, []string{"Foo"})
	if err != nil {
		t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
	}
	if stats.TargetsSelected != 1 {
		t.Fatalf("TargetsSelected = %d, want 1", stats.TargetsSelected)
	}
	if stats.ExactHits != 1 {
		t.Fatalf("ExactHits = %d, want 1", stats.ExactHits)
	}

	var gotDst sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edgeID).Scan(&gotDst); err != nil {
		t.Fatalf("QueryRow(dst_symbol_id) error = %v", err)
	}
	if !gotDst.Valid || gotDst.Int64 != dstID {
		t.Fatalf("dst_symbol_id = (%v,%d), want (%v,%d)", gotDst.Valid, gotDst.Int64, true, dstID)
	}
}

func TestResolveEdgesForNames_QualifiedSuffix(t *testing.T) {
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
	fileA, err := insertTestFile(ctx, s, repo.ID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile(a.go) error = %v", err)
	}
	fileB, err := insertTestFile(ctx, s, repo.ID, "b.go")
	if err != nil {
		t.Fatalf("insertTestFile(b.go) error = %v", err)
	}

	dstID, err := insertTestSymbol(ctx, s, repo.ID, fileA, "Foo", "pkg.Foo")
	if err != nil {
		t.Fatalf("insertTestSymbol(dst) error = %v", err)
	}
	srcID, err := insertTestSymbol(ctx, s, repo.ID, fileB, "Bar", "Bar")
	if err != nil {
		t.Fatalf("insertTestSymbol(src) error = %v", err)
	}
	edgeID, err := insertTestEdge(ctx, s, repo.ID, fileB, srcID, "pkg.Foo")
	if err != nil {
		t.Fatalf("insertTestEdge() error = %v", err)
	}

	stats, err := s.ResolveEdgesForNamesWithStats(ctx, repo.ID, []string{"Foo"})
	if err != nil {
		t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
	}
	if stats.TargetsSelected != 1 {
		t.Fatalf("TargetsSelected = %d, want 1", stats.TargetsSelected)
	}
	if stats.SuffixHits != 1 {
		t.Fatalf("SuffixHits = %d, want 1", stats.SuffixHits)
	}

	var gotDst sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edgeID).Scan(&gotDst); err != nil {
		t.Fatalf("QueryRow(dst_symbol_id) error = %v", err)
	}
	if !gotDst.Valid || gotDst.Int64 != dstID {
		t.Fatalf("dst_symbol_id = (%v,%d), want (%v,%d)", gotDst.Valid, gotDst.Int64, true, dstID)
	}
}

func insertTestFile(ctx context.Context, s *Store, repoID int64, path string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO files(repo_id, path, language, indexed_at) VALUES(?, ?, ?, '')`, repoID, path, "go")
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func insertTestSymbol(ctx context.Context, s *Store, repoID, fileID int64, name, qualified string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO symbols(
			repo_id, file_id, language, kind, name, qualified_name,
			start_line, start_col, end_line, end_col, stable_key, qualified_suffix
		)
		VALUES(?, ?, ?, ?, ?, ?, 1, 1, 1, 1, ?, ?)
	`, repoID, fileID, "go", "function", name, qualified, qualified, qualifiedSuffix(qualified))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func insertTestEdge(ctx context.Context, s *Store, repoID, fileID, srcSymbolID int64, dstName string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, NULL, ?, 'call', '', ?, 1)
	`, repoID, srcSymbolID, dstName, fileID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

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

package store

import (
	"context"
	"testing"
)

func TestBenchmarkTokensCountsCanonicalSemanticContextBytes(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	otherRepo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo(other) error = %v", err)
	}

	a, err := insertTestFile(ctx, s, repoID, "src/a.go")
	if err != nil {
		t.Fatal(err)
	}
	b, err := insertTestFile(ctx, s, repoID, "src/b.go")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := insertTestFile(ctx, s, repoID, "src/deleted.go")
	if err != nil {
		t.Fatal(err)
	}
	other, err := insertTestFile(ctx, s, otherRepo.ID, "src/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET size_bytes = CASE id WHEN ? THEN 100 WHEN ? THEN 200 WHEN ? THEN 400 WHEN ? THEN 800 END, is_deleted = CASE id WHEN ? THEN 1 ELSE 0 END WHERE id IN (?, ?, ?, ?)`,
		a, b, deleted, other, deleted, a, b, deleted, other); err != nil {
		t.Fatal(err)
	}

	for _, row := range []struct {
		repo int64
		file int64
		name string
	}{
		{repoID, a, "NeedleA"},
		{repoID, a, "NeedleADupe"},
		{repoID, b, "NeedleB"},
		{repoID, deleted, "NeedleDeleted"},
		{otherRepo.ID, other, "NeedleOther"},
	} {
		sym, err := insertTestSymbol(ctx, s, row.repo, row.file, row.name, "pkg."+row.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO symbol_tokens(symbol_id, token, weight) VALUES(?, 'needle', 1.0)`, sym); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.BenchmarkTokens(ctx, repoID, "needle")
	if err != nil {
		t.Fatalf("BenchmarkTokens() error = %v", err)
	}
	if got["context_files"] != int64(2) || got["context_bytes"] != int64(300) {
		t.Fatalf("context = (%v files, %v bytes), want (2 files, 300 bytes)", got["context_files"], got["context_bytes"])
	}
}

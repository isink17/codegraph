package store

import (
	"context"
	"path/filepath"
	"testing"
)

// resolveGoPackageScopedBareNames owns two temp tables. SQLite temp tables live
// on the connection, and the store hands connections back to a pool, so a table
// that survives a resolve is visible to whatever runs next -- and a stale one
// with an older shape fails the next INSERT instead of being repaired. The pass
// must therefore leave the connection clean, and repeating it must be a no-op
// rather than a second bind.
func TestGoBareScopeTempTablesDoNotLeak(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f1, _ := insertTestFile(ctx, s, repo.ID, "pkg/a.go")
	dst, _ := insertTestSymbol(ctx, s, repo.ID, f1, "Helper", "pkg.Helper")
	src, _ := insertTestSymbol(ctx, s, repo.ID, f1, "Caller", "pkg.Caller")
	if _, err := insertTestEdge(ctx, s, repo.ID, f1, src, "Helper"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM temp.sqlite_master WHERE name LIKE 'tmp_go_bare%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d tmp_go_bare* tables survived on a pooled connection", n)
	}
	var bound int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM edges WHERE dst_symbol_id = ?`, dst).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != 1 {
		t.Fatalf("bound = %d, want 1 (repeated resolves must be idempotent)", bound)
	}
}

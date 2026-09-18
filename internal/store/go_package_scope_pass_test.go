package store

import (
	"context"
	"database/sql"
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

// A '\' in files.path is filename data, not a separator (P23). `b/x\y.go` is a
// package sibling of `b/z.go`, and `b/x/y.go` is a different package in the
// nested directory even though it declares the same package name and the same
// bare name. The old storedPathDir treated '\' as a separator on every host, so
// the backslash file landed in a directory of its own and its bare call bound
// nothing; a separator fold the other way (`b/x/`) would bind the nested Helper.
func TestGoPackageScopeLiteralBackslashFilenameIsPackageSibling(t *testing.T) {
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
	must := func(id int64, err error) int64 {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	backslashFile := must(insertTestFile(ctx, s, repo.ID, `b/x\y.go`))
	siblingFile := must(insertTestFile(ctx, s, repo.ID, "b/z.go"))
	nestedFile := must(insertTestFile(ctx, s, repo.ID, "b/x/y.go"))

	siblingHelper := must(insertTestSymbol(ctx, s, repo.ID, siblingFile, "Helper", "b.Helper"))
	nestedHelper := must(insertTestSymbol(ctx, s, repo.ID, nestedFile, "Helper", "b.Helper"))
	backslashCaller := must(insertTestSymbol(ctx, s, repo.ID, backslashFile, "Caller", "b.Caller"))
	nestedCaller := must(insertTestSymbol(ctx, s, repo.ID, nestedFile, "NestedCaller", "b.NestedCaller"))
	backslashEdge := must(insertTestEdge(ctx, s, repo.ID, backslashFile, backslashCaller, "Helper"))
	nestedEdge := must(insertTestEdge(ctx, s, repo.ID, nestedFile, nestedCaller, "Helper"))

	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}

	boundTo := func(edgeID int64) (dst int64, strategy, srcPath string) {
		t.Helper()
		var nullable sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `
			SELECT e.dst_symbol_id, COALESCE(e.resolution_strategy, ''), f.path
			FROM edges e JOIN files f ON f.id = e.file_id WHERE e.id = ?`, edgeID,
		).Scan(&nullable, &strategy, &srcPath); err != nil {
			t.Fatal(err)
		}
		return nullable.Int64, strategy, srcPath
	}

	dst, strategy, srcPath := boundTo(backslashEdge)
	if srcPath != `b/x\y.go` {
		t.Fatalf("backslash edge file path = %q, want %q", srcPath, `b/x\y.go`)
	}
	if dst != siblingHelper {
		t.Fatalf("bare call from %q bound to symbol %d (strategy %q), want sibling b/z.go Helper %d (nested b/x/y.go Helper is %d)",
			srcPath, dst, strategy, siblingHelper, nestedHelper)
	}
	if strategy != ResolutionStrategyGoPackageScope {
		t.Fatalf("backslash edge strategy = %q, want %q", strategy, ResolutionStrategyGoPackageScope)
	}

	dst, _, srcPath = boundTo(nestedEdge)
	if srcPath != "b/x/y.go" {
		t.Fatalf("nested edge file path = %q, want %q", srcPath, "b/x/y.go")
	}
	if dst != nestedHelper {
		t.Fatalf("bare call from %q bound to symbol %d, want nested Helper %d (sibling b/z.go Helper is %d)",
			srcPath, dst, nestedHelper, siblingHelper)
	}
}

package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkStoreFindCallers_Hub(b *testing.B) {
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "graph.sqlite")
	s, err := Open(dbPath)
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	repo, err := s.UpsertRepo(ctx, b.TempDir())
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}
	fileID, err := insertTestFile(ctx, s, repo.ID, "a.go")
	if err != nil {
		b.Fatalf("insertTestFile() error = %v", err)
	}

	const n = 2000
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("Fn_%d", i)
		qualified := "pkg." + name
		id, err := insertTestSymbol(ctx, s, repo.ID, fileID, name, qualified)
		if err != nil {
			b.Fatalf("insertTestSymbol() error = %v", err)
		}
		ids = append(ids, id)
	}
	hubID := ids[0]
	hubQName := "pkg.Fn_0"

	// Many callers -> hub.
	for i := 1; i < n; i++ {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, ?, 'call', '', ?, 1)
		`, repo.ID, ids[i], hubID, hubQName, fileID); err != nil {
			b.Fatalf("insert edge callers->hub error = %v", err)
		}
	}

	// Hub -> many callees (so we can also bench FindCallees with same fixture).
	for i := 1; i < n; i++ {
		dstQName := fmt.Sprintf("pkg.Fn_%d", i)
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, ?, 'call', '', ?, 1)
		`, repo.ID, hubID, ids[i], dstQName, fileID); err != nil {
			b.Fatalf("insert edge hub->callee error = %v", err)
		}
	}

	b.Logf("symbols=%d edges=%d", n, 2*(n-1))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		callers, err := s.FindCallers(ctx, repo.ID, hubQName, 0, 20, 0)
		if err != nil {
			b.Fatalf("FindCallers() error = %v", err)
		}
		if len(callers) == 0 {
			b.Fatalf("expected non-empty callers")
		}
	}
}

func BenchmarkStoreFindCallees_Hub(b *testing.B) {
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "graph.sqlite")
	s, err := Open(dbPath)
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	repo, err := s.UpsertRepo(ctx, b.TempDir())
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}
	fileID, err := insertTestFile(ctx, s, repo.ID, "a.go")
	if err != nil {
		b.Fatalf("insertTestFile() error = %v", err)
	}

	const n = 2000
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("Fn_%d", i)
		qualified := "pkg." + name
		id, err := insertTestSymbol(ctx, s, repo.ID, fileID, name, qualified)
		if err != nil {
			b.Fatalf("insertTestSymbol() error = %v", err)
		}
		ids = append(ids, id)
	}
	hubID := ids[0]
	hubQName := "pkg.Fn_0"

	for i := 1; i < n; i++ {
		dstQName := fmt.Sprintf("pkg.Fn_%d", i)
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, ?, 'call', '', ?, 1)
		`, repo.ID, hubID, ids[i], dstQName, fileID); err != nil {
			b.Fatalf("insert edge hub->callee error = %v", err)
		}
	}

	b.Logf("symbols=%d edges=%d", n, n-1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		callees, err := s.FindCallees(ctx, repo.ID, hubQName, 0, 20, 0)
		if err != nil {
			b.Fatalf("FindCallees() error = %v", err)
		}
		if len(callees) == 0 {
			b.Fatalf("expected non-empty callees")
		}
	}
}

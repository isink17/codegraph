package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
)

func TestEdgeSourceColumnMigrationLeavesExistingEdgesUnknown(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		DROP INDEX IF EXISTS idx_edges_source;
		DELETE FROM schema_migrations WHERE version = 8;
		ALTER TABLE edges DROP COLUMN start_col`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(SQLiteDriverName(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO repos(id, root_path, canonical_path, created_at, updated_at) VALUES(991, '/legacy', '/legacy', '', '');
		INSERT INTO files(id, repo_id, path, language) VALUES(991, 991, 'old.lua', 'lua');
		INSERT INTO symbols(id, repo_id, file_id, language, kind, name, qualified_name, stable_key, start_line, start_col, end_line, end_col)
		VALUES(991, 991, 991, 'lua', 'function', 'caller', 'caller', 'caller', 1, 1, 3, 1);
		INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, file_id, line) VALUES(991, 991, 'target', 'calls', 991, 2)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var col sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT start_col FROM edges WHERE repo_id=991`).Scan(&col); err != nil {
		t.Fatal(err)
	}
	if col.Valid {
		t.Fatalf("legacy edge source column = %d, want unknown", col.Int64)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO references_tbl(repo_id, file_id, ref_kind, name, qualified_name, start_line, start_col, end_line, end_col)
		VALUES(991, 991, 'call', 'target', 'target', 2, 4, 2, 10)`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileReferenceIdentities(ctx, 991); err != nil {
		t.Fatal(err)
	}
	var target sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=991`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target.Valid {
		t.Fatalf("legacy edge attributed reference to target %d", target.Int64)
	}
}

func TestReferenceIdentityBindsResolvedEdgeAndActualContext(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	parsed, err := goparser.New().Parse(ctx, "main.go", []byte(`package main

func TargetA() {}
func TargetB() {}

func A() {
	TargetA()
}

func B() {
	Missing()
}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileGraph(ctx, repoID, 1, "main.go", "go", 1, 1, "main", parsed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveEdges(ctx, repoID); err != nil {
		t.Fatal(err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT r.name, r.symbol_id, r.context_symbol_id, e.dst_symbol_id, e.src_symbol_id
		FROM references_tbl r
		JOIN edges e ON e.repo_id = r.repo_id AND e.file_id = r.file_id AND e.line = r.start_line AND e.start_col = r.start_col
		WHERE r.repo_id = ? ORDER BY r.start_line`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var targetID, aID, bID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM symbols WHERE repo_id = ? AND qualified_name = 'main.TargetA'`, repoID).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM symbols WHERE repo_id = ? AND qualified_name = 'main.A'`, repoID).Scan(&aID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM symbols WHERE repo_id = ? AND qualified_name = 'main.B'`, repoID).Scan(&bID); err != nil {
		t.Fatal(err)
	}
	var name string
	var refTarget, refContext, edgeTarget, edgeSource *int64
	if !rows.Next() || rows.Scan(&name, &refTarget, &refContext, &edgeTarget, &edgeSource) != nil {
		t.Fatal("missing TargetA reference")
	}
	if name != "TargetA" || refTarget == nil || refContext == nil || edgeTarget == nil || edgeSource == nil || *refTarget != targetID || *refContext != aID || *edgeTarget != targetID || *edgeSource != aID {
		t.Fatalf("TargetA identity = %q ref=(%v,%v) edge=(%v,%v)", name, refTarget, refContext, edgeTarget, edgeSource)
	}
	if !rows.Next() || rows.Scan(&name, &refTarget, &refContext, &edgeTarget, &edgeSource) != nil {
		t.Fatal("missing Missing reference")
	}
	if name != "Missing" || refTarget != nil || refContext == nil || *refContext != bID || edgeTarget != nil || *edgeSource != bID {
		t.Fatalf("Missing identity = %q ref=(%v,%v) edge=(%v,%v)", name, refTarget, refContext, edgeTarget, edgeSource)
	}
}

func TestReferenceIdentityClearsCompetingTargetAndRejectsSyntheticEdge(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	if err := s.ReplaceFileGraph(ctx, repoID, 1, "caller.go", "go", 1, 1, "caller", graph.ParsedFile{
		Symbols:    []graph.Symbol{{Language: "go", Kind: "function", Name: "Caller", QualifiedName: "Caller", StableKey: "caller", Range: graph.Position{StartLine: 1, EndLine: 3}}},
		References: []graph.Reference{{Kind: "call", Name: "Target", QualifiedName: "Target", Range: graph.Position{StartLine: 2, StartCol: 4}}},
		Edges:      []graph.Edge{{Kind: "calls", DstName: "Target", Line: 2, Col: 4}},
	}); err != nil {
		t.Fatal(err)
	}
	var fileID, callerID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM files WHERE repo_id = ? AND path = 'caller.go'`, repoID).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM symbols WHERE repo_id = ? AND qualified_name = 'Caller'`, repoID).Scan(&callerID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, file_id, line, start_col)
		VALUES (?, ?, NULL, 'Target', 'calls', ?, 2, 4), (?, ?, NULL, 'Target', 'cross_language_ref', ?, 2, 4)`,
		repoID, callerID, fileID, repoID, callerID, fileID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileReferenceIdentities(ctx, repoID); err != nil {
		t.Fatal(err)
	}
	var target, source *int64
	if err := s.db.QueryRowContext(ctx, `SELECT symbol_id, context_symbol_id FROM references_tbl WHERE repo_id = ?`, repoID).Scan(&target, &source); err != nil {
		t.Fatal(err)
	}
	if target != nil || source == nil || *source != callerID {
		t.Fatalf("competing identity = (%v,%v), want (NULL,%d)", target, source, callerID)
	}
}

func TestReferenceIdentityIgnoresParserDatabaseIDs(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	bogusTarget, bogusContext := int64(111), int64(222)
	if err := s.ReplaceFileGraph(ctx, repoID, 1, "caller.go", "go", 1, 1, "caller", graph.ParsedFile{
		Language:   "go",
		Symbols:    []graph.Symbol{{Language: "go", Kind: "function", Name: "Caller", QualifiedName: "Caller", StableKey: "caller", Range: graph.Position{StartLine: 1, EndLine: 3}}},
		References: []graph.Reference{{Kind: "call", Name: "Missing", QualifiedName: "Missing", SymbolID: &bogusTarget, ContextSymbolID: &bogusContext, Range: graph.Position{StartLine: 2}}},
	}); err != nil {
		t.Fatal(err)
	}
	var target, contextID *int64
	if err := s.db.QueryRowContext(ctx, `SELECT symbol_id, context_symbol_id FROM references_tbl WHERE repo_id = ?`, repoID).Scan(&target, &contextID); err != nil {
		t.Fatal(err)
	}
	if target != nil || contextID != nil {
		t.Fatalf("parser IDs persisted: target=%v context=%v", target, contextID)
	}
}

func TestReferenceIdentityUsesExactCallOccurrence(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	parsed := graph.ParsedFile{
		Symbols: []graph.Symbol{
			{Language: "lua", Kind: "function", Name: "first", QualifiedName: "first", StableKey: "first", Range: graph.Position{StartLine: 1, EndLine: 4}},
			{Language: "lua", Kind: "function", Name: "second", QualifiedName: "second", StableKey: "second", Range: graph.Position{StartLine: 5, EndLine: 8}},
			{Language: "lua", Kind: "function", Name: "targetA", QualifiedName: "targetA", StableKey: "targetA", Range: graph.Position{StartLine: 10, EndLine: 11}},
			{Language: "lua", Kind: "function", Name: "targetB", QualifiedName: "targetB", StableKey: "targetB", Range: graph.Position{StartLine: 12, EndLine: 13}},
		},
		References: []graph.Reference{
			{Kind: "call", Name: "targetA", QualifiedName: "targetA", Range: graph.Position{StartLine: 2, StartCol: 4, EndLine: 2, EndCol: 13}},
			{Kind: "call", Name: "targetA", QualifiedName: "targetA", Range: graph.Position{StartLine: 2, StartCol: 20, EndLine: 2, EndCol: 29}},
			{Kind: "call", Name: "missing", QualifiedName: "missing", Range: graph.Position{StartLine: 2, StartCol: 36, EndLine: 2, EndCol: 43}},
		},
		Edges: []graph.Edge{
			{DstName: "targetA", Kind: "calls", Line: 2, Col: 4},
			{DstName: "targetA", Kind: "calls", Line: 2, Col: 20},
		},
	}
	if err := s.ReplaceFileGraph(ctx, repoID, 1, "main.lua", "lua", 1, 1, "lua", parsed); err != nil {
		t.Fatal(err)
	}
	var firstEdgeID, secondEdgeID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM symbols WHERE repo_id=? AND stable_key='targetA'`, repoID).Scan(&firstEdgeID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM symbols WHERE repo_id=? AND stable_key='targetB'`, repoID).Scan(&secondEdgeID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id=CASE start_col WHEN 4 THEN ? ELSE ? END WHERE repo_id=?`, firstEdgeID, secondEdgeID, repoID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileReferenceIdentities(ctx, repoID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.start_col,r.symbol_id FROM references_tbl r WHERE r.repo_id=? ORDER BY r.start_col`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for _, want := range []struct {
		col    int
		target int64
		bound  bool
	}{{4, firstEdgeID, true}, {20, secondEdgeID, true}, {36, 0, false}} {
		var col int
		var target *int64
		if !rows.Next() || rows.Scan(&col, &target) != nil || col != want.col || (want.bound && (target == nil || *target != want.target)) || (!want.bound && target != nil) {
			t.Fatalf("reference occurrence col=%d target=%v, want col=%d target=%d", col, target, want.col, want.target)
		}
	}
	if rows.Next() {
		t.Fatal("unexpected extra reference")
	}
}

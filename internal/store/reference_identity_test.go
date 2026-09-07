package store

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
)

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
		JOIN edges e ON e.repo_id = r.repo_id AND e.file_id = r.file_id AND e.line = r.start_line
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
		References: []graph.Reference{{Kind: "call", Name: "Target", QualifiedName: "Target", Range: graph.Position{StartLine: 2}}},
		Edges:      []graph.Edge{{Kind: "calls", DstName: "Target", Line: 2}},
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
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, file_id, line)
		VALUES (?, ?, NULL, 'Target', 'calls', ?, 2), (?, ?, NULL, 'Target', 'cross_language_ref', ?, 2)`,
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

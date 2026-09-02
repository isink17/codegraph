package store

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestReplaceFileGraphDropsUnattributableSourcesAndLeavesReferenceContextNull(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	stats := &WriteStats{}
	parsed := graph.ParsedFile{
		Symbols: []graph.Symbol{
			{Kind: "function", Name: "A", QualifiedName: "A", StableKey: "a", Range: graph.Position{StartLine: 10, EndLine: 20}},
			{Kind: "function", Name: "B", QualifiedName: "B", StableKey: "b", Range: graph.Position{StartLine: 30, EndLine: 40}},
		},
		Edges: []graph.Edge{
			{DstName: "inside", Kind: "calls", Line: 15},
			{DstName: "before", Kind: "calls", Line: 5},
			{DstName: "between", Kind: "calls", Line: 25},
			{DstName: "after", Kind: "calls", Line: 50},
		},
		References: []graph.Reference{{Kind: "read", Name: "A", Range: graph.Position{StartLine: 15}}},
	}
	if _, err := s.ReplaceFileGraphsBatchWithStats(ctx, repoID, 1, []ReplaceFileGraphInput{{
		Path: "a.go", Language: "go", Parsed: parsed,
	}}, stats); err != nil {
		t.Fatalf("ReplaceFileGraphsBatchWithStats: %v", err)
	}
	if stats.EdgeInsertRows != 1 || stats.EdgeSourceDroppedUnattributable != 3 || stats.EdgeSourceFallbackAttributed != 0 {
		t.Fatalf("source stats = %+v, want one exact, three dropped, no fallback", *stats)
	}
	var edges, contexts int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE repo_id = ?`, repoID).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM references_tbl WHERE repo_id = ? AND context_symbol_id IS NOT NULL`, repoID).Scan(&contexts); err != nil {
		t.Fatal(err)
	}
	if edges != 1 || contexts != 0 {
		t.Fatalf("persisted edges=%d contexts=%d, want 1 and 0", edges, contexts)
	}

	outside := parsed
	outside.Edges = []graph.Edge{{DstName: "moved", Kind: "calls", Line: 25}}
	updated := &WriteStats{}
	if _, err := s.ReplaceFileGraphsBatchWithStats(ctx, repoID, 2, []ReplaceFileGraphInput{{
		Path: "a.go", Language: "go", Parsed: outside,
	}}, updated); err != nil {
		t.Fatalf("ReplaceFileGraphsBatchWithStats(outside): %v", err)
	}
	if updated.EdgeSourceDroppedUnattributable != 1 {
		t.Fatalf("outside update stats = %+v, want one dropped source", *updated)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE repo_id = ?`, repoID).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if edges != 0 {
		t.Fatalf("edges after outside replacement = %d, want 0", edges)
	}

	restored := &WriteStats{}
	if _, err := s.ReplaceFileGraphsBatchWithStats(ctx, repoID, 3, []ReplaceFileGraphInput{{
		Path: "a.go", Language: "go", Parsed: parsed,
	}}, restored); err != nil {
		t.Fatalf("ReplaceFileGraphsBatchWithStats(inside): %v", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE repo_id = ?`, repoID).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if edges != 1 {
		t.Fatalf("edges after inside replacement = %d, want 1", edges)
	}
}

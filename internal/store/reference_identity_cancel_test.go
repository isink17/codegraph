package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/graph"
)

// A pass cancelled at any point leaves either every identity it would derive
// or none, and returns its connection to the pool outside a transaction.
func TestReferenceIdentityReconcileCancelledRollsBack(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	for i, name := range []string{"a.go", "b.go"} {
		parsed := graph.ParsedFile{
			Symbols:    []graph.Symbol{{Language: "go", Kind: "function", Name: "f", QualifiedName: "f" + name, StableKey: "f" + name, Range: graph.Position{StartLine: 1, EndLine: 3}}},
			References: []graph.Reference{{Kind: "call", Name: "f", Range: graph.Position{StartLine: 2, StartCol: 2, EndLine: 2, EndCol: 3}}},
			Edges:      []graph.Edge{{DstName: "f", Kind: "calls", Line: 2, Col: 2}},
		}
		if err := s.ReplaceFileGraph(ctx, repoID, int64(i+1), name, "go", 1, 1, "go", parsed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = src_symbol_id WHERE repo_id = ?`, repoID); err != nil {
		t.Fatal(err)
	}
	identities := func() string {
		var out string
		if err := s.db.QueryRowContext(ctx, `SELECT group_concat(id || ':' || COALESCE(symbol_id, '-') || ':' || COALESCE(context_symbol_id, '-'), ',') FROM (SELECT * FROM references_tbl WHERE repo_id = ? ORDER BY id)`, repoID).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	clearIdentities := func() {
		if _, err := s.db.ExecContext(ctx, `UPDATE references_tbl SET symbol_id = NULL, context_symbol_id = NULL WHERE repo_id = ?`, repoID); err != nil {
			t.Fatal(err)
		}
	}
	clearIdentities()
	before := identities()
	if err := s.ReconcileReferenceIdentities(ctx, repoID); err != nil {
		t.Fatal(err)
	}
	after := identities()
	if before == after {
		t.Fatalf("fixture derives nothing: %s", after)
	}
	cancelled := 0
	for i := range 200 {
		clearIdentities()
		runCtx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(time.Duration(i%40)*25*time.Microsecond, cancel)
		err := s.ReconcileReferenceIdentities(runCtx, repoID)
		timer.Stop()
		cancel()
		if err != nil {
			cancelled++
		}
		if got := identities(); got != before && got != after {
			t.Fatalf("run %d (err %v) left %s, want %s or %s", i, err, got, before, after)
		}
		if err != nil && identities() != before {
			t.Fatalf("run %d failed with %v but kept its writes", i, err)
		}
		// A connection returned inside a transaction would fail this write.
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE references_tbl SET start_col = start_col WHERE repo_id = %d`, repoID)); err != nil {
			t.Fatalf("run %d: pool unusable after cancellation: %v", i, err)
		}
	}
	if cancelled == 0 {
		t.Fatal("no run was cancelled")
	}
	if err := s.ReconcileReferenceIdentities(ctx, repoID); err != nil || identities() != after {
		t.Fatalf("pass after cancellations: %v, %s", err, identities())
	}
}

package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestGoReceiverScopeQueryShapeIsBounded pins that the pass loads evidence in
// batches rather than per edge. Doubling the number of receiver calls must not
// double the number of statements: a per-edge query here would be invisible on
// a fixture and quadratic on a real repository.
func TestGoReceiverScopeQueryShapeIsBounded(t *testing.T) {
	ctx := context.Background()

	statementsFor := func(edges int) int64 {
		s := newGoReceiverTestStore(t)
		repoID := seedGoReceiverScale(t, s, edges)
		counter := &countingQuerier{inner: s.db}
		if _, err := s.resolveGoReceiverScope(ctx, counter, repoID, nil); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return counter.queries.Load() + counter.execs.Load()
	}

	small := statementsFor(50)
	large := statementsFor(400)

	if small == 0 {
		t.Fatal("no statements issued; the fixture resolved nothing and the test is vacuous")
	}
	// Eight times the edges must not cost anything like eight times the
	// statements. The pass has a fixed set of queries plus one insert per 400
	// resolution rows, so the growth here is at most one statement.
	if large > small+2 {
		t.Errorf("statements grew with edge count: %d edges -> %d statements, %d edges -> %d statements",
			50, small, 400, large)
	}
}

// seedGoReceiverScale writes n callers, each with one receiver-scoped call on a
// proven type, all resolvable.
func seedGoReceiverScale(t *testing.T, s *Store, n int) int64 {
	t.Helper()
	ctx := context.Background()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("upsert repo: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO files(id, repo_id, path, language, is_deleted, parse_state)
	      VALUES(1, ?, 'pkg/main.go', 'go', 0, 'indexed')`, repo.ID)

	var b strings.Builder
	b.WriteString(`INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name, start_line, start_col, end_line, end_col, stable_key) VALUES `)
	args := []any{}
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`(?, 1, 'go', 'method', ?, ?, 'Thing', 1, 1, 1, 1, ?)`)
		method := fmt.Sprintf("M%d", i)
		args = append(args, repo.ID, method, "pkg.Thing."+method, "func:pkg:Thing:"+method)
	}
	exec(b.String(), args...)

	// One caller symbol owning every call line.
	exec(`INSERT INTO symbols(id, repo_id, file_id, language, kind, name, qualified_name, container_name, start_line, start_col, end_line, end_col, stable_key)
	      VALUES(100000, ?, 1, 'go', 'function', 'caller', 'pkg.caller', 'pkg', 1, 1, 100000, 1, 'func:pkg::caller')`, repo.ID)

	b.Reset()
	b.WriteString(`INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line) VALUES `)
	args = args[:0]
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`(?, 100000, NULL, ?, 'calls', '', 1, ?)`)
		args = append(args, repo.ID, fmt.Sprintf("recv.M%d", i), i+1)
	}
	exec(b.String(), args...)

	exec(`INSERT INTO go_local_binding_evidence(repo_id, file_id, name, scope_start_line, scope_end_line, type_name, type_package, type_import_path, is_pointer)
	      VALUES(?, 1, 'recv', 1, 100000, 'Thing', '', '', 1)`, repo.ID)
	return repo.ID
}

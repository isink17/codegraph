package store

import (
	"context"
	"testing"
)

func TestGoSelectorQualifierShape(t *testing.T) {
	cases := []struct {
		in         string
		qual, meth string
		ok         bool
		why        string
	}{
		{in: "store.Get", qual: "store", meth: "Get", ok: true, why: "the shape a local receiver call produces"},
		{in: "Helper", ok: false, why: "a bare call belongs to go_package_scope"},
		{in: "example.com/project/store.Get", ok: false, why: "an import path belongs to module_import"},
		{in: "s.db.QueryContext", ok: false, why: "a field chain names something this evidence cannot own"},
		{in: "pkg::Thing::Get", ok: false, why: "a foreign identity spelling"},
		{in: ".Get", ok: false, why: "no qualifier"},
		{in: "store.", ok: false, why: "no method"},
		{in: "", ok: false, why: "empty"},
	}
	for _, tc := range cases {
		qual, meth, ok := goSelectorQualifier(tc.in)
		if ok != tc.ok || qual != tc.qual || meth != tc.meth {
			t.Errorf("goSelectorQualifier(%q) = (%q, %q, %v), want (%q, %q, %v) -- %s",
				tc.in, qual, meth, ok, tc.qual, tc.meth, tc.ok, tc.why)
		}
	}
}

// TestGoLocalQualifierVetoSQLMatchesGoTwin pins resolverGoLocalQualifierSQL to
// goLocalQualifierClaims. The transactional resolver withholds edges from the
// generic strategies with the SQL predicate; the binder entrypoints do it with
// the Go function. Two definitions of "claimed" that can drift are exactly the
// failure mode this test exists to catch.
func TestGoLocalQualifierVetoSQLMatchesGoTwin(t *testing.T) {
	ctx := context.Background()
	s := newGoReceiverTestStore(t)
	repoID := seedGoReceiverFixture(t, s)

	claims, err := goLocalQualifierClaims(ctx, s.db, repoID)
	if err != nil {
		t.Fatalf("go twin: %v", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT edges.id
		FROM edges
		JOIN files f ON f.id = edges.file_id AND f.repo_id = edges.repo_id
		WHERE edges.repo_id = ? AND edges.dst_symbol_id IS NULL
		  AND NOT (`+resolverGoLocalQualifierSQL+`)
	`, repoID)
	if err != nil {
		t.Fatalf("sql twin: %v", err)
	}
	defer rows.Close()
	sqlClaims := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		sqlClaims[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(sqlClaims) == 0 {
		t.Fatal("fixture produced no claims; the test would pass vacuously")
	}
	for id := range sqlClaims {
		if _, ok := claims[id]; !ok {
			t.Errorf("edge %d claimed by the SQL predicate but not by the Go twin", id)
		}
	}
	for id := range claims {
		if _, ok := sqlClaims[id]; !ok {
			t.Errorf("edge %d claimed by the Go twin but not by the SQL predicate", id)
		}
	}
}

func newGoReceiverTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/codegraph.sqlite")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedGoReceiverFixture writes a Go file with three call edges: one on a local
// qualifier, one on an unshadowed import spelling, and one bare. Only the first
// may be claimed.
func seedGoReceiverFixture(t *testing.T, s *Store) int64 {
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
	      VALUES(1, ?, 'main.go', 'go', 0, 'indexed')`, repo.ID)
	exec(`INSERT INTO symbols(id, repo_id, file_id, language, kind, name, qualified_name, container_name, start_line, start_col, end_line, end_col, stable_key)
	      VALUES(1, ?, 1, 'go', 'function', 'shadow', 'main.shadow', 'main', 5, 1, 8, 1, 'func:main::shadow')`, repo.ID)
	insertEdge := func(id int64, dstName string, line int) {
		exec(`INSERT INTO edges(id, repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		      VALUES(?, ?, 1, NULL, ?, 'calls', '', 1, ?)`, id, repo.ID, dstName, line)
	}
	insertEdge(1, "store.Get", 6)                     // local qualifier: claimed
	insertEdge(2, "example.com/project/store.Get", 6) // import path: not claimed
	insertEdge(3, "Helper", 6)                        // bare: not claimed
	insertEdge(4, "store.Get", 99)                    // outside the binding's scope: not claimed
	// A field chain on a local. go_receiver_scope declines to bind it, but the
	// qualifier is every bit as local, so it must still be withheld from the
	// generic strategies. Deriving the veto from what the pass can bind would
	// silently let this one through.
	insertEdge(5, "store.inner.Get", 6)
	exec(`INSERT INTO go_local_binding_evidence(repo_id, file_id, name, scope_start_line, scope_end_line, type_name, type_package, type_import_path, is_pointer)
	      VALUES(?, 1, 'store', 5, 8, 'Thing', '', '', 1)`, repo.ID)
	return repo.ID
}

// TestGoLocalQualifierVetoScopeIsLineBounded pins that a binding claims only the
// lines its own scope spans: one function's local must not reach another's.
func TestGoLocalQualifierVetoScopeIsLineBounded(t *testing.T) {
	ctx := context.Background()
	s := newGoReceiverTestStore(t)
	repoID := seedGoReceiverFixture(t, s)

	claims, err := goLocalQualifierClaims(ctx, s.db, repoID)
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	for _, id := range []int64{1, 5} {
		if _, ok := claims[id]; !ok {
			t.Errorf("edge %d has a locally bound qualifier and must be claimed", id)
		}
	}
	for _, id := range []int64{2, 3, 4} {
		if _, ok := claims[id]; ok {
			t.Errorf("edge %d must not be claimed", id)
		}
	}
}

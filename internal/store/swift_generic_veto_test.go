package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestSwiftV3GenericVetoFullAndIncremental(t *testing.T) {
	build := func(t *testing.T, f *gateFixture) []int64 {
		t.Helper()
		defs := f.file(t, "A.swift", "swift")
		f.method(t, defs, "helper", "A.helper", "A", "swift")
		f.method(t, defs, "run", "Service.run", "Service", "swift")
		f.method(t, defs, "run", "Other.run", "Other", "swift")
		callerFile := f.file(t, "Caller.swift", "swift")
		caller := f.symbol(t, callerFile, "caller", "caller", "swift")
		calls := []struct{ name, evidence string }{
			{"helper", "swift:bare"},
			{"Service.run", "swift:member;type_path_unproven"},
			{"service.run", "swift:member;type_path_unproven"},
			{"foo.bar.run", "swift:chained"},
			{"value?.run", "swift:optional_member"},
			{"super.run", "swift:super"},
			{"Self.run", "swift:Self;trailing_labels=_"},
		}
		ids := make([]int64, 0, len(calls))
		for _, call := range calls {
			id, err := insertSwiftVetoEdge(f.ctx, f.store, f.repoID, callerFile, caller, call.name, call.evidence)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	for _, mode := range []string{"full", "incremental"} {
		t.Run(mode, func(t *testing.T) {
			f := newGateFixture(t)
			ids := build(t, f)
			var err error
			if mode == "full" {
				_, err = f.store.ResolveEdges(f.ctx, f.repoID)
			} else {
				_, err = f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"Caller.swift"}, []string{"helper", "Service.run", "service.run", "foo.bar.run", "value?.run", "super.run", "Self.run"})
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				var destination sql.NullInt64
				if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id FROM edges WHERE id=?`, id).Scan(&destination); err != nil {
					t.Fatal(err)
				}
				if destination.Valid {
					t.Fatalf("Swift edge %d unexpectedly resolved: %d", id, destination.Int64)
				}
			}
			var bound int
			if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM references_tbl WHERE repo_id=? AND symbol_id IS NOT NULL`, f.repoID).Scan(&bound); err != nil {
				t.Fatal(err)
			}
			if bound != 0 {
				t.Fatalf("Swift references bound=%d, want 0", bound)
			}
		})
	}
}

func insertSwiftVetoEdge(ctx context.Context, s *Store, repoID, fileID, srcID int64, name, evidence string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line) VALUES(?, ?, NULL, ?, 'calls', ?, ?, 1)`, repoID, srcID, name, evidence, fileID)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO references_tbl(repo_id, file_id, ref_kind, name, qualified_name, start_line, start_col, end_line, end_col) VALUES(?, ?, 'call', ?, ?, 1, 1, 1, 1)`, repoID, fileID, name, name); err != nil {
		return 0, err
	}
	return id, nil
}

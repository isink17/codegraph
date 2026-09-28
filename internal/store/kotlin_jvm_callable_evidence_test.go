package store

import (
	"testing"
)

func TestMigrationKotlinJVMCallableEvidenceFrom042(t *testing.T) {
	ctx := t.Context()
	path := t.TempDir() + "/graph.sqlite"
	if got := applyMigrationsBelow(t, ctx, path, 43); len(got) == 0 || got[len(got)-1] != 42 {
		t.Fatalf("pre-upgrade migrations end at %v, want 42", got)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, column := range []string{"repo_id", "file_id", "symbol_id", "is_known", "jvm_arity_min", "jvm_arity_max"} {
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('kotlin_jvm_callable_evidence') WHERE name=?`, column).Scan(&count); err != nil || count != 1 {
			t.Fatalf("column %s count=%d, err=%v", column, count, err)
		}
	}
	for _, index := range []string{"idx_kotlin_jvm_callable_repo_file"} {
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&count); err != nil || count != 1 {
			t.Fatalf("index %s count=%d, err=%v", index, count, err)
		}
	}
	var primaryIndex int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_index_list('kotlin_jvm_callable_evidence') WHERE origin='pk'`).Scan(&primaryIndex); err != nil || primaryIndex != 1 {
		t.Fatalf("primary-key index count=%d, err=%v", primaryIndex, err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 43 {
		t.Fatalf("migration ceiling=%d, err=%v", version, err)
	}
}

func TestKotlinJVMCallableEvidenceConstraints(t *testing.T) {
	f := newGateFixture(t)
	file := f.file(t, "lib/Actions.kt", "kotlin")
	symbol := func(name string) int64 {
		t.Helper()
		id := f.symbolKind(t, file, name, "lib."+name, "function", "kotlin")
		if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO kotlin_jvm_callable_evidence(repo_id,file_id,symbol_id,is_known,jvm_arity_min,jvm_arity_max) VALUES(?,?,?,1,0,1)`, f.repoID, file, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := symbol("first")
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO kotlin_jvm_callable_evidence(repo_id,file_id,symbol_id,is_known,jvm_arity_min,jvm_arity_max) VALUES(?,?,?,1,0,1)`, f.repoID, file, first); err == nil {
		t.Fatal("duplicate symbol fact accepted")
	}
	invalid := []struct {
		name     string
		known    int
		min, max any
	}{
		{"known null", 1, nil, nil},
		{"reversed", 1, 2, 1},
		{"negative", 1, -1, 1},
		{"unknown range", 0, 0, 1},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			id := f.symbolKind(t, file, "bad_"+tc.name, "lib.bad_"+tc.name, "function", "kotlin")
			if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO kotlin_jvm_callable_evidence(repo_id,file_id,symbol_id,is_known,jvm_arity_min,jvm_arity_max) VALUES(?,?,?,?,?,?)`, f.repoID, file, id, tc.known, tc.min, tc.max); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	unknown := f.symbolKind(t, file, "unknown", "lib.unknown", "function", "kotlin")
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO kotlin_jvm_callable_evidence(repo_id,file_id,symbol_id,is_known,jvm_arity_min,jvm_arity_max) VALUES(?,?,?,0,NULL,NULL)`, f.repoID, file, unknown); err != nil {
		t.Fatalf("valid unknown fact rejected: %v", err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE id=?`, first); err == nil {
		t.Fatal("symbol deletion left evidence row")
	}
}

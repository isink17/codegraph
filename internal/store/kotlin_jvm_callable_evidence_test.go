package store

import (
	"testing"
)

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

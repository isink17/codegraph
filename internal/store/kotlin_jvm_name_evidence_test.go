package store

import (
	"database/sql"
	"testing"
)

func TestMigrationKotlinJVMNameEvidenceFrom043(t *testing.T) {
	ctx := t.Context()
	path := t.TempDir() + "/graph.sqlite"
	if got := applyMigrationsBelow(t, ctx, path, 44); len(got) == 0 || got[len(got)-1] != 43 {
		t.Fatalf("pre-upgrade migrations end at %v, want 43", got)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, column := range []string{"repo_id", "file_id", "symbol_id", "is_known", "jvm_name"} {
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('kotlin_jvm_name_evidence') WHERE name=?`, column).Scan(&count); err != nil || count != 1 {
			t.Fatalf("column %s count=%d, err=%v", column, count, err)
		}
	}
	var index int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_kotlin_jvm_name_repo_file'`).Scan(&index); err != nil || index != 1 {
		t.Fatalf("file index count=%d, err=%v", index, err)
	}
	var primaryIndex int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_index_list('kotlin_jvm_name_evidence') WHERE origin='pk'`).Scan(&primaryIndex); err != nil || primaryIndex != 1 {
		t.Fatalf("primary-key index count=%d, err=%v", primaryIndex, err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kotlin_jvm_name_evidence`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("upgrade backfilled %d rows, err=%v; Kotlin reparse supplies facts", rows, err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 44 {
		t.Fatalf("migration ceiling=%d, err=%v", version, err)
	}
}

func TestKotlinJVMNameEvidenceConstraints(t *testing.T) {
	f := newGateFixture(t)
	file := f.file(t, "lib/Actions.kt", "kotlin")
	insert := func(id int64, known int, name any) error {
		_, err := f.store.db.ExecContext(f.ctx, `INSERT INTO kotlin_jvm_name_evidence(repo_id,file_id,symbol_id,is_known,jvm_name) VALUES(?,?,?,?,?)`, f.repoID, file, id, known, name)
		return err
	}
	first := f.symbolKind(t, file, "first", "lib.first", "function", "kotlin")
	if err := insert(first, 1, "execute"); err != nil {
		t.Fatalf("valid known fact rejected: %v", err)
	}
	if err := insert(first, 0, nil); err == nil {
		t.Fatal("second fact for one symbol accepted")
	}
	for _, tc := range []struct {
		name  string
		known int
		value any
	}{
		{"known null", 1, nil},
		{"known empty", 1, ""},
		{"unknown named", 0, "execute"},
		{"bad known flag", 2, "execute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := f.symbolKind(t, file, "bad_"+tc.name, "lib.bad_"+tc.name, "function", "kotlin")
			if err := insert(id, tc.known, tc.value); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	unknown := f.symbolKind(t, file, "unknown", "lib.unknown", "function", "kotlin")
	if err := insert(unknown, 0, nil); err != nil {
		t.Fatalf("valid unknown fact rejected: %v", err)
	}
	if err := insert(999999, 1, "orphan"); err == nil {
		t.Fatal("fact for a missing symbol accepted")
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE id=?`, first); err == nil {
		t.Fatal("symbol deletion left evidence row")
	}
}

// A JVM rename is exact about the bytecode name; whether Java source can spell
// that name is separate. Expectations are javac 17 results.
func TestJavaSourceMethodName(t *testing.T) {
	for name, want := range map[string]bool{
		"execute": true, "_execute": true, "execute1": true, "Execute": true,
		"var": true, "record": true, "yield": true,
		"class": false, "for": false, "_": false, "true": false, "false": false, "null": false,
		"goto": false, "const": false, "strictfp": false, "int": false, "this": false,
		"foo-bar": false, "1abc": false, "": false, "éxecute": false,
	} {
		if got := javaSourceMethodName(name); got != want {
			t.Errorf("javaSourceMethodName(%q) = %v, want %v", name, got, want)
		}
	}
}

// A known rename whose name Java cannot spell still retires the source name;
// an unknown rename never falls back to it.
func TestKotlinJVMNameStatus(t *testing.T) {
	known := func(name string) javaScopeSymbol {
		return javaScopeSymbol{name: "run", signature: `@JvmName("` + name + `") fun run(x: kotlin.Int) {}`, jvmNameEvidence: sql.NullInt64{Int64: 1, Valid: true}, jvmNameKnown: sql.NullInt64{Int64: 1, Valid: true}, jvmName: sql.NullString{String: name, Valid: true}}
	}
	unknown := javaScopeSymbol{name: "run", signature: `@JvmName(N) fun run(x: kotlin.Int) {}`, jvmNameEvidence: sql.NullInt64{Int64: 1, Valid: true}, jvmNameKnown: sql.NullInt64{Valid: true}}
	plain := javaScopeSymbol{name: "run", signature: `fun run(x: kotlin.Int) {}`}
	legacy := javaScopeSymbol{name: "run", signature: `@JvmName("execute") fun run(x: kotlin.Int) {}`}
	for _, tc := range []struct {
		name     string
		s        javaScopeSymbol
		javaName string
		want     int
	}{
		{"known match", known("execute"), "execute", 1},
		{"known retires source name", known("execute"), "run", 0},
		{"known reserved word", known("class"), "class", 0},
		{"known reserved retires source", known("class"), "run", 0},
		{"unknown vetoes source name", unknown, "run", -1},
		{"unknown vetoes other names", unknown, "execute", -1},
		{"no evidence source name", plain, "run", 1},
		{"no evidence other name", plain, "execute", 0},
		{"legacy signature never selects", legacy, "execute", -1},
		{"legacy signature retires source", legacy, "run", 0},
	} {
		if got := kotlinJVMNameStatus(tc.s, tc.javaName); got != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Older-parser rename syntax, including an aliased JvmName, must leave the
// historical zero-argument path so it can only refuse.
func TestKotlinZeroArgLegacyRenameUsesStatusPath(t *testing.T) {
	plain := javaScopeSymbol{name: "run", signature: `fun run() {}`}
	aliased := javaScopeSymbol{name: "run", signature: `@JN("execute") fun run() {}`, jvmNameAliases: "JN"}
	if kotlinNeedsJVMEvidenceClassification([]javaScopeSymbol{plain}) {
		t.Fatal("plain candidate left the historical path")
	}
	if !kotlinNeedsJVMEvidenceClassification([]javaScopeSymbol{plain, aliased}) {
		t.Fatal("aliased legacy rename stayed on the historical path")
	}
	if got := kotlinJVMNameStatus(aliased, "execute"); got != -1 {
		t.Fatalf("legacy aliased rename status = %d, want veto", got)
	}
}

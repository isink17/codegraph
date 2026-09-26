package store

import (
	"database/sql"
	"testing"
)

// facadeFixture is one Java caller in package app and Kotlin files in lib.
type facadeFixture struct {
	*gateFixture
	callerFile, caller int64
}

func newFacadeFixture(t *testing.T, imports ...string) *facadeFixture {
	t.Helper()
	f := &facadeFixture{gateFixture: newGateFixture(t)}
	f.callerFile = f.file(t, "app/Caller.java", "java")
	f.caller = f.symbolKind(t, f.callerFile, "call", "app.Caller.call", "function", "java")
	f.scope(t, f.callerFile, "java", "app", "", false, false)
	for _, source := range imports {
		local, wildcard := source[len("lib."):], 0
		if local == "*" {
			source, local, wildcard = "lib", "", 1
		}
		if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard) VALUES(?,?,?,?,?,?,?,?)`, f.repoID, f.callerFile, "java", source, local, local, "named", wildcard); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *facadeFixture) scope(t *testing.T, file int64, language, pkg, class string, explicit, multifile bool) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name,jvm_facade_class,jvm_facade_explicit,jvm_multifile) VALUES(?,?,?,?,?,?,?)`, f.repoID, file, language, pkg, class, boolInt(explicit), boolInt(multifile)); err != nil {
		t.Fatal(err)
	}
}

// kotlinFile adds a Kotlin file in lib whose persisted facade is class.
func (f *facadeFixture) kotlinFile(t *testing.T, path, class string, multifile bool) int64 {
	t.Helper()
	file := f.file(t, path, "kotlin")
	f.scope(t, file, "kotlin", "lib", class, class != "" && class[len(class)-2:] != "Kt", multifile)
	return file
}

func (f *facadeFixture) topLevel(t *testing.T, file int64, name, signature, visibility string) int64 {
	t.Helper()
	id := f.symbolKind(t, file, name, "lib."+name, "function", "kotlin")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='lib',visibility=?,signature=? WHERE id=?`, visibility, signature, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *facadeFixture) resolve(t *testing.T, dstName, evidence string) string {
	t.Helper()
	edge := f.edge(t, f.callerFile, f.caller, dstName)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET evidence=? WHERE id=?`, evidence, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJavaScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	dst, ok := f.dstSymbolID(t, edge)
	if !ok {
		return "<unresolved>"
	}
	var path, strategy string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT f.path,e.resolution_strategy FROM edges e JOIN symbols s ON s.id=e.dst_symbol_id JOIN files f ON f.id=s.file_id WHERE e.id=?`, edge).Scan(&path, &strategy); err != nil {
		t.Fatal(err)
	}
	return f.qualifiedNameOf(t, dst) + "@" + path + "|" + strategy
}

func TestJVMKotlinFileFacadeScopes(t *testing.T) {
	const unresolved = "<unresolved>"
	for _, tc := range []struct {
		name, dstName, evidence string
		imports                 []string
		build                   func(t *testing.T, f *facadeFixture)
		want                    string
	}{
		{"default facade via import", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "fun run() {}", "public")
		}, "lib.run@lib/Actions.kt|java_import_scope"},
		{"default facade fully qualified", "lib.ActionsKt.run", "lib.ActionsKt.run()", nil, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "fun run() {}", "public")
		}, "lib.run@lib/Actions.kt|java_package_scope"},
		{"default facade via wildcard import", "ActionsKt.run", "ActionsKt.run()", []string{"lib.*"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "fun run() {}", "public")
		}, "lib.run@lib/Actions.kt|java_package_scope"},
		{"explicit JvmName facade", "Actions.run", "Actions.run()", []string{"lib.Actions"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "Actions", false), "run", "fun run() {}", "public")
		}, "lib.run@lib/Actions.kt|java_import_scope"},
		{"no import and other package", "ActionsKt.run", "ActionsKt.run()", nil, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "fun run() {}", "public")
		}, unresolved},
		{"facade basename alone is not evidence", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "", false), "run", "fun run() {}", "public")
		}, unresolved},
		{"wrong facade spelling", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/FileName.kt", "FileNameKt", false), "run", "fun run() {}", "public")
		}, unresolved},
		{"renamed facade retires default spelling", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "Actions", false), "run", "fun run() {}", "public")
		}, unresolved},
		{"same-package function in another file", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false)
			f.topLevel(t, f.kotlinFile(t, "lib/Other.kt", "OtherKt", false), "run", "fun run() {}", "public")
		}, unresolved},
		{"Kotlin class of facade spelling", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "fun run() {}", "public")
			other := f.kotlinFile(t, "lib/Other.kt", "", false)
			f.symbolKind(t, other, "ActionsKt", "lib.ActionsKt", "class", "kotlin")
		}, unresolved},
		{"Java type of facade spelling", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "fun run() {}", "public")
			java := f.file(t, "lib/ActionsKt.java", "java")
			f.scope(t, java, "java", "lib", "", false, false)
			f.symbolKind(t, java, "ActionsKt", "lib.ActionsKt", "type", "java")
		}, unresolved},
		{"private top-level", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "private fun run() {}", "private")
		}, unresolved},
		{"internal top-level", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "internal fun run() {}", "internal")
		}, unresolved},
		{"extension", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "fun String.run() {}", "public")
		}, unresolved},
		{"declaration JvmName", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", `@JvmName("go") fun run() {}`, "public")
		}, unresolved},
		{"JvmSynthetic", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "@JvmSynthetic fun run() {}", "public")
		}, unresolved},
		{"suspend", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "suspend fun run() {}", "public")
		}, unresolved},
		{"reified", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "inline fun <reified T> run() {}", "public")
		}, unresolved},
		{"overload", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			file := f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false)
			f.topLevel(t, file, "run", "fun run() {}", "public")
			f.topLevel(t, file, "run", "fun run(value: Int) {}", "public")
		}, unresolved},
		{"argument-bearing call", "ActionsKt.run", "ActionsKt.run(1)", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "fun run(value: Int) {}", "public")
		}, unresolved},
		{"class member is not a facade member", "ActionsKt.run", "ActionsKt.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			file := f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false)
			f.symbolKind(t, file, "Box", "lib.Box", "class", "kotlin")
			member := f.symbolKind(t, file, "run", "lib.Box.run", "function", "kotlin")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='Box',visibility='public',signature='fun run() {}' WHERE id=?`, member); err != nil {
				t.Fatal(err)
			}
		}, unresolved},
		{"facade has no INSTANCE", "ActionsKt.INSTANCE.run", "ActionsKt.INSTANCE.run()", []string{"lib.ActionsKt"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "fun run() {}", "public")
		}, unresolved},
		{"multifile first part", "Utils.first", "Utils.first()", []string{"lib.Utils"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/A.kt", "Utils", true), "first", "fun first() {}", "public")
			f.topLevel(t, f.kotlinFile(t, "lib/B.kt", "Utils", true), "second", "fun second() {}", "public")
		}, "lib.first@lib/A.kt|java_import_scope"},
		{"multifile second part", "Utils.second", "Utils.second()", []string{"lib.Utils"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/A.kt", "Utils", true), "first", "fun first() {}", "public")
			f.topLevel(t, f.kotlinFile(t, "lib/B.kt", "Utils", true), "second", "fun second() {}", "public")
		}, "lib.second@lib/B.kt|java_import_scope"},
		{"multifile part missing annotation", "Utils.first", "Utils.first()", []string{"lib.Utils"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/A.kt", "Utils", true), "first", "fun first() {}", "public")
			f.topLevel(t, f.kotlinFile(t, "lib/B.kt", "Utils", false), "second", "fun second() {}", "public")
		}, unresolved},
		{"single facade name clash", "Utils.first", "Utils.first()", []string{"lib.Utils"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/A.kt", "Utils", false), "first", "fun first() {}", "public")
			f.topLevel(t, f.kotlinFile(t, "lib/B.kt", "Utils", false), "second", "fun second() {}", "public")
		}, unresolved},
		{"multifile duplicate member", "Utils.first", "Utils.first()", []string{"lib.Utils"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/A.kt", "Utils", true), "first", "fun first() {}", "public")
			f.topLevel(t, f.kotlinFile(t, "lib/B.kt", "Utils", true), "first", "fun first() {}", "public")
		}, unresolved},
		{"multifile other package", "Utils.second", "Utils.second()", []string{"lib.Utils"}, func(t *testing.T, f *facadeFixture) {
			f.topLevel(t, f.kotlinFile(t, "lib/A.kt", "Utils", true), "first", "fun first() {}", "public")
			other := f.file(t, "other/B.kt", "kotlin")
			f.scope(t, other, "kotlin", "other", "Utils", true, true)
			id := f.symbolKind(t, other, "second", "other.second", "function", "kotlin")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='other',visibility='public',signature='fun second() {}' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
		}, unresolved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFacadeFixture(t, tc.imports...)
			tc.build(t, f)
			if got := f.resolve(t, tc.dstName, tc.evidence); got != tc.want {
				t.Fatalf("%s = %s, want %s", tc.dstName, got, tc.want)
			}
		})
	}
}

// A same-package facade needs no import; the canonical symbol stays lib.run.
func TestJVMKotlinFileFacadeSamePackage(t *testing.T) {
	f := &facadeFixture{gateFixture: newGateFixture(t)}
	f.callerFile = f.file(t, "lib/Caller.java", "java")
	f.caller = f.symbolKind(t, f.callerFile, "call", "lib.Caller.call", "function", "java")
	f.scope(t, f.callerFile, "java", "lib", "", false, false)
	f.topLevel(t, f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false), "run", "fun run() {}", "public")
	if got := f.resolve(t, "ActionsKt.run", "ActionsKt.run()"); got != "lib.run@lib/Actions.kt|java_package_scope" {
		t.Fatalf("same-package facade = %s", got)
	}
	var symbols int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM symbols WHERE name LIKE '%Kt' OR qualified_name LIKE '%Kt%'`).Scan(&symbols); err != nil || symbols != 0 {
		t.Fatalf("facade symbols = %d, %v", symbols, err)
	}
}

func TestMigrationKotlinJVMFileFacade(t *testing.T) {
	const migrationVersion = 42
	ctx := t.Context()
	dbPath := t.TempDir() + "/graph.sqlite"
	if versions := applyMigrationsBelow(t, ctx, dbPath, migrationVersion); len(versions) == 0 {
		t.Fatal("no prior migrations")
	}
	// A v1 Kotlin row predates the facade facts and must survive unchanged
	// until a reparse replaces it.
	legacy, err := sql.Open(sqliteDriverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(1,7,'kotlin','lib')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var count int
	for _, column := range []string{"jvm_facade_class", "jvm_facade_explicit", "jvm_multifile"} {
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('file_scope_evidence') WHERE name=? AND "notnull"=1`, column).Scan(&count); err != nil || count != 1 {
			t.Fatalf("column %s = %d, %v", column, count, err)
		}
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_file_scope_evidence_repo_jvm_facade'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("facade index = %d, %v", count, err)
	}
	var pkg, class string
	var explicit, multifile int
	if err := s.db.QueryRowContext(ctx, `SELECT package_name,jvm_facade_class,jvm_facade_explicit,jvm_multifile FROM file_scope_evidence WHERE file_id=7`).Scan(&pkg, &class, &explicit, &multifile); err != nil || pkg != "lib" || class != "" || explicit != 0 || multifile != 0 {
		t.Fatalf("legacy row = (%q,%q,%d,%d), %v", pkg, class, explicit, multifile, err)
	}
}

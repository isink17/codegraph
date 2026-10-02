package store

import (
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

func TestJavaLocalMethodShadowsExplicitKotlinStaticImport(t *testing.T) {
	f := newFacadeFixture(t)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='app.Caller' WHERE id=?`, f.caller); err != nil {
		t.Fatal(err)
	}
	local := f.symbolKind(t, f.callerFile, "setLevel", "app.Caller.setLevel", "function", "java")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='app.Caller',visibility='public',signature='setLevel(int)',arity_min=1,arity_max=1 WHERE id=?`, local); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard,is_static) VALUES(?,?,?,?,?,?,?,0,1)`, f.repoID, f.callerFile, "java", "lib.Api.setLevel", "setLevel", "setLevel", "named"); err != nil {
		t.Fatal(err)
	}
	kotlinFile := f.kotlinFile(t, "lib/Api.kt", "Api", false)
	kotlin := f.topLevel(t, kotlinFile, "setLevel", "fun setLevel(x: Int) {}", "public")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_min=1,arity_max=1 WHERE id=?`, kotlin); err != nil {
		t.Fatal(err)
	}
	edge := f.edge(t, f.callerFile, f.caller, "setLevel")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET edge_kind='calls',evidence='setLevel(1)',call_arity=1 WHERE id=?`, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJavaScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.dstSymbolID(t, edge); !ok || got != local {
		qualified := "<unresolved>"
		if ok {
			qualified = f.qualifiedNameOf(t, got)
		}
		t.Fatalf("shadowed static import target=%q, want Java local app.Caller.setLevel", qualified)
	}
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

func TestJVMKotlinFileFacadeFixedArityCallable(t *testing.T) {
	for _, tc := range []struct {
		name, secondName, secondSignature, secondVisibility string
		secondArity                                         any
		want                                                bool
	}{
		{"unique", "run", "", "public", nil, true},
		{"different arity overload", "run", "fun run(x: kotlin.Int, y: kotlin.Int) {}", "public", 2, true},
		{"same arity overload", "run", "fun run(x: kotlin.String) {}", "public", 1, false},
		{"unknown default overload veto", "run", `@JvmOverloads fun run(x: kotlin.String = "") {}`, "public", nil, false},
		{"private sibling is irrelevant", "run", "private fun run(x: kotlin.String) {}", "private", nil, true},
		{"synthetic sibling is irrelevant", "run", "@JvmSynthetic fun run(x: kotlin.String) {}", "public", nil, true},
		{"different JVM name is irrelevant", "run", `@JvmName("other") fun run(x: kotlin.String) {}`, "public", nil, true},
		{"renamed source sibling veto", "other", `@JvmName("run") fun other(x: kotlin.String) {}`, "public", nil, false},
		{"internal sibling veto", "run", "internal fun run(x: kotlin.String) {}", "internal", nil, false},
		{"vararg sibling veto", "run", "fun run(vararg x: kotlin.Int) {}", "public", nil, false},
		{"suspend sibling veto", "run", "suspend fun run(x: kotlin.String) {}", "public", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFacadeFixture(t, "lib.ActionsKt")
			file := f.kotlinFile(t, "lib/Actions.kt", "ActionsKt", false)
			target := f.topLevel(t, file, "run", "fun run(x: kotlin.Int) {}", "public")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_min=1,arity_max=1 WHERE id=?`, target); err != nil {
				t.Fatal(err)
			}
			if tc.secondSignature != "" {
				extra := f.topLevel(t, file, tc.secondName, tc.secondSignature, tc.secondVisibility)
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_min=?,arity_max=? WHERE id=?`, tc.secondArity, tc.secondArity, extra); err != nil {
					t.Fatal(err)
				}
			}
			edge := f.edge(t, f.callerFile, f.caller, "ActionsKt.run")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET edge_kind='calls',evidence='ActionsKt.run(1)',call_arity=1 WHERE id=?`, edge); err != nil {
				t.Fatal(err)
			}
			if _, err := resolveJavaScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
				t.Fatal(err)
			}
			got, ok := f.dstSymbolID(t, edge)
			if ok != tc.want || ok && got != target {
				t.Fatalf("binding=(%d,%v), want target=%d resolved=%v", got, ok, target, tc.want)
			}
		})
	}
}

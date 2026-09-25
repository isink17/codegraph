//go:build cgo

package indexer

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

func TestJVMCoreInteropPeerLifecycle(t *testing.T) {
	caller := `package app
import lib.Service
fun call() { Service.run() }`
	java := `package lib; public class Service { public static void run() {} }`
	r := newLifecycleRepo(t, tree{"Caller.kt": caller})
	assertJVMUnresolved(t, r, "Caller.kt", "Service.run")

	r.write(t, "Service.java", java)
	r.update(t, "Service.java")
	assertJVMResolved(t, r, "Caller.kt", "Service.run", "Service.java", "kotlin_import_scope")
	assertJVMReference(t, r, "Caller.kt", "Service.run", true)
	r.assertFreshParity(t, "peer add")

	r.remove(t, "Service.java")
	r.update(t, "Service.java")
	assertJVMUnresolved(t, r, "Caller.kt", "Service.run")
	assertJVMReference(t, r, "Caller.kt", "Service.run", false)
	r.assertFreshParity(t, "peer delete")

	r.write(t, "Service.java", java)
	r.update(t, "Service.java")
	assertJVMResolved(t, r, "Caller.kt", "Service.run", "Service.java", "kotlin_import_scope")
	assertJVMReference(t, r, "Caller.kt", "Service.run", true)
	r.assertFreshParity(t, "peer restore")

	r.write(t, "Service.kt", "package lib\nclass Service")
	r.update(t, "Service.kt")
	assertJVMUnresolved(t, r, "Caller.kt", "Service.run")
	assertJVMReference(t, r, "Caller.kt", "Service.run", false)
	r.assertFreshParity(t, "peer competitor add")

	r.remove(t, "Service.kt")
	r.update(t, "Service.kt")
	assertJVMResolved(t, r, "Caller.kt", "Service.run", "Service.java", "kotlin_import_scope")
	assertJVMReference(t, r, "Caller.kt", "Service.run", true)
	r.assertFreshParity(t, "peer competitor delete")

	r.write(t, "Service.java", `package lib; public class Service { public void run() {} }`)
	r.update(t, "Service.java")
	assertJVMUnresolved(t, r, "Caller.kt", "Service.run")
	assertJVMReference(t, r, "Caller.kt", "Service.run", false)
	r.assertFreshParity(t, "staticness")
}

func TestJVMCoreInteropConstructorSafety(t *testing.T) {
	t.Run("Kotlin refuses only private Java constructor", func(t *testing.T) {
		r := newLifecycleRepo(t, tree{
			"Caller.kt":    "package app\nimport lib.Service\nfun call() { Service() }",
			"Service.java": "package lib; public class Service { private Service() {} }",
		})
		assertJVMUnresolved(t, r, "Caller.kt", "Service")
	})
	t.Run("Java refuses Kotlin class construction without constructor facts", func(t *testing.T) {
		r := newLifecycleRepo(t, tree{
			"Caller.java": "package app; import lib.Service; class Caller { void call() { new Service(); } }",
			"Service.kt":  "package lib\nclass Service",
		})
		for _, got := range r.projection(t) {
			if strings.Contains(got, `Caller.java:`) && strings.Contains(got, `"Service"`) && !strings.Contains(got, ":: [/]") {
				t.Fatalf("Java Kotlin construction = %s, want unresolved", got)
			}
		}
	})
}

func TestJVMCoreInteropImportTransition(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Caller.kt":      "package app\nimport a.Service\nfun call() { Service.run() }",
		"a/Service.java": "package a; public class Service { public static void run() {} }",
		"b/Service.java": "package b; public class Service { public static void run() {} }",
	})
	assertJVMResolved(t, r, "Caller.kt", "Service.run", "a/Service.java", "kotlin_import_scope")
	r.write(t, "Caller.kt", "package app\nimport b.Service\nfun call() { Service.run() }")
	r.update(t, "Caller.kt")
	assertJVMResolved(t, r, "Caller.kt", "Service.run", "b/Service.java", "kotlin_import_scope")
	r.assertFreshParity(t, "import transition")
}

func TestJVMCoreInteropKotlinJavaStaticOwnerForms(t *testing.T) {
	for _, tc := range []struct {
		name, caller, edge, strategy string
	}{
		{"same package", "package app\nfun call() { Service.run() }", "Service.run", "kotlin_package_scope"},
		{"explicit import", "package app\nimport lib.Service\nfun call() { Service.run() }", "Service.run", "kotlin_import_scope"},
		{"wildcard import", "package app\nimport lib.*\nfun call() { Service.run() }", "Service.run", "kotlin_import_scope"},
		{"alias import", "package app\nimport lib.Service as S\nfun call() { S.run() }", "S.run", "kotlin_import_scope"},
		{"fully qualified", "package app\nfun call() { lib.Service.run() }", "lib.Service.run", "kotlin_package_scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkg := "lib"
			if tc.name == "same package" {
				pkg = "app"
			}
			r := newLifecycleRepo(t, tree{
				"Caller.kt":    tc.caller,
				"Service.java": "package " + pkg + "; public class Service { public static void run() {} }",
			})
			if tc.name == "fully qualified" {
				r.write(t, "other/Service.java", "package other; public class Service { public static void run() {} }")
				r.update(t, "other/Service.java")
			}
			assertJVMResolved(t, r, "Caller.kt", tc.edge, "Service.java", tc.strategy)
			assertJVMQueryRelation(t, r, "app.call", pkg+".Service.run")
		})
	}
}

func TestJVMCoreInteropKotlinJavaConstructionVisibility(t *testing.T) {
	for _, tc := range []struct {
		name, caller, target string
		resolved             bool
	}{
		{"implicit public default", "package app\nimport lib.Service\nfun call() { Service() }", "package lib; public class Service {}", true},
		{"public", "package app\nimport lib.Service\nfun call() { Service() }", "package lib; public class Service { public Service() {} }", true},
		{"private", "package app\nimport lib.Service\nfun call() { Service() }", "package lib; public class Service { private Service() {} }", false},
		{"same package package", "package lib\nfun call() { Service() }", "package lib; public class Service { Service() {} }", true},
		{"cross package package", "package app\nimport lib.Service\nfun call() { Service() }", "package lib; public class Service { Service() {} }", false},
		{"same package protected", "package lib\nfun call() { Service() }", "package lib; public class Service { protected Service() {} }", true},
		{"cross package protected", "package app\nimport lib.Service\nfun call() { Service() }", "package lib; public class Service { protected Service() {} }", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tree{"Caller.kt": tc.caller, "Service.java": tc.target})
			if tc.resolved {
				assertJVMResolvedPrefix(t, r, "Caller.kt", "Service", "Service.java", "kotlin_")
			} else {
				assertJVMUnresolved(t, r, "Caller.kt", "Service")
			}
		})
	}
}

func TestJVMCoreInteropKotlinJavaMemberVisibilityAndStaticness(t *testing.T) {
	for _, tc := range []struct {
		name, caller, target string
		resolved             bool
	}{
		{"public cross package", "package app\nimport lib.Service\nfun call() { Service.run() }", "package lib; public class Service { public static void run() {} }", true},
		{"package same package", "package lib\nfun call() { Service.run() }", "package lib; public class Service { static void run() {} }", true},
		{"package cross package", "package app\nimport lib.Service\nfun call() { Service.run() }", "package lib; public class Service { static void run() {} }", false},
		{"protected same package", "package lib\nfun call() { Service.run() }", "package lib; public class Service { protected static void run() {} }", true},
		{"protected cross package", "package app\nimport lib.Service\nfun call() { Service.run() }", "package lib; public class Service { protected static void run() {} }", false},
		{"private", "package lib\nfun call() { Service.run() }", "package lib; public class Service { private static void run() {} }", false},
		{"instance only", "package app\nimport lib.Service\nfun call() { Service.run() }", "package lib; public class Service { public void run() {} }", false},
		{"static plus instance", "package app\nimport lib.Service\nfun call() { Service.run() }", "package lib; public class Service { public static void run() {} public void run(int n) {} }", true},
		{"static overload", "package app\nimport lib.Service\nfun call() { Service.run() }", "package lib; public class Service { public static void run() {} public static void run(int n) {} }", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tree{"Caller.kt": tc.caller, "Service.java": tc.target})
			if tc.resolved {
				assertJVMResolvedPrefix(t, r, "Caller.kt", "Service.run", "Service.java", "kotlin_")
			} else {
				assertJVMUnresolved(t, r, "Caller.kt", "Service.run")
			}
		})
	}
}

func TestJVMCoreInteropJavaPeerQNameLifecycle(t *testing.T) {
	caller := "package app; import lib.Service; class Caller { void call() { Service.run(); } }"
	java := "package lib; public class Service { public static void run() {} }"
	r := newLifecycleRepo(t, tree{"Caller.java": caller, "Service.java": java})
	assertJVMResolved(t, r, "Caller.java", "Service.run", "Service.java", "java_import_scope")
	r.write(t, "Service.kt", "package lib\nclass Service")
	r.update(t, "Service.kt")
	assertJVMUnresolved(t, r, "Caller.java", "Service.run")
	assertJVMReference(t, r, "Caller.java", "Service.run", false)
	r.assertFreshParity(t, "Java peer conflict")
	r.remove(t, "Service.kt")
	r.update(t, "Service.kt")
	assertJVMResolved(t, r, "Caller.java", "Service.run", "Service.java", "java_import_scope")
	assertJVMReference(t, r, "Caller.java", "Service.run", true)
	r.assertFreshParity(t, "Java peer conflict removed")
}

func TestJVMCoreInteropQueryParity(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Caller.kt":    "package app\nimport lib.Service\nfun call() { Service.run() }",
		"Service.java": "package lib; public class Service { public static void run() {} }",
	})
	assertJVMQueryRelation(t, r, "app.call", "lib.Service.run")
}

func TestJVMCoreInteropRefusalQueryMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, callerPath, caller, edge, callerQName, targetQName string
		files                                                    tree
	}{
		{"java Kotlin construction", "Caller.java", "package app; import lib.Service; class Caller { void call() { new Service(); } }", "Service", "app.Caller.call", "lib.Service", tree{"Service.kt": "package lib\nclass Service"}},
		{"java Kotlin member", "Caller.java", "package app; import lib.Service; class Caller { void call() { Service.run(); } }", "Service.run", "app.Caller.call", "lib.Service.run", tree{"Service.kt": "package lib\nclass Service { fun run() {} }"}},
		{"java explicit static import Kotlin", "Caller.java", "package app; import static lib.Service.run; class Caller { void call() { run(); } }", "run", "app.Caller.call", "lib.Service.run", tree{"Service.kt": "package lib\nclass Service { fun run() {} }"}},
		{"java wildcard static import Kotlin", "Caller.java", "package app; import static lib.Service.*; class Caller { void call() { run(); } }", "run", "app.Caller.call", "lib.Service.run", tree{"Service.kt": "package lib\nclass Service { fun run() {} }"}},
		{"Kotlin private Java static", "Caller.kt", "package lib\nfun call() { Service.run() }", "Service.run", "lib.call", "lib.Service.run", tree{"Service.java": "package lib; public class Service { private static void run() {} }"}},
		{"Kotlin cross package package static", "Caller.kt", "package app\nimport lib.Service\nfun call() { Service.run() }", "Service.run", "app.call", "lib.Service.run", tree{"Service.java": "package lib; public class Service { static void run() {} }"}},
		{"Kotlin cross package protected static", "Caller.kt", "package app\nimport lib.Service\nfun call() { Service.run() }", "Service.run", "app.call", "lib.Service.run", tree{"Service.java": "package lib; public class Service { protected static void run() {} }"}},
		{"Kotlin Java instance", "Caller.kt", "package app\nimport lib.Service\nfun call() { Service.run() }", "Service.run", "app.call", "lib.Service.run", tree{"Service.java": "package lib; public class Service { public void run() {} }"}},
		{"Kotlin Java static overload", "Caller.kt", "package app\nimport lib.Service\nfun call() { Service.run() }", "Service.run", "app.call", "lib.Service.run", tree{"Service.java": "package lib; public class Service { public static void run() {} public static void run(int n) {} }"}},
		{"Kotlin nested Java type", "Caller.kt", "package app\nimport lib.Outer\nfun call() { Outer.Nested.run() }", "Outer.Nested.run", "app.call", "lib.Outer.Nested.run", tree{"Outer.java": "package lib; public class Outer { public static class Nested { public static void run() {} } }"}},
		{"public Java Kotlin peer ambiguity", "Caller.java", "package app; import lib.Service; class Caller { void call() { Service.run(); } }", "Service.run", "app.Caller.call", "lib.Service.run", tree{"Service.java": "package lib; public class Service { public static void run() {} }", "Service.kt": "package lib\nclass Service"}},
		{"Kotlin bare Java member", "Caller.kt", "package app\nimport lib.Service\nfun call() { run() }", "run", "app.call", "lib.Service.run", tree{"Service.java": "package lib; public class Service { public static void run() {} }"}},
		{"synthetic facade spelling", "Caller.java", "package app; import lib.FileNameKt; class Caller { void call() { FileNameKt.run(); } }", "FileNameKt.run", "app.Caller.call", "lib.run", tree{"FileName.kt": "package lib\nfun run() {}"}},
		{"synthetic companion shadow", "Caller.java", "package app; import lib.Address; class Caller { void call() { Address.toString(null); } }", "Address.toString", "app.Caller.call", "lib.Address.toString", tree{"Address.kt": "package lib\nclass Address { fun toString(): String = \"\"; companion object { @JvmStatic fun toString(x: Array<String>): String = \"\" } }"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := tree{tc.callerPath: tc.caller}
			for path, source := range tc.files {
				files[path] = source
			}
			r := newLifecycleRepo(t, files)
			if tc.name == "java Kotlin construction" {
				assertJVMConstructUnresolved(t, r, tc.callerPath, tc.edge)
				assertJVMNoReference(t, r, tc.callerPath, tc.edge)
			} else {
				assertJVMUnresolved(t, r, tc.callerPath, tc.edge)
				assertJVMReference(t, r, tc.callerPath, tc.edge, false)
			}
			if tc.name != "synthetic companion shadow" {
				assertJVMNoQueryRelation(t, r, tc.callerQName, tc.targetQName)
			}
		})
	}
}

func TestJVMCoreInteropJavaPeerVisibility(t *testing.T) {
	for _, tc := range []struct {
		visibility string
		resolved   bool
	}{
		{"public", false},
		{"internal", true},
		{"private", true},
	} {
		t.Run(tc.visibility, func(t *testing.T) {
			r := newLifecycleRepo(t, tree{
				"Caller.java":  "package app; import lib.Service; class Caller { void call() { Service.run(); } }",
				"Service.java": "package lib; public class Service { public static void run() {} }",
				"Service.kt":   "package lib\n" + tc.visibility + " class Service",
			})
			if tc.resolved {
				assertJVMResolved(t, r, "Caller.java", "Service.run", "Service.java", "java_import_scope")
				assertJVMQueryRelation(t, r, "app.Caller.call", "lib.Service.run")
			} else {
				assertJVMUnresolved(t, r, "Caller.java", "Service.run")
				assertJVMReference(t, r, "Caller.java", "Service.run", false)
				assertJVMNoQueryRelation(t, r, "app.Caller.call", "lib.Service.run")
			}
		})
	}
}

func TestJVMCoreInteropVisibilityLifecycle(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Caller.kt":    "package app\nimport lib.Service\nfun call() { Service.run() }",
		"Service.java": "package lib; public class Service { public static void run() {} }",
	})
	assertJVMQueryRelation(t, r, "app.call", "lib.Service.run")
	assertJVMReference(t, r, "Caller.kt", "Service.run", true)
	r.write(t, "Service.java", "package lib; public class Service { private static void run() {} }")
	r.update(t, "Service.java")
	assertJVMUnresolved(t, r, "Caller.kt", "Service.run")
	assertJVMReference(t, r, "Caller.kt", "Service.run", false)
	assertJVMNoQueryRelation(t, r, "app.call", "lib.Service.run")
	r.assertFreshParity(t, "public to private")
	r.write(t, "Service.java", "package lib; public class Service { public static void run() {} }")
	r.update(t, "Service.java")
	assertJVMQueryRelation(t, r, "app.call", "lib.Service.run")
	assertJVMReference(t, r, "Caller.kt", "Service.run", true)
	r.assertFreshParity(t, "private to public")
}

func TestJVMCoreInteropPackageAndProtectedLifecycle(t *testing.T) {
	for _, visibility := range []string{"", "protected "} {
		t.Run(strings.TrimSpace(visibility)+" package", func(t *testing.T) {
			r := newLifecycleRepo(t, tree{
				"Caller.kt":    "package lib\nfun call() { Service.run() }",
				"Service.java": "package lib; public class Service { " + visibility + "static void run() {} }",
			})
			assertJVMQueryRelation(t, r, "lib.call", "lib.Service.run")
			assertJVMReference(t, r, "Caller.kt", "Service.run", true)
			r.write(t, "Caller.kt", "package app\nimport lib.Service\nfun call() { Service.run() }")
			r.update(t, "Caller.kt")
			assertJVMUnresolved(t, r, "Caller.kt", "Service.run")
			assertJVMReference(t, r, "Caller.kt", "Service.run", false)
			assertJVMNoQueryRelation(t, r, "app.call", "lib.Service.run")
			r.assertFreshParity(t, "cross package")
			if visibility == "" {
				r.write(t, "Service.java", "package lib; public class Service { public static void run() {} }")
				r.update(t, "Service.java")
			} else {
				r.write(t, "Caller.kt", "package lib\nfun call() { Service.run() }")
				r.update(t, "Caller.kt")
			}
			caller := "lib.call"
			if visibility == "" {
				caller = "app.call"
			}
			assertJVMQueryRelation(t, r, caller, "lib.Service.run")
			assertJVMReference(t, r, "Caller.kt", "Service.run", true)
			r.assertFreshParity(t, "same package restore")
		})
	}
}

func TestJVMCoreInteropAliasAndWildcardLifecycle(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Caller.kt":      "package app\nimport a.Service as S\nfun call() { S.run() }",
		"a/Service.java": "package a; public class Service { public static void run() {} }",
		"b/Service.java": "package b; public class Service { public static void run() {} }",
	})
	assertJVMResolved(t, r, "Caller.kt", "S.run", "a/Service.java", "kotlin_import_scope")
	assertJVMReference(t, r, "Caller.kt", "S.run", true)
	r.write(t, "Caller.kt", "package app\nimport b.Service as S\nfun call() { S.run() }")
	r.update(t, "Caller.kt")
	assertJVMResolved(t, r, "Caller.kt", "S.run", "b/Service.java", "kotlin_import_scope")
	assertJVMReference(t, r, "Caller.kt", "S.run", true)
	r.assertFreshParity(t, "alias transition")

	wildcard := newLifecycleRepo(t, tree{
		"Caller.kt":      "package app\nimport a.*\nimport b.*\nfun call() { Service.run() }",
		"a/Service.java": "package a; public class Service { public static void run() {} }",
		"b/Service.java": "package b; public class Service { public static void run() {} }",
	})
	assertJVMUnresolved(t, wildcard, "Caller.kt", "Service.run")
	assertJVMReference(t, wildcard, "Caller.kt", "Service.run", false)
	wildcard.remove(t, "b/Service.java")
	wildcard.update(t, "b/Service.java")
	assertJVMResolved(t, wildcard, "Caller.kt", "Service.run", "a/Service.java", "kotlin_import_scope")
	assertJVMReference(t, wildcard, "Caller.kt", "Service.run", true)
	wildcard.assertFreshParity(t, "wildcard competitor delete")
}

func assertJVMResolved(t *testing.T, r *lifecycleRepo, path, name, target, strategy string) {
	t.Helper()
	if got := r.edgeState(t, path, name); !strings.Contains(got, target) || !strings.Contains(got, strategy+"/high") {
		t.Fatalf("%s = %s, want %s via %s", name, got, target, strategy)
	}
}

func assertJVMResolvedPrefix(t *testing.T, r *lifecycleRepo, path, name, target, strategyPrefix string) {
	t.Helper()
	if got := r.edgeState(t, path, name); !strings.Contains(got, target) || !strings.Contains(got, strategyPrefix) || !strings.Contains(got, "/high") {
		t.Fatalf("%s = %s, want %s via %s", name, got, target, strategyPrefix)
	}
}

func assertJVMUnresolved(t *testing.T, r *lifecycleRepo, path, name string) {
	t.Helper()
	if got := r.edgeState(t, path, name); !strings.Contains(got, ":: [/]") {
		t.Fatalf("%s = %s, want unresolved", name, got)
	}
}

func assertJVMConstructUnresolved(t *testing.T, r *lifecycleRepo, path, name string) {
	t.Helper()
	prefix := "edge " + path + ":"
	needle := `-> "` + name + `"`
	for _, line := range r.projection(t) {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, needle) {
			if strings.Contains(line, ":: [/]") {
				return
			}
			t.Fatalf("%s = %s, want unresolved", name, line)
		}
	}
	t.Fatalf("%s has no construct edge", name)
}

func assertJVMReference(t *testing.T, r *lifecycleRepo, path, name string, bound bool) {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var symbol sql.NullInt64
	err = db.QueryRowContext(r.ctx, `SELECT r.symbol_id FROM references_tbl r JOIN files f ON f.id=r.file_id WHERE r.repo_id=? AND f.path=? AND r.ref_kind='call' AND r.qualified_name=?`, r.repoID, path, name).Scan(&symbol)
	if err != nil || symbol.Valid != bound {
		t.Fatalf("reference %s bound=(%v,%v), want %v", name, symbol, err, bound)
	}
}

func assertJVMNoReference(t *testing.T, r *lifecycleRepo, path, name string) {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	err = db.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM references_tbl r JOIN files f ON f.id=r.file_id WHERE r.repo_id=? AND f.path=? AND r.qualified_name=?`, r.repoID, path, name).Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("reference %s count=(%d,%v), want 0", name, n, err)
	}
}

func assertJVMQueryRelation(t *testing.T, r *lifecycleRepo, caller, target string) {
	t.Helper()
	callees, err := r.store.FindCallees(r.ctx, r.repoID, caller, 0, 10, 0)
	if err != nil || !hasSymbolQName(callees, target) {
		t.Fatalf("FindCallees(%s) = %#v, %v; want %s", caller, callees, err, target)
	}
	callers, err := r.store.FindCallers(r.ctx, r.repoID, target, 0, 10, 0)
	if err != nil || !hasSymbolQName(callers, caller) {
		t.Fatalf("FindCallers(%s) = %#v, %v; want %s", target, callers, err, caller)
	}
}

func assertJVMNoQueryRelation(t *testing.T, r *lifecycleRepo, caller, target string) {
	t.Helper()
	callees, err := r.store.FindCallees(r.ctx, r.repoID, caller, 0, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasSymbolQName(callees, target) {
		t.Fatalf("FindCallees(%s) unexpectedly contains %s: %#v", caller, target, callees)
	}
	callers, err := r.store.FindCallers(r.ctx, r.repoID, target, 0, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasSymbolQName(callers, caller) {
		t.Fatalf("FindCallers(%s) unexpectedly contains %s: %#v", target, caller, callers)
	}
}

//go:build cgo

package indexer

import (
	"strings"
	"testing"
)

func TestJavaPackageImportAndConstructorScope(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a/Util.java":   `package a; public class Util { public static void run() {} }`,
		"a/Foo.java":    `package a; public class Foo { public Foo() {} }`,
		"b/Caller.java": `package b; import static a.Util.run; class Caller { void x() { run(); new a.Foo(); } }`,
		"c/Foo.java":    `package c; public class Foo { public Foo() {} }`,
	})
	if got := r.edgeState(t, "b/Caller.java", "run"); !strings.Contains(got, "a/Util.java") || !strings.Contains(got, "java_static_import/high") {
		t.Fatalf("static import = %s", got)
	}
	found := false
	for _, line := range r.projection(t) {
		if strings.Contains(line, `b/Caller.java:`) && strings.Contains(line, `a.Foo.Foo`) && strings.Contains(line, "java_constructor/high") {
			found = true
		}
	}
	if !found {
		t.Fatalf("constructor not resolved: %v", r.projection(t))
	}
}

func TestJavaPackageIsolationRejectsUnimportedType(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a/Foo.java":    `package a; public class Foo { public static void run() {} }`,
		"b/Caller.java": `package b; class Caller { void x() { Foo.run(); } }`,
	})
	if got := r.edgeState(t, "b/Caller.java", "Foo.run"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("unimported type = %s", got)
	}
}

func TestJavaWildcardImportResolvesSingleCandidate(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a/Foo.java": `package a; public class Foo { public static void run() {} }`,
		"b/C.java":   "package b; import a.*; class C { void x() {\n Foo.run();\n } }",
	})
	if got := r.edgeState(t, "b/C.java", "Foo.run"); !strings.Contains(got, "a/Foo.java") {
		t.Fatalf("wildcard import = %s", got)
	}
}

func TestJavaTypeMemberRequiresUniqueStaticMethodAndOwnerProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, target, caller, want string
	}{
		{"ambiguous static overload", `package app; class Service { static void run(String x) {} static void run(byte[] x) {} }`, `package app; class Caller { void call() { Service.run(null); } }`, ":: [/]"},
		{"instance refusal", `package app; class Service { void run() {} }`, `package app; class Caller { void call() { Service.run(); } }`, ":: [/]"},
		{"same package static", `package app; class Service { static void run() {} }`, `package app; class Caller { void call() { Service.run(); } }`, "java_package_scope/high"},
		{"import provenance", `package lib; public class Service { public static void run() {} }`, `package app; import lib.Service; class Caller { void call() { Service.run(); } }`, "java_import_scope/high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tree{"Service.java": tc.target, "Caller.java": tc.caller})
			if got := r.edgeState(t, "Caller.java", "Service.run"); !strings.Contains(got, tc.want) {
				t.Fatalf("Service.run = %s, want %s", got, tc.want)
			}
		})
	}
}

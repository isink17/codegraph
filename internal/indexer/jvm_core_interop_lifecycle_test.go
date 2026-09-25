//go:build cgo

package indexer

import (
	"strings"
	"testing"
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
	r.assertFreshParity(t, "peer add")

	r.remove(t, "Service.java")
	r.update(t, "Service.java")
	assertJVMUnresolved(t, r, "Caller.kt", "Service.run")
	r.assertFreshParity(t, "peer delete")

	r.write(t, "Service.java", java)
	r.update(t, "Service.java")
	assertJVMResolved(t, r, "Caller.kt", "Service.run", "Service.java", "kotlin_import_scope")
	r.assertFreshParity(t, "peer restore")

	r.write(t, "Service.kt", "package lib\nclass Service")
	r.update(t, "Service.kt")
	assertJVMUnresolved(t, r, "Caller.kt", "Service.run")
	r.assertFreshParity(t, "peer competitor add")

	r.remove(t, "Service.kt")
	r.update(t, "Service.kt")
	assertJVMResolved(t, r, "Caller.kt", "Service.run", "Service.java", "kotlin_import_scope")
	r.assertFreshParity(t, "peer competitor delete")

	r.write(t, "Service.java", `package lib; public class Service { public void run() {} }`)
	r.update(t, "Service.java")
	assertJVMUnresolved(t, r, "Caller.kt", "Service.run")
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

func assertJVMResolved(t *testing.T, r *lifecycleRepo, path, name, target, strategy string) {
	t.Helper()
	if got := r.edgeState(t, path, name); !strings.Contains(got, target) || !strings.Contains(got, strategy+"/high") {
		t.Fatalf("%s = %s, want %s via %s", name, got, target, strategy)
	}
}

func assertJVMUnresolved(t *testing.T, r *lifecycleRepo, path, name string) {
	t.Helper()
	if got := r.edgeState(t, path, name); !strings.Contains(got, ":: [/]") {
		t.Fatalf("%s = %s, want unresolved", name, got)
	}
}

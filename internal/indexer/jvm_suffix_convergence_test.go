//go:build cgo

package indexer

import "testing"

// A symbol-free alias file used to trigger a repo-wide suffix pass without
// the Kotlin scope veto, binding a dynamic receiver that fresh indexing refuses.
func TestKotlinAliasMutationSuffixConvergence(t *testing.T) {
	aliases := "package api\ntypealias Long = kotlin.Long\ntypealias String = kotlin.String\n"
	defs := `package api
class TaskRunner {
    interface Backend { fun nanoTime(): kotlin.Long }
}`
	r := newLifecycleRepo(t, tree{
		"TaskRunner.kt": defs,
		"Caller.kt": `package api
fun stable() {}
fun call() { taskRunner.backend.nanoTime(); stable() }`,
	})
	check := func(stage string) {
		t.Helper()
		assertJVMUnresolved(t, r, "Caller.kt", "taskRunner.backend.nanoTime")
		assertJVMReference(t, r, "Caller.kt", "taskRunner.backend.nanoTime", false)
		assertJVMResolved(t, r, "Caller.kt", "stable", "Caller.kt", "kotlin_package_scope")
		r.assertFreshParity(t, stage)
	}
	check("baseline")
	r.write(t, "Aliases.kt", aliases)
	r.update(t, "Aliases.kt")
	check("alias add")
	r.write(t, "Aliases.kt", "package api\ntypealias Long = kotlin.Int\ntypealias String = kotlin.String\n")
	r.update(t, "Aliases.kt")
	check("alias mutation")
	r.write(t, "Aliases.kt", aliases)
	r.update(t, "Aliases.kt")
	check("reverse mutation")
	r.remove(t, "Aliases.kt")
	r.update(t, "Aliases.kt")
	check("alias removal")
	r.write(t, "Other.kt", defs)
	r.update(t, "Other.kt")
	check("competing declaration")
	r.remove(t, "Other.kt")
	r.update(t, "Other.kt")
	check("competitor removal")
}

func TestJVMSuffixUpgradeClearsStaleEdgesAndReferences(t *testing.T) {
	for _, language := range []string{"kotlin", "java"} {
		for _, strategy := range []string{"dot_suffix", "dot_tail3"} {
			t.Run(language+"/"+strategy, func(t *testing.T) {
				files := tree{
					"TaskRunner.kt": "package api\nclass TaskRunner {\n    interface Backend {\n        fun nanoTime(): kotlin.Long\n    }\n}",
					"Caller.kt":     "package api\nfun stable() {}\nfun call() { taskRunner.backend.nanoTime(); stable() }",
				}
				path := "Caller.kt"
				if language == "java" {
					delete(files, path)
					path = "Caller.java"
					files[path] = "package api;\nclass Caller {\n    static void stable() {}\n    void call() {\n        taskRunner.backend.nanoTime();\n        stable();\n    }\n}"
				}
				r := newLifecycleRepo(t, files)
				db := openLifecycleTestDB(t, r)
				// Simulate an older binary after all prior repairs were marked done.
				if _, err := db.Exec(`UPDATE edges SET dst_symbol_id=(SELECT id FROM symbols WHERE qualified_name='api.TaskRunner.Backend.nanoTime'),resolution_strategy=?,resolution_confidence='low' WHERE dst_name='taskRunner.backend.nanoTime'`, strategy); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE references_tbl SET symbol_id=(SELECT id FROM symbols WHERE qualified_name='api.TaskRunner.Backend.nanoTime') WHERE qualified_name='taskRunner.backend.nanoTime'`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`DELETE FROM settings WHERE key LIKE 'resolver.jvm_suffix_scope_repaired.v1.%'`); err != nil {
					t.Fatal(err)
				}
				assertJVMReference(t, r, path, "taskRunner.backend.nanoTime", true)
				r.update(t)
				assertJVMUnresolved(t, r, path, "taskRunner.backend.nanoTime")
				assertJVMReference(t, r, path, "taskRunner.backend.nanoTime", false)
				r.assertFreshParity(t, "upgrade unchanged tree")
				r.update(t)
				r.assertFreshParity(t, "second unchanged update")
			})
		}
	}
}

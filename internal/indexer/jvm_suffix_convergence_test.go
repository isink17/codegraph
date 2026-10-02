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

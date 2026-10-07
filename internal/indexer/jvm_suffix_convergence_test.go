//go:build cgo

package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
		noop := r.update(t)
		if noop.FilesChanged != 0 || noop.FilesIndexed != 0 || noop.FilesDeleted != 0 {
			t.Fatalf("%s second update was not a no-op: %+v", stage, noop)
		}
		assertJVMUnresolved(t, r, "Caller.kt", "taskRunner.backend.nanoTime")
		assertJVMReference(t, r, "Caller.kt", "taskRunner.backend.nanoTime", false)
		assertJVMResolved(t, r, "Caller.kt", "stable", "Caller.kt", "kotlin_package_scope")
		scope, err := r.store.JVMCompilationScope(r.ctx, r.repoID)
		if err != nil || scope.Domain != "jvm-type-identity" || scope.State != "unknown" || scope.Version != 1 || scope.Provenance == "" {
			t.Fatalf("%s compilation scope = %+v, err=%v", stage, scope, err)
		}
		finalTree := tree{}
		if err := filepath.WalkDir(r.root, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			rel, err := filepath.Rel(r.root, path)
			if err != nil {
				return err
			}
			content, err := os.ReadFile(path)
			if err == nil {
				finalTree[filepath.ToSlash(rel)] = string(content)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		fresh := newLifecycleRepo(t, finalTree)
		gotFacts, err := r.store.JVMTypeEvidence(r.ctx, r.repoID)
		if err != nil {
			t.Fatal(err)
		}
		wantFacts, err := fresh.store.JVMTypeEvidence(fresh.ctx, fresh.repoID)
		if err != nil || !reflect.DeepEqual(gotFacts, wantFacts) {
			t.Fatalf("%s persisted JVM facts differ from fresh index:\nupdate=%+v\nfresh=%+v err=%v", stage, gotFacts, wantFacts, err)
		}
		r.assertFreshParity(t, stage)
	}
	check("baseline")
	r.write(t, "Aliases.kt", aliases)
	r.update(t, "Aliases.kt")
	assertAliasFact := func(want string) {
		t.Helper()
		facts, err := r.store.JVMTypeEvidence(r.ctx, r.repoID)
		got := ""
		if err == nil {
			for _, fact := range facts {
				if fact.FilePath == "Aliases.kt" && fact.Kind == "typealias" {
					got = fact.AliasTarget
					break
				}
			}
		}
		if want == "" {
			if err != nil || got != "" {
				t.Fatalf("alias fact remains with target %q", got)
			}
			return
		}
		if err != nil || got != want {
			t.Fatalf("alias target = %q, want %q, err=%v", got, want, err)
		}
	}
	assertAliasFact("kotlin.Long")
	check("alias add")
	r.write(t, "Aliases.kt", "package api\ntypealias Long = kotlin.Int\ntypealias String = kotlin.String\n")
	r.update(t, "Aliases.kt")
	assertAliasFact("kotlin.Int")
	check("alias mutation")
	r.write(t, "Aliases.kt", aliases)
	r.update(t, "Aliases.kt")
	assertAliasFact("kotlin.Long")
	check("reverse mutation")
	r.remove(t, "Aliases.kt")
	r.update(t, "Aliases.kt")
	assertAliasFact("")
	check("alias removal")
	r.write(t, "Other.kt", defs)
	r.update(t, "Other.kt")
	check("competing declaration")
	r.remove(t, "Other.kt")
	r.update(t, "Other.kt")
	check("competitor removal")
}

func TestJVMValueAliasIdentityRemainsUnresolved(t *testing.T) {
	var java strings.Builder
	java.WriteString("package okhttp3.internal;\nimport static okhttp3.internal.Internal.parseCookie;\nclass Caller { Cookie call(HttpUrl url) {\n")
	for i := range 24 {
		fmt.Fprintf(&java, `parseCookie(%dL, url, "x=y");`+"\n", i)
	}
	java.WriteString("return null; } }\n")
	r := newLifecycleRepo(t, tree{
		"Internal.kt": `@file:JvmName("Internal")
package okhttp3.internal
@JvmInline value class Millis(val value: kotlin.Long)
typealias Long = Millis
@JvmInline value class Header(val value: kotlin.String)
typealias String = Header
class HttpUrl
class Cookie
fun parseCookie(currentTimeMillis: Long, url: HttpUrl, setCookie: String): Cookie? = null
`,
		"Caller.java": java.String(),
	})
	assertJVMUnresolved(t, r, "Caller.java", "parseCookie")
	assertJVMReference(t, r, "Caller.java", "parseCookie", false)
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	var unresolved int
	for _, edge := range edges {
		if edge.FilePath == "Caller.java" && edge.Kind == "calls" && edge.DstName == "parseCookie" {
			if edge.DstSymbolID != nil {
				t.Fatalf("parseCookie edge resolved to %s", edge.DstQualifiedName)
			}
			unresolved++
		}
	}
	if unresolved != 24 {
		t.Fatalf("unresolved parseCookie calls = %d, want 24", unresolved)
	}
	facts, err := r.store.JVMTypeEvidence(r.ctx, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	var aliases int
	for _, fact := range facts {
		if fact.Kind == "typealias" && (fact.AliasTarget == "Millis" || fact.AliasTarget == "Header") {
			aliases++
		}
	}
	if aliases != 2 {
		t.Fatalf("persisted value alias facts = %d, want 2: %+v", aliases, facts)
	}
}

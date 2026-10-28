//go:build cgo

package indexer

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestKotlinReceiverHelperProviderLifecycle(t *testing.T) {
	for _, extension := range []bool{false, true} {
		for _, constructor := range []bool{false, true} {
			t.Run(fmt.Sprintf("extension=%v/constructor=%v", extension, constructor), func(t *testing.T) {
				caller := `package p
class Target
class UserService {
    fun validate() {}
    fun run(api: ReceiverApi) { api.forEach { this.validate() } }
}`
				if constructor {
					caller = strings.Replace(caller, "api.forEach", "Target(); api.forEach", 1)
				}
				provider := func(visibility, name, callback string) string {
					prefix := "package p\nclass OtherService\n"
					if name == "" {
						return prefix + "class ReceiverApi\n"
					}
					decl := visibility + " fun " + name + "(action: " + callback + ") {}"
					if extension {
						return prefix + "class ReceiverApi\n" + visibility + " fun ReceiverApi." + name + "(action: " + callback + ") {}\n"
					}
					return prefix + "class ReceiverApi { " + decl + " }\n"
				}
				r := newLifecycleRepo(t, tree{"Caller.kt": caller, "Provider.kt": provider("", "", ""), "Unrelated.kt": "package unrelated\nclass Unrelated"})
				check := func(t *testing.T, bound bool) {
					t.Helper()
					got := r.edgeState(t, "Caller.kt", "this.validate")
					if bound && !strings.Contains(got, "p.UserService.validate(function)") || !bound && !strings.Contains(got, ":: [/]") {
						t.Errorf("this.validate = %s, bound want %v", got, bound)
					}
					want := ""
					if bound {
						want = "p.UserService.validate"
					}
					if got := r.refTarget(t, "Caller.kt", "this.validate"); got != want {
						t.Errorf("this.validate reference = %q, want %q", got, want)
					}
					if constructor {
						var kind, target string
						if err := r.raw(t).QueryRow(`SELECT e.edge_kind,s.qualified_name FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols s ON s.id=e.dst_symbol_id WHERE e.repo_id=? AND f.path='Caller.kt' AND e.dst_name='Target'`, r.repoID).Scan(&kind, &target); err != nil || kind != "constructs" || target != "p.Target" {
							t.Errorf("constructor = %s %s, err=%v", kind, target, err)
						}
						if got := r.refTarget(t, "Caller.kt", "Target"); got != "p.Target" {
							t.Errorf("constructor reference = %q", got)
						}
					}
					assertKotlinReceiverHelperParity(t, r)
				}
				check(t, true)
				type helperStep struct {
					name, visibility, helper, callback string
					bound                              bool
				}
				steps := []helperStep{
					{"added", "", "forEach", "OtherService.() -> Unit", false},
					{"removed", "", "", "", true},
					{"private invisible", "private", "forEach", "OtherService.() -> Unit", true},
					{"made public", "public", "forEach", "OtherService.() -> Unit", false},
					{"made private", "private", "forEach", "OtherService.() -> Unit", true},
					{"public restored", "public", "forEach", "OtherService.() -> Unit", false},
					{"renamed", "public", "onEach", "OtherService.() -> Unit", true},
				}
				if !extension {
					steps = append(steps,
						helperStep{"ordinary signature", "public", "forEach", "() -> Unit", true},
						helperStep{"receiver signature", "public", "forEach", "OtherService.() -> Unit", false},
						helperStep{"ordinary signature restored", "public", "forEach", "() -> Unit", true})
				}
				for _, step := range steps {
					t.Run(step.name, func(t *testing.T) {
						r.write(t, "Provider.kt", provider(step.visibility, step.helper, step.callback))
						summary := r.update(t, "Provider.kt")
						if summary.FilesIndexed != 1 || summary.ResolveMode != "paths+names" || len(summary.ParserProfileLanguages) != 0 || len(summary.ResolverPolicyLanguages) != 0 {
							t.Fatalf("provider-only update = %+v", summary)
						}
						check(t, step.bound)
					})
				}
				// Start deletion from a correctly refused graph, not a stale old binding.
				r.write(t, "Provider.kt", provider("public", "forEach", "OtherService.() -> Unit"))
				r.update(t, "Provider.kt")
				check(t, false)
				r.remove(t, "Provider.kt")
				r.update(t, "Provider.kt")
				check(t, true)
				r.write(t, "Unrelated.kt", "package unrelated\nclass UnrelatedRenamed")
				summary := r.update(t, "Unrelated.kt")
				if summary.ResolveMode != "paths+names" || summary.ResolveCrossFileTargets != 0 || summary.ResolveCrossFile != nil && summary.ResolveCrossFile.InvalidatedBindings != 0 {
					t.Fatalf("unrelated update selected caller work: %+v", summary)
				}
				check(t, true)
			})
		}
	}
}

func TestKotlinReceiverHelperRemovedFromFreshRefusal(t *testing.T) {
	for _, extension := range []bool{false, true} {
		t.Run(fmt.Sprint(extension), func(t *testing.T) {
			provider := "package p\nclass OtherService\nclass ReceiverApi { fun forEach(action: OtherService.() -> Unit) {} }"
			if extension {
				provider = "package p\nclass OtherService\nclass ReceiverApi\nfun ReceiverApi.forEach(action: OtherService.() -> Unit) {}"
			}
			r := newLifecycleRepo(t, tree{"Caller.kt": "package p\nclass UserService { fun validate() {}\nfun run(api: ReceiverApi) { api.forEach { this.validate() } } }", "Provider.kt": provider})
			assertJVMReference(t, r, "Caller.kt", "this.validate", false)
			r.write(t, "Provider.kt", "package p\nclass OtherService\nclass ReceiverApi")
			r.update(t, "Provider.kt")
			if got := r.refTarget(t, "Caller.kt", "this.validate"); got != "p.UserService.validate" {
				t.Errorf("removed helper reference = %q", got)
			}
			assertKotlinReceiverHelperParity(t, r)
		})
	}
}

func TestKotlinReceiverHelperScopedResolutionUsesEvidence(t *testing.T) {
	r := newLifecycleRepo(t, tree{"Caller.kt": "package p\nclass UserService { fun validate() {}\nfun run(api: ReceiverApi) { api.forEach { this.validate() } } }", "Provider.kt": "package p\nclass OtherService\nclass ReceiverApi { fun forEach(action: OtherService.() -> Unit) {} }"})
	if _, err := r.store.ResolveEdgesForPathsAndNames(r.ctx, r.repoID, nil, []string{"this.validate"}); err != nil {
		t.Fatal(err)
	}
	if got := r.refTarget(t, "Caller.kt", "this.validate"); got != "" {
		t.Errorf("scoped helper decision reference = %q", got)
	}
	assertKotlinReceiverHelperParity(t, r)
}

// Compare semantic reference identities as well as edges; row IDs differ in a fresh database.
func assertKotlinReceiverHelperParity(t *testing.T, r *lifecycleRepo) {
	t.Helper()
	fresh := newLifecycleRepo(t, r.currentTree(t))
	if diff := projectionDiff(fresh.projection(t), r.projection(t)); diff != "" {
		t.Errorf("edge parity:\n%s", diff)
	}
	references := func(r *lifecycleRepo) []string {
		rows, err := r.raw(t).Query(`SELECT f.path,r.ref_kind,r.name,r.qualified_name,r.start_line,r.start_col,r.end_line,r.end_col,
COALESCE(tf.path,''),COALESCE(s.qualified_name,''),COALESCE(s.kind,''),COALESCE(c.qualified_name,'')
FROM references_tbl r JOIN files f ON f.id=r.file_id
LEFT JOIN symbols s ON s.id=r.symbol_id LEFT JOIN files tf ON tf.id=s.file_id
LEFT JOIN symbols c ON c.id=r.context_symbol_id WHERE r.repo_id=? AND f.is_deleted=0
ORDER BY f.path,r.start_line,r.start_col,r.ref_kind,r.name,r.qualified_name`, r.repoID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []string
		for rows.Next() {
			var file, kind, name, qname, targetFile, target, targetKind, owner string
			var line, col, endLine, endCol int
			if err := rows.Scan(&file, &kind, &name, &qname, &line, &col, &endLine, &endCol, &targetFile, &target, &targetKind, &owner); err != nil {
				t.Fatal(err)
			}
			result = append(result, fmt.Sprintf("%s:%d:%d-%d:%d %s %s %s => %s:%s(%s) owner=%s", file, line, col, endLine, endCol, kind, name, qname, targetFile, target, targetKind, owner))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if got, want := references(r), references(fresh); !reflect.DeepEqual(got, want) {
		t.Errorf("reference parity:\ngot %v\nwant %v", got, want)
	}
}

func TestKotlinReceiverHelperPolicyUpgrade(t *testing.T) {
	r := newRegisteredPolicyRepo(t, tree{
		"Caller.kt":   "package p\nclass UserService { fun validate() {}\nfun run(api: ReceiverApi) { api.forEach { this.validate() } } }",
		"Provider.kt": "package p\nclass OtherService\nclass ReceiverApi { fun forEach(action: OtherService.() -> Unit) {} }",
		"Peer.java":   "class Peer { void run() { helper(); } void helper() {} }",
		"main.go":     "package main\nfunc main() { helper() }\nfunc helper() {}",
	})
	r.bind(t, "Caller.kt", "this.validate", "p.UserService.validate")
	r.exec(t, `UPDATE settings SET value='1' WHERE key=?`, r.markerKey("kotlin"))
	r.watchLanguages(t, "java", "go")
	summary := r.update(t)
	if summary.FilesIndexed != 0 || summary.ResolveMode != "resolver_policy" || strings.Join(summary.ResolverPolicyLanguages, ",") != "kotlin" {
		t.Fatalf("upgrade = %+v", summary)
	}
	if got := r.refTarget(t, "Caller.kt", "this.validate"); got != "" {
		t.Errorf("stale reference survived upgrade: %s", got)
	}
	assertKotlinReceiverHelperParity(t, r)
	if r.touched(t) != 0 {
		t.Fatal("Kotlin upgrade rewrote another language")
	}
	if again := r.update(t); again.ResolveMode != "none" {
		t.Fatalf("steady state resolved again: %+v", again)
	}
}

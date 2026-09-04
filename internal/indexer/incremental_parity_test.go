//go:build cgo

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// Full-index / incremental parity across BOTH directions of a uniqueness change
// (P22.12).
//
// P22.8 closed the "a declaration arrived" direction: a binding made while a
// name was unique is invalidated when a second declaration shows up. The other
// direction was still open, because the indexer derived the batch's names from
// the NEW parse only -- so a name a file STOPPED declaring appeared in no batch,
// and an edge that was unresolved *because* that declaration made the name
// ambiguous stayed unresolved forever while a fresh index of the same tree
// resolved it.
//
// The oracle here is the P22.8 semantic projection (SemanticGraphForTest): file
// paths, qualified names, strategies and confidences, never row ids. Every test
// compares an incrementally-updated database against a fresh index of the exact
// same final tree.

// tree is a repository's complete source content, keyed by repo-relative path.
type tree map[string]string

// lifecycleRepo drives the real indexer over a real temp tree.
type lifecycleRepo struct {
	ctx    context.Context
	root   string
	store  *store.Store
	idx    *Indexer
	repoID int64
}

// Built on the tree-sitter adapters, which is why this file is cgo-only: the
// Python and C++ fixtures below need the adapters that actually emit call
// edges, the same reason cpp_callgraph_test.go carries the tag.
func lifecycleRegistry() *parser.Registry {
	return parser.NewRegistry(goparser.New(), tsparser.NewTypeScript(), tsparser.NewPython(), tsparser.NewCpp(), tsparser.NewJava(), tsparser.NewKotlin(), tsparser.NewRust())
}

func TestTypeScriptModuleAcceptanceLifecycle(t *testing.T) {
	base := tree{
		"a.ts": "export function foo() {}\n",
		"b.ts": "function run() { foo(); }\n",
	}
	r := newLifecycleRepo(t, base)
	if got := r.edgeState(t, "b.ts", "foo"); !strings.Contains(got, ":: [/") {
		t.Fatalf("unimported call bound: %s", got)
	}
	r.write(t, "b.ts", "import { foo } from \"./a\";\nfunction run() { foo(); }\n")
	r.update(t, "b.ts")
	if got := r.edgeState(t, "b.ts", "foo"); !strings.Contains(got, "a.ts:a.foo") {
		t.Fatalf("named import: %s", got)
	}
	r.assertFreshParity(t, "named import added")
	r.write(t, "b.ts", "function run() { foo(); }\n")
	r.update(t, "b.ts")
	if got := r.edgeState(t, "b.ts", "foo"); !strings.Contains(got, ":: [/") {
		t.Fatalf("removed import remained bound: %s", got)
	}
	r.assertFreshParity(t, "named import removed")
}

func TestTypeScriptNamespaceAndReExportParity(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a.ts": "export function foo() {}\n",
		"b.ts": "export { foo as bar } from \"./a\";\n",
		"c.ts": "import { bar } from \"./b\";\nfunction run() { bar(); }\n",
	})
	if got := r.edgeState(t, "c.ts", "bar"); !strings.Contains(got, "a.ts:a.foo") {
		t.Fatalf("aliased re-export: %s", got)
	}
	r.assertFreshParity(t, "aliased re-export")
	r.write(t, "c.ts", "import * as ns from \"./a\";\nfunction run() { ns.foo(); }\n")
	r.update(t, "c.ts")
	if got := r.edgeState(t, "c.ts", "ns.foo"); !strings.Contains(got, "a.ts:a.foo") {
		t.Fatalf("namespace import: %s", got)
	}
	r.assertFreshParity(t, "namespace import")
}

func TestTypeScriptModuleQuerySurface(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a.ts": "export function foo() {}\n",
		"b.ts": "import { foo as bar } from \"./a\";\nfunction run() { bar(); }\n",
	})
	result, err := query.New(r.store, nil).FindCalleesResult(r.ctx, r.repoID, "b.run", 0, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Callees) != 1 || result.Callees[0].QualifiedName != "a.foo" || filepath.ToSlash(result.Callees[0].FilePath) != "a.ts" {
		t.Fatalf("TypeScript query result = %+v", result.Callees)
	}
}

func TestTypeScriptModuleCandidateLifecycle(t *testing.T) {
	t.Run("target addition and deletion", func(t *testing.T) {
		r := newLifecycleRepo(t, tree{"b.ts": "import { foo } from \"./a\";\nfunction run() { foo(); }\n"})
		if got := r.edgeState(t, "b.ts", "foo"); !strings.Contains(got, ":: [/") {
			t.Fatalf("initial = %s", got)
		}
		r.write(t, "a.ts", "export function foo() {}\n")
		r.update(t, "a.ts")
		if got := r.edgeState(t, "b.ts", "foo"); !strings.Contains(got, "a.ts:a.foo") {
			t.Fatalf("addition = %s", got)
		}
		r.assertFreshParity(t, "target addition")
		r.remove(t, "a.ts")
		r.update(t, "a.ts")
		if got := r.edgeState(t, "b.ts", "foo"); !strings.Contains(got, ":: [/") {
			t.Fatalf("deletion = %s", got)
		}
		r.assertFreshParity(t, "target deletion")
	})
	t.Run("ambiguity transitions", func(t *testing.T) {
		r := newLifecycleRepo(t, tree{"a.ts": "export function foo() {}\n", "b.ts": "import { foo } from \"./a\";\nfunction run() { foo(); }\n"})
		if got := r.edgeState(t, "b.ts", "foo"); !strings.Contains(got, "a.ts:a.foo") {
			t.Fatalf("unique = %s", got)
		}
		r.write(t, "a.tsx", "export function foo() {}\n")
		r.update(t, "a.tsx")
		if got := r.edgeState(t, "b.ts", "foo"); !strings.Contains(got, ":: [/") {
			t.Fatalf("competitor addition = %s", got)
		}
		r.assertFreshParity(t, "ambiguity addition")
		r.remove(t, "a.tsx")
		r.update(t, "a.tsx")
		if got := r.edgeState(t, "b.ts", "foo"); !strings.Contains(got, "a.ts:a.foo") {
			t.Fatalf("competitor deletion = %s", got)
		}
		r.assertFreshParity(t, "ambiguity deletion")
	})
	t.Run("target and caller moves", func(t *testing.T) {
		r := newLifecycleRepo(t, tree{"a.ts": "export function foo() {}\n", "sub/b.ts": "import { foo } from \"../a\";\nfunction run() { foo(); }\n"})
		r.remove(t, "a.ts")
		r.write(t, "c.ts", "export function foo() {}\n")
		r.write(t, "sub/b.ts", "import { foo } from \"../c\";\nfunction run() { foo(); }\n")
		r.update(t)
		if got := r.edgeState(t, "sub/b.ts", "foo"); !strings.Contains(got, "c.ts:c.foo") {
			t.Fatalf("target move = %s", got)
		}
		r.remove(t, "sub/b.ts")
		r.write(t, "deep/b.ts", "import { foo } from \"../c\";\nfunction run() { foo(); }\n")
		r.update(t)
		if got := r.edgeState(t, "deep/b.ts", "foo"); !strings.Contains(got, "c.ts:c.foo") {
			t.Fatalf("caller move = %s", got)
		}
		r.assertFreshParity(t, "caller move")
	})
}

func TestTypeScriptModuleQueryAcceptance(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a.ts": "export function foo() {}\nexport function bar() {}\n",
		"b.ts": "export * from \"./a\";\n",
		"c.ts": "import { foo as alias } from \"./b\";\nimport * as ns from \"./a\";\nfunction run() { alias(); ns.bar(); obj.foo(); }\n",
	})
	q := query.New(r.store, nil)
	got, err := q.FindCalleesResult(r.ctx, r.repoID, "c.run", 0, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Callees) != 2 {
		t.Fatalf("query callees = %+v, want named+namespace only", got.Callees)
	}
	for _, callee := range got.Callees {
		if (callee.QualifiedName != "a.foo" && callee.QualifiedName != "a.bar") || filepath.ToSlash(callee.FilePath) != "a.ts" {
			t.Fatalf("query target = %+v", callee)
		}
	}
	r.write(t, "b.ts", "")
	r.update(t, "b.ts")
	got, err = q.FindCalleesResult(r.ctx, r.repoID, "c.run", 0, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Callees) != 1 || got.Callees[0].QualifiedName != "a.bar" {
		t.Fatalf("query after re-export removal = %+v", got.Callees)
	}
}

func TestTypeScriptModuleIsolationAcceptance(t *testing.T) {
	base := tree{"a.ts": "export function foo() {}\n", "b.ts": "import { foo } from \"./a\";\nfunction run() { foo(); }\n"}
	a := newLifecycleRepo(t, base)
	b := newLifecycleRepo(t, base)
	before := b.projection(t)
	a.write(t, "a.ts", "export function bar() {}\n")
	a.update(t, "a.ts")
	if got := a.edgeState(t, "b.ts", "foo"); !strings.Contains(got, ":: [/") {
		t.Fatalf("repo A stale target = %s", got)
	}
	if got := b.projection(t); !reflect.DeepEqual(got, before) {
		t.Fatalf("repo B changed: before=%v after=%v", before, got)
	}
	a.assertFreshParity(t, "repo A isolation")

	a.write(t, "unrelated/a.ts", "export function foo() {}\n")
	a.write(t, "unrelated/b.ts", "function run() { foo(); }\n")
	a.update(t)
	if got := a.edgeState(t, "b.ts", "foo"); !strings.Contains(got, ":: [/") {
		t.Fatalf("unrelated export rescued import = %s", got)
	}
	if got := a.edgeState(t, "unrelated/b.ts", "foo"); !strings.Contains(got, ":: [/") {
		t.Fatalf("unrelated unimported call bound = %s", got)
	}
}

func TestTypeScriptDefaultImportLifecycle(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a.ts": "export default function foo() {}\n",
		"b.ts": "import x from \"./a\";\nfunction run() { x(); }\n",
	})
	if got := r.edgeState(t, "b.ts", "x"); !strings.Contains(got, "a.ts:a.foo") {
		t.Fatalf("default import = %s", got)
	}
	r.write(t, "a.ts", "function foo() {}\n")
	r.update(t, "a.ts")
	if got := r.edgeState(t, "b.ts", "x"); !strings.Contains(got, ":: [/") {
		t.Fatalf("removed default export = %s", got)
	}
	r.assertFreshParity(t, "default export removal")
}

func TestTypeScriptModuleAcceptanceMatrix(t *testing.T) {
	tests := []struct {
		name, dst, want string
		base            tree
		mutate          func(*lifecycleRepo, *testing.T)
	}{
		{"A same-file", "foo", "b.ts:b.foo", tree{"b.ts": "function foo() {}\nfunction run() { foo(); }\n"}, func(*lifecycleRepo, *testing.T) {}},
		{"B import added", "foo", "a.ts:a.foo", tree{"a.ts": "export function foo() {}\n", "b.ts": "function run() { foo(); }\n"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "b.ts", "import { foo } from \"./a\";\nfunction run() { foo(); }\n")
			r.update(t, "b.ts")
		}},
		{"C import removed", "foo", ":: [", tree{"a.ts": "export function foo() {}\n", "b.ts": "import { foo } from \"./a\";\nfunction run() { foo(); }\n"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "b.ts", "function run() { foo(); }\n")
			r.update(t, "b.ts")
		}},
		{"D alias changed", "baz", "a.ts:a.foo", tree{"a.ts": "export function foo() {}\n", "b.ts": "import { foo as bar } from \"./a\";\nfunction run() { bar(); }\n"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "b.ts", "import { foo as baz } from \"./a\";\nfunction run() { baz(); }\n")
			r.update(t, "b.ts")
		}},
		{"E stale alias", "bar", ":: [", tree{"a.ts": "export function foo() {}\n", "b.ts": "import { foo as bar } from \"./a\";\nfunction run() { bar(); }\n"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "b.ts", "import { foo as baz } from \"./a\";\nfunction run() { bar(); }\n")
			r.update(t, "b.ts")
		}},
		{"F namespace", "mod.foo", "a.ts:a.foo", tree{"a.ts": "export function foo() {}\n", "b.ts": "import * as ns from \"./a\";\nfunction run() { ns.foo(); }\n"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "b.ts", "import * as mod from \"./a\";\nfunction run() { mod.foo(); }\n")
			r.update(t, "b.ts")
		}},
		{"G module path", "foo", "c.ts:c.foo", tree{"a.ts": "export function foo() {}\n", "b.ts": "import { foo } from \"./a\";\nfunction run() { foo(); }\n"}, func(r *lifecycleRepo, t *testing.T) {
			r.remove(t, "a.ts")
			r.write(t, "c.ts", "export function foo() {}\n")
			r.write(t, "b.ts", "import { foo } from \"./c\";\nfunction run() { foo(); }\n")
			r.update(t)
		}},
		{"H named re-export", "foo", "a.ts:a.foo", tree{"a.ts": "export function foo() {}\n", "b.ts": "", "c.ts": "import { foo } from \"./b\";\nfunction run() { foo(); }\n"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "b.ts", "export { foo } from \"./a\";\n")
			r.update(t, "b.ts")
		}},
		{"I aliased re-export", "baz", "a.ts:a.foo", tree{"a.ts": "export function foo() {}\n", "b.ts": "export { foo as baz } from \"./a\";\n", "c.ts": "import { baz } from \"./b\";\nfunction run() { baz(); }\n"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "b.ts", "export { foo as baz } from \"./a\";\n")
			r.update(t, "b.ts")
		}},
		{"J star competitor added", "foo", ":: [", tree{"a.ts": "export function foo() {}\n", "b.ts": "export function foo() {}\n", "barrel.ts": "export * from \"./a\";\n", "c.ts": "import { foo } from \"./barrel\";\nfunction run() { foo(); }\n"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "barrel.ts", "export * from \"./a\";\nexport * from \"./b\";\n")
			r.update(t, "barrel.ts")
		}},
		{"K star competitor removed", "foo", "a.ts:a.foo", tree{"a.ts": "export function foo() {}\n", "b.ts": "export function foo() {}\n", "barrel.ts": "export * from \"./a\";\nexport * from \"./b\";\n", "c.ts": "import { foo } from \"./barrel\";\nfunction run() { foo(); }\n"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "barrel.ts", "export * from \"./a\";\n")
			r.update(t, "barrel.ts")
		}},
		{"L target delete", "foo", ":: [", tree{"a.ts": "export function foo() {}\n", "b.ts": "import { foo } from \"./a\";\nfunction run() { foo(); }\n"}, func(r *lifecycleRepo, t *testing.T) { r.remove(t, "a.ts"); r.update(t) }},
		{"M cycle", "foo", ":: [", tree{"a.ts": "export { foo } from \"./b\";\n", "b.ts": "export { foo } from \"./a\";\n", "c.ts": "import { foo } from \"./a\";\nfunction run() { foo(); }\n"}, func(*lifecycleRepo, *testing.T) {}},
		{"N explicit over star", "foo", "a.ts:a.foo", tree{"a.ts": "export function foo() {}\n", "b.ts": "export function foo() {}\n", "barrel.ts": "export { foo } from \"./a\"; export * from \"./b\";\n", "c.ts": "import { foo } from \"./barrel\";\nfunction run() { foo(); }\n"}, func(*lifecycleRepo, *testing.T) {}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tc.base)
			tc.mutate(r, t)
			r.assertFreshParity(t, tc.name)
			got := r.edgeState(t, "c.ts", tc.dst)
			if got == "<no edge>" {
				got = r.edgeState(t, "b.ts", tc.dst)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("want %q, got %s; projection=%v", tc.want, got, r.projection(t))
			}
		})
	}
}

func TestKotlinPackageScopeProductSemantics(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a/Helper.kt": `package a
fun helper() {}`,
		"b/Helper.kt": `package b
fun helper() {}`,
		"a/Caller.kt": `package a
fun run() { helper() }`,
		"c/Caller.kt": `package c
fun run() { helper() }`,
	})
	if got := r.edgeState(t, "a/Caller.kt", "helper"); !strings.Contains(got, "a/Helper.kt:a.helper") {
		t.Fatalf("same-package Kotlin call = %s", got)
	}
	if got := r.edgeState(t, "c/Caller.kt", "helper"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("foreign-package Kotlin call = %s", got)
	}
}

func TestKotlinPackageScopeAcceptanceMatrix(t *testing.T) {
	cases := []struct {
		name   string
		dst    string
		base   tree
		mutate func(*lifecycleRepo, *testing.T)
		want   string
	}{
		{"A same-package", "helper", tree{"a.kt": "package a\nfun helper() {}", "caller.kt": "package a\nfun run() { helper() }"}, func(*lifecycleRepo, *testing.T) {}, "a.kt"},
		{"B import added", "helper", tree{"a.kt": "package a\nfun helper() {}", "caller.kt": "package b\nfun run() { helper() }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "caller.kt", "package b\nimport a.helper\nfun run() { helper() }")
			r.update(t, "caller.kt")
		}, "a.kt"},
		{"C import removed", "helper", tree{"a.kt": "package a\nfun helper() {}", "caller.kt": "package b\nimport a.helper\nfun run() { helper() }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "caller.kt", "package b\nfun run() { helper() }")
			r.update(t, "caller.kt")
		}, ":: [/]"},
		{"D alias changed", "new", tree{"a.kt": "package a\nfun helper() {}", "caller.kt": "package b\nimport a.helper as old\nfun run() { old() }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "caller.kt", "package b\nimport a.helper as new\nfun run() { new() }")
			r.update(t, "caller.kt")
		}, "a.kt"},
		{"E stale alias", "old", tree{"a.kt": "package a\nfun helper() {}", "caller.kt": "package b\nimport a.helper as old\nfun run() { old() }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "caller.kt", "package b\nimport a.helper as new\nfun run() { old() }")
			r.update(t, "caller.kt")
		}, ":: [/]"},
		{"F wildcard competitor", "helper", tree{"a.kt": "package a\nfun helper() {}", "caller.kt": "package c\nimport a.*\nfun run() { helper() }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "b.kt", "package b\nfun helper() {}")
			r.write(t, "caller.kt", "package c\nimport a.*\nimport b.*\nfun run() { helper() }")
			r.update(t, "b.kt", "caller.kt")
		}, ":: [/]"},
		{"G wildcard competitor removed", "helper", tree{"a.kt": "package a\nfun helper() {}", "b.kt": "package b\nfun helper() {}", "caller.kt": "package c\nimport a.*\nimport b.*\nfun run() { helper() }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "caller.kt", "package c\nimport a.*\nfun run() { helper() }")
			r.update(t, "caller.kt")
		}, "a.kt"},
		{"H package changed", "helper", tree{"a.kt": "package a\nfun helper() {}", "caller.kt": "package b\nimport a.helper\nfun run() { helper() }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a.kt", "package z\nfun helper() {}")
			r.update(t, "a.kt")
		}, ":: [/]"},
		{"I target renamed", "helper", tree{"a.kt": "package a\nfun helper() {}", "caller.kt": "package b\nimport a.helper\nfun run() { helper() }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a.kt", "package a\nfun renamed() {}")
			r.update(t, "a.kt")
		}, ":: [/]"},
		{"J target deleted", "helper", tree{"a.kt": "package a\nfun helper() {}", "caller.kt": "package b\nimport a.helper\nfun run() { helper() }"}, func(r *lifecycleRepo, t *testing.T) { r.remove(t, "a.kt"); r.update(t) }, ":: [/]"},
		{"K object member", "Util.run", tree{"util.kt": "package a\nobject Util { fun run() {} }", "caller.kt": "package b\nimport a.Util\nfun call() { Util.run() }"}, func(*lifecycleRepo, *testing.T) {}, "util.kt"},
		{"L owner moved", "Util.run", tree{"util.kt": "package a\nobject Util { fun run() {} }", "caller.kt": "package b\nimport a.Util\nfun call() { Util.run() }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "util.kt", "package z\nobject Util { fun run() {} }")
			r.update(t, "util.kt")
		}, ":: [/]"},
		{"M visibility changed", "helper", tree{"a.kt": "package a\nfun helper() {}", "caller.kt": "package b\nimport a.helper\nfun run() { helper() }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a.kt", "package a\nprivate fun helper() {}")
			r.update(t, "a.kt")
		}, ":: [/]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tc.base)
			tc.mutate(r, t)
			r.assertFreshParity(t, tc.name)
			if got := r.edgeState(t, "caller.kt", tc.dst); !strings.Contains(got, tc.want) {
				t.Fatalf("want %q, got %s; projection=%v", tc.want, got, r.projection(t))
			}
		})
	}
}

func TestKotlinPackageScopeQuerySurface(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a.kt":      "package a\nfun helper() {}",
		"caller.kt": "package b\nimport a.helper as h\nfun run() { h() }",
	})
	result, err := query.New(r.store, nil).FindCalleesResult(r.ctx, r.repoID, "b.run", 0, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Callees) != 1 || result.Callees[0].QualifiedName != "a.helper" {
		t.Fatalf("query alias result = %+v", result)
	}
	if got := r.edgeState(t, "caller.kt", "h"); !strings.Contains(got, "a.kt:a.helper [kotlin_import_scope/high]") {
		t.Fatalf("alias provenance = %s", got)
	}

	r = newLifecycleRepo(t, tree{
		"a.kt":      "package a\nfun helper() {}",
		"b.kt":      "package b\nfun helper() {}",
		"caller.kt": "package c\nimport a.*\nimport b.*\nfun run() { helper() }",
	})
	result, err = query.New(r.store, nil).FindCalleesResult(r.ctx, r.repoID, "c.run", 0, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Callees) != 0 {
		t.Fatalf("ambiguous query result = %+v", result)
	}
}

func TestKotlinPackageScopeMultiRepoIsolation(t *testing.T) {
	files := tree{
		"a.kt":      "package a\nfun helper() {}",
		"caller.kt": "package b\nimport a.helper\nfun run() { helper() }",
	}
	a := newLifecycleRepo(t, files)
	b := newLifecycleRepo(t, files)
	wantB := b.projection(t)
	a.write(t, "a.kt", "package z\nfun helper() {}")
	summary := a.update(t, "a.kt")
	if summary.ResolveMode == "repo" {
		t.Fatalf("local Kotlin update used repo-wide resolution: %+v", summary)
	}
	if got := b.projection(t); !equalStrings(got, wantB) {
		t.Fatalf("repo B changed after repo A update: got %v want %v", got, wantB)
	}
}

func TestKotlinDefaultPackageIsNotGlobal(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"helper.kt":  "fun helper() {}",
		"default.kt": "fun run() { helper() }",
		"named.kt":   "package named\nfun run() { helper() }",
	})
	if got := r.edgeState(t, "default.kt", "helper"); !strings.Contains(got, "helper.kt:helper") {
		t.Fatalf("default-package binding = %s", got)
	}
	if got := r.edgeState(t, "named.kt", "helper"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("named package saw default package = %s", got)
	}
	r.assertFreshParity(t, "default package")
}

func TestRustModuleScopeResolvesOnlyDeclaredModule(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"lib.rs":   `mod a; fn run() { crate::a::helper(); }`,
		"a.rs":     `pub fn helper() {}`,
		"other.rs": `pub fn helper() {}`,
	})
	if got := r.edgeState(t, "lib.rs", "crate::a::helper"); !strings.Contains(got, "a.rs:crate::a::helper") {
		t.Fatalf("crate-qualified Rust call = %s", got)
	}
}

func TestRustModuleScopeReexportsAndVisibility(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"lib.rs": `mod source; mod barrel; mod sibling; fn run() {
			crate::barrel::public_helper();
			crate::source::private_helper();
			crate::sibling::private_helper();
		}`,
		"source.rs":  `pub fn public_helper() {} fn private_helper() {}`,
		"barrel.rs":  `pub use crate::source::public_helper as public_helper;`,
		"sibling.rs": `fn private_helper() {}`,
	})
	if got := r.edgeState(t, "lib.rs", "crate::barrel::public_helper"); !strings.Contains(got, "source.rs:crate::source::public_helper") {
		t.Fatalf("re-export = %s", got)
	}
	for _, name := range []string{"crate::source::private_helper", "crate::sibling::private_helper"} {
		if got := r.edgeState(t, "lib.rs", name); !strings.Contains(got, ":: [/]") {
			t.Fatalf("inaccessible %s = %s", name, got)
		}
	}
}

func TestRustModuleScopeProductSemantics(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"src/lib.rs":     `mod source; mod b; mod c; mod s1; mod s2; mod barrel; mod cycle_a; mod cycle_b; fn run() { crate::c::public_helper(); crate::barrel::helper(); crate::cycle_a::x(); crate::ghost::helper(); }`,
		"src/source.rs":  `pub fn helper() {}`,
		"src/b.rs":       `pub use crate::source::helper as h;`,
		"src/c.rs":       `pub use crate::b::h as public_helper;`,
		"src/s1.rs":      `pub fn helper() {}`,
		"src/s2.rs":      `pub fn helper() {}`,
		"src/barrel.rs":  `pub use crate::s1::*; pub use crate::s2::*;`,
		"src/cycle_a.rs": `pub use crate::cycle_b::x;`,
		"src/cycle_b.rs": `pub use crate::cycle_a::x;`,
		"src/ghost.rs":   `pub fn helper() {}`,
		"alt/main.rs":    `mod util; fn run() { crate::util::run(); }`,
		"alt/util.rs":    `pub fn run() {}`,
	})
	if got := r.edgeState(t, "src/lib.rs", "crate::c::public_helper"); !strings.Contains(got, "source.rs:crate::source::helper") {
		t.Fatalf("multi-hop alias = %s", got)
	}
	for _, name := range []string{"crate::barrel::helper", "crate::cycle_a::x", "crate::ghost::helper"} {
		if got := r.edgeState(t, "src/lib.rs", name); !strings.Contains(got, ":: [/]") {
			t.Fatalf("Rust fail-closed %s = %s", name, got)
		}
	}
	if got := r.edgeState(t, "alt/main.rs", "crate::util::run"); !strings.Contains(got, "alt/util.rs:crate::util::run") {
		t.Fatalf("independent bin crate = %s", got)
	}
}

func TestRustModuleScopeKeepsCrateRootsIsolated(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"src/lib.rs":  `mod util; fn run() { crate::util::helper(); }`,
		"src/util.rs": `pub fn helper() {}`,
		"alt/main.rs": `mod util; fn run() { crate::util::helper(); }`,
		"alt/util.rs": `pub fn helper() {}`,
	})
	if got := r.edgeState(t, "src/lib.rs", "crate::util::helper"); !strings.Contains(got, "src/util.rs") {
		t.Fatalf("src crate binding = %s", got)
	}
	if got := r.edgeState(t, "alt/main.rs", "crate::util::helper"); !strings.Contains(got, "alt/util.rs") {
		t.Fatalf("alt crate binding = %s", got)
	}
}

func TestRustModuleScopeAcceptanceMatrix(t *testing.T) {
	tests := []struct {
		name   string
		base   tree
		mutate func(*lifecycleRepo, *testing.T)
		check  func(*lifecycleRepo, *testing.T)
	}{
		{"A same-module unique", tree{
			"lib.rs":    "mod a; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"caller.rs": "use crate::a::helper; fn run() { helper(); }",
		}, func(r *lifecycleRepo, t *testing.T) {}, func(r *lifecycleRepo, t *testing.T) {
			assertRustResolved(t, r, "caller.rs", "helper", "a.rs")
		}},
		{"B eligible competitor added", tree{
			"lib.rs":    "mod a; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"caller.rs": "use crate::a::*; fn run() { helper(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "b.rs", "pub fn helper() {}")
			r.write(t, "lib.rs", "mod a; mod b; mod caller;")
			r.update(t, "b.rs", "lib.rs")
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustResolved(t, r, "caller.rs", "helper", "a.rs")
		}},
		{"C competitor removed", tree{
			"lib.rs":    "mod a; mod b; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"b.rs":      "pub fn helper() {}",
			"caller.rs": "use crate::a::*; use crate::b::*; fn run() { helper(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			r.remove(t, "b.rs")
			r.write(t, "lib.rs", "mod a; mod caller;")
			r.update(t)
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustResolved(t, r, "caller.rs", "helper", "a.rs")
		}},
		{"D explicit use added", tree{
			"lib.rs":    "mod a; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"caller.rs": "fn run() { helper(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "caller.rs", "use crate::a::helper; fn run() { helper(); }")
			r.update(t, "caller.rs")
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustResolved(t, r, "caller.rs", "helper", "a.rs")
		}},
		{"E explicit use removed", tree{
			"lib.rs":    "mod a; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"caller.rs": "use crate::a::helper; fn run() { helper(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "caller.rs", "fn run() { helper(); }")
			r.update(t, "caller.rs")
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustUnresolved(t, r, "caller.rs", "helper")
		}},
		{"F alias changed", tree{
			"lib.rs":    "mod a; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"caller.rs": "use crate::a::helper as old; fn run() { old(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "caller.rs", "use crate::a::helper as new; fn run() { new(); }")
			r.update(t, "caller.rs")
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustResolved(t, r, "caller.rs", "new", "a.rs")
		}},
		{"F2 alias changed, stale call", tree{
			"lib.rs":    "mod a; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"caller.rs": "use crate::a::helper as old; fn run() { old(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "caller.rs", "use crate::a::helper as new; fn run() { old(); }")
			r.update(t, "caller.rs")
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustUnresolved(t, r, "caller.rs", "old")
		}},
		{"G target renamed", tree{
			"lib.rs":    "mod a; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"caller.rs": "use crate::a::helper; fn run() { helper(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a.rs", "pub fn renamed() {}")
			r.update(t, "a.rs")
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustUnresolved(t, r, "caller.rs", "helper")
		}},
		{"H target deleted", tree{
			"lib.rs":    "mod a; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"caller.rs": "use crate::a::helper; fn run() { helper(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			r.remove(t, "a.rs")
			r.update(t)
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustUnresolved(t, r, "caller.rs", "helper")
		}},
		{"I mod declaration removed", tree{
			"lib.rs":    "mod a; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"caller.rs": "use crate::a::helper; fn run() { helper(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "lib.rs", "mod caller;")
			r.update(t, "lib.rs")
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustUnresolved(t, r, "caller.rs", "helper")
		}},
		{"J module moved", tree{
			"lib.rs":    "mod a; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"caller.rs": "use crate::a::helper; fn run() { helper(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			r.remove(t, "a.rs")
			r.write(t, "b.rs", "pub fn helper() {}")
			r.write(t, "lib.rs", "mod b; mod caller;")
			r.write(t, "caller.rs", "use crate::b::helper; fn run() { helper(); }")
			r.update(t)
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustResolved(t, r, "caller.rs", "helper", "b.rs")
		}},
		{"K visibility restricted", tree{
			"lib.rs":    "mod a; mod caller;",
			"a.rs":      "pub fn helper() {}",
			"caller.rs": "use crate::a::helper; fn run() { helper(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a.rs", "fn helper() {}")
			r.update(t, "a.rs")
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustUnresolved(t, r, "caller.rs", "helper")
		}},
		{"L pub-use add/remove", tree{
			"lib.rs":    "mod source; mod barrel; mod caller;",
			"source.rs": "pub fn helper() {}",
			"barrel.rs": "",
			"caller.rs": "use crate::barrel::helper; fn run() { helper(); }",
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustUnresolved(t, r, "caller.rs", "helper")
			r.write(t, "barrel.rs", "pub use crate::source::helper;")
			r.update(t, "barrel.rs")
			assertRustResolved(t, r, "caller.rs", "helper", "source.rs")
			r.write(t, "barrel.rs", "")
			r.update(t, "barrel.rs")
		}, func(r *lifecycleRepo, t *testing.T) {
			assertRustUnresolved(t, r, "caller.rs", "helper")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tc.base)
			tc.mutate(r, t)
			r.assertFreshParity(t, tc.name)
			tc.check(r, t)
		})
	}
}

func assertRustResolved(t *testing.T, r *lifecycleRepo, src, name, target string) {
	t.Helper()
	if got := r.edgeState(t, src, name); !strings.Contains(got, target) {
		t.Fatalf("%s %s: want target %q, got %s", src, name, target, got)
	}
}

func assertRustUnresolved(t *testing.T, r *lifecycleRepo, src, name string) {
	t.Helper()
	if got := r.edgeState(t, src, name); !strings.Contains(got, ":: [/") {
		t.Fatalf("%s %s: want unresolved, got %s", src, name, got)
	}
}

func newLifecycleRepo(t *testing.T, files tree) *lifecycleRepo {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	s, err := store.Open(filepath.Join(t.TempDir(), "codegraph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &lifecycleRepo{ctx: ctx, root: root, store: s, idx: New(s, lifecycleRegistry(), nil)}
	for rel, content := range files {
		r.write(t, rel, content)
	}
	if _, err := r.idx.Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatalf("upsert repo: %v", err)
	}
	r.repoID = repo.ID
	return r
}

func (r *lifecycleRepo) write(t *testing.T, rel, content string) {
	t.Helper()
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func (r *lifecycleRepo) remove(t *testing.T, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(r.root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("remove %s: %v", rel, err)
	}
}

// update runs an ordinary incremental update. With no paths it is a whole-tree
// update scan (the `codegraph update` shape, deletions via MarkMissingDeleted);
// with paths it is the path-scoped shape (deletions via MarkFilesDeletedBatch).
func (r *lifecycleRepo) update(t *testing.T, paths ...string) store.ScanSummary {
	t.Helper()
	summary, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Paths: paths})
	if err != nil {
		t.Fatalf("update(%v): %v", paths, err)
	}
	return summary
}

// projection renders the repository's relationships as sorted, id-free text --
// the P22.8 semantic projection, rebuilt here on the exported export API
// because the store's own helper is package-private to its tests.
//
// Every field is a semantic identity (file path, qualified name, source line,
// strategy, confidence). No row ids and no timestamps, so two databases that
// hold the same graph render identically however they were built.
func (r *lifecycleRepo) projection(t *testing.T) []string {
	t.Helper()
	symbols, err := r.store.ExportSymbolsPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatalf("export symbols: %v", err)
	}
	symbolFile := make(map[int64]string, len(symbols))
	for _, sym := range symbols {
		symbolFile[sym.ID] = filepath.ToSlash(sym.FilePath)
	}
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatalf("export edges: %v", err)
	}
	lines := make([]string, 0, len(edges))
	for _, e := range edges {
		dst := "::"
		if e.DstSymbolID != nil {
			dst = symbolFile[*e.DstSymbolID] + ":" + e.DstQualifiedName
		}
		lines = append(lines, "edge "+filepath.ToSlash(e.FilePath)+":"+e.SrcQualifiedName+
			" -"+e.Kind+`-> "`+e.DstName+`" => `+dst+
			" ["+e.ResolutionStrategy+"/"+e.ResolutionConfidence+"]")
	}
	sort.Strings(lines)
	return lines
}

// currentTree reads the repository back off disk, so a fresh index can be run
// over exactly what the incremental run ended up with.
func (r *lifecycleRepo) currentTree(t *testing.T) tree {
	t.Helper()
	files := tree{}
	err := filepath.WalkDir(r.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(r.root, path)
		if relErr != nil {
			return relErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("read tree: %v", err)
	}
	return files
}

// freshProjection indexes the given tree from scratch in its own database.
func freshProjection(t *testing.T, files tree) []string {
	t.Helper()
	return newLifecycleRepo(t, files).projection(t)
}

// assertFreshParity is the whole point: the updated database must project
// exactly what a fresh index of the same final tree projects.
func (r *lifecycleRepo) assertFreshParity(t *testing.T, step string) {
	t.Helper()
	got := r.projection(t)
	want := freshProjection(t, r.currentTree(t))
	if diff := projectionDiff(want, got); diff != "" {
		t.Fatalf("%s: update diverges from a fresh index of the same tree:\n%s", step, diff)
	}
}

func projectionDiff(want, got []string) string {
	inWant := map[string]int{}
	for _, line := range want {
		inWant[line]++
	}
	var only []string
	for _, line := range got {
		if inWant[line] > 0 {
			inWant[line]--
			continue
		}
		only = append(only, "update-only: "+line)
	}
	for line, n := range inWant {
		for i := 0; i < n; i++ {
			only = append(only, "fresh-only:  "+line)
		}
	}
	sort.Strings(only)
	return strings.Join(only, "\n")
}

// edgeState renders one caller's outgoing edge for a dst_name, so a test can
// assert the decision itself rather than only parity.
func (r *lifecycleRepo) edgeState(t *testing.T, srcPath, dstName string) string {
	t.Helper()
	prefix := "edge " + srcPath + ":"
	needle := `-calls-> "` + dstName + `"`
	for _, line := range r.projection(t) {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, needle) {
			return line[strings.Index(line, needle)+len(needle):]
		}
	}
	return "<no edge>"
}

// -- removed declaration ------------------------------------------------------

const pyCaller = "from helpers_a import *\n\n\ndef run():\n    return Foo()\n"

func ambiguousPythonTree() tree {
	return tree{
		"helpers_a.py": "def Foo():\n    return 1\n",
		"helpers_b.py": "def Foo():\n    return 2\n",
		"main.py":      pyCaller,
	}
}

// The reproduction: two declarations make the bare call undecidable, removing
// one makes it decidable again, and an ordinary update must notice.
func TestUpdateReconsidersEdgeAfterCompetingDeclarationRemoved(t *testing.T) {
	r := newLifecycleRepo(t, ambiguousPythonTree())
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("initial state: want unresolved, got %s", got)
	}
	r.assertFreshParity(t, "initial")

	// helpers_b keeps existing; it just stops declaring Foo.
	r.write(t, "helpers_b.py", "def Bar():\n    return 2\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("after removing the competing declaration: want a binding to helpers_a.Foo, got %s", got)
	}
	r.assertFreshParity(t, "after removal")
}

// Removal is not, by itself, evidence that the survivor wins: a third
// declaration keeps the name undecidable.
func TestUpdateKeepsEdgeUnresolvedWhenAnotherDeclarationRemains(t *testing.T) {
	files := ambiguousPythonTree()
	files["helpers_c.py"] = "def Foo():\n    return 3\n"
	r := newLifecycleRepo(t, files)

	r.write(t, "helpers_b.py", "def Bar():\n    return 2\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("two declarations still remain; want unresolved, got %s", got)
	}
	r.assertFreshParity(t, "after removal with a third declaration")
}

// Deleting the whole file must mean the same thing as deleting the declaration,
// on both update shapes: a path-scoped update (MarkFilesDeletedBatch) and a
// whole-tree update scan (MarkMissingDeleted).
func TestUpdateReconsidersEdgeAfterCompetingFileDeleted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string
	}{
		{"whole-tree update scan", nil},
		{"path-scoped update", []string{"helpers_b.py"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, ambiguousPythonTree())
			r.remove(t, "helpers_b.py")
			summary := r.update(t, tc.paths...)

			// The summary has to admit that edge resolution ran: a deletion-only
			// run used to report `test_links`, and a mode that denies the work
			// is how this class stayed invisible.
			if summary.ResolveMode != "paths+names" {
				t.Fatalf("ResolveMode = %q, want paths+names", summary.ResolveMode)
			}
			if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
				t.Fatalf("after deleting the competing file: want a binding to helpers_a.Foo, got %s", got)
			}
			r.assertFreshParity(t, "after file deletion")
		})
	}
}

// A rename removes one name and adds another in the same write. Both sides have
// to reach the invalidation/reconsideration pass.
func TestUpdateCarriesBothSidesOfARename(t *testing.T) {
	files := ambiguousPythonTree()
	// A second caller, for the name the rename introduces. It is unresolved to
	// begin with (nothing declares Bar) and must bind after the rename.
	files["other.py"] = "from helpers_b import *\n\n\ndef go():\n    return Bar()\n"
	r := newLifecycleRepo(t, files)
	if got := r.edgeState(t, "other.py", "Bar"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("initial Bar: want unresolved, got %s", got)
	}

	r.write(t, "helpers_b.py", "def Bar():\n    return 2\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("rename removed the competing Foo; want a binding, got %s", got)
	}
	if got := r.edgeState(t, "other.py", "Bar"); !strings.Contains(got, "helpers_b.py") {
		t.Fatalf("rename added Bar; want a binding, got %s", got)
	}
	r.assertFreshParity(t, "after rename")
}

// The direction P22.8 fixed, kept as a direct regression: P22.12 must not fix
// removal by breaking addition.
func TestUpdateInvalidatesWhenCompetingDeclarationArrives(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"helpers_a.py": "def Foo():\n    return 1\n",
		"main.py":      pyCaller,
	})
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("initial state: want a binding, got %s", got)
	}

	r.write(t, "helpers_b.py", "def Foo():\n    return 2\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("a second declaration arrived; want unresolved, got %s", got)
	}
	r.assertFreshParity(t, "after arrival")
}

// Resolved and unresolved must be able to alternate indefinitely, with no
// sticky state in either direction, entirely through incremental updates.
func TestAmbiguityOscillatesWithFreshParity(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"helpers_a.py": "def Foo():\n    return 1\n",
		"main.py":      pyCaller,
	})
	competing := "def Foo():\n    return 2\n"
	absent := "def Bar():\n    return 2\n"

	for round := 0; round < 3; round++ {
		if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
			t.Fatalf("round %d: one declaration, want a binding, got %s", round, got)
		}
		r.assertFreshParity(t, "one declaration")

		r.write(t, "helpers_b.py", competing)
		r.update(t)
		if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
			t.Fatalf("round %d: two declarations, want unresolved, got %s", round, got)
		}
		r.assertFreshParity(t, "two declarations")

		r.write(t, "helpers_b.py", absent)
		r.update(t)
	}
}

// Deleting and recreating the destination itself, with a competitor appearing
// in between. Every step must match a fresh index of that exact tree.
func TestTargetDeleteAndRecreateLifecycle(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"helpers_a.py": "def Foo():\n    return 1\n",
		"main.py":      pyCaller,
	})
	r.assertFreshParity(t, "initial")

	r.remove(t, "helpers_a.py")
	r.update(t)
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("destination deleted; want unresolved, got %s", got)
	}
	r.assertFreshParity(t, "destination deleted")

	r.write(t, "helpers_b.py", "def Foo():\n    return 2\n")
	r.update(t)
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_b.py") {
		t.Fatalf("a new sole declaration; want a binding to helpers_b.Foo, got %s", got)
	}
	r.assertFreshParity(t, "replacement declared")

	r.write(t, "helpers_a.py", "def Foo():\n    return 1\n")
	r.update(t)
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("both declarations back; want unresolved, got %s", got)
	}
	r.assertFreshParity(t, "both declared")

	r.remove(t, "helpers_b.py")
	r.update(t)
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("one declaration left; want a binding to helpers_a.Foo, got %s", got)
	}
	r.assertFreshParity(t, "back to one")
}

// -- gates that removal must not weaken ---------------------------------------

// P22.6: a bare Go call is answered by its own package. Removing a package-local
// declaration must leave the edge unresolved, never hand it to an unrelated
// package's symbol of the same name.
func TestRemovedGoDeclarationDoesNotEscapePackageScope(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"go.mod":         "module example.com/proj\n",
		"pkga/a.go":      "package pkga\n\nfunc Foo() int { return 1 }\n",
		"pkga/dup.go":    "package pkga\n\nfunc Foo2() int { return Foo() }\n",
		"pkgb/b.go":      "package pkgb\n\nfunc Foo() int { return 2 }\n",
		"pkgb/caller.go": "package pkgb\n\nfunc Call() int { return Foo() }\n",
	})
	r.assertFreshParity(t, "initial")

	// pkgb stops declaring Foo. pkga's Foo is now the repository's only Foo, and
	// must still not be reachable from pkgb.
	r.write(t, "pkgb/b.go", "package pkgb\n\nfunc Bar() int { return 2 }\n")
	r.update(t)

	if got := r.edgeState(t, filepath.ToSlash(filepath.Join("pkgb", "caller.go")), "Foo"); strings.Contains(got, "pkga") {
		t.Fatalf("a removed package-local Foo let pkgb bind pkga's: %s", got)
	}
	r.assertFreshParity(t, "after package-local removal")
}

// P2/P22.7: an old name is a trigger key, not permission to widen candidate
// semantics. Removing a Python Foo must not wake a Go Foo for a Python caller.
func TestRemovedNameDoesNotCrossLanguages(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"go.mod":       "module example.com/proj\n",
		"pkga/a.go":    "package pkga\n\nfunc Foo() int { return 1 }\n",
		"helpers_a.py": "def Foo():\n    return 1\n",
		"helpers_b.py": "def Foo():\n    return 2\n",
		"main.py":      pyCaller,
	})
	r.write(t, "helpers_b.py", "def Bar():\n    return 2\n")
	r.update(t)

	got := r.edgeState(t, "main.py", "Foo")
	if strings.Contains(got, "pkga") {
		t.Fatalf("a Python call bound a Go symbol after a removal: %s", got)
	}
	r.assertFreshParity(t, "after cross-language removal")
}

// P22.11: a receiver-qualified C++ spelling matches nothing at any evidence
// level, and a removal that makes a bare `size` unique must not change that.
func TestRemovedDeclarationDoesNotWakeCppReceiverCall(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a.cc":      "struct Holder {\n  int size() const { return 1; }\n};\n",
		"b.cc":      "struct Other {\n  int size() const { return 2; }\n};\n",
		"caller.cc": "#include <vector>\nint use(const std::vector<int>& v) {\n  return v.size();\n}\n",
	})
	r.assertFreshParity(t, "initial")

	r.write(t, "b.cc", "struct Other {\n  int capacity() const { return 2; }\n};\n")
	r.update(t)

	if got := r.edgeState(t, "caller.cc", "v.size"); strings.Contains(got, ".cc:") {
		t.Fatalf("a receiver-qualified call bound a project symbol after a removal: %s", got)
	}
	r.assertFreshParity(t, "after removal")
}

// P7: reconsideration goes through the resolver, so the production/test
// preference still applies to a name a removal just woke up.
func TestRemovedDeclarationKeepsTestShadowPreference(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"helpers_a.py":      "def Foo():\n    return 1\n",
		"helpers_b.py":      "def Foo():\n    return 2\n",
		"test_helpers.py":   "def Foo():\n    return 3\n",
		"main.py":           pyCaller,
		"tests/test_use.py": "from helpers_a import *\n\n\ndef test_run():\n    return Foo()\n",
	})
	r.write(t, "helpers_b.py", "def Bar():\n    return 2\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("a production caller facing one production and one test Foo must bind the production one, got %s", got)
	}
	if got := r.edgeState(t, filepath.ToSlash(filepath.Join("tests", "test_use.py")), "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("a test caller facing two candidates must stay unresolved, got %s", got)
	}
	r.assertFreshParity(t, "after removal with a test declaration")
}

// P22.9: uniqueness and visibility are separate questions. Removing a competing
// class declaration makes the name unique, but a caller that neither declares
// nor imports the survivor still may not bind it.
func TestRemovedDeclarationDoesNotBypassTypeScope(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"models_a.py": "class Widget:\n    pass\n",
		"models_b.py": "class Widget:\n    pass\n",
		// No import of either module: repository-global uniqueness is not scope
		// evidence for a class name.
		"main.py": "def run():\n    return Widget()\n",
	})
	r.assertFreshParity(t, "initial")

	r.write(t, "models_b.py", "class Gadget:\n    pass\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Widget"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("an unimported class bound after a removal made it unique: %s", got)
	}
	r.assertFreshParity(t, "after removal")
}

// The positive control for the test above: with the import present, the removal
// is exactly what makes the edge decidable, and the update must notice.
func TestRemovedDeclarationResolvesImportedTypeTarget(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"models_a.py": "class Widget:\n    pass\n",
		"models_b.py": "class Widget:\n    pass\n",
		"main.py":     "from models_a import Widget\n\n\ndef run():\n    return Widget()\n",
	})
	if got := r.edgeState(t, "main.py", "Widget"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("initial state: want unresolved, got %s", got)
	}

	r.write(t, "models_b.py", "class Gadget:\n    pass\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Widget"); !strings.Contains(got, "models_a.py") {
		t.Fatalf("an imported, now-unique class must bind: %s", got)
	}
	r.assertFreshParity(t, "after removal")
}

// A run whose only change is a deletion has no changed path to dispatch on. It
// must still reconsider, on the full-index shape as well as the update shapes --
// otherwise `codegraph index` over a tree that only lost a file would leave the
// graph where the deletion found it.
func TestFullIndexAfterDeletionOnlyStillReconsiders(t *testing.T) {
	r := newLifecycleRepo(t, ambiguousPythonTree())
	r.remove(t, "helpers_b.py")
	if _, err := r.idx.Index(r.ctx, Options{RepoRoot: r.root, ScanKind: "index"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("deletion-only full index: want a binding to helpers_a.Foo, got %s", got)
	}
	r.assertFreshParity(t, "deletion-only full index")
}

func TestJavaDotSuffixFreshParityAcrossTargetStates(t *testing.T) {
	const caller = "class Caller { void run() { A.B.C.m(); } }\n"
	const target = "class A { class B { class C { static void m() {} } } }\n"
	const renamed = "class X { class Y { class Z { static void m() {} } } }\n"

	r := newLifecycleRepo(t, tree{"Caller.java": caller, "Target.java": target})
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); !strings.Contains(got, "Target.java") || !strings.Contains(got, "java_package_scope/high") {
		t.Fatalf("unique dot-suffix state: %s", got)
	}
	r.assertFreshParity(t, "unique")

	r.write(t, "Competitor.java", target)
	r.update(t, "Competitor.java")
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("competitor should make dot-suffix ambiguous: %s", got)
	}
	r.assertFreshParity(t, "competitor added")

	r.remove(t, "Competitor.java")
	r.update(t, "Competitor.java")
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); !strings.Contains(got, "Target.java") || !strings.Contains(got, "java_package_scope/high") {
		t.Fatalf("competitor removal should recover dot-suffix: %s", got)
	}
	r.assertFreshParity(t, "competitor removed")

	r.remove(t, "Target.java")
	r.update(t, "Target.java")
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); strings.Contains(got, "Target.java") {
		t.Fatalf("deleted target survived: %s", got)
	}
	r.assertFreshParity(t, "target deleted")

	r.write(t, "Target.java", target)
	r.update(t, "Target.java")
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); !strings.Contains(got, "Target.java") {
		t.Fatalf("target recreation should restore dot-suffix: %s", got)
	}

	r.write(t, "Target.java", renamed)
	r.update(t, "Target.java")
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); strings.Contains(got, "Target.java") {
		t.Fatalf("renamed target retained old binding: %s", got)
	}
	r.assertFreshParity(t, "target renamed")
}

func TestJavaPackageConstructorIncrementalMatrix(t *testing.T) {
	tests := []struct {
		name   string
		base   tree
		mutate func(*lifecycleRepo, *testing.T)
		want   string
	}{
		{"A same-class", tree{"A.java": "class A { void helper() {}\nvoid run() { helper(); }\n}"}, func(*lifecycleRepo, *testing.T) {}, "A.java"},
		{"B same-package", tree{"a/Foo.java": `package a; public class Foo { public static void run() {} }`, "a/C.java": "package a; class C { void x() {\n Foo.run();\n } }"}, func(*lifecycleRepo, *testing.T) {}, "a/Foo.java"},
		{"C import added", tree{"a/Foo.java": `package a; public class Foo { public static void run() {} }`, "b/C.java": "package b; class C { void x() {\n Foo.run();\n } }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "b/C.java", "package b; import a.Foo; class C { void x() {\n Foo.run();\n } }")
			r.update(t, "b/C.java")
		}, "a/Foo.java"},
		{"D import removed", tree{"a/Foo.java": `package a; public class Foo { public static void run() {} }`, "b/C.java": "package b; import a.Foo; class C { void x() {\n Foo.run();\n } }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "b/C.java", "package b; class C { void x() {\n Foo.run();\n } }")
			r.update(t, "b/C.java")
		}, ":: [/]"},
		{"E wildcard competitor", tree{"a/Foo.java": `package a; public class Foo { public static void run() {} }`, "b/C.java": "package b; import a.*; class C { void x() {\n Foo.run();\n } }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "c/Foo.java", `package c; public class Foo { public static void run() {} }`)
			r.write(t, "b/C.java", "package b; import a.*; import c.*; class C { void x() {\n Foo.run();\n } }")
			r.update(t, "c/Foo.java", "b/C.java")
		}, ":: [/]"},
		{"F wildcard competitor removed", tree{"a/Foo.java": `package a; public class Foo { public static void run() {} }`, "c/Foo.java": `package c; public class Foo { public static void run() {} }`, "b/C.java": "package b; import a.*; import c.*; class C { void x() {\n Foo.run();\n } }"}, func(r *lifecycleRepo, t *testing.T) { r.remove(t, "c/Foo.java"); r.update(t, "c/Foo.java") }, "a/Foo.java"},
		{"G static lifecycle", tree{"a/U.java": `package a; public class U { public void run() {} }`, "b/C.java": `package b; import static a.U.run; class C { void x() { run(); } }`}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a/U.java", `package a; public class U { public static void run() {} }`)
			r.update(t, "a/U.java")
		}, "a/U.java"},
		{"H package changed", tree{"a/Foo.java": `package a; public class Foo { public static void run() {} }`, "a/C.java": `package a; class C { void x() { Foo.run(); } }`}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a/Foo.java", `package b; public class Foo { public static void run() {} }`)
			r.update(t, "a/Foo.java")
		}, ":: [/]"},
		{"I type renamed", tree{"a/Foo.java": `package a; public class Foo { public static void run() {} }`, "a/C.java": `package a; class C { void x() { Foo.run(); } }`}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a/Foo.java", `package a; public class Bar { public static void run() {} }`)
			r.update(t, "a/Foo.java")
		}, ":: [/]"},
		{"J member deleted", tree{"a/Foo.java": `package a; public class Foo { public static void run() {} }`, "a/C.java": `package a; class C { void x() { Foo.run(); } }`}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a/Foo.java", `package a; public class Foo { }`)
			r.update(t, "a/Foo.java")
		}, ":: [/]"},
		{"K constructor added", tree{"a/Foo.java": `package a; public class Foo { }`, "a/C.java": "package a; class C { void x() {\n new Foo();\n } }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a/Foo.java", `package a; public class Foo { public Foo() {} }`)
			r.update(t, "a/Foo.java")
		}, "a/Foo.java"},
		{"L constructor overload ambiguity", tree{"a/Foo.java": `package a; public class Foo { public Foo(int x) {} }`, "a/C.java": "package a; class C { void x() {\n new Foo(1);\n } }"}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a/Foo.java", `package a; public class Foo { public Foo(int x) {} public Foo(String x) {} }`)
			r.update(t, "a/Foo.java")
		}, ":: [/]"},
		{"M visibility changed", tree{"a/Foo.java": `package a; public class Foo { public static void run() {} }`, "b/C.java": `package b; import a.Foo; class C { void x() { Foo.run(); } }`}, func(r *lifecycleRepo, t *testing.T) {
			r.write(t, "a/Foo.java", `package a; public class Foo { static void run() {} }`)
			r.update(t, "a/Foo.java")
		}, ":: [/]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tc.base)
			tc.mutate(r, t)
			r.assertFreshParity(t, tc.name)
			got := r.projection(t)
			matched := false
			for _, line := range got {
				if strings.Contains(line, `-calls->`) || strings.Contains(line, `-constructs->`) {
					if strings.Contains(line, tc.want) {
						matched = true
					}
				}
			}
			if !matched {
				t.Fatalf("%s: expected %q in %v", tc.name, tc.want, got)
			}
		})
	}
}

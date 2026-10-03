package indexer

import (
	"strings"
	"testing"
)

// rustGlobShadowCases pin Rust's name precedence for a bare call in a module
// that also has glob imports: an item declared in the module, or a name
// imported explicitly, shadows a glob import of the same name; two globs that
// supply the same name are ambiguous only where it is used. Each case was
// checked against rustc with deliberately conflicting return types.
var rustGlobShadowCases = []struct {
	name   string
	files  tree
	target string // "" means the call must stay unresolved
}{
	{"own fn after glob", tree{
		"lib.rs": "mod a; use a::*; pub fn f() {}\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "lib.rs:crate::f"},
	{"own fn before glob", tree{
		"lib.rs": "mod a; pub fn f() {} use a::*;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "lib.rs:crate::f"},
	// The own item wins in Rust. Private items do not bind today, so the call
	// stays unresolved; what must never happen is the glob winning.
	{"private own fn", tree{
		"lib.rs": "mod a; use a::*; fn f() {}\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, ""},
	{"private own fn, glob re-export", tree{
		"lib.rs": "mod a; pub use a::*; fn f() {}\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, ""},
	{"explicit use after glob", tree{
		"lib.rs": "mod a; mod b; use a::*; use b::f;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub fn f() {}",
	}, "b.rs:crate::b::f"},
	{"explicit use before glob", tree{
		"lib.rs": "mod a; mod b; use b::f; use a::*;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub fn f() {}",
	}, "b.rs:crate::b::f"},
	{"glob only", tree{
		"lib.rs": "mod a; use a::*;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "a.rs:crate::a::f"},
	{"two globs", tree{
		"lib.rs": "mod a; mod b; use a::*; use b::*;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub fn f() {}",
	}, ""},
	// A trait lives only in the type namespace, so it does not shadow a
	// glob-imported function.
	{"own trait", tree{
		"lib.rs": "mod a; use a::*; pub trait f {}\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "a.rs:crate::a::f"},
	// A tuple struct shadows the function and a braced struct does not; the
	// parser does not record which form it saw, so the call fails closed.
	{"own struct", tree{
		"lib.rs": "mod a; use a::*; pub struct f(u8);\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, ""},
	// An item of a child module is not declared in the caller's module.
	{"child module fn", tree{
		"lib.rs": "mod a; mod c; use a::*;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"c.rs":   "pub fn f() {}",
	}, "a.rs:crate::a::f"},
}

func assertRustCallTarget(t *testing.T, r *lifecycleRepo, step, target string) {
	t.Helper()
	got := r.edgeState(t, "lib.rs", "f")
	if target == "" {
		if !strings.Contains(got, ":: [/") {
			t.Fatalf("%s: want unresolved, got %s", step, got)
		}
		return
	}
	if !strings.Contains(got, target+"(") {
		t.Fatalf("%s: want %s, got %s", step, target, got)
	}
}

func TestRustGlobImportShadowing(t *testing.T) {
	for _, tc := range rustGlobShadowCases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tc.files)
			assertRustCallTarget(t, r, "fresh", tc.target)

			// Every resolver entrypoint must reach the same decision from an
			// unresolved edge.
			for _, ep := range []struct {
				name string
				run  func() error
			}{
				{"repo", func() error { _, err := r.store.ResolveEdges(r.ctx, r.repoID); return err }},
				{"paths", func() error { return r.store.ResolveEdgesForPaths(r.ctx, r.repoID, []string{"lib.rs"}) }},
				{"names", func() error {
					_, err := r.store.ResolveEdgesForNamesWithStats(r.ctx, r.repoID, []string{"f"})
					return err
				}},
				{"paths+names", func() error {
					_, err := r.store.ResolveEdgesForPathsAndNames(r.ctx, r.repoID, []string{"lib.rs"}, []string{"f"})
					return err
				}},
			} {
				if _, err := r.raw(t).ExecContext(r.ctx, `UPDATE edges SET dst_symbol_id=NULL,resolution_strategy='',resolution_confidence='' WHERE repo_id=? AND dst_name='f'`, r.repoID); err != nil {
					t.Fatal(err)
				}
				if err := ep.run(); err != nil {
					t.Fatalf("%s: %v", ep.name, err)
				}
				assertRustCallTarget(t, r, ep.name, tc.target)
			}
		})
	}
}

// TestRustGlobShadowingFollowsIncrementalChanges adds and removes the own item
// that shadows a glob, through both update shapes, and requires every step to
// match a fresh index of the same tree.
func TestRustGlobShadowingFollowsIncrementalChanges(t *testing.T) {
	const globOnly = "mod a; use a::*;\npub fn caller() {\n    f();\n}\n"
	const withOwn = "mod a; use a::*; pub fn f() {}\npub fn caller() {\n    f();\n}\n"
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{"lib.rs": globOnly, "a.rs": "pub fn f() {}"})
		update := func() {
			if scoped {
				r.update(t, "lib.rs")
			} else {
				r.update(t)
			}
		}
		assertRustCallTarget(t, r, "glob only", "a.rs:crate::a::f")
		r.write(t, "lib.rs", withOwn)
		update()
		r.assertFreshParity(t, "own fn added")
		assertRustCallTarget(t, r, "own fn added", "lib.rs:crate::f")
		r.write(t, "lib.rs", globOnly)
		update()
		r.assertFreshParity(t, "own fn removed")
		assertRustCallTarget(t, r, "own fn removed", "a.rs:crate::a::f")
	}
}

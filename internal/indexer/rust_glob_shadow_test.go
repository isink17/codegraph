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
	// src and call default to lib.rs and f.
	src, call string
}{
	{"own fn after glob", tree{
		"lib.rs": "mod a; use a::*; pub fn f() {}\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "lib.rs:crate::f", "", ""},
	{"own fn before glob", tree{
		"lib.rs": "mod a; pub fn f() {} use a::*;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "lib.rs:crate::f", "", ""},
	// The own item wins in Rust. Private items do not bind today, so the call
	// stays unresolved; what must never happen is the glob winning.
	{"private own fn", tree{
		"lib.rs": "mod a; use a::*; fn f() {}\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "", "", ""},
	{"private own fn, glob re-export", tree{
		"lib.rs": "mod a; pub use a::*; fn f() {}\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "", "", ""},
	{"explicit use after glob", tree{
		"lib.rs": "mod a; mod b; use a::*; use b::f;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub fn f() {}",
	}, "b.rs:crate::b::f", "", ""},
	{"explicit use before glob", tree{
		"lib.rs": "mod a; mod b; use b::f; use a::*;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub fn f() {}",
	}, "b.rs:crate::b::f", "", ""},
	{"glob only", tree{
		"lib.rs": "mod a; use a::*;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "a.rs:crate::a::f", "", ""},
	{"two globs", tree{
		"lib.rs": "mod a; mod b; use a::*; use b::*;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub fn f() {}",
	}, "", "", ""},
	// A trait lives only in the type namespace, so it does not shadow a
	// glob-imported function.
	{"own trait", tree{
		"lib.rs": "mod a; use a::*; pub trait f {}\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "a.rs:crate::a::f", "", ""},
	// A tuple struct shadows the function and a braced struct does not; the
	// parser does not record which form it saw, so the call fails closed.
	{"own struct", tree{
		"lib.rs": "mod a; use a::*; pub struct f(u8);\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "", "", ""},
	{"own enum", tree{
		"lib.rs": "mod a; use a::*; pub enum f { A }\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "a.rs:crate::a::f", "", ""},
	// An explicit import shadows a glob only in its item's namespace. A trait
	// leaves the glob function visible, and a struct may or may not be a value,
	// so neither may claim the call; both fail closed.
	{"explicit use of a struct after glob fn", tree{
		"lib.rs": "mod a; mod b; use a::*; use b::f;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub struct f { x: u8 }",
	}, "", "", ""},
	{"explicit use of a trait before glob fn", tree{
		"lib.rs": "mod a; mod b; use b::f; use a::*;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub trait f {}",
	}, "", "", ""},
	// The first segment of a qualified call names a module or type: an own
	// module shadows the glob-imported one.
	{"own module path", tree{
		"lib.rs": "mod a; mod x; use a::*;\npub fn caller() {\n    x::f();\n}\n",
		"a.rs":   "pub mod x {\n    pub fn f() {}\n}\n",
		"x.rs":   "pub fn f() {}",
	}, "x.rs:crate::x::f", "", "x::f"},
	{"glob module path", tree{
		"lib.rs": "mod a; use a::*;\npub fn caller() {\n    x::f();\n}\n",
		"a.rs":   "pub mod x {\n    pub fn f() {}\n}\n",
	}, "a.rs:crate::a::x::f", "", "x::f"},
	// A trait impl for a glob-imported type declares no own type, so the
	// inherent method reached through the glob keeps the call.
	{"own trait impl for a glob type", tree{
		"lib.rs": "mod a; use a::*; trait T { fn new(); }\nimpl T for S {\n    fn new() {}\n}\npub fn caller() {\n    S::new();\n}\n",
		"a.rs":   "pub struct S;\nimpl S {\n    pub fn new() {}\n}\n",
	}, "a.rs:crate::a::S::new", "", "S::new"},
	// An explicit import of the first segment shadows the glob as well. The
	// call is not resolved through that import, so it stays unresolved.
	{"explicit type use after glob, type path", tree{
		"lib.rs": "mod a; mod b; use a::*; use b::S;\npub fn caller() {\n    S::new();\n}\n",
		"a.rs":   "pub struct S;\nimpl S {\n    pub fn new() {}\n}\n",
		"b.rs":   "pub struct S;\nimpl S {\n    pub fn new() {}\n}\n",
	}, "", "", "S::new"},
	{"explicit type use before glob, type path", tree{
		"lib.rs": "mod a; mod b; use b::S; use a::*;\npub fn caller() {\n    S::new();\n}\n",
		"a.rs":   "pub struct S;\nimpl S {\n    pub fn new() {}\n}\n",
		"b.rs":   "pub struct S;\nimpl S {\n    pub fn new() {}\n}\n",
	}, "", "", "S::new"},
	{"explicit module use, module path", tree{
		"lib.rs": "mod a; mod b; use a::*; use b::x;\npub fn caller() {\n    x::f();\n}\n",
		"a.rs":   "pub mod x {\n    pub fn f() {}\n}\n",
		"b.rs":   "pub mod x {\n    pub fn f() {}\n}\n",
	}, "", "", "x::f"},
	// Two explicit imports of one name must live in different namespaces;
	// the function is the called value, in either order.
	{"explicit struct then fn", tree{
		"lib.rs": "mod b; mod c; use b::f; use c::f;\npub fn caller() {\n    f();\n}\n",
		"b.rs":   "pub struct f { x: u8 }",
		"c.rs":   "pub fn f() {}",
	}, "c.rs:crate::c::f", "", ""},
	{"explicit fn then struct", tree{
		"lib.rs": "mod b; mod c; use c::f; use b::f;\npub fn caller() {\n    f();\n}\n",
		"b.rs":   "pub struct f { x: u8 }",
		"c.rs":   "pub fn f() {}",
	}, "c.rs:crate::c::f", "", ""},
	{"two explicit fns", tree{
		"lib.rs": "mod b; mod c; use b::f; use c::f;\npub fn caller() {\n    f();\n}\n",
		"b.rs":   "pub fn f() {}",
		"c.rs":   "pub fn f() {}",
	}, "", "", ""},
	// An own module still answers through its own re-exports.
	{"own module re-export path", tree{
		"lib.rs":     "mod a; mod x; use a::*;\npub fn caller() {\n    x::f();\n}\n",
		"a.rs":       "pub mod x {\n    pub fn f() {}\n}\n",
		"x.rs":       "mod inner;\npub use inner::f;\n",
		"x/inner.rs": "pub fn f() {}",
	}, "x/inner.rs:crate::inner::f", "", "x::f"}, // persisted name is the parser's path guess
	// A glob re-export may put a function beside the module's own type of the
	// same name; Rust calls the function.
	{"module glob re-export beside a struct", tree{
		"lib.rs": "mod a; mod m;\npub fn caller() {\n    m::f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "pub use crate::a::*;\npub struct f { x: u8 }\n",
	}, "", "", "m::f"},
	{"module glob re-export alone", tree{
		"lib.rs": "mod a; mod m;\npub fn caller() {\n    m::f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "pub use crate::a::*;\n",
	}, "a.rs:crate::a::f", "", "m::f"},
	// Another crate root's `crate::f` is not the caller's own item.
	{"other crate's own fn", tree{
		"src/lib.rs":  "mod a; use a::*;\npub fn caller() {\n    f();\n}\n",
		"src/a.rs":    "pub fn f() {}",
		"src/main.rs": "pub fn f() {}\nfn main() {}\n",
	}, "src/a.rs:crate::a::f", "src/lib.rs", ""},
	// An item of a child module is not declared in the caller's module.
	{"child module fn", tree{
		"lib.rs": "mod a; mod c; use a::*;\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"c.rs":   "pub fn f() {}",
	}, "a.rs:crate::a::f", "", ""},
}

func assertRustCallTarget(t *testing.T, r *lifecycleRepo, step, src, call, target string) {
	t.Helper()
	got := r.edgeState(t, src, call)
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
			src, call := tc.src, tc.call
			if src == "" {
				src = "lib.rs"
			}
			if call == "" {
				call = "f"
			}
			r := newLifecycleRepo(t, tc.files)
			assertRustCallTarget(t, r, "fresh", src, call, tc.target)

			// Every resolver entrypoint must reach the same decision from an
			// unresolved edge.
			for _, ep := range []struct {
				name string
				run  func() error
			}{
				{"repo", func() error { _, err := r.store.ResolveEdges(r.ctx, r.repoID); return err }},
				{"paths", func() error { return r.store.ResolveEdgesForPaths(r.ctx, r.repoID, []string{src}) }},
				{"names", func() error {
					_, err := r.store.ResolveEdgesForNamesWithStats(r.ctx, r.repoID, []string{call})
					return err
				}},
				{"paths+names", func() error {
					_, err := r.store.ResolveEdgesForPathsAndNames(r.ctx, r.repoID, []string{src}, []string{call})
					return err
				}},
			} {
				if _, err := r.raw(t).ExecContext(r.ctx, `UPDATE edges SET dst_symbol_id=NULL,resolution_strategy='',resolution_confidence='' WHERE repo_id=? AND dst_name=?`, r.repoID, call); err != nil {
					t.Fatal(err)
				}
				if err := ep.run(); err != nil {
					t.Fatalf("%s: %v", ep.name, err)
				}
				assertRustCallTarget(t, r, ep.name, src, call, tc.target)
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
		assertRustCallTarget(t, r, "glob only", "lib.rs", "f", "a.rs:crate::a::f")
		r.write(t, "lib.rs", withOwn)
		update()
		r.assertFreshParity(t, "own fn added")
		assertRustCallTarget(t, r, "own fn added", "lib.rs", "f", "lib.rs:crate::f")
		r.write(t, "lib.rs", globOnly)
		update()
		r.assertFreshParity(t, "own fn removed")
		assertRustCallTarget(t, r, "own fn removed", "lib.rs", "f", "a.rs:crate::a::f")
		r.write(t, "b.rs", "pub fn f() {}")
		r.write(t, "lib.rs", "mod a; mod b; use a::*; use b::f;\npub fn caller() {\n    f();\n}\n")
		r.update(t)
		r.assertFreshParity(t, "explicit use added")
		assertRustCallTarget(t, r, "explicit use added", "lib.rs", "f", "b.rs:crate::b::f")
		r.write(t, "lib.rs", "mod a; mod b; use a::*;\npub fn caller() {\n    f();\n}\n")
		update()
		r.assertFreshParity(t, "explicit use removed")
		assertRustCallTarget(t, r, "explicit use removed", "lib.rs", "f", "a.rs:crate::a::f")
	}
}

// TestRustGlobReexportBesideTypeFollowsIncrementalChanges adds and removes the
// module's own type beside its glob re-export, and requires every step to match
// a fresh index of the same tree.
func TestRustGlobReexportBesideTypeFollowsIncrementalChanges(t *testing.T) {
	const caller = "mod a; mod m;\npub fn caller() {\n    m::f();\n}\n"
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{"lib.rs": caller, "a.rs": "pub fn f() {}", "m.rs": "pub use crate::a::*;\n"})
		update := func() {
			if scoped {
				r.update(t, "m.rs")
			} else {
				r.update(t)
			}
		}
		assertRustCallTarget(t, r, "glob re-export", "lib.rs", "m::f", "a.rs:crate::a::f")
		r.write(t, "m.rs", "pub use crate::a::*;\npub struct f { x: u8 }\n")
		update()
		r.assertFreshParity(t, "struct added")
		assertRustCallTarget(t, r, "struct added", "lib.rs", "m::f", "")
		r.write(t, "m.rs", "pub use crate::a::*;\n")
		update()
		r.assertFreshParity(t, "struct removed")
		assertRustCallTarget(t, r, "struct removed", "lib.rs", "m::f", "a.rs:crate::a::f")
	}
}

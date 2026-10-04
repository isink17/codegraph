//go:build cgo

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
	// The module sees its own private items, so the own item wins here too.
	{"private own fn", tree{
		"lib.rs": "mod a; use a::*; fn f() {}\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "lib.rs:crate::f", "", ""},
	{"private own fn, glob re-export", tree{
		"lib.rs": "mod a; pub use a::*; fn f() {}\npub fn caller() {\n    f();\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "lib.rs:crate::f", "", ""},
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
	// Another crate root's `crate::x` re-exports are not the caller's:
	// here `x::f` is std's abort, which nothing in the repository declares.
	{"other crate root's re-export", tree{
		"src/lib.rs":  "mod a; mod x; mod z; use a::*;\npub fn caller() {\n    x::f();\n}\n",
		"src/a.rs":    "pub mod x {\n    pub fn f() {}\n}\n",
		"src/x.rs":    "pub use std::process::abort as f;\n",
		"src/z.rs":    "pub fn f() {}",
		"src/main.rs": "mod x {\n    pub use crate::z::f;\n}\nmod z {\n    pub fn f() {}\n}\nfn main() {}\n",
	}, "", "src/lib.rs", "x::f"},
	// A type reached through a re-export is just as unsafe to call when its
	// own module imports by glob.
	{"re-exported struct beside a glob", tree{
		"lib.rs": "mod k; mod m;\npub fn caller() {\n    m::f();\n}\n",
		"m.rs":   "pub use crate::k::f;\n",
		"k.rs":   "pub use ext::*;\npub struct f { x: u8 }\n",
	}, "", "", "m::f"},
	{"re-exported struct alone", tree{
		"lib.rs": "mod k; mod m;\npub fn caller() {\n    m::f();\n}\n",
		"m.rs":   "pub use crate::k::f;\n",
		"k.rs":   "pub struct f(u8);\n",
	}, "k.rs:crate::k::f", "", "m::f"},
	// One import spelled under two cfg arms is one import only if the arms are
	// complementary; proving that needs a cfg evaluator (rustc agrees here, but
	// cfg(a) / cfg(b) arms may both be off), so every cfg import is unproven.
	{"cfg-duplicated explicit use", tree{
		"lib.rs": "mod b;\n#[cfg(unix)]\nuse b::f;\n#[cfg(not(unix))]\nuse b::f;\npub fn caller() {\n    f();\n}\n",
		"b.rs":   "pub fn f() {}",
	}, "", "", ""},
	// An inline module holds only its own items: the file module's `f` is
	// not `m::x::f`, so x's re-export answers the call.
	{"inline module re-export beside a file item", tree{
		"lib.rs":   "mod m; mod other;\npub fn caller() {\n    m::x::f();\n}\n",
		"m.rs":     "pub fn f() {}\npub mod x {\n    pub use crate::other::f;\n}\n",
		"other.rs": "pub fn f() {}",
	}, "other.rs:crate::other::f", "", "m::x::f"},
	{"file item is not in an inline module", tree{
		"lib.rs": "mod m;\npub fn caller() {\n    m::x::f();\n}\n",
		"m.rs":   "pub fn f() {}\npub mod x {}\n",
	}, "", "", "m::x::f"},
	{"inline module item", tree{
		"lib.rs": "mod m;\npub fn caller() {\n    m::x::g();\n}\n",
		"m.rs":   "pub fn f() {}\npub mod x {\n    pub fn g() {}\n}\n",
	}, "m.rs:crate::m::x::g", "", "m::x::g"},
	{"file item beside an inline module", tree{
		"lib.rs": "mod m;\npub fn caller() {\n    m::f();\n}\n",
		"m.rs":   "pub fn f() {}\npub mod x {\n    pub fn f() {}\n}\n",
	}, "m.rs:crate::m::f", "", "m::f"},
	// `use super::*` imports the parent module itself, as a test module does.
	{"glob of super from an inline module", tree{
		"lib.rs": "mod m;",
		"m.rs":   "pub struct S;\nimpl S {\n    pub fn empty() {}\n}\nmod tests {\n    use super::*;\n    fn t() {\n        S::empty();\n    }\n}\n",
	}, "m.rs:crate::m::S::empty", "m.rs", "S::empty"},
	// A module's own item hides its glob re-exports of the same name, even
	// through a child's `pub use super::*` or `pub use crate::*`. The own
	// item here is private, so the call stays unresolved; it never reaches
	// the glob's function.
	{"prelude of super with own private item", tree{
		"lib.rs":   "mod other; fn f() {} pub use other::*;\npub mod prelude {\n    pub use super::*;\n}\nfn caller() {\n    prelude::f();\n}\n",
		"other.rs": "pub fn f() {}",
	}, "", "", "prelude::f"},
	{"crate glob re-export with own private item", tree{
		"lib.rs": "mod b; mod a; pub use b::*; fn f() {}\nfn caller() {\n    a::f();\n}\n",
		"a.rs":   "pub use crate::*;\n",
		"b.rs":   "pub fn f() {}",
	}, "", "", "a::f"},
	// An explicit import of the name, private or not, hides the glob too.
	{"prelude of super with explicit private use", tree{
		"lib.rs": "mod a; mod b; use a::f; pub use b::*;\npub mod prelude {\n    pub use super::*;\n}\nfn caller() {\n    prelude::f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub fn f() {}",
	}, "", "", "prelude::f"},
	{"inline module with private use beside glob of super", tree{
		"lib.rs": "mod a; mod b; mod m;\nfn caller() {\n    m::p::f();\n}\n",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub fn f() {}",
		"m.rs":   "pub mod p {\n    use crate::a::f;\n    pub use super::*;\n}\npub use crate::b::*;\n",
	}, "", "", "m::p::f"},
	// A trait does not hide a glob-imported function, so the walk reaches
	// both; with two candidates in different namespaces the call fails
	// closed (rustc: b::f).
	{"prelude of super with own trait", tree{
		"lib.rs": "mod b; pub trait f {} pub use b::*;\npub mod prelude {\n    pub use super::*;\n}\nfn caller() {\n    prelude::f();\n}\n",
		"b.rs":   "pub fn f() {}",
	}, "", "", "prelude::f"},
	// A test module's own fn shadows its `use super::*` glob.
	{"test module own fn beside glob of super", tree{
		"lib.rs": "mod m;",
		"m.rs":   "pub fn helper() {}\nmod tests {\n    use super::*;\n    fn helper() {}\n    fn t() {\n        helper();\n    }\n}\n",
	}, "", "m.rs", "helper"},
	{"glob of crate from a child", tree{
		"lib.rs": "mod m; pub fn top() {}",
		"m.rs":   "use crate::*;\nfn t() {\n    top();\n}\n",
	}, "lib.rs:crate::top", "m.rs", "top"},
	{"glob of super in a nested inline module", tree{
		"lib.rs": "mod m;",
		"m.rs":   "mod a {\n    pub fn f() {}\n    pub mod b {\n        use super::*;\n        fn t() {\n            f();\n        }\n    }\n}\n",
	}, "m.rs:crate::m::a::f", "m.rs", "f"},
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

// TestRustExplicitUseKindFlipFollowsIncrementalChanges flips an explicitly
// imported item between a function and a struct while a glob supplies the same
// name; every step must match a fresh index of the same tree.
func TestRustExplicitUseKindFlipFollowsIncrementalChanges(t *testing.T) {
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{
			"lib.rs": "mod a; mod b; use a::*; use b::f;\npub fn caller() {\n    f();\n}\n",
			"a.rs":   "pub fn f() {}",
			"b.rs":   "pub fn f() {}",
		})
		update := func() {
			if scoped {
				r.update(t, "b.rs")
			} else {
				r.update(t)
			}
		}
		assertRustCallTarget(t, r, "explicit fn", "lib.rs", "f", "b.rs:crate::b::f")
		r.write(t, "b.rs", "pub struct f { x: u8 }")
		update()
		r.assertFreshParity(t, "flipped to struct")
		assertRustCallTarget(t, r, "flipped to struct", "lib.rs", "f", "")
		r.write(t, "b.rs", "pub fn f() {}")
		update()
		r.assertFreshParity(t, "flipped back")
		assertRustCallTarget(t, r, "flipped back", "lib.rs", "f", "b.rs:crate::b::f")
	}
}

// TestRustInlineModuleReexportFollowsIncrementalChanges adds and removes an
// inline module's re-export beside a file item of the same name; every step
// must match a fresh index of the same tree.
func TestRustInlineModuleReexportFollowsIncrementalChanges(t *testing.T) {
	const without = "pub fn f() {}\npub mod x {}\n"
	const with = "pub fn f() {}\npub mod x {\n    pub use crate::other::f;\n}\n"
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{
			"lib.rs":   "mod m; mod other;\npub fn caller() {\n    m::x::f();\n}\n",
			"m.rs":     without,
			"other.rs": "pub fn f() {}",
		})
		update := func() {
			if scoped {
				r.update(t, "m.rs")
			} else {
				r.update(t)
			}
		}
		assertRustCallTarget(t, r, "no re-export", "lib.rs", "m::x::f", "")
		r.write(t, "m.rs", with)
		update()
		r.assertFreshParity(t, "re-export added")
		assertRustCallTarget(t, r, "re-export added", "lib.rs", "m::x::f", "other.rs:crate::other::f")
		r.write(t, "m.rs", without)
		update()
		r.assertFreshParity(t, "re-export removed")
		assertRustCallTarget(t, r, "re-export removed", "lib.rs", "m::x::f", "")
	}
}

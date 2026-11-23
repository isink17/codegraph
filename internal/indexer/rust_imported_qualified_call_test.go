//go:build cgo

package indexer

import "testing"

// rustImportedQualifiedCases pin qualified calls whose first segment an
// explicit `use` binds. A followed import must give the answer the fully
// qualified spelling gives; a "" target is the fail-closed answer.
var rustImportedQualifiedCases = []struct {
	name   string
	files  tree
	src    string // defaults to m.rs
	call   string
	target string // "" means the call must stay unresolved
}{
	{"fully qualified control", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "pub fn c() {\n    crate::a::f();\n}\n",
	}, "", "crate::a::f", "a.rs:crate::a::f"},
	{"use_free", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a;\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", "a.rs:crate::a::f"},
	{"self in a use list", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}\npub struct T;",
		"m.rs":   "use crate::a::{self, T};\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", "a.rs:crate::a::f"},
	{"renamed import", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a as b;\npub fn c() {\n    b::f();\n}\n",
	}, "", "b::f", "a.rs:crate::a::f"},
	{"super import", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use super::a;\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", "a.rs:crate::a::f"},
	{"nested module path", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub mod b {\n    pub fn f() {}\n}\n",
		"m.rs":   "use crate::a;\npub fn c() {\n    a::b::f();\n}\n",
	}, "", "a::b::f", "a.rs:crate::a::b::f"},
	{"use_impl", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a;\npub struct S;\nimpl S {\n    pub fn c(&self) {\n        a::f();\n    }\n}\n",
	}, "", "a::f", "a.rs:crate::a::f"},
	{"use_trait_impl", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a;\npub trait T {\n    fn c(&self);\n}\npub struct S;\nimpl T for S {\n    fn c(&self) {\n        a::f();\n    }\n}\n",
	}, "", "a::f", "a.rs:crate::a::f"},
	{"inline module import", tree{
		"lib.rs": "mod a;\nmod m {\n    use crate::a;\n    pub fn c() {\n        a::f();\n    }\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "lib.rs", "a::f", "a.rs:crate::a::f"},
	{"imported type associated function", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub struct S;\nimpl S {\n    pub fn new() -> S {\n        S\n    }\n}\n",
		"m.rs":   "use crate::a::S;\npub fn c() {\n    S::new();\n}\n",
	}, "", "S::new", "a.rs:crate::a::S::new"},
	{"imported type associated function control", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub struct S;\nimpl S {\n    pub fn new() -> S {\n        S\n    }\n}\n",
		"m.rs":   "pub fn c() {\n    crate::a::S::new();\n}\n",
	}, "", "crate::a::S::new", "a.rs:crate::a::S::new"},
	// The fully qualified spelling of rust_visibility_test's nfa cases.
	{"inherent method beside a trait impl control", tree{
		"lib.rs": "mod nfa; mod r;",
		"nfa.rs": "pub struct NFA;\nimpl NFA {\n    pub(crate) fn swap(&mut self) {}\n}\n",
		"r.rs":   "pub trait R {\n    fn swap(&mut self);\n}\nimpl R for crate::nfa::NFA {\n    fn swap(&mut self) {\n        crate::nfa::NFA::swap(self);\n    }\n}\n",
	}, "r.rs", "crate::nfa::NFA::swap", "nfa.rs:crate::nfa::NFA::swap"},
	// An explicit import shadows a glob that brings the same module name.
	{"explicit import beats glob", tree{
		"lib.rs": "mod a; mod g; mod m;",
		"a.rs":   "pub fn f() {}",
		"g.rs":   "pub mod a {\n    pub fn f() {}\n}\n",
		"m.rs":   "use crate::a;\nuse crate::g::*;\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", "a.rs:crate::a::f"},
	// Without an explicit import the glob's module answers, as before.
	{"glob brings the segment", tree{
		"lib.rs": "mod g; mod m;",
		"g.rs":   "pub mod a {\n    pub fn f() {}\n}\n",
		"m.rs":   "use crate::g::*;\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", "g.rs:crate::g::a::f"},
	// An own module wins over an imported function of the same name; the
	// path's first segment lives in the type namespace.
	{"own module beats imported fn", tree{
		"lib.rs": "mod b; mod m;",
		"b.rs":   "pub fn a() {}",
		"m.rs":   "use crate::b::a;\nmod a {\n    pub fn f() {}\n}\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", "m.rs:crate::m::a::f"},
	// An item macro may not declare a second `a` in the type namespace beside
	// the explicit import (E0255), so it does not shadow the import.
	{"item macro beside the import", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a;\nmacro_rules! mk { ($n:ident) => { fn $n() {} } }\nmk!(g);\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", "a.rs:crate::a::f"},
	// -- refused
	{"attribute macro on the caller", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a;\n#[inject]\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", ""},
	{"cfg-gated import", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "#[cfg(unix)]\nuse crate::a;\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", ""},
	{"two imports of the segment", tree{
		"lib.rs": "mod a; mod b; mod m;",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub fn a() {}\npub fn f() {}",
		"m.rs":   "use crate::a;\nuse crate::b::a;\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", ""},
	{"conflicting cfg-arm imports", tree{
		"lib.rs": "mod a; mod b; mod m;",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub fn f() {}",
		"m.rs":   "#[cfg(unix)]\nuse crate::a;\n#[cfg(not(unix))]\nuse crate::b as a;\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", ""},
	{"block-local import shadows", tree{
		"lib.rs": "mod a; mod b; mod m;",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a;\npub fn c() {\n    use crate::b as a;\n    a::f();\n}\n",
	}, "", "a::f", ""},
	{"unresolved import", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::nope;\npub fn c() {\n    nope::f();\n}\n",
	}, "", "nope::f", ""},
	{"external crate import", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use serde;\npub fn c() {\n    serde::f();\n}\n",
	}, "", "serde::f", ""},
	{"private target stays private", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "fn f() {}",
		"m.rs":   "use crate::a;\npub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", ""},
	{"import in another module", tree{
		"lib.rs": "mod a; mod m; mod n;",
		"a.rs":   "pub fn f() {}",
		"n.rs":   "use crate::a;",
		"m.rs":   "pub fn c() {\n    a::f();\n}\n",
	}, "", "a::f", ""},
}

func TestRustImportedQualifiedCalls(t *testing.T) {
	for _, layout := range []string{"src/", ""} {
		for _, tc := range rustImportedQualifiedCases {
			files := tree{}
			for path, content := range tc.files {
				files[layout+path] = content
			}
			src, target := tc.src, tc.target
			if src == "" {
				src = "m.rs"
			}
			if target != "" {
				target = layout + target
			}
			runRustCallEntrypoints(t, "layout="+layout+"/"+tc.name, files, layout+src, tc.call, target)
		}
	}
}

// TestRustImportedQualifiedCallsFollowIncrementalChanges adds and removes the
// import, the target and a competing import through both update shapes; every
// step must match a fresh index of the same tree.
func TestRustImportedQualifiedCallsFollowIncrementalChanges(t *testing.T) {
	const caller = "pub fn c() {\n    a::f();\n}\n"
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{"lib.rs": "mod a; mod b; mod m;", "a.rs": "pub fn f() {}", "b.rs": "pub fn a() {}", "m.rs": caller})
		update := func(paths ...string) {
			if scoped {
				r.update(t, paths...)
			} else {
				r.update(t)
			}
		}
		assertRustCallTarget(t, r, "no import", "m.rs", "a::f", "")
		for _, step := range []struct {
			name, path, content, target string
		}{
			{"import added", "m.rs", "use crate::a;\n" + caller, "a.rs:crate::a::f"},
			{"target removed", "a.rs", "pub fn g() {}", ""},
			{"target restored", "a.rs", "pub fn f() {}", "a.rs:crate::a::f"},
			{"competing import added", "m.rs", "use crate::a;\nuse crate::b::a;\n" + caller, ""},
			{"competing import removed", "m.rs", "use crate::a;\n" + caller, "a.rs:crate::a::f"},
			{"import removed", "m.rs", caller, ""},
		} {
			r.write(t, step.path, step.content)
			update(step.path)
			r.assertFreshParity(t, step.name)
			assertRustCallTarget(t, r, step.name, "m.rs", "a::f", step.target)
		}
	}
}

//go:build cgo

package indexer

import (
	"database/sql"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// rustAttrShadowCases pin which declaration answers `f()` when an attribute
// the parser cannot expand sits near it. A procedural derive may emit a sibling
// `fn f`, and an attribute macro may also rename or drop the item it sits on,
// so neither is read as proof. Each outcome was checked against rustc 1.98.1
// (the rust-attr-oracle crate with a real proc-macro crate); a "" target is the
// fail-closed answer.
var rustAttrShadowCases = []struct {
	name   string
	files  tree
	src    string // defaults to m.rs
	call   string // defaults to f
	target string // "" means the call must stay unresolved
}{
	// -- a derive may emit a sibling fn f that shadows the glob
	{"custom derive beside a glob", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\n#[derive(procgen::Gen)]\nstruct Marker;\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"imported custom derive", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nuse procgen::Gen;\n#[derive(Gen)]\nstruct Marker;\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"custom derive aliased as a builtin name", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nuse procgen::Gen as Clone;\n#[derive(Clone)]\nstruct Marker;\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"builtin derive name imported from elsewhere", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nuse procgen::Debug;\n#[derive(Debug)]\nstruct Marker;\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"custom derive among builtin ones", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\n#[derive(Debug, procgen::Gen, Clone)]\nstruct Marker;\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"custom derive on an enum behind a doc comment", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\n#[derive(procgen::Gen)]\n/// doc\n#[allow(dead_code)]\nenum Marker { A }\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"custom derive in a function block", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\npub fn c() {\n    #[derive(procgen::Gen)]\n    struct Marker;\n    f();\n}\n",
	}, "", "", ""},
	{"custom derive beside a re-exporting glob", tree{
		"lib.rs": "mod a; mod b; mod m;",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub use crate::a::*;\n#[derive(procgen::Gen)]\npub struct Marker;\n",
		"m.rs":   "pub fn c() {\n    crate::b::f();\n}\n",
	}, "", "crate::b::f", ""},
	// -- an attribute macro may transform or remove the item it sits on
	{"attribute macro on a sibling item", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\n#[procgen::keep]\nfn other() {}\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"attribute macro on the own fn beside a glob", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\n#[procgen::rename]\npub fn f() {}\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"attribute macro on the own fn, no glob", tree{
		"lib.rs": "mod m;",
		"m.rs":   "#[procgen::rename]\npub fn f() {}\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"imported attribute macro on the own fn", tree{
		"lib.rs": "mod m;",
		"m.rs":   "use procgen::rename;\n#[rename]\npub fn f() {}\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"builtin attribute name imported as a custom macro", tree{
		"lib.rs": "mod m;",
		"m.rs":   "use procgen::inline;\n#[inline]\npub fn f() {}\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"qualified call to an attribute-macro fn", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "#[procgen::erase]\npub fn f() {}",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "crate::n::f", ""},
	{"explicit import of an attribute-macro fn", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "#[procgen::erase]\npub fn f() {}",
		"m.rs":   "use crate::n::f;\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"attribute macro on an inline module", tree{
		"lib.rs": "#[procgen::erase]\npub mod n {\n    pub fn f() {}\n}\nmod m;",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "crate::n::f", ""},
	{"attribute macro on an out-of-line module", tree{
		"lib.rs": "#[procgen::erase]\nmod n;\nmod m;",
		"n.rs":   "pub fn f() {}",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "crate::n::f", ""},
	{"cfg_attr may inject any attribute", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\n#[cfg_attr(feature = \"x\", derive(procgen::Gen))]\nstruct Marker;\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"attribute macro on an attributed use", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "#[procgen::erase]\nuse crate::a::f;\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	// -- negative controls: builtin, non-generative attributes keep what is proven
	{"builtin attributes, own fn", tree{
		"lib.rs": "mod m;",
		"m.rs":   "#[derive(Debug, Clone, PartialEq)]\n#[allow(dead_code)]\n/// doc\nstruct Marker;\n#[inline]\n#[cfg(unix)]\n#[doc = \"x\"]\nfn g() {}\nfn f() {}\npub fn c() {\n    f();\n}\n",
	}, "", "", "m.rs:crate::m::f"},
	{"builtin derive in a function block, own fn", tree{
		"lib.rs": "mod m;",
		"m.rs":   "fn f() {}\npub fn c() {\n    #[derive(Debug)]\n    struct Marker;\n    f();\n}\n",
	}, "", "", "m.rs:crate::m::f"},
	{"own fn with builtin attributes", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::f as _unused;\n#[inline]\n#[allow(dead_code)]\npub fn f() {}\npub fn c() {\n    f();\n}\n",
	}, "", "", "m.rs:crate::m::f"},
	{"builtin attributes, explicit import", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::f;\n#[derive(Debug)]\nstruct Marker;\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"builtin attributes on the callee", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "#[inline]\n#[must_use]\npub fn f() {}",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "crate::n::f", "n.rs:crate::n::f"},
	{"std-imported derive name, explicit import", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::f;\nuse std::fmt::Debug;\n#[derive(Debug)]\nstruct Marker;\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"own fn beside a custom derive", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\n#[derive(procgen::Gen)]\nstruct Marker;\nfn f() {}\npub fn c() {\n    f();\n}\n",
	}, "", "", "m.rs:crate::m::f"},
	{"explicit import beside a custom derive", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::f;\n#[derive(procgen::Gen)]\nstruct Marker;\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"custom derive in a sibling file only", tree{
		"lib.rs": "mod a; mod n; mod m;",
		"a.rs":   "pub fn f() {}",
		"n.rs":   "#[derive(procgen::Gen)]\npub struct Marker;",
		"m.rs":   "use crate::a::*;\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"custom derive in a nested module only", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\npub mod inner {\n    #[derive(procgen::Gen)]\n    pub struct Marker;\n}\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"qualified crate path past a block derive", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "pub fn c() {\n    #[derive(procgen::Gen)]\n    struct Marker;\n    crate::a::f();\n}\n",
	}, "", "crate::a::f", "a.rs:crate::a::f"},
	{"attribute macro on an unrelated fn, own fn kept", tree{
		"lib.rs": "mod m;",
		"m.rs":   "#[procgen::keep]\nfn other() {}\nfn f() {}\npub fn c() {\n    f();\n}\n",
	}, "", "", "m.rs:crate::m::f"},
	{"cfg on a module", tree{
		"lib.rs": "#[cfg(unix)]\nmod n;\nmod m;",
		"n.rs":   "pub fn f() {}",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "crate::n::f", "n.rs:crate::n::f"},
	// -- statement macros in a block: rustc calls the macro's local fn, and a
	// nested block's macro does not reach the call outside it
	{"statement macro declaring f in the call's block", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nmacro_rules! mk { () => { fn f() {} } }\npub fn c() {\n    mk!();\n    f();\n}\n",
	}, "", "", ""},
	{"statement macro in an inner block only", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nmacro_rules! mk { () => { fn f() {} } }\npub fn c() {\n    let _x = { mk!(); 1 };\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"statement macro, crate path call", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "macro_rules! mk { () => { fn f() {} } }\npub fn c() {\n    mk!();\n    crate::a::f();\n}\n",
	}, "", "crate::a::f", "a.rs:crate::a::f"},
}

func TestRustAttributeShadowing(t *testing.T) {
	for _, layout := range []string{"src/", ""} {
		for _, tc := range rustAttrShadowCases {
			files := tree{}
			for path, content := range tc.files {
				files[layout+path] = content
			}
			src, call, target := tc.src, tc.call, tc.target
			if src == "" {
				src = "m.rs"
			}
			if call == "" {
				call = "f"
			}
			if target != "" {
				target = layout + target
			}
			runRustCallEntrypoints(t, "layout="+layout+"/"+tc.name, files, layout+src, call, target)
		}
	}
}

// TestRustAttributeShadowingFollowsIncrementalChanges adds and removes the
// attribute through both update shapes; every step must match a fresh index.
func TestRustAttributeShadowingFollowsIncrementalChanges(t *testing.T) {
	const caller = "pub fn c() {\n    f();\n}\n"
	steps := []struct{ name, m, target string }{
		{"glob only", "use crate::a::*;\n" + caller, "a.rs:crate::a::f"},
		{"custom derive added", "use crate::a::*;\n#[derive(procgen::Gen)]\nstruct M;\n" + caller, ""},
		{"attribute macro added", "use crate::a::*;\n#[procgen::x]\nstruct M;\n" + caller, ""},
		{"derive removed", "use crate::a::*;\nstruct M;\n" + caller, "a.rs:crate::a::f"},
	}
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{"lib.rs": "mod a; mod m;", "a.rs": "pub fn f() {}", "m.rs": steps[0].m})
		for _, step := range steps {
			r.write(t, "m.rs", step.m)
			if scoped {
				r.update(t, "m.rs")
			} else {
				r.update(t)
			}
			r.assertFreshParity(t, step.name)
			assertRustCallTarget(t, r, step.name, "m.rs", "f", step.target)
		}
	}
	// An attribute macro on the callee changes the answer for a caller in a
	// file that is not itself touched.
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{
			"lib.rs": "mod n; mod m;",
			"n.rs":   "pub fn f() {}\n",
			"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
		})
		assertRustCallTarget(t, r, "callee", "m.rs", "crate::n::f", "n.rs:crate::n::f")
		for _, step := range []struct{ name, n, target string }{
			{"attribute added", "#[procgen::erase]\npub fn f() {}\n", ""},
			{"attribute removed", "pub fn f() {}\n", "n.rs:crate::n::f"},
		} {
			r.write(t, "n.rs", step.n)
			if scoped {
				r.update(t, "n.rs")
			} else {
				r.update(t)
			}
			r.assertFreshParity(t, step.name)
			assertRustCallTarget(t, r, step.name, "m.rs", "crate::n::f", step.target)
		}
	}
}

// TestRustAttributeProfileUpgradeConverges rebuilds the state a Rust v4 index
// left behind (no attribute evidence, the calls bound as the v4 resolver did)
// and requires one update to refresh every Rust file, settle on the fresh
// answer, and a second update to change nothing; a fresh index agrees.
func TestRustAttributeProfileUpgradeConverges(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"lib.rs": "mod a; mod n; mod m; mod p;",
		"a.rs":   "pub fn f() {}",
		"n.rs":   "#[procgen::erase]\npub fn f() {}\n",
		"m.rs":   "use crate::a::*;\n#[derive(procgen::Gen)]\nstruct Marker;\npub fn c() {\n    f();\n}\n",
		"p.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	})
	assertRustUnresolved(t, r, "m.rs", "f")
	assertRustUnresolved(t, r, "p.rs", "crate::n::f")
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE files SET parser_profile='treesitter:rust:v4' WHERE repo_id=? AND language='rust'", r.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM rust_value_item_evidence WHERE repo_id=?", r.repoID); err != nil {
		t.Fatal(err)
	}
	// What v4 persisted: the glob's function for m, the attributed fn for p.
	if _, err := db.Exec(`UPDATE edges SET dst_symbol_id=(SELECT id FROM symbols WHERE repo_id=? AND qualified_name='crate::a::f'), resolution_strategy='rust_use_scope', resolution_confidence='high' WHERE repo_id=? AND dst_name='f'`, r.repoID, r.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE edges SET dst_symbol_id=(SELECT id FROM symbols WHERE repo_id=? AND qualified_name='crate::n::f'), resolution_strategy='rust_module_scope', resolution_confidence='high' WHERE repo_id=? AND dst_name='crate::n::f'`, r.repoID, r.repoID); err != nil {
		t.Fatal(err)
	}
	assertRustResolved(t, r, "m.rs", "f", "a.rs:crate::a::f")
	assertRustResolved(t, r, "p.rs", "crate::n::f", "n.rs:crate::n::f")
	summary := r.update(t)
	if summary.FilesIndexed != 5 || len(summary.ParserProfileLanguages) != 1 || summary.ParserProfileLanguages[0] != "rust" {
		t.Fatalf("profile update = %+v", summary)
	}
	assertRustUnresolved(t, r, "m.rs", "f")
	assertRustUnresolved(t, r, "p.rs", "crate::n::f")
	r.assertFreshParity(t, "Rust v4 to v5 attribute upgrade")
	if again := r.update(t); again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v", again)
	}
}

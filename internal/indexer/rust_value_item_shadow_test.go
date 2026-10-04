//go:build cgo

package indexer

import (
	"database/sql"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// rustValueShadowCases pin which item answers `f()` when a module imports a
// function `f` by glob and also declares, or may declare, a value item `f`
// the parser records no symbol for: a const, a static, an extern-block item or
// something a macro expands to. Each outcome was checked against rustc 1.98.1
// (the cg70 oracle crate); a "" target is the fail-closed answer.
var rustValueShadowCases = []struct {
	name   string
	files  tree
	src    string // defaults to m.rs
	call   string // defaults to f
	target string // "" means the call must stay unresolved
}{
	// -- an own value item shadows the glob (rustc calls the own item)
	{"const", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nconst f: fn() = || {};\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"pub const", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\npub const f: fn() = || {};\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"static", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nstatic f: fn() = || {};\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"extern block fn", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nextern \"C\" {\n    #[link_name = \"ext\"]\n    fn f();\n}\npub fn c() {\n    unsafe { f(); }\n}\n",
	}, "", "", ""},
	{"extern block static", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nextern \"C\" {\n    static f: fn();\n}\npub fn c() {\n    unsafe { f(); }\n}\n",
	}, "", "", ""},
	{"macro generated fn", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nmacro_rules! mk { ($n:ident) => { fn $n() {} } }\nmk!(f);\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	// The expansion is not read, so a macro that generates another name
	// refuses too: the recall ceiling of the fail-closed rule.
	{"item macro generating another name", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nmacro_rules! mk { ($n:ident) => { fn $n() {} } }\nmk!(zz);\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"unknown item macro from elsewhere", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nthread_local! { static X: u8 = 0; }\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"item macro beside a glob re-export", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "pub use crate::a::*;\nmacro_rules! mk { ($n:ident) => { fn $n() {} } }\nmk!(f);\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"const beside a glob re-export", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "pub use crate::a::*;\nconst f: fn() = || {};\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"const in an inline module", tree{
		"lib.rs": "mod a;\nmod m {\n    use crate::a::*;\n    const f: fn() = || {};\n    pub fn c() {\n        f();\n    }\n}\n",
		"a.rs":   "pub fn f() {}",
	}, "lib.rs", "", ""},
	{"const in a nested inline module", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "pub mod n {\n    use crate::a::*;\n    const f: fn() = || {};\n    pub fn c() {\n        f();\n    }\n}\n",
	}, "", "", ""},
	// -- imported and re-exported shadow items
	{"explicit import of a const beside a glob fn", tree{
		"lib.rs": "mod a; mod b; mod m;",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub const f: fn() = || {};",
		"m.rs":   "use crate::a::*;\nuse crate::b::f;\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"explicit import of a re-exporting module's own const", tree{
		"lib.rs": "mod a; mod b; mod m;",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub use crate::a::*;\npub const f: fn() = || {};",
		"m.rs":   "use crate::b::f;\npub fn c() {\n    f();\n}\n",
	}, "", "", ""},
	{"qualified call into a re-exporter with an own const", tree{
		"lib.rs": "mod a; mod b; mod m;",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub use crate::a::*;\npub const f: fn() = || {};",
		"m.rs":   "pub fn c() {\n    crate::b::f();\n}\n",
	}, "", "crate::b::f", ""},
	{"qualified call into a re-exporter with an item macro", tree{
		"lib.rs": "mod a; mod b; mod m;",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub use crate::a::*;\nmacro_rules! mk { ($n:ident) => { pub fn $n() {} } }\nmk!(f);",
		"m.rs":   "pub fn c() {\n    crate::b::f();\n}\n",
	}, "", "crate::b::f", ""},
	{"re-export chain through a glob over an own const", tree{
		"lib.rs": "mod a; mod b; mod c; mod m;",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub use crate::a::*;\npub const f: fn() = || {};",
		"c.rs":   "pub use crate::b::*;",
		"m.rs":   "pub fn c() {\n    crate::c::f();\n}\n",
	}, "", "crate::c::f", ""},
	// -- negative controls: nothing shadows, the glob answers
	{"const of another name", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nconst h: u8 = 1;\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"const in an impl", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\npub struct S;\nimpl S {\n    const f: u8 = 1;\n}\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"const in a function block, other name", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\npub fn c() {\n    const h: u8 = 1;\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"const in a child module", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\npub mod x {\n    pub const f: u8 = 0;\n}\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"const of a sibling module", tree{
		"lib.rs": "mod a; mod n; mod m;",
		"a.rs":   "pub fn f() {}",
		"n.rs":   "pub const f: u8 = 0;",
		"m.rs":   "use crate::a::*;\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"parent const, child with its own glob", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nconst f: fn() = || {};\npub mod child {\n    use crate::a::*;\n    pub fn c() {\n        f();\n    }\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"item macro in a nested module only", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\npub mod inner {\n    macro_rules! mk { ($n:ident) => { fn $n() {} } }\n    mk!(zzz);\n}\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"item macro in a sibling file only", tree{
		"lib.rs": "mod a; mod n; mod m;",
		"a.rs":   "pub fn f() {}",
		"n.rs":   "macro_rules! mk { ($n:ident) => { fn $n() {} } }\nmk!(f);",
		"m.rs":   "use crate::a::*;\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"expression macro", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\npub fn c() {\n    let _v = vec![1];\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"macro definition without an invocation", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nmacro_rules! unused { () => {} }\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"macro in an impl", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\npub struct S;\nimpl S {\n    mk!();\n}\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
	{"own fn beside an item macro", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nmk!(zz);\nfn f() {}\npub fn c() {\n    f();\n}\n",
	}, "", "", "m.rs:crate::m::f"},
	{"qualified call past a const", tree{
		"lib.rs": "mod a; mod m;",
		"a.rs":   "pub fn f() {}",
		"m.rs":   "use crate::a::*;\nconst f: u8 = 1;\npub fn c() {\n    crate::a::f();\n}\n",
	}, "", "crate::a::f", "a.rs:crate::a::f"},
	{"re-exporter with an unrelated const", tree{
		"lib.rs": "mod a; mod b; mod m;",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub use crate::a::*;\npub const h: u8 = 0;",
		"m.rs":   "pub fn c() {\n    crate::b::f();\n}\n",
	}, "", "crate::b::f", "a.rs:crate::a::f"},
	{"explicit import of a fn beside a const elsewhere", tree{
		"lib.rs": "mod a; mod n; mod m;",
		"a.rs":   "pub fn f() {}",
		"n.rs":   "pub const f: u8 = 0;",
		"m.rs":   "use crate::a::f;\npub fn c() {\n    f();\n}\n",
	}, "", "", "a.rs:crate::a::f"},
}

func TestRustValueItemShadowing(t *testing.T) {
	for _, layout := range []string{"src/", ""} {
		for _, tc := range rustValueShadowCases {
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

// TestRustValueItemShadowingFollowsIncrementalChanges adds and removes the
// shadowing item, and the item macro, through both update shapes; every step
// must match a fresh index of the same tree.
func TestRustValueItemShadowingFollowsIncrementalChanges(t *testing.T) {
	const caller = "pub fn c() {\n    f();\n}\n"
	steps := []struct {
		name, m string
		target  string
	}{
		{"glob only", "use crate::a::*;\n" + caller, "a.rs:crate::a::f"},
		{"const added", "use crate::a::*;\nconst f: fn() = || {};\n" + caller, ""},
		{"const removed", "use crate::a::*;\n" + caller, "a.rs:crate::a::f"},
		{"static added", "use crate::a::*;\nstatic f: fn() = || {};\n" + caller, ""},
		{"extern item added", "use crate::a::*;\nextern \"C\" { fn f(); }\n" + caller, ""},
		{"macro added", "use crate::a::*;\nmk!(f);\n" + caller, ""},
		{"macro removed", "use crate::a::*;\n" + caller, "a.rs:crate::a::f"},
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
	// A re-exporter's own const changes the answer for a caller in another
	// file that is not itself touched.
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{
			"lib.rs": "mod a; mod b; mod m;",
			"a.rs":   "pub fn f() {}",
			"b.rs":   "pub use crate::a::*;\n",
			"m.rs":   "pub fn c() {\n    crate::b::f();\n}\n",
		})
		assertRustCallTarget(t, r, "reexport", "m.rs", "crate::b::f", "a.rs:crate::a::f")
		for _, step := range []struct{ name, b, target string }{
			{"const added", "pub use crate::a::*;\npub const f: fn() = || {};\n", ""},
			{"const removed", "pub use crate::a::*;\n", "a.rs:crate::a::f"},
			{"macro added", "pub use crate::a::*;\nmk!(f);\n", ""},
		} {
			r.write(t, "b.rs", step.b)
			if scoped {
				r.update(t, "b.rs")
			} else {
				r.update(t)
			}
			r.assertFreshParity(t, step.name)
			assertRustCallTarget(t, r, step.name, "m.rs", "crate::b::f", step.target)
		}
	}
}

// TestRustValueItemProfileUpgradeConverges rebuilds the state a Rust v4 index
// left behind (no value-item evidence, the shadowed call bound to the glob's
// function) and requires one update to refresh every Rust file, settle on the
// fresh answer, and a second update to change nothing.
func TestRustValueItemProfileUpgradeConverges(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"lib.rs": "mod a; mod b; mod m; mod n;",
		"a.rs":   "pub fn f() {}",
		"b.rs":   "pub use crate::a::*;\npub const f: fn() = || {};\n",
		"m.rs":   "use crate::a::*;\nconst f: fn() = || {};\npub fn c() {\n    f();\n}\n",
		"n.rs":   "pub fn c() {\n    crate::b::f();\n}\n",
	})
	assertRustUnresolved(t, r, "m.rs", "f")
	assertRustUnresolved(t, r, "n.rs", "crate::b::f")
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
	// What v4 persisted: both shadowed calls bound to the glob's function.
	if _, err := db.Exec(`UPDATE edges SET dst_symbol_id=(SELECT id FROM symbols WHERE repo_id=? AND qualified_name='crate::a::f'), resolution_strategy='rust_use_scope', resolution_confidence='high' WHERE repo_id=? AND dst_name IN ('f','crate::b::f')`, r.repoID, r.repoID); err != nil {
		t.Fatal(err)
	}
	assertRustResolved(t, r, "m.rs", "f", "a.rs:crate::a::f")
	summary := r.update(t)
	if summary.FilesIndexed != 5 || len(summary.ParserProfileLanguages) != 1 || summary.ParserProfileLanguages[0] != "rust" {
		t.Fatalf("profile update = %+v", summary)
	}
	assertRustUnresolved(t, r, "m.rs", "f")
	assertRustUnresolved(t, r, "n.rs", "crate::b::f")
	r.assertFreshParity(t, "Rust v4 to v5 profile upgrade")
	if again := r.update(t); again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v", again)
	}
}

//go:build cgo

package indexer

import (
	"database/sql"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// rustCfgGateCases pin calls into, out of and through code that an inner
// #![cfg] (or a cfg_attr that may expand to one) or a cfg-gated inline module
// removes in some configuration. No configuration is evaluated, so none of
// them may bind. Each gated case was compiled with rustc 1.98.1 with and
// without --cfg foo: the call exists in at most one of the two builds. A ""
// target is the fail-closed answer.
var rustCfgGateCases = []struct {
	name   string
	files  tree
	src    string // defaults to m.rs
	call   string // defaults to crate::n::f
	target string // "" means the call must stay unresolved
}{
	// rustc: without foo, E0433 `n` not found; with foo, n::f runs.
	{"inner cfg on the callee file", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "#![cfg(foo)]\npub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "", ""},
	// rustc: without foo, m is absent; with foo, n::f runs.
	{"inner cfg on the caller file", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "pub fn f() {}\n",
		"m.rs":   "#![cfg(foo)]\npub fn c() {\n    crate::n::f();\n}\n",
	}, "", "", ""},
	{"inner cfg behind a doc comment", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "//! platform code\n#![allow(dead_code)]\n#![cfg(foo)]\npub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "", ""},
	{"inner cfg file imported by name", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "#![cfg(foo)]\npub fn f() {}\n",
		"m.rs":   "use crate::n::f;\npub fn c() {\n    f();\n}\n",
	}, "", "f", ""},
	{"inner cfg file imported by glob", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "#![cfg(foo)]\npub fn f() {}\n",
		"m.rs":   "use crate::n::*;\npub fn c() {\n    f();\n}\n",
	}, "", "f", ""},
	// rustc: without foo, E0433 `n` not found; with foo, a::f runs.
	{"re-export through an inner cfg file", tree{
		"lib.rs": "mod a; mod n; mod m;",
		"a.rs":   "pub fn f() {}\n",
		"n.rs":   "#![cfg(foo)]\npub use crate::a::f;\n",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "", ""},
	// rustc: without foo, E0433 `n` not found; with foo, n::sub::g runs.
	{"child module of an inner cfg file", tree{
		"lib.rs":   "mod n; mod m;",
		"n.rs":     "#![cfg(foo)]\npub mod sub;\n",
		"n/sub.rs": "pub fn g() {}\n",
		"m.rs":     "pub fn c() {\n    crate::n::sub::g();\n}\n",
	}, "", "crate::n::sub::g", ""},
	{"call inside an inner cfg file", tree{
		"lib.rs": "mod n;",
		"n.rs":   "#![cfg(foo)]\nfn g() {}\npub fn f() {\n    g();\n}\n",
	}, "n.rs", "g", ""},
	{"inner cfg on the crate root", tree{
		"lib.rs": "#![cfg(foo)]\nmod n; mod m;",
		"n.rs":   "pub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "", ""},
	// rustc: with foo alone, E0433 `n` not found; without foo, or with foo and
	// bar, n::f runs.
	{"inner cfg_attr that expands to a cfg", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "#![cfg_attr(foo, cfg(bar))]\npub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "", ""},
	// rustc: without foo, E0433 `x` not found; with foo, x::f runs.
	{"inner cfg on an inline module", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "pub mod x {\n    #![cfg(foo)]\n    pub fn f() {}\n}\n",
		"m.rs":   "pub fn c() {\n    crate::n::x::f();\n}\n",
	}, "", "crate::n::x::f", ""},
	// rustc: without foo, c is absent; with foo, f runs.
	{"inner cfg in the caller's body", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "pub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    #![cfg(foo)]\n    crate::n::f();\n}\n",
	}, "", "", ""},
	// rustc: without foo, E0599 no function `f` on S; with foo, S::f runs.
	{"inner cfg in an impl body", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "pub struct S;\nimpl S {\n    #![cfg(foo)]\n    pub fn f() {}\n}\n",
		"m.rs":   "pub fn c() {\n    crate::n::S::f();\n}\n",
	}, "", "crate::n::S::f", ""},
	// rustc: without foo, E0433 `n` not found; with foo, a::f runs.
	{"re-export inside a cfg-gated inline module", tree{
		"lib.rs": "mod a;\n#[cfg(foo)]\npub mod n {\n    pub use crate::a::f;\n}\nmod m;",
		"a.rs":   "pub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "", ""},
	{"re-export inside an inline module with an inner cfg", tree{
		"lib.rs": "mod a;\npub mod n {\n    #![cfg(foo)]\n    pub use crate::a::f;\n}\nmod m;",
		"a.rs":   "pub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "", ""},
	{"inner cfg through a nested cfg_attr", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "#![cfg_attr(foo, cfg_attr(bar, cfg(baz)))]\npub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "", ""},
	{"inner cfg in a trait body", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "pub trait T {\n    #![cfg(foo)]\n    fn f() {}\n}\npub struct S;\nimpl T for S {}\n",
		"m.rs":   "pub fn c() {\n    crate::n::S::f();\n}\n",
	}, "", "crate::n::S::f", ""},
	{"inner cfg after a shebang", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "#!/usr/bin/env run-cargo-script\n#![cfg(foo)]\npub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "", ""},
	{"inner cfg fn beside a glob of the same name", tree{
		"lib.rs": "mod a; mod n; mod m;",
		"a.rs":   "pub fn f() {}\n",
		"n.rs":   "use crate::a::*;\nfn f() {\n    #![cfg(foo)]\n}\npub fn c() {\n    f();\n}\n",
	}, "n.rs", "f", ""},
	{"glob re-export of an inner cfg file", tree{
		"lib.rs": "mod n; mod r; mod m;",
		"n.rs":   "#![cfg(foo)]\npub fn f() {}\n",
		"r.rs":   "pub use crate::n::*;\n",
		"m.rs":   "pub fn c() {\n    crate::r::f();\n}\n",
	}, "", "crate::r::f", ""},
	// -- a cfg on a statement or block gates the call site only: caller and
	// target exist in every configuration, and a gated target is refused on
	// its own. rustc: without foo, nothing runs; with foo, each call runs.
	{"cfg on statements and blocks", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "pub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    #[cfg(foo)]\n    crate::n::f();\n    #[cfg(foo)]\n    let _x = crate::n::f();\n    {\n        #![cfg(foo)]\n        crate::n::f();\n    }\n}\n",
	}, "", "", "n.rs:crate::n::f"},
	// -- attributes that remove nothing
	{"allow on the call statement", tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "pub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    #[allow(unused_must_use)]\n    crate::n::f();\n    {\n        #![allow(unused)]\n        crate::n::f();\n    }\n}\n",
	}, "", "", "n.rs:crate::n::f"},
	{"inner cfg_attr without a cfg", tree{
		"lib.rs": "#![cfg_attr(not(feature = \"std\"), no_std)]\n#![cfg_attr(docsrs, feature(doc_cfg))]\nmod n; mod m;",
		"n.rs":   "#![cfg_attr(feature = \"cfg\", allow(dead_code))]\n#![cfg_attr(docsrs, doc(cfg(feature = \"x\")))]\npub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	}, "", "", "n.rs:crate::n::f"},
	{"inner allow and doc", tree{
		"lib.rs": "//! crate docs\n#![allow(dead_code)]\nmod n; mod m;",
		"n.rs":   "#![doc = \"n\"]\n#![allow(unused)]\npub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    #![allow(unused)]\n    crate::n::f();\n}\n",
	}, "", "", "n.rs:crate::n::f"},
}

func TestRustCfgGate(t *testing.T) {
	for _, layout := range []string{"src/", ""} {
		for _, tc := range rustCfgGateCases {
			files := tree{}
			for path, content := range tc.files {
				files[layout+path] = content
			}
			src, call, target := tc.src, tc.call, tc.target
			if src == "" {
				src = "m.rs"
			}
			if call == "" {
				call = "crate::n::f"
			}
			if target != "" {
				target = layout + target
			}
			runRustCallEntrypoints(t, "layout="+layout+"/"+tc.name, files, layout+src, call, target)
		}
	}
}

// TestRustCfgGateFollowsIncrementalChanges adds and removes inner cfg gates on
// the callee file, the caller file and a declaring file through both update
// shapes; only the edited file is passed to a scoped update, and every step
// must match a fresh index.
func TestRustCfgGateFollowsIncrementalChanges(t *testing.T) {
	const caller = "pub fn c() {\n    crate::n::f();\n}\n"
	steps := []struct{ name, file, content, target string }{
		{"callee file gated", "n.rs", "#![cfg(foo)]\npub fn f() {}\n", ""},
		{"callee gate removed", "n.rs", "pub fn f() {}\n", "n.rs:crate::n::f"},
		{"caller file gated", "m.rs", "#![cfg(foo)]\n" + caller, ""},
		{"caller gate removed", "m.rs", caller, "n.rs:crate::n::f"},
		{"callee re-exports through a gated file", "n.rs", "#![cfg(foo)]\npub use crate::a::f;\n", ""},
		{"callee re-export ungated", "n.rs", "pub use crate::a::f;\n", "a.rs:crate::a::f"},
		{"crate root gated", "lib.rs", "#![cfg(foo)]\nmod a; mod n; mod m;", ""},
		{"crate root gate removed", "lib.rs", "mod a; mod n; mod m;", "a.rs:crate::a::f"},
		{"callee gated by cfg_attr", "n.rs", "#![cfg_attr(foo, cfg(bar))]\npub fn f() {}\n", ""},
		{"cfg_attr without a cfg", "n.rs", "#![cfg_attr(foo, allow(dead_code))]\npub fn f() {}\n", "n.rs:crate::n::f"},
		{"callee body gated", "n.rs", "pub fn f() {\n    #![cfg(foo)]\n}\n", ""},
		{"callee moved into an inline module gated", "n.rs", "pub mod x {\n    #![cfg(foo)]\n    pub fn f() {}\n}\npub use x::f;\n", ""},
		{"inline module gate removed", "n.rs", "pub mod x {\n    pub fn f() {}\n}\npub use x::f;\n", "n.rs:crate::n::x::f"},
		{"caller body gated", "m.rs", "pub fn c() {\n    #![cfg(foo)]\n    crate::n::f();\n}\n", ""},
		{"caller body ungated", "m.rs", caller, "n.rs:crate::n::x::f"},
	}
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{
			"lib.rs": "mod a; mod n; mod m;",
			"a.rs":   "pub fn f() {}\n",
			"n.rs":   "pub fn f() {}\n",
			"m.rs":   caller,
		})
		assertRustCallTarget(t, r, "initial", "m.rs", "crate::n::f", "n.rs:crate::n::f")
		for _, step := range steps {
			r.write(t, step.file, step.content)
			if scoped {
				r.update(t, step.file)
			} else {
				r.update(t)
			}
			r.assertFreshParity(t, step.name)
			assertRustCallTarget(t, r, step.name, "m.rs", "crate::n::f", step.target)
		}
	}
}

// TestRustCfgGateProfileUpgradeConverges rebuilds what a Rust v6 index left
// for a file under an inner #![cfg] (no gate evidence, the call bound high)
// and requires one update to reparse every Rust file and drop the edge, and a
// second update to change nothing; a fresh index agrees.
func TestRustCfgGateProfileUpgradeConverges(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"lib.rs": "mod n; mod m;",
		"n.rs":   "#![cfg(foo)]\npub fn f() {}\n",
		"m.rs":   "pub fn c() {\n    crate::n::f();\n}\n",
	})
	assertRustUnresolved(t, r, "m.rs", "crate::n::f")
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE files SET parser_profile='treesitter:rust:v6' WHERE repo_id=? AND language='rust'", r.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM rust_value_item_evidence WHERE repo_id=?", r.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE edges SET dst_symbol_id=(SELECT id FROM symbols WHERE repo_id=? AND qualified_name='crate::n::f'), resolution_strategy='rust_module_scope', resolution_confidence='high' WHERE repo_id=? AND dst_name='crate::n::f'`, r.repoID, r.repoID); err != nil {
		t.Fatal(err)
	}
	assertRustResolved(t, r, "m.rs", "crate::n::f", "n.rs:crate::n::f")
	summary := r.update(t)
	if summary.FilesIndexed != 3 || len(summary.ParserProfileLanguages) != 1 || summary.ParserProfileLanguages[0] != "rust" {
		t.Fatalf("profile update = %+v", summary)
	}
	assertRustUnresolved(t, r, "m.rs", "crate::n::f")
	r.assertFreshParity(t, "Rust v6 to v7 inner cfg upgrade")
	if again := r.update(t); again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v", again)
	}
}

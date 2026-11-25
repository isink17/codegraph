//go:build cgo

package indexer

import (
	"strings"
	"testing"
)

// rustNestedModuleCases pin where rustc 1.98.1 loads a `mod name;` declared
// inside an inline module: under the declaring file's module directory plus one
// directory per enclosing inline module. Each tree carries a decoy at the path
// that ignores the inline modules; a "" target is the fail-closed answer for a
// tree rustc rejects (E0583) or a location the resolver does not model.
var rustNestedModuleCases = []struct {
	name   string
	files  tree
	src    string
	call   string
	target string
}{
	{"inline module in lib.rs", tree{
		"src/lib.rs":   "pub mod m { pub mod sub; }\npub fn g() { m::sub::s(); }\n",
		"src/m/sub.rs": "pub fn s() {}\n",
		"src/sub.rs":   "pub fn s() {}\n",
	}, "src/lib.rs", "m::sub::s", "src/m/sub.rs:crate::m::sub::s"},
	{"inline module in lib.rs, only the decoy", tree{
		"src/lib.rs": "pub mod m { pub mod sub; }\npub fn g() { m::sub::s(); }\n",
		"src/sub.rs": "pub fn s() {}\n",
	}, "src/lib.rs", "m::sub::s", ""},
	{"two inline modules", tree{
		"src/lib.rs":   "pub mod a { pub mod b { pub mod c; } }\npub fn g() { a::b::c::s(); }\n",
		"src/a/b/c.rs": "pub fn s() {}\n",
		"src/c.rs":     "pub fn s() {}\n",
		"src/b/c.rs":   "pub fn s() {}\n",
		"src/a/c.rs":   "pub fn s() {}\n",
	}, "src/lib.rs", "a::b::c::s", "src/a/b/c.rs:crate::a::b::c::s"},
	{"two inline modules, only decoys", tree{
		"src/lib.rs": "pub mod a { pub mod b { pub mod c; } }\npub fn g() { a::b::c::s(); }\n",
		"src/c.rs":   "pub fn s() {}\n",
		"src/b/c.rs": "pub fn s() {}\n",
		"src/a/c.rs": "pub fn s() {}\n",
	}, "src/lib.rs", "a::b::c::s", ""},
	{"inline module in a non-mod-rs file", tree{
		"src/lib.rs":     "pub mod x;\npub fn g() { x::m::sub::s(); }\n",
		"src/x.rs":       "pub mod m { pub mod sub; }\n",
		"src/x/m/sub.rs": "pub fn s() {}\n",
		"src/x/sub.rs":   "pub fn s() {}\n",
		"src/m/sub.rs":   "pub fn s() {}\n",
	}, "src/lib.rs", "x::m::sub::s", "src/x/m/sub.rs:crate::x::m::sub::s"},
	{"inline module in a non-mod-rs file, only the decoy", tree{
		"src/lib.rs":   "pub mod x;\npub fn g() { x::m::sub::s(); }\n",
		"src/x.rs":     "pub mod m { pub mod sub; }\n",
		"src/x/sub.rs": "pub fn s() {}\n",
	}, "src/lib.rs", "x::m::sub::s", ""},
	{"inline module in mod.rs", tree{
		"src/lib.rs":     "pub mod x;\npub fn g() { x::m::sub::s(); }\n",
		"src/x/mod.rs":   "pub mod m { pub mod sub; }\n",
		"src/x/m/sub.rs": "pub fn s() {}\n",
		"src/x/sub.rs":   "pub fn s() {}\n",
	}, "src/lib.rs", "x::m::sub::s", "src/x/m/sub.rs:crate::x::m::sub::s"},
	{"inline module in mod.rs, only the decoy", tree{
		"src/lib.rs":   "pub mod x;\npub fn g() { x::m::sub::s(); }\n",
		"src/x/mod.rs": "pub mod m { pub mod sub; }\n",
		"src/x/sub.rs": "pub fn s() {}\n",
	}, "src/lib.rs", "x::m::sub::s", ""},
	{"inline module in main.rs", tree{
		"src/main.rs":  "mod m { pub mod sub; }\nfn main() { m::sub::s(); }\n",
		"src/m/sub.rs": "pub fn s() {}\n",
		"src/sub.rs":   "pub fn s() {}\n",
	}, "src/main.rs", "m::sub::s", "src/m/sub.rs:crate::m::sub::s"},
	{"inline module in main.rs, only the decoy", tree{
		"src/main.rs": "mod m { pub mod sub; }\nfn main() { m::sub::s(); }\n",
		"src/sub.rs":  "pub fn s() {}\n",
	}, "src/main.rs", "m::sub::s", ""},
	{"inline module in a bin directory root", tree{
		"src/bin/tool/main.rs":  "mod m { pub mod sub; }\nfn main() { m::sub::s(); }\n",
		"src/bin/tool/m/sub.rs": "pub fn s() {}\n",
		"src/bin/tool/sub.rs":   "pub fn s() {}\n",
	}, "src/bin/tool/main.rs", "m::sub::s", "src/bin/tool/m/sub.rs:crate::tool::m::sub::s"},
	{"inline module in a bin directory root, only the decoy", tree{
		"src/bin/tool/main.rs": "mod m { pub mod sub; }\nfn main() { m::sub::s(); }\n",
		"src/bin/tool/sub.rs":  "pub fn s() {}\n",
	}, "src/bin/tool/main.rs", "m::sub::s", ""},
	{"doc comment and allow on the inline module", tree{
		"src/lib.rs":   "/// docs\n#[allow(dead_code)]\npub mod m { pub mod sub; }\npub fn g() { m::sub::s(); }\n",
		"src/m/sub.rs": "pub fn s() {}\n",
		"src/sub.rs":   "pub fn s() {}\n",
	}, "src/lib.rs", "m::sub::s", "src/m/sub.rs:crate::m::sub::s"},
	{"same name at the crate root and in an inline module (root)", tree{
		"src/lib.rs":   "pub mod sub;\npub mod m { pub mod sub; }\npub fn g() { sub::s(); m::sub::t(); }\n",
		"src/sub.rs":   "pub fn s() {}\n",
		"src/m/sub.rs": "pub fn t() {}\n",
	}, "src/lib.rs", "sub::s", "src/sub.rs:crate::sub::s"},
	{"same name at the crate root and in an inline module (nested)", tree{
		"src/lib.rs":   "pub mod sub;\npub mod m { pub mod sub; }\npub fn g() { sub::s(); m::sub::t(); }\n",
		"src/sub.rs":   "pub fn s() {}\n",
		"src/m/sub.rs": "pub fn t() {}\n",
	}, "src/lib.rs", "m::sub::t", "src/m/sub.rs:crate::m::sub::t"},
	{"use of an item", tree{
		"src/lib.rs":   "pub mod m { pub mod sub; }\nuse crate::m::sub::s;\npub fn g() { s(); }\n",
		"src/m/sub.rs": "pub fn s() {}\n",
		"src/sub.rs":   "pub fn s() {}\n",
	}, "src/lib.rs", "s", "src/m/sub.rs:crate::m::sub::s"},
	{"use of an item, only the decoy", tree{
		"src/lib.rs": "pub mod m { pub mod sub; }\nuse crate::m::sub::s;\npub fn g() { s(); }\n",
		"src/sub.rs": "pub fn s() {}\n",
	}, "src/lib.rs", "s", ""},
	{"use of the module", tree{
		"src/lib.rs":   "pub mod m { pub mod sub; }\nuse m::sub;\npub fn g() { sub::s(); }\n",
		"src/m/sub.rs": "pub fn s() {}\n",
		"src/sub.rs":   "pub fn s() {}\n",
	}, "src/lib.rs", "sub::s", "src/m/sub.rs:crate::m::sub::s"},
	{"use of the module, only the decoy", tree{
		"src/lib.rs": "pub mod m { pub mod sub; }\nuse m::sub;\npub fn g() { sub::s(); }\n",
		"src/sub.rs": "pub fn s() {}\n",
	}, "src/lib.rs", "sub::s", ""},
	{"private child called from its inline module", tree{
		"src/lib.rs":   "pub mod m {\n    mod sub;\n    pub fn h() { self::sub::s(); }\n}\npub fn top() {}\n",
		"src/m/sub.rs": "pub fn s() { super::super::top(); }\n",
		"src/sub.rs":   "pub fn s() {}\n",
	}, "src/lib.rs", "self::sub::s", "src/m/sub.rs:crate::m::sub::s"},
	{"super path out of the nested file", tree{
		"src/lib.rs":   "pub mod m {\n    mod sub;\n    pub fn h() { self::sub::s(); }\n}\npub fn top() {}\n",
		"src/m/sub.rs": "pub fn s() { super::super::top(); }\n",
		"src/sub.rs":   "pub fn top() {}\n",
	}, "src/m/sub.rs", "super::super::top", "src/lib.rs:crate::top"},
	// rustc loads src/p/sub.rs here; #[path] is not evaluated, so the
	// child stays unlinked rather than taking src/m/sub.rs or src/sub.rs.
	{"path on the inline module", tree{
		"src/lib.rs":   "#[path = \"p\"]\npub mod m { pub mod sub; }\npub fn g() { m::sub::s(); }\n",
		"src/p/sub.rs": "pub fn s() {}\n",
		"src/m/sub.rs": "pub fn s() {}\n",
		"src/sub.rs":   "pub fn s() {}\n",
	}, "src/lib.rs", "m::sub::s", ""},
	{"cfg_attr path on the inline module", tree{
		"src/lib.rs":   "#[cfg_attr(all(), path = \"p\")]\npub mod m { pub mod sub; }\npub fn g() { m::sub::s(); }\n",
		"src/p/sub.rs": "pub fn s() {}\n",
		"src/m/sub.rs": "pub fn s() {}\n",
	}, "src/lib.rs", "m::sub::s", ""},
	{"path on the inline module in mod.rs", tree{
		"src/lib.rs":     "pub mod x;\npub fn g() { x::m::sub::s(); }\n",
		"src/x/mod.rs":   "#[path = \"p\"]\npub mod m { pub mod sub; }\n",
		"src/x/p/sub.rs": "pub fn s() {}\n",
		"src/x/m/sub.rs": "pub fn s() {}\n",
	}, "src/lib.rs", "x::m::sub::s", ""},
	{"path on the outer of two inline modules", tree{
		"src/lib.rs":   "#[path = \"p\"]\npub mod a { pub mod b { pub mod c; } }\npub fn g() { a::b::c::s(); }\n",
		"src/p/b/c.rs": "pub fn s() {}\n",
		"src/a/b/c.rs": "pub fn s() {}\n",
		"src/b/c.rs":   "pub fn s() {}\n",
	}, "src/lib.rs", "a::b::c::s", ""},
	// Without --cfg foo the module is gone (E0433); a cfg is not evaluated.
	{"cfg on the inline module", tree{
		"src/lib.rs":   "#[cfg(foo)]\npub mod m { pub mod sub; }\npub fn g() { m::sub::s(); }\n",
		"src/m/sub.rs": "pub fn s() {}\n",
		"src/sub.rs":   "pub fn s() {}\n",
	}, "src/lib.rs", "m::sub::s", ""},
}

func TestRustNestedModuleFiles(t *testing.T) {
	for _, tc := range rustNestedModuleCases {
		runRustCallEntrypoints(t, tc.name, tc.files, tc.src, tc.call, tc.target)
	}
}

// TestRustNestedModuleFilesFollowIncrementalChanges adds, gates, moves and
// removes the nested module through both update shapes; every step must match
// a fresh index of the same tree.
func TestRustNestedModuleFilesFollowIncrementalChanges(t *testing.T) {
	const caller = "pub fn g() { m::sub::s(); }\n"
	const decl = "pub mod m { pub mod sub; }\n"
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{"src/lib.rs": decl + caller, "src/sub.rs": "pub fn s() {}\n"})
		update := func(paths ...string) {
			if scoped {
				r.update(t, paths...)
			} else {
				r.update(t)
			}
		}
		assertRustCallTarget(t, r, "only the decoy", "src/lib.rs", "m::sub::s", "")
		const want = "src/m/sub.rs:crate::m::sub::s"
		for _, step := range []struct {
			name, path, content, target string
		}{
			{"nested file added", "src/m/sub.rs", "pub fn s() {}\n", want},
			{"inline module gated", "src/lib.rs", "#[cfg(foo)]\n" + decl + caller, ""},
			{"gate removed", "src/lib.rs", decl + caller, want},
			{"inline module moved by path", "src/lib.rs", "#[path = \"p\"]\n" + decl + caller, ""},
			{"path removed", "src/lib.rs", decl + caller, want},
			{"declaration moved to the crate root", "src/lib.rs", "pub mod sub;\npub mod m {}\n" + caller, ""},
			{"declaration moved back", "src/lib.rs", decl + caller, want},
			{"inline module renamed", "src/lib.rs", "pub mod n { pub mod sub; }\npub mod m {}\n" + caller, ""},
			{"inline module name restored", "src/lib.rs", decl + caller, want},
			{"nested file removed", "src/m/sub.rs", "", ""},
		} {
			if step.content == "" {
				r.remove(t, step.path)
			} else {
				r.write(t, step.path, step.content)
			}
			update(step.path)
			r.assertFreshParity(t, step.name)
			assertRustCallTarget(t, r, step.name, "src/lib.rs", "m::sub::s", step.target)
		}
	}
}

// TestResolverPolicyRustUpgradeRelinksNestedModuleFile simulates a graph an
// older resolver decided: the call bound to the decoy file, under the previous
// policy marker. An ordinary update with no source change must re-decide it.
func TestResolverPolicyRustUpgradeRelinksNestedModuleFile(t *testing.T) {
	r := newRegisteredPolicyRepo(t, tree{
		"src/lib.rs":   "pub mod m { pub mod sub; }\npub fn g() { m::sub::s(); }\n",
		"src/m/sub.rs": "pub fn s() {}\n",
		"src/sub.rs":   "pub fn s() {}\n",
	})
	const want = "src/m/sub.rs:crate::m::sub::s"
	assertRustCallTarget(t, r, "fresh", "src/lib.rs", "m::sub::s", want)
	r.bind(t, "src/lib.rs", "m::sub::s", "crate::sub::s")
	r.exec(t, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, r.markerKey("rust"), "2")

	summary := r.update(t)
	if summary.FilesIndexed != 0 {
		t.Fatalf("policy-only upgrade parsed source: %+v", summary)
	}
	if got := strings.Join(summary.ResolverPolicyLanguages, ","); got != "rust" {
		t.Fatalf("re-decided languages = %q, want rust", got)
	}
	assertRustCallTarget(t, r, "upgrade update", "src/lib.rs", "m::sub::s", want)
	if got := r.refTarget(t, "src/lib.rs", "m::sub::s"); got != "crate::m::sub::s" {
		t.Fatalf("reference identity = %q", got)
	}
}

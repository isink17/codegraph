//go:build cgo

package indexer

import (
	"slices"
	"strings"
	"testing"
)

// One-file edits to a Rust crate must leave the graph a fresh index of the same
// tree builds, including the persisted crate membership, while the edits that
// matter reach callers in files the update did not touch.
func TestRustOneFileEditsKeepFreshParity(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"src/lib.rs":          "mod a; mod b; mod c; mod barrel; mod caller; mod nested;\n",
		"src/a.rs":            "pub fn helper() {}\npub struct S;\nimpl S { pub fn new() -> Self { S } }\n",
		"src/b.rs":            "pub fn other() {}\n",
		"src/c.rs":            "pub fn third() {}\n",
		"src/barrel.rs":       "pub use crate::a::helper;\npub use crate::b::*;\n",
		"src/nested/mod.rs":   "pub mod inner;\n",
		"src/nested/inner.rs": "pub fn deep() {}\n",
		"src/caller.rs": "use crate::a;\nuse crate::b::*;\nfn run() {\n    a::helper();\n    crate::barrel::helper();\n" +
			"    crate::barrel::other();\n    other();\n    a::S::new();\n    crate::nested::inner::deep();\n}\n",
		"alt/main.rs": "mod util;\nfn run() { crate::util::helper(); }\n",
		"alt/util.rs": "pub fn helper() {}\n",
	})
	step := func(name, path, content string) {
		t.Helper()
		if content == "" {
			r.remove(t, path)
		} else {
			r.write(t, path, content)
		}
		r.update(t, path)
		r.assertFreshParityAndCrateRoots(t, name)
	}
	resolved := func(name, target string) { t.Helper(); assertRustResolved(t, r, "src/caller.rs", name, target) }
	unresolved := func(name string) { t.Helper(); assertRustUnresolved(t, r, "src/caller.rs", name) }
	assertBaseline := func() {
		t.Helper()
		resolved("a::helper", "src/a.rs")
		resolved("crate::barrel::helper", "src/a.rs")
		resolved("crate::barrel::other", "src/b.rs")
		resolved("other", "src/b.rs")
		resolved("a::S::new", "src/a.rs")
		resolved("crate::nested::inner::deep", "src/nested/inner.rs")
		assertRustResolved(t, r, "alt/main.rs", "crate::util::helper", "alt/util.rs")
	}
	assertBaseline()
	r.assertFreshParityAndCrateRoots(t, "index")

	before, beforeRoots := r.projection(t), crateRoots(t, r.dbPath)
	if summary := r.update(t); summary.FilesIndexed != 0 {
		t.Fatalf("no-op update indexed files: %+v", summary)
	}
	if after := r.projection(t); !slices.Equal(before, after) {
		t.Fatalf("no-op update changed the graph:\n%s", projectionDiff(before, after))
	}
	if after := crateRoots(t, r.dbPath); !slices.Equal(beforeRoots, after) {
		t.Fatalf("no-op update changed crate membership: %v -> %v", beforeRoots, after)
	}

	a := "pub fn helper() {}\npub struct S;\nimpl S { pub fn new() -> Self { S } }\n"
	step("comment-only edit", "src/a.rs", "// touched\n"+a)
	assertBaseline()

	step("declaration removed", "src/a.rs", "pub struct S;\nimpl S { pub fn new() -> Self { S } }\n")
	unresolved("a::helper")
	unresolved("crate::barrel::helper")
	resolved("a::S::new", "src/a.rs")
	step("declaration restored", "src/a.rs", a)
	assertBaseline()

	step("glob competitor introduced", "src/caller.rs", "use crate::c::*;\n"+r.read(t, "src/caller.rs"))
	resolved("other", "src/b.rs")
	step("ambiguity introduced in another file", "src/c.rs", "pub fn third() {}\npub fn other() {}\n")
	unresolved("other")
	step("ambiguity removed in another file", "src/c.rs", "pub fn third() {}\n")
	resolved("other", "src/b.rs")

	step("re-export retargeted", "src/barrel.rs", "pub use crate::c::third as helper;\npub use crate::b::*;\n")
	resolved("crate::barrel::helper", "src/c.rs")
	resolved("a::helper", "src/a.rs")
	step("named re-export shadows a glob re-export", "src/barrel.rs", "pub use crate::a::helper;\npub use crate::b::*;\npub use crate::c::third as other;\n")
	resolved("crate::barrel::other", "src/c.rs")
	resolved("other", "src/b.rs")
	step("re-export restored", "src/barrel.rs", "pub use crate::a::helper;\npub use crate::b::*;\n")
	resolved("crate::barrel::helper", "src/a.rs")
	resolved("crate::barrel::other", "src/b.rs")

	step("caller import rebound", "src/caller.rs", strings.Replace(r.read(t, "src/caller.rs"), "use crate::a;", "use crate::c as a;", 1))
	unresolved("a::helper")
	step("caller import restored", "src/caller.rs", strings.Replace(r.read(t, "src/caller.rs"), "use crate::c as a;", "use crate::a;", 1))
	resolved("a::helper", "src/a.rs")

	step("module declaration removed", "src/nested/mod.rs", "// no modules\n")
	unresolved("crate::nested::inner::deep")
	step("module declaration restored", "src/nested/mod.rs", "pub mod inner;\n")
	resolved("crate::nested::inner::deep", "src/nested/inner.rs")

	step("module file deleted", "src/b.rs", "")
	unresolved("other")
	unresolved("crate::barrel::other")
	step("module file recreated", "src/b.rs", "pub fn other() {}\n")
	resolved("other", "src/b.rs")
	resolved("crate::barrel::other", "src/b.rs")

	step("crate root comment-only edit", "src/lib.rs", "// root\nmod a; mod b; mod c; mod barrel; mod caller; mod nested;\n")
	assertRustResolved(t, r, "alt/main.rs", "crate::util::helper", "alt/util.rs")
	resolved("a::helper", "src/a.rs")
}

func (r *lifecycleRepo) read(t *testing.T, rel string) string {
	t.Helper()
	content, ok := r.currentTree(t)[rel]
	if !ok {
		t.Fatalf("read %s: missing", rel)
	}
	return content
}

// assertFreshParityAndCrateRoots compares the graph and the persisted crate
// membership with one fresh index of the same tree.
func (r *lifecycleRepo) assertFreshParityAndCrateRoots(t *testing.T, step string) {
	t.Helper()
	fresh := newLifecycleRepo(t, r.currentTree(t))
	if diff := projectionDiff(fresh.projection(t), r.projection(t)); diff != "" {
		t.Fatalf("%s: update diverges from a fresh index of the same tree:\n%s", step, diff)
	}
	if got, want := crateRoots(t, r.dbPath), crateRoots(t, fresh.dbPath); !slices.Equal(got, want) {
		t.Fatalf("%s: crate membership diverges from a fresh index:\nupdate: %v\nfresh:  %v", step, got, want)
	}
}

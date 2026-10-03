//go:build cgo

package indexer

import "testing"

// rustPrivateVisibilityCases pin Rust's private visibility: an item without
// `pub` (or with `pub(self)`) is visible in its own module and every module
// nested inside it, and nowhere else. Each case was checked against rustc.
var rustPrivateVisibilityCases = []struct {
	name      string
	files     tree
	src, call string
	target    string // "" means the call must stay unresolved
}{
	{"same module", tree{
		"lib.rs": "fn helper() {}\npub fn caller() {\n    helper();\n}\n",
	}, "lib.rs", "helper", "lib.rs:crate::helper"},
	{"same file in a child module", tree{
		"lib.rs": "mod m;",
		"m.rs":   "fn inner() {}\npub fn outer() {\n    inner();\n}\n",
	}, "m.rs", "inner", "m.rs:crate::m::inner"},
	{"pub(self) in the same module", tree{
		"lib.rs": "pub(self) fn helper() {}\npub fn caller() {\n    helper();\n}\n",
	}, "lib.rs", "helper", "lib.rs:crate::helper"},
	{"child reaches parent through super", tree{
		"lib.rs": "mod m; fn helper() {}",
		"m.rs":   "pub fn outer() {\n    super::helper();\n}\n",
	}, "m.rs", "super::helper", "lib.rs:crate::helper"},
	{"child reaches parent through crate", tree{
		"lib.rs": "mod m; fn helper() {}",
		"m.rs":   "pub fn outer() {\n    crate::helper();\n}\n",
	}, "m.rs", "crate::helper", "lib.rs:crate::helper"},
	{"child imports parent item", tree{
		"lib.rs": "mod m; fn helper() {}",
		"m.rs":   "use super::helper;\npub fn outer() {\n    helper();\n}\n",
	}, "m.rs", "helper", "lib.rs:crate::helper"},
	{"grandchild reaches root", tree{
		"lib.rs": "mod m; fn helper() {}",
		"m.rs":   "mod n;",
		"m/n.rs": "pub fn deep() {\n    crate::helper();\n}\n",
	}, "m/n.rs", "crate::helper", "lib.rs:crate::helper"},
	// A bare name does not see the parent's items without an import.
	{"child bare name", tree{
		"lib.rs": "mod m; fn helper() {}",
		"m.rs":   "pub fn outer() {\n    helper();\n}\n",
	}, "m.rs", "helper", ""},
	{"sibling denied", tree{
		"lib.rs": "mod a; mod b;",
		"a.rs":   "fn helper() {}",
		"b.rs":   "pub fn outer() {\n    crate::a::helper();\n}\n",
	}, "b.rs", "crate::a::helper", ""},
	{"sibling denied pub(self)", tree{
		"lib.rs": "mod a; mod b;",
		"a.rs":   "pub(self) fn helper() {}",
		"b.rs":   "pub fn outer() {\n    crate::a::helper();\n}\n",
	}, "b.rs", "crate::a::helper", ""},
	{"parent denied a child's item", tree{
		"lib.rs": "mod a;\npub fn caller() {\n    a::helper();\n}\n",
		"a.rs":   "fn helper() {}",
	}, "lib.rs", "a::helper", ""},
	{"pub(crate) sibling", tree{
		"lib.rs": "mod a; mod b;",
		"a.rs":   "pub(crate) fn helper() {}",
		"b.rs":   "pub fn outer() {\n    crate::a::helper();\n}\n",
	}, "b.rs", "crate::a::helper", "a.rs:crate::a::helper"},
	{"pub(super) sibling", tree{
		"lib.rs": "mod a; mod b;",
		"a.rs":   "pub(super) fn helper() {}",
		"b.rs":   "pub fn outer() {\n    crate::a::helper();\n}\n",
	}, "b.rs", "crate::a::helper", "a.rs:crate::a::helper"},
	// `pub(in path)` is recorded but not modelled, so it stays fail-closed.
	{"pub(in crate) sibling", tree{
		"lib.rs": "mod a; mod b;",
		"a.rs":   "pub(in crate) fn helper() {}",
		"b.rs":   "pub fn outer() {\n    crate::a::helper();\n}\n",
	}, "b.rs", "crate::a::helper", ""},
	// Another crate root's private `crate::helper` is not the caller's.
	{"other crate root", tree{
		"src/lib.rs":  "fn helper() {}",
		"src/main.rs": "fn main() {\n    helper();\n}\n",
	}, "src/main.rs", "helper", ""},
}

func TestRustPrivateVisibility(t *testing.T) {
	for _, tc := range rustPrivateVisibilityCases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tc.files)
			assertRustCallTarget(t, r, "fresh", tc.src, tc.call, tc.target)
			for _, ep := range []struct {
				name string
				run  func() error
			}{
				{"repo", func() error { _, err := r.store.ResolveEdges(r.ctx, r.repoID); return err }},
				{"paths", func() error { return r.store.ResolveEdgesForPaths(r.ctx, r.repoID, []string{tc.src}) }},
				{"names", func() error {
					_, err := r.store.ResolveEdgesForNamesWithStats(r.ctx, r.repoID, []string{tc.call})
					return err
				}},
				{"paths+names", func() error {
					_, err := r.store.ResolveEdgesForPathsAndNames(r.ctx, r.repoID, []string{tc.src}, []string{tc.call})
					return err
				}},
			} {
				if _, err := r.raw(t).ExecContext(r.ctx, `UPDATE edges SET dst_symbol_id=NULL,resolution_strategy='',resolution_confidence='' WHERE repo_id=? AND dst_name=?`, r.repoID, tc.call); err != nil {
					t.Fatal(err)
				}
				if err := ep.run(); err != nil {
					t.Fatalf("%s: %v", ep.name, err)
				}
				assertRustCallTarget(t, r, ep.name, tc.src, tc.call, tc.target)
			}
		})
	}
}

// TestRustPrivateVisibilityFollowsIncrementalChanges adds and removes a private
// item and narrows a crate-visible one, through both update shapes; every step
// must match a fresh index of the same tree.
func TestRustPrivateVisibilityFollowsIncrementalChanges(t *testing.T) {
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{
			"lib.rs": "mod m; mod a; mod b;",
			"m.rs":   "pub fn outer() {\n    super::helper();\n}\n",
			"a.rs":   "pub(crate) fn shared() {}",
			"b.rs":   "pub fn user() {\n    crate::a::shared();\n}\n",
		})
		update := func(paths ...string) {
			if scoped {
				r.update(t, paths...)
			} else {
				r.update(t)
			}
		}
		assertRustCallTarget(t, r, "no helper", "m.rs", "super::helper", "")
		r.write(t, "lib.rs", "mod m; mod a; mod b; fn helper() {}")
		update("lib.rs")
		r.assertFreshParity(t, "private helper added")
		assertRustCallTarget(t, r, "private helper added", "m.rs", "super::helper", "lib.rs:crate::helper")
		r.write(t, "lib.rs", "mod m; mod a; mod b;")
		update("lib.rs")
		r.assertFreshParity(t, "private helper removed")
		assertRustCallTarget(t, r, "private helper removed", "m.rs", "super::helper", "")

		assertRustCallTarget(t, r, "crate visible", "b.rs", "crate::a::shared", "a.rs:crate::a::shared")
		r.write(t, "a.rs", "fn shared() {}")
		update("a.rs")
		r.assertFreshParity(t, "narrowed to private")
		assertRustCallTarget(t, r, "narrowed to private", "b.rs", "crate::a::shared", "")
	}
}

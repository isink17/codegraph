//go:build cgo

package treesitter

import (
	"context"
	"runtime"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// rustExternalModules parses src as the file at path and returns the
// external_path evidence of every out-of-line `mod name;` keyed by name.
func rustExternalModules(t *testing.T, path, src string) map[string]string {
	t.Helper()
	pf, err := NewRust().Parse(context.Background(), path, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range pf.Scope.Modules {
		if !m.Inline {
			out[m.Name] = m.ExternalPath
		}
	}
	return out
}

// TestRustModuleEvidenceIsRelativeToDeclaringFile pins external_path to the
// candidate stem relative to the declaring file's directory: siblings for a
// directory owner (lib.rs, main.rs, mod.rs), `<stem>/name` for any other file.
// The store joins it with the declaring file's logical path; the adapter never
// sees or persists the repository layout above that file.
func TestRustModuleEvidenceIsRelativeToDeclaringFile(t *testing.T) {
	cases := []struct {
		path, want string
	}{
		{"/home/a/repo/src/lib.rs", "foo"},
		{"/home/a/repo/src/main.rs", "foo"},
		{"/home/a/repo/src/foo/mod.rs", "foo"},
		{"/home/a/repo/src/bar.rs", "bar/foo"},
		{"/home/a/repo/src/bin/tool.rs", "tool/foo"},
		{"lib.rs", "foo"},
	}
	for _, tc := range cases {
		got := rustExternalModules(t, tc.path, "mod foo;\nmod inline { }\n")
		if got["foo"] != tc.want {
			t.Errorf("%s: external_path(foo) = %q, want %q", tc.path, got["foo"], tc.want)
		}
		if _, inline := got["inline"]; inline {
			t.Errorf("%s: inline module carried external_path", tc.path)
		}
	}
}

// TestRustModuleEvidenceIgnoresCheckoutPrefix is the portability contract: the
// same logical file parsed under two native checkout roots persists identical
// module evidence. The old contract embedded the absolute root, so a DB indexed
// at one path described another checkout's files.
func TestRustModuleEvidenceIgnoresCheckoutPrefix(t *testing.T) {
	const src = "mod foo;\n"
	a := rustExternalModules(t, "/home/a/repo/src/lib.rs", src)
	b := rustExternalModules(t, "/mnt/other/checkout/src/lib.rs", src)
	if a["foo"] != b["foo"] || a["foo"] == "" {
		t.Fatalf("external_path differs by checkout root: %q vs %q", a["foo"], b["foo"])
	}
	nestedA := rustExternalModules(t, "/home/a/repo/src/bar.rs", src)
	nestedB := rustExternalModules(t, "/mnt/other/checkout/src/bar.rs", src)
	if nestedA["foo"] != nestedB["foo"] || nestedA["foo"] != "bar/foo" {
		t.Fatalf("nested external_path differs by checkout root: %q vs %q", nestedA["foo"], nestedB["foo"])
	}
}

// TestRustModuleEvidenceKeepsBackslashFileNameAsData: on POSIX a backslash in
// the declaring file's name is filename data; the stem must carry it verbatim
// so the store's exact join meets `src/x\y/foo.rs`, never `src/x/y/foo.rs`.
func TestRustModuleEvidenceKeepsBackslashFileNameAsData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a backslash cannot be filename data on Windows")
	}
	got := rustExternalModules(t, `/home/a/repo/src/x\y.rs`, "mod foo;\n")
	if got["foo"] != `x\y/foo` {
		t.Fatalf("external_path(foo) = %q, want %q", got["foo"], `x\y/foo`)
	}
}

// TestRustValueItemEvidence pins which module-level syntax is recorded as a
// value item: const, static and extern-block items by name, any item-level
// macro invocation (statement or braced form, in an extern block too) and an
// unparsable span as an unread expansion. Impl bodies, function bodies and
// macro definitions record nothing, and an inline module's items carry the
// inline module's own owner.
func TestRustValueItemEvidence(t *testing.T) {
	src := `const A: u8 = 1;
static mut B: u8 = 2;
extern "C" { fn c(); static D: u8; mk!(); }
mk!(e);
thread_local! { static F: u8 = 0; }
macro_rules! mk { () => {} }
struct S;
impl S { const G: u8 = 1; mk!(); }
fn body() { const H: u8 = 1; mk!(); }
mod n { const I: u8 = 1; mk!(j); }
`
	pf, err := NewRust().Parse(context.Background(), "src/lib.rs", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, item := range pf.Scope.RustValueItems {
		got[item.OwnerModule+" "+item.Kind+" "+item.Name] = true
	}
	want := []string{
		"crate decl A", "crate decl B", "crate decl c", "crate decl D",
		"crate macro mk", "crate macro thread_local",
		"crate::n decl I", "crate::n macro mk",
	}
	if len(got) != len(want) {
		t.Fatalf("value items = %v, want exactly %v", got, want)
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("value items = %v, missing %q", got, w)
		}
	}
}

// TestRustAttributeEvidence pins which attributes leave an item proven: only
// the built-in, non-generative ones, and a derive only when every entry is a
// built-in derive no import, macro or glob could shadow.
func TestRustAttributeEvidence(t *testing.T) {
	cases := []struct {
		name, src string
		want      []string
	}{
		{"builtin only", "#[derive(Debug, Clone)]\n#[allow(dead_code)]\n/// doc\nstruct S;\n#[inline]\n#[cfg(unix)]\nfn f() {}\n", nil},
		{"custom derive", "#[derive(Debug, procgen::Gen)]\nstruct S;\n", []string{"crate macro "}},
		{"attribute macro on a fn", "#[tokio::main]\nasync fn f() {}\n", []string{"crate macro ", "crate unproven crate::f"}},
		{"attribute macro on a method", "struct S;\nimpl S {\n    #[rename]\n    fn m() {}\n}\n", []string{"crate unproven crate::S::m"}},
		{"attribute macro on an impl", "struct S;\n#[x]\nimpl S {\n    fn m() {}\n}\n", []string{"crate macro ", "crate unproven crate::S"}},
		{"attribute macro on a module", "#[x]\nmod n {\n    pub fn f() {}\n}\n", []string{"crate macro ", "crate unproven crate::n"}},
		{"cfg_attr", "#[cfg_attr(a, derive(Debug))]\nstruct S;\n", []string{"crate macro ", "crate unproven crate::S"}},
		{"derive alias", "use p::Gen as Clone;\n#[derive(Clone)]\nstruct S;\n", []string{"crate macro "}},
		{"std import of a builtin name", "use std::fmt::Debug;\n#[derive(Debug)]\nstruct S;\n", nil},
		{"glob is not counted", "use crate::a::*;\n#[derive(Debug)]\nstruct S;\n", nil},
		{"macro_use", "#[macro_use]\nextern crate p;\n#[inline]\nfn f() {}\n", []string{"crate macro ", "crate unproven crate::f"}},
		{"macro named like a builtin", "macro_rules! inline { () => {} }\n#[inline]\nfn f() {}\n", []string{"crate macro ", "crate unproven crate::f"}},
		{"block item is the block scope's concern", "fn f() {\n    #[derive(p::G)]\n    struct S;\n}\n", nil},
	}
	for _, tc := range cases {
		pf, err := NewRust().Parse(context.Background(), "src/lib.rs", []byte(tc.src))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, item := range pf.Scope.RustValueItems {
			if item.Kind != graph.RustValueItemDecl {
				got[item.OwnerModule+" "+item.Kind+" "+item.Name] = true
			}
		}
		for _, w := range tc.want {
			if !got[w] {
				t.Errorf("%s: missing %q in %v", tc.name, w, got)
			}
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: value items = %v, want exactly %v", tc.name, got, tc.want)
		}
	}
}

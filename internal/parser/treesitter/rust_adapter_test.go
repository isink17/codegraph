//go:build cgo

package treesitter

import (
	"context"
	"runtime"
	"testing"
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

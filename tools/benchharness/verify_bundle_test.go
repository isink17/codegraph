package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// testVerifyBundle runs from TestRunFixtureReproducible, which builds bin.
func testVerifyBundle(t *testing.T, bin string) {
	t.Helper()
	const manifest = "testdata/manifests/fixtures.json"
	root := t.TempDir()
	good := filepath.Join(root, "good")
	if err := run([]string{"produce", "--manifest", manifest, "--codegraph", bin, "--sha", testSHA, "--date", "2026-10-09T00:00:00Z", "--out", good}, io.Discard); err != nil {
		t.Fatal(err)
	}
	verify := func(dir, binary string) (string, error) {
		var out bytes.Buffer
		err := run([]string{"verify-bundle", "--bundle", dir, "--manifest", manifest, "--codegraph", binary}, &out)
		return out.String(), err
	}
	if out, err := verify(good, bin); err != nil || !strings.Contains(out, "5 files match") {
		t.Fatalf("intact bundle: %v\n%s", err, out)
	}

	// Each case starts from a copy of the intact bundle. resum rewrites
	// SHA256SUMS so that only the rerun can notice the change.
	copyBundle := func(name string) string {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		entries, _ := os.ReadDir(good)
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(good, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	edit := func(dir, name, old, new string) {
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte(old)) {
			t.Fatalf("%s does not contain %q", name, old)
		}
		if err := os.WriteFile(path, bytes.Replace(b, []byte(old), []byte(new), 1), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resum := func(dir string) {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			if e.Name() != "SHA256SUMS" {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		var sums bytes.Buffer
		for _, n := range names {
			b, _ := os.ReadFile(filepath.Join(dir, n))
			fmt.Fprintf(&sums, "%s  %s\n", hashBytes(b), n)
		}
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), sums.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	otherBin := filepath.Join(root, "codegraph-other")
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherBin, append(b, 0), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, want string
		mutate     func(dir string)
		binary     string
	}{
		{"modified record, checksums updated", "go-calls.json: differs from the rerun", func(d string) {
			edit(d, "go-calls.json", `"value": 7`, `"value": 8`)
			resum(d)
		}, bin},
		{"modified record, checksums stale", "go-calls.json: sha256", func(d string) {
			edit(d, "go-calls.json", `"value": 7`, `"value": 8`)
		}, bin},
		{"modified SHA256SUMS", "summary.md: sha256", func(d string) {
			b, _ := os.ReadFile(filepath.Join(d, "summary.md"))
			edit(d, "SHA256SUMS", hashBytes(b), strings.Repeat("0", 64))
		}, bin},
		{"missing file", "summary.md: listed in SHA256SUMS but missing", func(d string) {
			os.Remove(filepath.Join(d, "summary.md"))
		}, bin},
		{"unlisted file", "extra.json: not listed in SHA256SUMS", func(d string) {
			os.WriteFile(filepath.Join(d, "extra.json"), []byte("{}"), 0o644)
		}, bin},
		{"different binary", "binary sha256", func(string) {}, otherBin},
		{"subdirectory", "sub is not a regular file", func(d string) {
			os.Mkdir(filepath.Join(d, "sub"), 0o755)
		}, bin},
		{"symlink", "summary.md is not a regular file", func(d string) {
			os.Remove(filepath.Join(d, "summary.md"))
			os.Symlink(filepath.Join(good, "summary.md"), filepath.Join(d, "summary.md"))
		}, bin},
		{"CRLF SHA256SUMS", `SHA256SUMS: differs from the rerun`, func(d string) {
			b, _ := os.ReadFile(filepath.Join(d, "SHA256SUMS"))
			os.WriteFile(filepath.Join(d, "SHA256SUMS"), bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n")), 0o644)
		}, bin},
		{"different recorded commit", "go-calls.json: differs from the rerun", func(d string) {
			edit(d, "MANIFEST.json", testSHA, strings.Repeat("c", 40))
			resum(d)
		}, bin},
	}
	for i, c := range cases {
		dir := copyBundle(fmt.Sprintf("case%d", i))
		c.mutate(dir)
		out, err := verify(dir, c.binary)
		if err == nil || !strings.Contains(out+err.Error(), c.want) {
			t.Errorf("%s: err %v, want report containing %q, got:\n%s", c.name, err, c.want, out)
		}
	}
}

func TestParseSumsRejectsMalformedAndDuplicateLines(t *testing.T) {
	h := strings.Repeat("a", 64)
	for _, in := range []string{
		h + " x.json\n",                     // one space
		h + "  ../x.json\n",                 // path
		h + "  SHA256SUMS\n",                // self
		h + "  x.json\n" + h + "  x.json\n", // duplicate
		strings.ToUpper(h) + "  x.json\n",   // case
	} {
		if _, err := parseSums([]byte(in)); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
	if got, err := parseSums([]byte(h + "  x.json\n")); err != nil || got["x.json"] != h {
		t.Fatalf("valid line: %v %v", got, err)
	}
}

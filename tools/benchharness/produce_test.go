package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProduceUsesLocalBinaryInsteadOfPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell executables")
	}
	manifest, err := filepath.Abs("testdata/manifests/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	for dir, label := range map[string]string{root: "local", pathDir: "PATH"} {
		if err := os.WriteFile(filepath.Join(dir, "codegraph"), []byte("#!/bin/sh\necho "+label+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", pathDir)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Error(err)
		}
	})
	stop := errors.New("stop after checking execution")
	err = produce([]string{"--manifest", manifest, "--codegraph", "codegraph", "--sha", testSHA, "--date", "2026-10-09T00:00:00Z", "--out", filepath.Join(root, "out")}, func(cmd []string, _ io.Writer) error {
		if cmd[0] != "run" || cmd[3] != "--codegraph" {
			t.Fatalf("unexpected command: %v", cmd)
		}
		if cmd[4] != filepath.Join(root, "codegraph") {
			t.Fatalf("binary = %q, want absolute local path", cmd[4])
		}
		output, err := exec.Command(cmd[4]).Output()
		if err != nil || string(output) != "local\n" {
			t.Fatalf("executed binary: output %q, error %v", output, err)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("produce: %v", err)
	}
}

func testProduce(t *testing.T, bin string) {
	t.Helper()
	root := t.TempDir()
	args := func(out string) []string {
		return []string{"produce", "--manifest", "testdata/manifests/fixtures.json", "--codegraph", bin, "--sha", testSHA, "--date", "2026-10-09T00:00:00Z", "--out", out}
	}
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, out := range []string{a, b} {
		if err := run(args(out), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("bundle files: %v", entries)
	}
	sums, _ := os.ReadFile(filepath.Join(a, "SHA256SUMS"))
	for _, entry := range entries {
		x, err := os.ReadFile(filepath.Join(a, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		y, err := os.ReadFile(filepath.Join(b, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(x, y) {
			t.Fatalf("%s differs", entry.Name())
		}
		if entry.Name() != "SHA256SUMS" && !strings.Contains(string(sums), hashBytes(x)+"  "+entry.Name()+"\n") {
			t.Fatalf("missing checksum: %s", entry.Name())
		}
	}
	// The bundled record has its fixture path rewritten; it must still pass
	// the validator on its own.
	if err := run([]string{"validate", "--expect-sha", testSHA, "--fixture", "testdata/go-calls", filepath.Join(a, "go-calls.json")}, io.Discard); err != nil {
		t.Fatalf("bundled record: %v", err)
	}
	if err := run(args(a), io.Discard); err == nil {
		t.Fatal("nonempty output accepted")
	}
	bad := filepath.Join(root, "bad")
	corrupt := func(cmd []string, w io.Writer) error {
		if err := run(cmd, w); err != nil {
			return err
		}
		if cmd[0] == "run" {
			path := cmd[len(cmd)-1]
			rec, err := load(path)
			if err != nil {
				return err
			}
			rec.CodeGraph.SHA = strings.Repeat("b", 40)
			raw, err := Marshal(rec)
			if err != nil {
				return err
			}
			return os.WriteFile(path, raw, 0600)
		}
		return nil
	}
	if err := produce(args(bad)[1:], corrupt); err == nil || !strings.Contains(err.Error(), "stale record") {
		t.Fatalf("stale record: %v", err)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("failed run wrote output")
	}
}

func TestProduceRejectsFixtureMismatch(t *testing.T) {
	root := t.TempDir()
	m := produceFixtureManifest{Schema: "codegraph.benchmark_fixtures/v1", Fixtures: []produceFixture{{Name: "go-calls", Path: ".", SHA256: strings.Repeat("0", 64)}}}
	raw, _ := marshalJSON(m)
	path := filepath.Join(root, "fixtures.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"produce", "--manifest", path, "--codegraph", "unused", "--sha", testSHA, "--date", "2026-10-09T00:00:00Z", "--out", filepath.Join(root, "out")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "hash differs") {
		t.Fatalf("fixture mismatch: %v", err)
	}
}

func TestProduceRejectsReservedFixtureName(t *testing.T) {
	root := t.TempDir()
	m := produceFixtureManifest{Schema: "codegraph.benchmark_fixtures/v1", Fixtures: []produceFixture{{Name: "manifest", Path: ".", SHA256: strings.Repeat("0", 64)}}}
	raw, _ := marshalJSON(m)
	path := filepath.Join(root, "fixtures.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// manifest.json is MANIFEST.json on case-insensitive filesystems.
	err := run([]string{"produce", "--manifest", path, "--codegraph", "unused", "--sha", testSHA, "--date", "2026-10-09T00:00:00Z", "--out", filepath.Join(root, "out")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `invalid or duplicate fixture "manifest"`) {
		t.Fatalf("reserved name: %v", err)
	}
}

func TestWriteBundleLeavesNothingOnFailure(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "bundle")
	// A name with a missing subdirectory cannot be written.
	err := writeBundle(out, map[string][]byte{"a.json": []byte("{}"), "missing/b.json": []byte("{}")})
	if err == nil {
		t.Fatal("write into a missing subdirectory succeeded")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("failed write left %v", entries)
	}
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeBundle(out, map[string][]byte{"a.json": []byte("{}")}); err != nil {
		t.Fatalf("empty existing output: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "a.json")); err != nil || string(b) != "{}" {
		t.Fatalf("written bundle: %q %v", b, err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatalf("staging left behind: %v", entries)
	}
}

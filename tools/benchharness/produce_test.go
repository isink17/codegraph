package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

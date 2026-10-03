package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The CLI relationship commands disclose the persisted graph's limitations
// under the same key and shape as the MCP tools. Which build wrote the graph
// decides: the cgo build indexes Java with tree-sitter (call-capable, no
// field), the non-cgo build with the symbols-only heuristic adapter (field
// present). Go stays call-capable either way.
func TestCLIRelationshipCommandsDiscloseGraphLimitations(t *testing.T) {
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "codegraph-home"))
	repoRoot := filepath.Join(t.TempDir(), "repo")
	files := map[string]string{
		"main.go":         "package main\n\nfunc goHelper() int { return 1 }\n\nfunc main() { _ = goHelper() }\n",
		"src/Widget.java": "package demo;\n\npublic class Widget {\n    public int size() { return helper(); }\n    private int helper() { return 1; }\n}\n",
	}
	for rel, content := range files {
		path := filepath.Join(repoRoot, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })
	run := func(args ...string) map[string]any {
		var out, errOut bytes.Buffer
		if err := Run(context.Background(), args, &out, &errOut); err != nil {
			t.Fatalf("Run(%v): %v\n%s", args, err, errOut.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
			t.Fatalf("Run(%v): %v\n%s", args, err, out.String())
		}
		return payload
	}
	run("index", repoRoot)

	javaCallEdges := false
	for _, lang := range newDefaultRegistry().SupportedLanguages() {
		if lang.Language == "java" {
			javaCallEdges = lang.CallEdges
		}
	}
	for _, args := range [][]string{
		{"callers", repoRoot, "goHelper"},
		{"callees", repoRoot, "main"},
		{"impact", repoRoot, "goHelper"},
		{"find_related_tests", "--json", "--repo-root", repoRoot, "main.go"},
	} {
		payload := run(args...)
		raw, present := payload["limitations"]
		if present == javaCallEdges {
			t.Fatalf("%v: limitations present=%v with java call edges=%v: %v", args, present, javaCallEdges, payload)
		}
		if !present {
			continue
		}
		list, _ := raw.([]any)
		if len(list) != 1 {
			t.Fatalf("%v: limitations = %v, want java only", args, raw)
		}
		entry := list[0].(map[string]any)
		if entry["language"] != "java" || entry["graph_capability"] != "symbols_only" || entry["effect"] == "" {
			t.Fatalf("%v: limitation = %v", args, entry)
		}
	}
}

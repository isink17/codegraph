package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/compactfmt"
	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/mcp"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	heuristicparser "github.com/isink17/codegraph/internal/parser/heuristic"
	"github.com/isink17/codegraph/internal/store"
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

// One reduced graph, three encodings: MCP JSON, MCP compact and CLI JSON carry
// identical limitation rows, and the CLI text mode of find_related_tests keeps
// stdout a bare path list while stating the limitation on stderr. The graph is
// written with the non-cgo adapters explicitly, so the test means the same in
// both build modes; the queries never reparse.
func TestLimitationRowsAgreeAcrossMCPCompactAndCLI(t *testing.T) {
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "codegraph-home"))
	repoRoot := filepath.Join(t.TempDir(), "repo")
	for rel, content := range map[string]string{
		"main.go":         "package main\n\nfunc goHelper() int { return 1 }\n\nfunc main() { _ = goHelper() }\n",
		"main_test.go":    "package main\n\nimport \"testing\"\n\nfunc TestGoHelper(t *testing.T) { _ = goHelper() }\n",
		"src/Widget.java": "package demo;\n\npublic class Widget {\n    private int helper() { return 1; }\n}\n",
	} {
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

	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	app, repo, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	reduced := indexer.New(app.Store, parser.NewRegistry(goparser.New(), heuristicparser.NewJava()), nil)
	if _, err := reduced.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatal(err)
	}
	want, err := app.Store.GraphLimitations(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 1 || want[0].Language != "java" || want[0].Capability != store.GraphSymbolsOnly {
		t.Fatalf("fixture limitations = %+v", want)
	}

	callTool := func(args map[string]any) string {
		frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "find_callers", "arguments": args}})
		var out bytes.Buffer
		server := mcp.NewServer(repo.RootPath, repoID, app.Store, reduced, app.Query, io.Discard)
		if err := server.Serve(ctx, bytes.NewReader(append(frame, '\n')), &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		var resp struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil || len(resp.Result.Content) != 1 {
			t.Fatalf("MCP response %s: %v", out.String(), err)
		}
		return resp.Result.Content[0].Text
	}
	var mcpJSON struct {
		Limitations []store.GraphLimitation `json:"limitations"`
	}
	if err := json.Unmarshal([]byte(callTool(map[string]any{"symbol": "goHelper"})), &mcpJSON); err != nil {
		t.Fatal(err)
	}
	doc, err := compactfmt.Decode(callTool(map[string]any{"symbol": "goHelper", "format": "compact"}))
	if err != nil {
		t.Fatal(err)
	}
	var compactRows []store.GraphLimitation
	if sec := doc.Section("limitations"); sec != nil {
		for _, row := range sec.Rows {
			compactRows = append(compactRows, store.GraphLimitation{Language: row[0], Capability: row[1], Effect: row[2]})
		}
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := Run(ctx, []string{"callers", repoRoot, "goHelper"}, &out, &errOut); err != nil {
		t.Fatalf("callers: %v %s", err, errOut.String())
	}
	var cliJSON struct {
		Limitations []store.GraphLimitation `json:"limitations"`
	}
	if err := json.Unmarshal(out.Bytes(), &cliJSON); err != nil {
		t.Fatal(err)
	}
	for label, got := range map[string][]store.GraphLimitation{"mcp json": mcpJSON.Limitations, "mcp compact": compactRows, "cli json": cliJSON.Limitations} {
		if len(got) != len(want) || got[0] != want[0] {
			t.Fatalf("%s limitations = %+v, want %+v", label, got, want)
		}
	}

	out.Reset()
	errOut.Reset()
	if err := Run(ctx, []string{"find_related_tests", "--repo-root", repoRoot, "main.go"}, &out, &errOut); err != nil {
		t.Fatalf("find_related_tests: %v %s", err, errOut.String())
	}
	if strings.Contains(out.String(), "limitation") || strings.TrimSpace(out.String()) != "main_test.go" {
		t.Fatalf("stdout = %q, want the bare test path", out.String())
	}
	if strings.Count(errOut.String(), "limitations: java=symbols_only") != 1 {
		t.Fatalf("stderr = %q, want one limitation line", errOut.String())
	}
}

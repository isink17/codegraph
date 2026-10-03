package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// explainMain is the caller file. Its name carries `&` so CLI/MCP byte parity
// covers HTML escaping. Line by line: 10 resolves through the own-module
// import; 12 binds through the receiver's proven type; 13 is a selector on a
// locally bound qualifier with no such method (go_local_qualifier); 14 names
// an external package (no evaluated rule refuses it); 15 is a bare call the Go
// package-scope pass owns (go_bare_package_scope); 16 holds two edges.
const explainMain = "package main\n\nimport \"example.com/p/pkg\"\n\ntype T struct{}\n\nfunc (T) Get() {}\n\nfunc main() {\n" +
	"\tpkg.Open()\n" +
	"\tvar store T\n" +
	"\tstore.Get()\n" +
	"\tstore.Missing()\n" +
	"\tfmt.Println()\n" +
	"\tOpen()\n" +
	"\tpkg.Open(); Open()\n" +
	"}\n"

const explainFile = "cmd/r&d.go"

func explainRepo(t *testing.T) string {
	t.Helper()
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	root := t.TempDir()
	for rel, body := range map[string]string{
		"go.mod":    "module example.com/p\n\ngo 1.22\n",
		"pkg/a.go":  "package pkg\n\nfunc Open() {}\n",
		explainFile: explainMain,
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := runCLI(t, "index", root); err != nil {
		t.Fatal(err)
	}
	return root
}

func explainCLI(t *testing.T, args ...string) (string, store.ExplainResult) {
	t.Helper()
	out, _, err := runCLI(t, append([]string{"explain"}, args...)...)
	if err != nil {
		t.Fatalf("explain %v: %v", args, err)
	}
	var res store.ExplainResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return out, res
}

func TestExplainCLI(t *testing.T) {
	root := explainRepo(t)
	one := func(line int) store.EdgeExplanation {
		t.Helper()
		_, res := explainCLI(t, root, "--file", explainFile, "--line", strconv.Itoa(line))
		if res.Total != 1 || len(res.Edges) != 1 {
			t.Fatalf("line %d: %d edges, want 1", line, res.Total)
		}
		if res.Scope != "current_graph_state" || !slices.Contains(res.NotEvaluated, "own_module_import") ||
			!slices.Contains(res.NotEvaluated, "bare_type_scope") || !slices.Equal(res.PartiallyEvaluated, []string{"broad_ambiguity"}) {
			t.Fatalf("line %d: scope/evaluation notes = %+v", line, res)
		}
		return res.Edges[0]
	}

	resolved := one(10)
	if resolved.Status != store.ExplainStatusResolved || resolved.Explanation != store.ExplainPersisted ||
		resolved.ResolutionStrategy != "module_import" || resolved.ResolutionConfidence == "" ||
		resolved.Target == nil || resolved.Target.QualifiedName != "pkg.Open" || resolved.Target.File != "pkg/a.go" ||
		resolved.Source == nil || resolved.Source.QualifiedName != "main.main" || len(resolved.RefusingRules) != 0 ||
		resolved.Language != "go" || resolved.File != explainFile || resolved.DstName != "example.com/p/pkg.Open" {
		t.Fatalf("resolved edge = %+v", resolved)
	}

	for line, want := range map[int]store.ExplainRule{
		13: {ID: "go_local_qualifier", Stage: "ownership", Disposition: "owned"},
		15: {ID: "go_bare_package_scope", Stage: "ownership", Disposition: "owned"},
	} {
		got := one(line)
		if got.Status != store.ExplainStatusUnresolved || got.Explanation != store.ExplainRefused ||
			!slices.Equal(got.RefusingRules, []store.ExplainRule{want}) || got.Target != nil ||
			got.ResolutionStrategy != "" || got.ResolutionConfidence != "" {
			t.Fatalf("line %d = %+v, want refused by %s", line, got, want.ID)
		}
	}

	unknown := one(14)
	if unknown.Status != store.ExplainStatusUnresolved || unknown.Explanation != store.ExplainUnknown || len(unknown.RefusingRules) != 0 {
		t.Fatalf("unknown edge = %+v", unknown)
	}

	// By id: the same explanation.
	_, byID := explainCLI(t, root, "--edge-id", strconv.FormatInt(unknown.EdgeID, 10))
	if len(byID.Edges) != 1 || byID.Edges[0].EdgeID != unknown.EdgeID || byID.Edges[0].DstName != "fmt.Println" {
		t.Fatalf("by id = %+v", byID)
	}

	// A line with two edges: ordered by destination name, paged, and the same
	// bytes on every run.
	first, page := explainCLI(t, root, "--file", explainFile, "--line", "16")
	if page.Total != 2 || len(page.Edges) != 2 || page.Edges[0].DstName != "Open" || page.Edges[1].DstName != "example.com/p/pkg.Open" {
		t.Fatalf("line 16 = %+v", page)
	}
	if again, _ := explainCLI(t, root, "--file", explainFile, "--line", "16"); again != first {
		t.Fatalf("explain is not deterministic:\n%s\n%s", first, again)
	}
	_, second := explainCLI(t, root, "--file", explainFile, "--line", "16", "--limit", "1", "--offset", "1")
	if second.Total != 2 || len(second.Edges) != 1 || second.Edges[0].EdgeID != page.Edges[1].EdgeID {
		t.Fatalf("second page = %+v", second)
	}
	_, named := explainCLI(t, root, "--file", "./"+explainFile, "--line", "16", "--name", "Open")
	if named.Total != 1 || named.Edges[0].EdgeID != page.Edges[0].EdgeID {
		t.Fatalf("--name = %+v", named)
	}

	for _, args := range [][]string{
		{"--edge-id", "999999"},
		{"--file", explainFile, "--line", "1"},
		{"--file", "no/such.go", "--line", "10"},
		{"--file", explainFile},
		{"--edge-id", "1", "--line", "10"},
		{"--edge-id", "1", "--limit", "501"},
		{},
	} {
		if out, _, err := runCLI(t, append([]string{"explain", root}, args...)...); err == nil {
			t.Errorf("explain %v succeeded: %s", args, out)
		}
	}
}

// TestExplainMCPParity: explain_edge is absent from tools/list in both modes,
// callable by name in full mode, reached through tool_search and tool_call in
// gateway mode, and its data is byte-equal to the CLI output.
func TestExplainMCPParity(t *testing.T) {
	root := explainRepo(t)
	cliOut, _ := explainCLI(t, root, "--file", explainFile, "--line", "13")
	if !strings.Contains(cliOut, `r\u0026d.go`) {
		t.Fatalf("CLI output does not escape &:\n%s", cliOut)
	}
	var cliCompact bytes.Buffer
	if err := json.Compact(&cliCompact, []byte(cliOut)); err != nil {
		t.Fatal(err)
	}
	args := `{"file":"cmd/r&d.go","line":13}`
	cfg := loadTestConfig(t)
	serve := func(mode string, calls ...string) []map[string]json.RawMessage {
		t.Helper()
		lines := append([]string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`}, calls...)
		var stdout, stderr bytes.Buffer
		if err := runServeWith(context.Background(), cfg, strings.NewReader(strings.Join(lines, "\n")+"\n"), &stdout, &stderr,
			[]string{"--repo-root", root, "--tool-mode", mode}); err != nil {
			t.Fatalf("serve: %v (%s)", err, stderr.String())
		}
		var out []map[string]json.RawMessage
		for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("response is not JSON: %v (%s)", err, line)
			}
			out = append(out, m)
		}
		return out
	}
	listed := func(resp map[string]json.RawMessage) []string {
		var r struct {
			Tools []struct{ Name string } `json:"tools"`
		}
		if err := json.Unmarshal(resp["result"], &r); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range r.Tools {
			names = append(names, tool.Name)
		}
		return names
	}
	text := func(resp map[string]json.RawMessage) string {
		var r struct {
			IsError bool                    `json:"isError"`
			Content []struct{ Text string } `json:"content"`
		}
		if err := json.Unmarshal(resp["result"], &r); err != nil || r.IsError || len(r.Content) == 0 {
			t.Fatalf("tool call failed: %s", resp["result"])
		}
		return r.Content[0].Text
	}
	data := func(resp map[string]json.RawMessage) string {
		t.Helper()
		var envelope struct {
			OK   bool            `json:"ok"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(text(resp)), &envelope); err != nil || !envelope.OK {
			t.Fatalf("envelope = %s (%v)", text(resp), err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, envelope.Data); err != nil {
			t.Fatal(err)
		}
		return compact.String()
	}

	full := serve("full", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"explain_edge","arguments":`+args+`}}`)
	if contains(listed(full[1]), "explain_edge") {
		t.Fatalf("full tools/list = %v; explain_edge must be hidden", listed(full[1]))
	}
	if got := data(full[2]); got != cliCompact.String() {
		t.Fatalf("MCP data differs from CLI output\nmcp: %s\ncli: %s", got, cliCompact.String())
	}

	gateway := serve("gateway",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":"why is this edge unresolved"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"tool_call","arguments":{"name":"explain_edge","arguments":`+args+`}}}`)
	if contains(listed(gateway[1]), "explain_edge") {
		t.Fatalf("gateway tools/list = %v; explain_edge must be hidden", listed(gateway[1]))
	}
	if !strings.Contains(text(gateway[2]), "explain_edge") {
		t.Fatalf("tool_search did not find explain_edge: %s", text(gateway[2]))
	}
	if got := data(gateway[3]); got != cliCompact.String() {
		t.Fatalf("gateway data differs from CLI output\n%s\n%s", got, cliCompact.String())
	}
}

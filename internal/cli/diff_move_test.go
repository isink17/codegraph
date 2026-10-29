package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/diff"
	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/mcp"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// These literal source trees pin CG-18's remaining modified-content move
// acceptance through the public entry points. No Git similarity heuristic is
// involved: the declaration's bytes and package identity decide continuity.
func TestDiffMoveContinuityCLIAndMCP(t *testing.T) {
	const original = "package p\n\nfunc A() {}\n"
	for _, tc := range []struct {
		name, content, destination, refusal string
		duplicate                           bool
		pairs, score                        int
	}{
		{name: "doc edit across module root move", content: "package p\n\n// A does nothing.\nfunc A() {}\n", pairs: 1},
		{name: "sibling added across module root move", content: original + "\nfunc B() {}\n", pairs: 1},
		{name: "body edit refuses continuity", content: "package p\n\nfunc A() { println() }\n", refusal: "declaration_content_changed", score: 5},
		{name: "duplicate shape refuses continuity", content: original + "\n// edited file\n", duplicate: true, refusal: "declaration_not_unique", score: 20},
		{name: "different import path refuses continuity", content: original + "\n// edited file\n", destination: "q", score: 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gittest.Init(t)
			r.Write("go.mod", "module example.com/app\n\ngo 1.22\n")
			r.Write("main.go", "package main\n\nimport \"example.com/lib/p\"\n\nfunc main() { p.A() }\n")
			r.Write("libs/go.mod", "module example.com/lib\n\ngo 1.22\n")
			r.Write("libs/p/a.go", original)
			base := r.Commit("", "pinned base")
			r.Git("mv", "libs", "lib")
			destination := "p"
			if tc.destination != "" {
				destination = tc.destination
				r.Git("rm", "-f", "lib/p/a.go")
			}
			r.Write("lib/"+destination+"/a.go", tc.content)
			if tc.duplicate {
				r.Write("lib/p/b.go", original+"\n// competing destination\n")
			}
			head := r.Commit("", "pinned move")
			var stdout, stderr bytes.Buffer
			err := Run(t.Context(), []string{"diff", base, head, "--repo-root", r.Dir, "--fail-on-risk", "low"}, &stdout, &stderr)
			wantExit := diffExitPass
			if tc.score > 0 {
				wantExit = diffExitRisk
			}
			code := diffExitPass
			if err != nil {
				var exit *ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("CLI: %v (%s)", err, stderr.String())
				}
				code = exit.Code
			}
			if code != wantExit {
				t.Fatalf("gate exit %d, want %d: %v (%s)\n%s", code, wantExit, err, stderr.String(), stdout.String())
			}
			var got diff.Result
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Diff.DeclarationPairs.Pairs) != tc.pairs || got.Diff.Risk.Score == nil || *got.Diff.Risk.Score != tc.score {
				t.Fatalf("pairs/risk: %s", stdout.String())
			}
			if tc.pairs == 1 {
				pair := got.Diff.DeclarationPairs.Pairs[0]
				if pair.FromPath != "libs/p/a.go" || pair.ToPath != "lib/p/a.go" || pair.Before.QualifiedName != "p.A" || pair.After.QualifiedName != "p.A" {
					t.Fatalf("wrong declaration paired: %+v", pair)
				}
			}
			wantRule := diff.RuleRemovedDeclarationDependent
			if tc.score == 5 {
				wantRule = diff.RuleDependentRetargeted
			}
			for _, component := range got.Diff.Risk.Components {
				wantCount := 0
				if tc.score > 0 && component.Rule == wantRule {
					wantCount = 1
				}
				if component.Count != wantCount {
					t.Fatalf("risk component %s count %d, want %d", component.Rule, component.Count, wantCount)
				}
			}
			if tc.refusal != "" {
				found := false
				for _, refusal := range got.Diff.DeclarationPairs.Refused {
					found = found || refusal.Reason == tc.refusal
				}
				if !found {
					t.Fatalf("missing %s: %+v", tc.refusal, got.Diff.DeclarationPairs)
				}
			}
			st, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			server := mcp.NewServer(r.Dir, 1, st, indexer.New(st, newDefaultRegistry(), nil), query.New(st, nil), io.Discard)
			request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "diff", "arguments": map[string]any{"base": base, "head": head}}})
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := server.Serve(t.Context(), strings.NewReader(string(request)+"\n"), &output, io.Discard); err != nil {
				t.Fatal(err)
			}
			var response struct {
				Result struct {
					Content []struct{ Text string } `json:"content"`
				} `json:"result"`
			}
			if err := json.Unmarshal(output.Bytes(), &response); err != nil || len(response.Result.Content) != 1 {
				t.Fatalf("MCP response: %s (%v)", output.String(), err)
			}
			var payload struct {
				OK   bool        `json:"ok"`
				Data diff.Result `json:"data"`
			}
			if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &payload); err != nil || !payload.OK {
				t.Fatalf("MCP payload: %s (%v)", response.Result.Content[0].Text, err)
			}
			// Compare the JSON documents; Change.Before/After hold untyped
			// values whose concrete Go types differ after typed decoding.
			var cliDocument, mcpDocument any
			if err := json.Unmarshal(stdout.Bytes(), &cliDocument); err != nil {
				t.Fatal(err)
			}
			var rawPayload struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &rawPayload); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(rawPayload.Data, &mcpDocument); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cliDocument, mcpDocument) {
				t.Fatalf("CLI/MCP differ:\nCLI %s\nMCP %s", stdout.String(), response.Result.Content[0].Text)
			}
		})
	}
}

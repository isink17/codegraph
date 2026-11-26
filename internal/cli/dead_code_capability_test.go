package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

// deadCodeFixtureFiles mixes a call-capable language with languages whose
// production adapters build no call graph in one build mode or both: Java and
// TypeScript are symbols-only without cgo, Svelte and Solidity have no call
// graph with cgo and no parser without it. Every function except unusedGo,
// unusedTs, Run and size has a caller in source, and the Svelte script
// constructs Box.
var deadCodeFixtureFiles = map[string]string{
	"lib/lib.go": "package lib\n\nfunc Helper(n int) int { return n * 2 }\n\nfunc Run() int { return Helper(1) }\n\nfunc unusedGo() int { return 0 }\n",
	"web/util.ts": "export function formatName(n: string): string { return n.trim(); }\n" +
		"export class Box { size(): number { return 1; } }\n",
	"web/other.ts": "export function unusedTs(): number { return 1; }\n",
	"web/App.svelte": "<script lang=\"ts\">\n  import { formatName, Box } from './util';\n  const b = new Box();\n" +
		"  function greet(name: string): string { return formatName(name); }\n" +
		"  function shout(): string { return greet(\"x\").toUpperCase(); }\n  const out = shout();\n</script>\n\n<h1>{out}</h1>\n",
	"contracts/Token.sol": "pragma solidity ^0.8.0;\n\ncontract Token {\n" +
		"    function transfer(address to, uint256 amount) public returns (bool) {\n        return _move(to, amount);\n    }\n" +
		"    function _move(address to, uint256 amount) internal returns (bool) {\n        return to != address(0) && amount > 0;\n    }\n}\n",
	"src/Widget.java": "package demo;\n\npublic class Widget {\n    public int size() { return helper(); }\n    private int helper() { return 1; }\n}\n",
}

type deadCodeAnswer struct {
	Data struct {
		DeadCode []struct {
			Name     string `json:"name"`
			Language string `json:"language"`
		} `json:"dead_code"`
		Skipped []store.DeadCodeSkippedLanguage `json:"skipped_languages"`
	} `json:"data"`
	Limitations []store.GraphLimitation `json:"limitations"`
}

// serveDeadCode asks the production serve path for find_dead_code and for the
// persisted graph capability.
func serveDeadCode(t *testing.T, root string) (deadCodeAnswer, store.GraphCapability) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	lines := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"find_dead_code","arguments":{"limit":100}}}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"supported_languages","arguments":{}}}` + "\n"
	if err := runServeWith(context.Background(), loadTestConfig(t), strings.NewReader(lines), &stdout, &stderr, []string{"--repo-root", root}); err != nil {
		t.Fatalf("serve: %v (%s)", err, stderr.String())
	}
	responses := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	text := func(line string) []byte {
		var resp struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil || len(resp.Result.Content) == 0 {
			t.Fatalf("response %s (%v)", line, err)
		}
		return []byte(resp.Result.Content[0].Text)
	}
	var answer deadCodeAnswer
	if err := json.Unmarshal(text(responses[len(responses)-2]), &answer); err != nil {
		t.Fatal(err)
	}
	var languages struct {
		Data struct {
			Capability store.GraphCapability `json:"graph_capability"`
		} `json:"data"`
	}
	if err := json.Unmarshal(text(responses[len(responses)-1]), &languages); err != nil {
		t.Fatal(err)
	}
	return answer, languages.Data.Capability
}

// Under the production registry of either build mode, find_dead_code
// classifies only languages whose persisted graph is call-capable, names every
// other indexed language in skipped_languages, and never declares a function
// that source calls dead.
func TestFindDeadCodeProductionRegistryNeverDeclaresUnsupportedLanguagesDead(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	root := t.TempDir()
	for name, content := range deadCodeFixtureFiles {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if out, stderr, err := runCLI(t, "index", root, "--no-history"); err != nil {
		t.Fatalf("index: %v\n%s\n%s", err, out, stderr)
	}
	answer, capability := serveDeadCode(t, root)

	// Call-capable here means what find_dead_code classifies.
	var callCapable, notCallCapable []string
	for _, language := range capability.Languages {
		if language.Capability == store.GraphCallCapable && !slices.ContainsFunc(language.Profiles, parser.SameFileCallsOnly) {
			callCapable = append(callCapable, language.Language)
		} else {
			notCallCapable = append(notCallCapable, language.Language)
		}
	}
	var skipped []string
	for _, s := range answer.Data.Skipped {
		skipped = append(skipped, s.Language)
	}
	if !slices.Equal(skipped, notCallCapable) {
		t.Fatalf("skipped_languages = %v, want the non-call-capable languages %v", skipped, notCallCapable)
	}
	var names []string
	for _, item := range answer.Data.DeadCode {
		if !slices.Contains(callCapable, item.Language) {
			t.Fatalf("%s declared dead in %s, which is not call-capable (%+v)", item.Name, item.Language, capability)
		}
		names = append(names, item.Name)
	}
	for _, called := range []string{"Helper", "helper", "_move", "greet", "shout", "formatName", "Box"} {
		if slices.Contains(names, called) {
			t.Fatalf("called function %s declared dead: %v", called, names)
		}
	}
	for _, unused := range []string{"Run", "unusedGo"} {
		if !slices.Contains(names, unused) {
			t.Fatalf("go candidate %s missing: %v", unused, names)
		}
	}
	if !slices.Contains(callCapable, "go") {
		t.Fatalf("go is not call-capable: %+v", capability)
	}
	if slices.Contains(callCapable, "java") != slices.Contains(names, "size") {
		t.Fatalf("java call-capable=%v but candidates %v", slices.Contains(callCapable, "java"), names)
	}
	if slices.Contains(callCapable, "typescript") != slices.Contains(names, "unusedTs") {
		t.Fatalf("typescript call-capable=%v but candidates %v", slices.Contains(callCapable, "typescript"), names)
	}
	if len(notCallCapable) > 0 && len(answer.Limitations) == 0 {
		t.Fatalf("skipped languages without limitations: %+v", answer)
	}
}

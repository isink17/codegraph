package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
)

// Missing related-test targets are successful domain results, not driver errors.
func TestMissingSymbolIsSuccessfulEmptyPresence(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	isErr, text := callResult(t, server, "find_related_tests", map[string]any{"symbol": "NoSuchSymbolAnywhere"})
	if isErr || !strings.Contains(text, `"target_found":false`) || !strings.Contains(text, `"tests":[]`) {
		t.Fatalf("missing related test target = %v, %s", isErr, text)
	}
}

// Direct and gateway responses preserve the same missing-target state.
func TestMissingSymbolReadsTheSameThroughTheGateway(t *testing.T) {
	_, direct := callResult(t, newGatewayTestServer(t, ToolModeFull), "find_related_tests", map[string]any{"symbol": "NoSuchSymbolAnywhere"})

	gateway := newGatewayTestServer(t, ToolModeGateway)
	isErr, text := callResult(t, gateway, "tool_call", map[string]any{
		"name":      "find_related_tests",
		"arguments": map[string]any{"symbol": "NoSuchSymbolAnywhere"},
	})
	if isErr {
		t.Fatalf("gateway rejected a missing symbol: %s", text)
	}
	if !strings.Contains(text, `"target_found":false`) || !strings.Contains(direct, `"target_found":false`) {
		t.Fatalf("gateway/direct missing presence mismatch: %q / %q", text, direct)
	}
}

// TestEmptyIsNotMissing draws the contract boundary. A search with no
// matches, and an indexed symbol that simply has no tests, are both ordinary
// answers -- not errors.
func TestEmptyIsNotMissing(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)

	// A search that matches nothing is a valid empty result.
	for _, tool := range []string{"find_symbol", "search_symbols", "search_semantic"} {
		if isErr, text := callResult(t, server, tool, map[string]any{"query": "NoSuchSymbolAnywhere"}); isErr {
			t.Fatalf("%s turned a zero-match search into an error: %s", tool, text)
		}
	}

	// A file that is indexed but has no related tests is a valid empty result,
	// and so is a path that is not indexed at all.
	for _, file := range []string{"helper.go", "no/such/file.go"} {
		if isErr, text := callResult(t, server, "find_related_tests", map[string]any{"file": file}); isErr {
			t.Fatalf("find_related_tests(file=%s) errored: %s", file, text)
		}
	}
}

// Unknown traversal inputs are ordinary empty domain results and never driver
// errors.
func TestUnknownSymbolKeepsItsExistingShapeOnTraversals(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"find_callers", map[string]any{"symbol": "NoSuchSymbolAnywhere"}},
		{"find_callees", map[string]any{"symbol": "NoSuchSymbolAnywhere"}},
		{"get_impact_radius", map[string]any{"symbols": []any{"NoSuchSymbolAnywhere"}}},
		{"trace_dependencies", map[string]any{"symbol": "NoSuchSymbolAnywhere"}},
	} {
		isErr, text := callResult(t, server, tc.tool, tc.args)
		if isErr {
			t.Fatalf("%s changed an unknown symbol into an error: %s", tc.tool, text)
		}
		if strings.Contains(text, noDriverErrorSubstring) {
			t.Fatalf("%s leaked the driver message: %s", tc.tool, text)
		}
	}
}

// Singular name queries must report ambiguity identically through direct and
// gateway calls, including compact output; absence remains a separate result.
func TestAmbiguousNameIsNotReportedAsMissing(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	writeRepoFile(t, server.repoRoot, "other/helper.go", "package other\nfunc Helper() int { return 2 }\n")
	if _, err := server.indexer.Update(context.Background(), indexer.Options{RepoRoot: server.repoRoot}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"find_callers", "find_callees", "find_related_tests", "get_impact_radius", "trace_dependencies"} {
		var ambiguityText string
		for _, mode := range []ToolMode{ToolModeFull, ToolModeGateway} {
			if err := server.SetToolMode(mode); err != nil {
				t.Fatal(err)
			}
			for _, format := range []string{"json", "compact"} {
				for _, symbol := range []string{"Helper", "main.Helper"} {
					args := map[string]any{"symbol": symbol, "format": format}
					if tool == "get_impact_radius" {
						delete(args, "symbol")
						args["symbols"] = []string{symbol}
					}
					name := tool
					if mode == ToolModeGateway {
						name, args = "tool_call", map[string]any{"name": tool, "arguments": args}
					}
					isErr, text := callResult(t, server, name, args)
					if symbol == "main.Helper" {
						if isErr {
							t.Errorf("%s %s %s exact target errored: %s", tool, mode, format, text)
						}
						continue
					}
					if !isErr || !strings.Contains(text, "symbol is ambiguous") || strings.Contains(text, "not found") {
						t.Errorf("%s %s %s ambiguity = %v, %s", tool, mode, format, isErr, text)
					}
					if ambiguityText == "" {
						ambiguityText = text
					} else if text != ambiguityText {
						t.Errorf("%s ambiguity differs: %q / %q", tool, ambiguityText, text)
					}
				}
			}
		}
	}
}

// TestNoPublicToolLeaksTheDriverMessage sweeps the whole advertised surface
// with inputs that name things which do not exist.
func TestNoPublicToolLeaksTheDriverMessage(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	probes := []struct {
		tool string
		args map[string]any
	}{
		{"find_symbol", map[string]any{"query": "NoSuchSymbolAnywhere"}},
		{"search_symbols", map[string]any{"query": "NoSuchSymbolAnywhere"}},
		{"search_semantic", map[string]any{"query": "NoSuchSymbolAnywhere"}},
		{"find_callers", map[string]any{"symbol": "NoSuchSymbolAnywhere"}},
		{"find_callees", map[string]any{"symbol": "NoSuchSymbolAnywhere"}},
		{"find_related_tests", map[string]any{"symbol": "NoSuchSymbolAnywhere"}},
		{"find_related_tests", map[string]any{"file": "no/such/file.go"}},
		{"get_impact_radius", map[string]any{"symbols": []any{"NoSuchSymbolAnywhere"}}},
		{"get_impact_radius", map[string]any{"files": []any{"no/such/file.go"}}},
		{"trace_dependencies", map[string]any{"symbol": "NoSuchSymbolAnywhere"}},
		{"graph_stats", map[string]any{}},
		{"architecture_overview", map[string]any{}},
		{"list_files", map[string]any{"path_filter": "no/such/dir"}},
		{"graph_analytics", map[string]any{"analysis": "pagerank"}},
		{"find_dead_code", map[string]any{}},
	}
	for _, probe := range probes {
		_, text := callResult(t, server, probe.tool, probe.args)
		if strings.Contains(text, noDriverErrorSubstring) {
			t.Fatalf("%s(%v) leaked the driver message: %s", probe.tool, probe.args, text)
		}
	}
}

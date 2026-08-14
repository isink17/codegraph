package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMissingSymbolIsACodeGraphError is the P16 finding P18 closes. Asking for
// the related tests of a symbol that is not indexed used to answer with the
// SQLite driver's own words.
func TestMissingSymbolIsACodeGraphError(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	msg := callError(t, server, "find_related_tests", map[string]any{"symbol": "NoSuchSymbolAnywhere"})
	if strings.Contains(msg, noDriverErrorSubstring) {
		t.Fatalf("error leaked the driver message: %q", msg)
	}
	if !strings.Contains(msg, "not found") {
		t.Fatalf("error = %q, want it to say the symbol was not found", msg)
	}
	if !strings.Contains(msg, "NoSuchSymbolAnywhere") {
		t.Fatalf("error = %q, want it to name the symbol that was requested", msg)
	}
}

// TestMissingSymbolReadsTheSameThroughTheGateway keeps P16's parity exact: the
// canonical handler owns the error, so the gateway cannot phrase it differently.
func TestMissingSymbolReadsTheSameThroughTheGateway(t *testing.T) {
	direct := callError(t, newGatewayTestServer(t, ToolModeFull), "find_related_tests", map[string]any{"symbol": "NoSuchSymbolAnywhere"})

	gateway := newGatewayTestServer(t, ToolModeGateway)
	isErr, text := callResult(t, gateway, "tool_call", map[string]any{
		"name":      "find_related_tests",
		"arguments": map[string]any{"symbol": "NoSuchSymbolAnywhere"},
	})
	if !isErr {
		t.Fatalf("gateway accepted a missing symbol: %s", text)
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if envelope.Error != direct {
		t.Fatalf("gateway error = %q, direct error = %q", envelope.Error, direct)
	}
}

// TestEmptyIsNotMissing draws the line P18 must not blur. A search with no
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

// TestUnknownSymbolKeepsItsExistingShapeOnTraversals records a deliberate limit
// of P18's scope. These tools answer an unknown symbol with an empty result
// today; P18 normalizes the surface that leaked a driver string, and does not
// convert working empty answers into errors. The inconsistency is recorded as
// follow-up work rather than fixed here.
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

// TestAmbiguousNameIsNotReportedAsMissing preserves the existing behaviour for
// a name several definitions share: the deterministic first candidate answers.
// Folding that into "not found" would be a lie about the index.
func TestAmbiguousNameIsNotReportedAsMissing(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	// "Helper" exists in the fixture; ask for it by its short name.
	isErr, text := callResult(t, server, "find_related_tests", map[string]any{"symbol": "Helper"})
	if isErr {
		t.Fatalf("an existing symbol was reported as an error: %s", text)
	}
	if strings.Contains(text, "not found") {
		t.Fatalf("an existing symbol was described as not found: %s", text)
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

package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestTraceJSONCompactAndGatewayShareClampedMetadata(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	pageArgs := map[string]any{"symbol": "Helper", "limit": 2, "offset": 0}
	pageJSON := jsonRecords(t, server, context.Background(), "trace_dependencies", "dependencies", pageArgs)
	pageCompact := callToolCompactDoc(t, server, context.Background(), "trace_dependencies", pageArgs)
	assertSectionMatchesJSON(t, "trace dependencies", pageCompact.Section("dependencies"), pageJSON, map[string]bool{"depth": true})

	args := map[string]any{"symbol": "Helper", "limit": 1, "offset": 100000}

	jsonText, isErr := callToolRaw(t, server, context.Background(), "trace_dependencies", args)
	if isErr {
		t.Fatalf("JSON trace returned an error: %s", jsonText)
	}
	var envelope struct {
		Data struct {
			Dependencies []map[string]any `json:"dependencies"`
			TargetFound  bool             `json:"target_found"`
			Total        int              `json:"total"`
			Offset       int              `json:"offset"`
			Truncated    bool             `json:"truncated"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(jsonText), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Dependencies) != 0 || envelope.Data.Offset != envelope.Data.Total || envelope.Data.Truncated {
		t.Fatalf("JSON past-end metadata = %+v", envelope.Data)
	}

	compact := callToolCompactDoc(t, server, context.Background(), "trace_dependencies", args)
	presence := compact.Section("presence")
	var offset, total, truncated string
	var offsetOK, totalOK, truncatedOK bool
	if presence != nil {
		offset, offsetOK = presence.Cell(0, "offset")
		total, totalOK = presence.Cell(0, "total")
		truncated, truncatedOK = presence.Cell(0, "truncated")
	}
	if !offsetOK || !totalOK || !truncatedOK || offset != total || truncated != "false" {
		t.Fatalf("compact past-end metadata = %+v", presence)
	}

	if err := server.SetToolMode(ToolModeGateway); err != nil {
		t.Fatal(err)
	}
	gatewayErr, gatewayText := callResult(t, server, "tool_call", map[string]any{"name": "trace_dependencies", "arguments": args})
	if gatewayErr || gatewayText != jsonText {
		t.Fatalf("gateway mismatch: err=%v text=%s want=%s", gatewayErr, gatewayText, jsonText)
	}
}

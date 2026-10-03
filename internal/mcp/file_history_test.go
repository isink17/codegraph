package mcp

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/isink17/codegraph/internal/githistory"
	"github.com/isink17/codegraph/internal/store"
)

// file_history is a specialized tool: absent from tools/list in both modes,
// found by tool_search, callable directly and through tool_call.
func TestFileHistoryIsDiscoverableButNotListed(t *testing.T) {
	for _, mode := range []ToolMode{ToolModeFull, ToolModeGateway} {
		if names := toolNames(listedTools(t, newGatewayTestServer(t, mode))); slices.Contains(names, "file_history") {
			t.Fatalf("%s tools/list advertises file_history: %v", mode, names)
		}
	}
	server := newGatewayTestServer(t, ToolModeGateway)
	for _, query := range []string{"git history", "file history churn", "file_history"} {
		names := searchNames(t, searchTools(t, server, map[string]any{"query": query}))
		if len(names) == 0 || names[0] != "file_history" {
			t.Errorf("tool_search(%q) = %v, want file_history first", query, names)
		}
	}
	decode := func(text string) store.GitHistoryResult {
		t.Helper()
		var envelope struct {
			OK   bool                   `json:"ok"`
			Data store.GitHistoryResult `json:"data"`
		}
		if err := json.Unmarshal([]byte(text), &envelope); err != nil || !envelope.OK {
			t.Fatalf("file_history payload %s: %v", text, err)
		}
		return envelope.Data
	}
	// The fixture root is a temp directory outside any Git repository.
	isErr, direct := callResult(t, server, "file_history", map[string]any{"files": []string{"main.go"}})
	if isErr {
		t.Fatalf("file_history failed: %s", direct)
	}
	isErr, viaGateway := callResult(t, server, "tool_call", map[string]any{"name": "file_history", "arguments": map[string]any{"files": []string{"main.go"}}})
	if isErr {
		t.Fatalf("tool_call file_history failed: %s", viaGateway)
	}
	a, b := decode(direct), decode(viaGateway)
	if a.History.Status != githistory.StatusAbsent || a.History.AbsentReason == "" || len(a.Files) != 0 {
		t.Fatalf("history = %+v", a)
	}
	if a.History != b.History || len(b.Files) != 0 {
		t.Fatalf("direct %+v != gateway %+v", a, b)
	}
	if msg := callError(t, server, "file_history", map[string]any{"limit": 100000}); msg == "" {
		t.Fatal("unbounded limit accepted")
	}
}

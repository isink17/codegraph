package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateToolArgumentsRequiresObjectRoot(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(` null `)} {
		err := validateToolArguments("graph_stats", raw)
		if err == nil || !strings.Contains(err.Error(), "JSON object") {
			t.Errorf("validateToolArguments(graph_stats, %q) = %v, want JSON object error", raw, err)
		}
	}
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`{}`)} {
		if err := validateToolArguments("graph_stats", raw); err != nil {
			t.Errorf("validateToolArguments(graph_stats, %q): %v", raw, err)
		}
	}
}

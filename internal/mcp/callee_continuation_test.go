package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/compactfmt"
)

func TestCalleeContinuationResponseContracts(t *testing.T) {
	s, ctx := detailFixtureWithStableIDs(t, true)
	for _, format := range []string{"json", "compact", "compact-v2"} {
		for _, offset := range []int{0, 1, 2, 9} {
			args := map[string]any{"symbol": "lib.AlsoUseHelper", "limit": 1, "offset": offset, "format": format}
			direct, failed := callToolRaw(t, s, ctx, "find_callees", args)
			if failed {
				t.Fatal(direct)
			}
			if err := s.SetToolMode(ToolModeGateway); err != nil {
				t.Fatal(err)
			}
			gateway, failed := callToolRaw(t, s, ctx, "tool_call", map[string]any{"name": "find_callees", "arguments": args})
			if failed || gateway != direct {
				t.Fatal("gateway changed response", gateway)
			}
			more := offset == 0
			switch format {
			case "json":
				var result struct {
					Data struct {
						HasMore    *bool             `json:"has_more"`
						NextOffset *int              `json:"next_offset"`
						Callees    []json.RawMessage `json:"callees"`
					}
				}
				if err := json.Unmarshal([]byte(direct), &result); err != nil {
					t.Fatal(err)
				}
				if result.Data.HasMore == nil || *result.Data.HasMore != more {
					t.Fatal(direct)
				}
				if more {
					if result.Data.NextOffset == nil || *result.Data.NextOffset != 1 {
						t.Fatal(direct)
					}
				} else if result.Data.NextOffset != nil {
					t.Fatal(direct)
				}
			case "compact":
				doc, err := compactfmt.Decode(direct)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, sec := range doc.Sections {
					if sec.Name == "pagination" {
						found = true
						next := ""
						if more {
							next = "1"
						}
						if len(sec.Rows) != 1 || strings.Join(sec.Rows[0], "|") != fmt.Sprintf("%t|%s", more, next) {
							t.Fatal(direct)
						}
					}
				}
				if !found {
					t.Fatal("missing pagination section")
				}
			case "compact-v2":
				doc, err := compactfmt.DecodeV2(direct)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, sec := range doc.Sections {
					if sec.Name == "pagination" {
						found = true
						if len(sec.Rows) != 1 || sec.Rows[0][0].Scalar != fmt.Sprint(more) {
							t.Fatal(direct)
						}
						next := ""
						if more {
							next = "1"
						}
						if sec.Rows[0][1].Scalar != next {
							t.Fatal(direct)
						}
					}
				}
				if !found {
					t.Fatal("missing pagination section")
				}
			}
		}
	}
	for _, args := range []map[string]any{{"symbol": "missing"}, {"symbol_id": 999999}} {
		text, failed := callToolRaw(t, s, ctx, "find_callees", args)
		if failed || !strings.Contains(text, `"has_more":false`) {
			t.Fatal(text)
		}
	}
	for _, args := range []map[string]any{{"symbol": "lib.AlsoUseHelper", "limit": 501}, {"symbol": "lib.AlsoUseHelper", "offset": -1}, {"symbol": "lib.AlsoUseHelper", "limit": 1e100}} {
		text, failed := callToolRaw(t, s, ctx, "find_callees", args)
		if !failed {
			t.Fatal("invalid input accepted", text)
		}
	}
}

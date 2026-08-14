package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/compactfmt"
	"github.com/isink17/codegraph/internal/limits"
)

// noDriverErrorSubstring is the string P18 exists to keep off the public
// surface. A caller that reads it learns nothing about its own request and may
// well conclude the index is broken.
const noDriverErrorSubstring = "sql: no rows in result set"

// pagedTools are the public tools that take a `limit` and are bounded by the
// shared policy. tool_search and usage_stats are deliberately absent: both own
// a smaller maximum and clamp rather than reject, which TestSelfBoundedToolsKeepTheirOwnPolicy covers.
var pagedTools = []struct {
	name string
	args map[string]any
}{
	{"search_symbols", map[string]any{"query": "Helper"}},
	{"find_symbol", map[string]any{"query": "Helper"}},
	{"find_callers", map[string]any{"symbol": "Helper"}},
	{"find_callees", map[string]any{"symbol": "Helper"}},
	{"find_related_tests", map[string]any{"symbol": "Helper"}},
	{"search_semantic", map[string]any{"query": "helper"}},
	{"find_dead_code", map[string]any{}},
	{"list_files", map[string]any{}},
	{"list_repos", map[string]any{}},
	{"list_scans", map[string]any{}},
	{"latest_scan_errors", map[string]any{}},
	{"session_history", map[string]any{}},
	{"session_hot_files", map[string]any{}},
	{"graph_analytics", map[string]any{"analysis": "pagerank"}},
	{"get_impact_radius", map[string]any{"symbols": []any{"Helper"}}},
	{"trace_dependencies", map[string]any{"symbol": "Helper"}},
}

func withArgs(base map[string]any, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// TestEveryPagedToolRejectsAnAbsurdLimit is the P18 headline. Before this
// phase, limit=100000000 travelled through validation, through the query layer,
// and into SQLite as a real LIMIT on every one of these tools.
func TestEveryPagedToolRejectsAnAbsurdLimit(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	for _, tool := range pagedTools {
		t.Run(tool.name, func(t *testing.T) {
			msg := callError(t, server, tool.name, withArgs(tool.args, map[string]any{"limit": 100000000}))
			if !strings.Contains(msg, "limit") || !strings.Contains(msg, "500") {
				t.Fatalf("%s: error = %q, want it to name the limit and its maximum", tool.name, msg)
			}
		})
	}
}

// TestPagedToolLimitBoundary walks the full matrix on one representative tool
// from each family, rather than repeating ten assertions across sixteen tools.
func TestPagedToolLimitBoundary(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	for _, tool := range []string{"search_symbols", "find_callers", "list_files", "get_impact_radius"} {
		args := map[string]any{}
		for _, candidate := range pagedTools {
			if candidate.name == tool {
				args = candidate.args
			}
		}
		t.Run(tool, func(t *testing.T) {
			accepted := []any{limits.MaxPage - 1, limits.MaxPage, 1, 0}
			for _, value := range accepted {
				if isErr, text := callResult(t, server, tool, withArgs(args, map[string]any{"limit": value})); isErr {
					t.Fatalf("limit=%v was rejected: %s", value, text)
				}
			}
			rejected := []any{limits.MaxPage + 1, 100000000, -1, int64(1) << 40}
			for _, value := range rejected {
				if isErr, _ := callResult(t, server, tool, withArgs(args, map[string]any{"limit": value})); !isErr {
					t.Fatalf("limit=%v was accepted, want a validation error", value)
				}
			}
			// An omitted limit keeps the tool's own default; it is not an error.
			if isErr, text := callResult(t, server, tool, args); isErr {
				t.Fatalf("omitted limit was rejected: %s", text)
			}
			// A wrong JSON type is still a type error, not a range error.
			msg := callError(t, server, tool, withArgs(args, map[string]any{"limit": "many"}))
			if !strings.Contains(msg, "must be an integer") {
				t.Fatalf("limit=\"many\" error = %q, want an integer type error", msg)
			}
		})
	}
}

// TestLimitZeroStillMeansDefault pins the established convention. Turning 0
// into "return nothing" would silently empty every caller that spells "use the
// default" that way.
func TestLimitZeroStillMeansDefault(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	explicit := searchMatchCount(t, server, map[string]any{"query": "Helper", "limit": 0})
	omitted := searchMatchCount(t, server, map[string]any{"query": "Helper"})
	if explicit != omitted {
		t.Fatalf("limit=0 returned %d matches, omitted returned %d; 0 must mean the default", explicit, omitted)
	}
}

func searchMatchCount(t *testing.T, server *Server, args map[string]any) int {
	t.Helper()
	isErr, text := callResult(t, server, "search_symbols", args)
	if isErr {
		t.Fatalf("search_symbols(%v): %s", args, text)
	}
	var envelope struct {
		Data struct {
			Matches []json.RawMessage `json:"matches"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return len(envelope.Data.Matches)
}

// TestSourceDetailLowersTheRowCeiling records the measurement that motivated a
// per-detail ceiling: a page of `full` records carries source text, so the same
// row count costs several times what a page of cards costs.
func TestSourceDetailLowersTheRowCeiling(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	cases := []struct {
		level string
		max   int
	}{
		{"card", limits.MaxPage},
		{"skeleton", limits.MaxPageSkeleton},
		{"excerpt", limits.MaxPageSource},
		{"full", limits.MaxPageSource},
	}
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			args := map[string]any{"query": "Helper", "detail": tc.level}
			if isErr, text := callResult(t, server, "search_symbols", withArgs(args, map[string]any{"limit": tc.max})); isErr {
				t.Fatalf("limit=%d at detail=%s was rejected: %s", tc.max, tc.level, text)
			}
			msg := callError(t, server, "search_symbols", withArgs(args, map[string]any{"limit": tc.max + 1}))
			if !strings.Contains(msg, tc.level) {
				t.Fatalf("error for limit=%d at detail=%s = %q, want it to name the detail level", tc.max+1, tc.level, msg)
			}
		})
	}
}

// TestFormatCannotBypassTheLimit is the encoder-versus-query distinction: a
// compact row is smaller, but the ceiling belongs to the query that produced
// the rows, not to the way they are written down.
func TestFormatCannotBypassTheLimit(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	for _, format := range []string{"json", "compact"} {
		args := map[string]any{"query": "Helper", "format": format, "limit": 100000000}
		isErr, text := callResult(t, server, "search_symbols", args)
		if !isErr {
			t.Fatalf("format=%s accepted limit=100000000", format)
		}
		// The response is an error or a document, never a document with an
		// error appended to it.
		if strings.Contains(text, compactfmt.Version) {
			t.Fatalf("format=%s produced a partial compact document alongside the error: %s", format, text)
		}
	}
}

// TestDepthBoundary covers the recursive-expansion family. Depth is what
// actually bounds a traversal's work, so a row limit alone would not protect it.
func TestDepthBoundary(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"get_impact_radius", map[string]any{"symbols": []any{"Helper"}}},
		{"trace_dependencies", map[string]any{"symbol": "Helper"}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			for _, depth := range []any{0, 1, limits.MaxDepth} {
				if isErr, text := callResult(t, server, tc.tool, withArgs(tc.args, map[string]any{"depth": depth})); isErr {
					t.Fatalf("depth=%v was rejected: %s", depth, text)
				}
			}
			for _, depth := range []any{limits.MaxDepth + 1, 1000000, -1} {
				msg := callError(t, server, tc.tool, withArgs(tc.args, map[string]any{"depth": depth}))
				if !strings.Contains(msg, "depth") {
					t.Fatalf("depth=%v error = %q, want it to name depth", depth, msg)
				}
			}
		})
	}
}

// TestTraversalPageDeclaresItsOwnIncompleteness is the alternative to silently
// dropping graph nodes: a bounded page says how much of the traversal it is.
func TestTraversalPageDeclaresItsOwnIncompleteness(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)

	isErr, text := callResult(t, server, "trace_dependencies", map[string]any{"symbol": "Helper", "limit": 1})
	if isErr {
		t.Fatalf("trace_dependencies: %s", text)
	}
	var trace struct {
		Data struct {
			Dependencies []json.RawMessage `json:"dependencies"`
			Total        int               `json:"total"`
			Offset       int               `json:"offset"`
			Truncated    bool              `json:"truncated"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &trace); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if trace.Data.Total < len(trace.Data.Dependencies) {
		t.Fatalf("total %d is below the returned page size %d", trace.Data.Total, len(trace.Data.Dependencies))
	}
	if trace.Data.Total > 1 && !trace.Data.Truncated {
		t.Fatalf("a 1-row page of a %d-row chain must report truncated", trace.Data.Total)
	}

	isErr, text = callResult(t, server, "get_impact_radius", map[string]any{"symbols": []any{"Helper"}, "limit": 1})
	if isErr {
		t.Fatalf("get_impact_radius: %s", text)
	}
	var impact struct {
		Data struct {
			Symbols []json.RawMessage `json:"symbols"`
			Summary struct {
				AffectedSymbols int  `json:"affected_symbols"`
				ReturnedSymbols int  `json:"returned_symbols"`
				Truncated       bool `json:"truncated"`
			} `json:"summary"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &impact); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if impact.Data.Summary.ReturnedSymbols != len(impact.Data.Symbols) {
		t.Fatalf("returned_symbols %d does not match the %d symbols in the page", impact.Data.Summary.ReturnedSymbols, len(impact.Data.Symbols))
	}
	if impact.Data.Summary.AffectedSymbols > impact.Data.Summary.ReturnedSymbols && !impact.Data.Summary.Truncated {
		t.Fatalf("a page of %d from %d affected symbols must report truncated", impact.Data.Summary.ReturnedSymbols, impact.Data.Summary.AffectedSymbols)
	}
}

// TestPagingCoversEveryRowExactlyOnce proves the hard maximum bounds a page and
// not the conceptual result set: limit+offset beyond the ceiling is a normal
// request, and walking the pages reproduces the whole result without gaps or
// repeats.
func TestPagingCoversEveryRowExactlyOnce(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)

	whole := searchNamesPage(t, server, limits.MaxPage, 0)
	if len(whole) < 3 {
		t.Skipf("fixture has %d matches, too few to page", len(whole))
	}

	var walked []string
	for offset := 0; offset < len(whole); offset += 2 {
		page := searchNamesPage(t, server, 2, offset)
		walked = append(walked, page...)
	}
	if len(walked) != len(whole) {
		t.Fatalf("paged walk returned %d rows, single page returned %d", len(walked), len(whole))
	}
	for i := range whole {
		if walked[i] != whole[i] {
			t.Fatalf("row %d differs: paged %q, single page %q", i, walked[i], whole[i])
		}
	}

	// A page well inside the ceiling, at an offset past it, is legitimate.
	if isErr, text := callResult(t, server, "search_symbols", map[string]any{"query": "Helper", "limit": 100, "offset": limits.MaxPage + 100}); isErr {
		t.Fatalf("limit=100 offset=%d was rejected: %s", limits.MaxPage+100, text)
	}
	// Only the negative side of offset is an error.
	if msg := callError(t, server, "search_symbols", map[string]any{"query": "Helper", "offset": -1}); !strings.Contains(msg, "offset") {
		t.Fatalf("offset=-1 error = %q, want it to name offset", msg)
	}
}

func searchNamesPage(t *testing.T, server *Server, limit, offset int) []string {
	t.Helper()
	isErr, text := callResult(t, server, "search_symbols", map[string]any{"query": "Helper", "limit": limit, "offset": offset})
	if isErr {
		t.Fatalf("search_symbols limit=%d offset=%d: %s", limit, offset, text)
	}
	var envelope struct {
		Data struct {
			Matches []struct {
				StableKey string `json:"stable_key"`
			} `json:"matches"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make([]string, 0, len(envelope.Data.Matches))
	for _, m := range envelope.Data.Matches {
		out = append(out, m.StableKey)
	}
	return out
}

// TestSelfBoundedToolsKeepTheirOwnPolicy guards the two tools P18 deliberately
// left alone. Both were already bounded, and both clamp rather than reject;
// routing them through the shared policy would change a working contract for no
// safety gain.
func TestSelfBoundedToolsKeepTheirOwnPolicy(t *testing.T) {
	gateway := newGatewayTestServer(t, ToolModeGateway)
	data := searchTools(t, gateway, map[string]any{"query": "symbol", "limit": 100})
	limit, ok := data["limit"].(float64)
	if !ok || int(limit) != maxToolSearchLimit {
		t.Fatalf("tool_search limit = %v, want it clamped to %d", data["limit"], maxToolSearchLimit)
	}

	full := newGatewayTestServer(t, ToolModeFull)
	if isErr, text := callResult(t, full, "usage_stats", map[string]any{"limit": 100000000}); isErr {
		t.Fatalf("usage_stats owns its own limit policy and must not reject: %s", text)
	}
}

// TestContextForTaskBudgetSemanticsAreUnchanged checks that the shared numeric
// validator did not reinterpret P14's arguments. max_tokens keeps its own
// zero-means-default and clamp-above-maximum behaviour.
func TestContextForTaskBudgetSemanticsAreUnchanged(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	for _, args := range []map[string]any{
		{"task": "helper", "max_tokens": 0},
		{"task": "helper", "max_tokens": 1000000},
		{"task": "helper", "max_files": 0, "max_symbols": 0},
	} {
		if isErr, text := callResult(t, server, "context_for_task", args); isErr {
			t.Fatalf("context_for_task(%v) was rejected: %s", args, text)
		}
	}
	// Only the new upper bound on the selection arguments is added.
	if msg := callError(t, server, "context_for_task", map[string]any{"task": "helper", "max_symbols": 100000000}); !strings.Contains(msg, "max_symbols") {
		t.Fatalf("max_symbols=100000000 error = %q, want it to name max_symbols", msg)
	}
}

// TestBatchArgumentsAreBounded covers the work multiplier that is not a row
// count: these arguments cost one lookup per element.
func TestBatchArgumentsAreBounded(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	many := make([]any, limits.MaxBatchItems+1)
	for i := range many {
		many[i] = "Helper"
	}
	if msg := callError(t, server, "get_impact_radius", map[string]any{"symbols": many}); !strings.Contains(msg, "symbols") {
		t.Fatalf("oversized symbols error = %q, want it to name symbols", msg)
	}
	if msg := callError(t, server, "find_related_tests", map[string]any{"files": many}); !strings.Contains(msg, "files") {
		t.Fatalf("oversized files error = %q, want it to name files", msg)
	}
}

// TestSchemaAdvertisesTheEnforcedBounds keeps the published contract and the
// runtime one from drifting: a client that reads `maximum` and obeys it must
// never be rejected. Every bounded integer is checked, not just `limit`, and
// the two self-bounded tools are checked against their own maxima rather than
// the shared one.
func TestSchemaAdvertisesTheEnforcedBounds(t *testing.T) {
	// Both advertised surfaces plus the definitions of the tools that appear in
	// neither, so a hidden or gateway-only tool cannot drift unnoticed.
	seen := map[string]bool{}
	var defs []map[string]any
	for _, set := range [][]map[string]any{staticToolDefinitions, gatewayToolDefinitions} {
		for _, def := range set {
			name, _ := def["name"].(string)
			if seen[name] {
				continue
			}
			seen[name] = true
			defs = append(defs, def)
		}
	}
	for _, desc := range toolRegistry {
		if seen[desc.name] {
			continue
		}
		seen[desc.name] = true
		defs = append(defs, desc.definition())
	}

	for _, def := range defs {
		name, _ := def["name"].(string)
		schema, _ := def["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)

		wantMax := map[string]int{
			"limit":       limits.MaxPage,
			"depth":       limits.MaxDepth,
			"max_files":   limits.MaxContextSelection,
			"max_symbols": limits.MaxContextSelection,
			"examples":    limits.MaxAuditExamples,
			"max_steps":   limits.MaxAgentSteps,
		}
		switch name {
		case "tool_search":
			wantMax["limit"] = maxToolSearchLimit
		case "usage_stats":
			wantMax["limit"] = maxUsageToolRows
		}

		for prop, want := range wantMax {
			raw, ok := props[prop]
			if !ok {
				continue
			}
			spec, _ := raw.(map[string]any)
			if spec["minimum"] != 0 {
				t.Fatalf("%s.%s advertises minimum %v, want 0", name, prop, spec["minimum"])
			}
			if spec["maximum"] != want {
				t.Fatalf("%s.%s advertises maximum %v, want %d", name, prop, spec["maximum"], want)
			}
		}

		// Offsets are deliberately uncapped: a large one costs no more than a
		// scan of the rows that exist, and deep paging is legitimate.
		if raw, ok := props["offset"]; ok {
			spec, _ := raw.(map[string]any)
			if spec["minimum"] != 0 {
				t.Fatalf("%s.offset advertises minimum %v, want 0", name, spec["minimum"])
			}
			if _, has := spec["maximum"]; has {
				t.Fatalf("%s.offset advertises a maximum; offsets are deliberately uncapped", name)
			}
		}
	}
}

// TestEveryBoundedArgumentIsAdvertised is the drift guard for the test above:
// if validateArgumentBounds learns a new argument, the schema must learn it too.
func TestEveryBoundedArgumentIsAdvertised(t *testing.T) {
	bounded := []string{"limit", "offset", "depth", "max_files", "max_symbols", "examples", "max_steps"}
	for _, def := range staticToolDefinitions {
		name, _ := def["name"].(string)
		schema, _ := def["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, prop := range bounded {
			raw, ok := props[prop]
			if !ok {
				continue
			}
			spec, _ := raw.(map[string]any)
			if _, has := spec["minimum"]; !has {
				t.Fatalf("%s.%s is bounded at runtime but advertises no minimum", name, prop)
			}
			if _, has := spec["maximum"]; !has && prop != "offset" {
				t.Fatalf("%s.%s is bounded at runtime but advertises no maximum", name, prop)
			}
		}
	}
}

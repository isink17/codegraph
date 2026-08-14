package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/tokenest"
	"github.com/isink17/codegraph/internal/usage"
)

// newBulkUsageTestServer indexes a repository big enough for a bulk result.
//
// The compact encoding amortizes one header across many rows, so it only wins on
// a result with many rows -- comparing the two encodings on a four-symbol
// repository would measure the header, not the encoding.
func newBulkUsageTestServer(t *testing.T) *Server {
	t.Helper()
	var src strings.Builder
	src.WriteString("package widgets\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&src, "\n// WidgetHandler%02d handles widget %d.\nfunc WidgetHandler%02d(input int) int { return input + %d }\n", i, i, i, i)
	}

	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "widgets.go", src.String())
	st := openTestStore(t)
	t.Cleanup(func() { st.Close() })
	idx := indexer.New(st, parser.NewRegistry(goparser.New()), nil)
	repo, err := st.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	return NewServer(repoRoot, repoRoot, repo.ID, st, idx, query.New(st, nil), io.Discard)
}

// usageReport drives a real `tools/call usage_stats` through Serve and decodes
// the report. Every assertion in this file reads the meter the way an agent
// would, so what is tested is the surface, not an internal counter.
func usageReport(t *testing.T, server *Server, reset bool) usage.Report {
	t.Helper()
	args := map[string]any{}
	if reset {
		args["reset"] = true
	}
	isErr, text := callResult(t, server, "usage_stats", args)
	if isErr {
		t.Fatalf("usage_stats failed: %s", text)
	}
	var envelope struct {
		OK   bool         `json:"ok"`
		Data usage.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode usage_stats: %v (%s)", err, text)
	}
	if !envelope.OK {
		t.Fatalf("usage_stats envelope not ok: %s", text)
	}
	return envelope.Data
}

// toolRow finds the single row for a dimension tuple.
func toolRow(t *testing.T, report usage.Report, name string, via usage.Via) usage.ToolRow {
	t.Helper()
	var found []usage.ToolRow
	for _, row := range report.Tools {
		if row.Name == name && row.Via == via {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("rows for %s/%s = %d, want 1: %+v", name, via, len(found), report.Tools)
	}
	return found[0]
}

func hasRow(report usage.Report, name string) bool {
	for _, row := range report.Tools {
		if row.Name == name {
			return true
		}
	}
	return false
}

// TestUsageStatsIsNotAdvertisedInEitherMode is the P16 invariant P17 must not
// spend: a meter that made every session pay for its own schema would cost more
// context than it could ever account for.
func TestUsageStatsIsNotAdvertisedInEitherMode(t *testing.T) {
	for _, mode := range []ToolMode{ToolModeFull, ToolModeGateway} {
		names := toolNames(listedTools(t, newGatewayTestServer(t, mode)))
		for _, name := range names {
			if name == "usage_stats" {
				t.Fatalf("%s tools/list advertises usage_stats: %v", mode, names)
			}
		}
	}
	// The advertised payloads are byte-identical to what P16 shipped.
	if got := len(toolNames(staticToolDefinitions)); got != len(preGatewayToolNames) {
		t.Fatalf("full tools/list = %d tools, want %d", got, len(preGatewayToolNames))
	}
	if got := toolNames(gatewayToolDefinitions); !reflect.DeepEqual(got, wantGatewayToolNames) {
		t.Fatalf("gateway tools/list = %v, want %v", got, wantGatewayToolNames)
	}
}

// TestPrecomputedDefinitionSizesMatchTheWire proves the meter is not sizing a
// payload it re-serialized: the constants it reads equal the bytes a client gets.
func TestPrecomputedDefinitionSizesMatchTheWire(t *testing.T) {
	for _, tc := range []struct {
		mode ToolMode
		want int
	}{
		{ToolModeFull, staticToolDefinitionsBytes},
		{ToolModeGateway, gatewayToolDefinitionsBytes},
	} {
		payload, err := json.Marshal(listedTools(t, newGatewayTestServer(t, tc.mode)))
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		if len(payload) != tc.want {
			t.Fatalf("%s tools/list = %d bytes, precomputed %d", tc.mode, len(payload), tc.want)
		}
	}
	if staticToolDefinitionsBytes != fullToolsListBytes {
		t.Fatalf("full definitions = %d bytes, byte-identity guard = %d", staticToolDefinitionsBytes, fullToolsListBytes)
	}
}

func TestZeroCallReportIsValid(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	report := server.UsageSnapshot()
	if report.Schema != usage.Schema {
		t.Fatalf("schema = %q", report.Schema)
	}
	if report.Session.MCPCalls != 0 || len(report.Tools) != 0 {
		t.Fatalf("fresh server already has usage: %+v", report)
	}
	if report.Session.ToolMode != string(ToolModeFull) {
		t.Fatalf("tool_mode = %q, want full", report.Session.ToolMode)
	}
}

// TestUsageStatsExcludesItself pins the self-accounting rule: the snapshot covers
// usage up to the asking call, and the asking call lands in the next one.
func TestUsageStatsExcludesItself(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	first := usageReport(t, server, false)
	if hasRow(first, "usage_stats") {
		t.Fatalf("first usage_stats counted itself: %+v", first.Tools)
	}
	second := usageReport(t, server, false)
	row := toolRow(t, second, "usage_stats", usage.ViaDirect)
	if row.Calls != 1 {
		t.Fatalf("usage_stats calls in the next snapshot = %d, want 1", row.Calls)
	}
	if row.ResponseBytes <= 0 {
		t.Fatalf("usage_stats response bytes = %d, want > 0", row.ResponseBytes)
	}
}

// TestDirectCallAccounting covers the base case and the independent byte oracle:
// the meter's response figure is len() of the exact text the model received.
func TestDirectCallAccounting(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	args := map[string]any{"query": "Helper"}
	isErr, text := callResult(t, server, "find_symbol", args)
	if isErr {
		t.Fatalf("find_symbol failed: %s", text)
	}
	report := usageReport(t, server, false)
	row := toolRow(t, report, "find_symbol", usage.ViaDirect)
	if row.Calls != 1 || row.Errors != 0 {
		t.Fatalf("row = %+v", row)
	}
	if int(row.ResponseBytes) != len(text) {
		t.Fatalf("metered response = %d bytes, actual content = %d bytes", row.ResponseBytes, len(text))
	}
	if row.ResponseEstimatedTokens != tokenest.FromBytes(len(text)) {
		t.Fatalf("metered tokens = %d, oracle = %d", row.ResponseEstimatedTokens, tokenest.FromBytes(len(text)))
	}
	// Request cost is the arguments object as it arrived, separate from output.
	wantArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if int(row.RequestBytes) != len(wantArgs) {
		t.Fatalf("metered request = %d bytes, want %d", row.RequestBytes, len(wantArgs))
	}
	if row.EstimatedTokens != row.RequestEstimatedTokens+row.ResponseEstimatedTokens {
		t.Fatalf("row totals do not add up: %+v", row)
	}
	// Defaulted dimensions are the real effective defaults, not a fifth bucket.
	if row.Detail != string(defaultToolDetail) || row.Format != string(defaultToolFormat) {
		t.Fatalf("omitted detail/format = %q/%q, want %q/%q", row.Detail, row.Format,
			defaultToolDetail, defaultToolFormat)
	}
}

func TestDirectErrorIsCounted(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	isErr, text := callResult(t, server, "find_symbol", map[string]any{})
	if !isErr {
		t.Fatalf("missing required argument did not fail: %s", text)
	}
	report := usageReport(t, server, false)
	row := toolRow(t, report, "find_symbol", usage.ViaDirect)
	if row.Calls != 1 || row.Errors != 1 {
		t.Fatalf("error row = %+v", row)
	}
	if int(row.ResponseBytes) != len(text) {
		t.Fatalf("metered error response = %d, actual = %d", row.ResponseBytes, len(text))
	}
	if report.Session.Errors != 1 {
		t.Fatalf("session errors = %d, want 1", report.Session.Errors)
	}
	// The call failed before the tool ran, so no format/detail is claimed for it.
	if row.Format != "" || row.Detail != "" {
		t.Fatalf("rejected call claimed dimensions: %+v", row)
	}
}

// TestUnknownToolIsBucketed keeps a typo or a probe from becoming a metric label.
func TestUnknownToolIsBucketed(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	if isErr, _ := callResult(t, server, "no_such_tool_xyz", map[string]any{}); !isErr {
		t.Fatal("unknown tool did not fail")
	}
	report := usageReport(t, server, false)
	row := toolRow(t, report, usage.UnknownTool, usage.ViaDirect)
	if row.Calls != 1 || row.Errors != 1 {
		t.Fatalf("unknown row = %+v", row)
	}
	if hasRow(report, "no_such_tool_xyz") {
		t.Fatalf("unknown tool name became a metric label: %+v", report.Tools)
	}
}

// TestGatewayAttributionIsCanonicalAndSingle is the central regression: the same
// semantic call reached two ways produces two rows for one tool, not a tool_call
// row and a double-counted target.
func TestGatewayAttributionIsCanonicalAndSingle(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	args := map[string]any{"query": "Helper"}
	_, directText := callResult(t, server, "find_symbol", args)
	_, gatewayText := callResult(t, server, "tool_call", map[string]any{"name": "find_symbol", "arguments": args})

	if directText != gatewayText {
		t.Fatalf("gateway changed the payload:\n%s\n%s", directText, gatewayText)
	}
	report := usageReport(t, server, false)
	if hasRow(report, "tool_call") {
		t.Fatalf("a forwarded call was charged to tool_call: %+v", report.Tools)
	}
	direct := toolRow(t, report, "find_symbol", usage.ViaDirect)
	gateway := toolRow(t, report, "find_symbol", usage.ViaGateway)
	if direct.Calls != 1 || gateway.Calls != 1 {
		t.Fatalf("calls = %d direct, %d gateway, want 1 each", direct.Calls, gateway.Calls)
	}
	// Same target, same output: the meter must not count the response twice.
	if direct.ResponseBytes != gateway.ResponseBytes || int(direct.ResponseBytes) != len(directText) {
		t.Fatalf("response bytes: direct %d, gateway %d, actual %d",
			direct.ResponseBytes, gateway.ResponseBytes, len(directText))
	}
	// The indirection shows up exactly where it is paid: the request.
	if gateway.RequestBytes <= direct.RequestBytes {
		t.Fatalf("gateway request %d not larger than direct %d", gateway.RequestBytes, direct.RequestBytes)
	}
	invocation := map[usage.Via]usage.InvocationRow{}
	for _, row := range report.Invocation {
		invocation[row.Via] = row
	}
	if invocation[usage.ViaGateway].Calls != 1 {
		t.Fatalf("gateway invocation total = %+v", invocation[usage.ViaGateway])
	}
}

// TestGatewayTargetErrorIsCharged covers the explicit rule for an unresolvable
// forward: it is a gateway call, and it is charged to tool_call, because
// tool_call is the only thing that ran.
func TestGatewayTargetErrorIsCharged(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	if isErr, _ := callResult(t, server, "tool_call", map[string]any{"name": "no_such_tool_xyz"}); !isErr {
		t.Fatal("unknown gateway target did not fail")
	}
	// A resolvable target that fails its own validation is charged to the target.
	if isErr, _ := callResult(t, server, "tool_call", map[string]any{"name": "find_symbol", "arguments": map[string]any{}}); !isErr {
		t.Fatal("invalid target arguments did not fail")
	}
	report := usageReport(t, server, false)
	unresolved := toolRow(t, report, "tool_call", usage.ViaGateway)
	if unresolved.Calls != 1 || unresolved.Errors != 1 {
		t.Fatalf("unresolved gateway row = %+v", unresolved)
	}
	target := toolRow(t, report, "find_symbol", usage.ViaGateway)
	if target.Calls != 1 || target.Errors != 1 {
		t.Fatalf("failed target row = %+v", target)
	}
}

// TestToolSearchIsCountedSeparately: discovery through the gateway is a real
// model-visible call and gets its own row.
func TestToolSearchIsCountedSeparately(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	isErr, text := callResult(t, server, "tool_search", map[string]any{"query": "usage tokens"})
	if isErr {
		t.Fatalf("tool_search failed: %s", text)
	}
	// The hidden diagnostic is discoverable, which is how a gateway agent finds it
	// without it becoming a seventh advertised tool.
	if !strings.Contains(text, "usage_stats") {
		t.Fatalf("tool_search did not find usage_stats: %s", text)
	}
	report := usageReport(t, server, false)
	row := toolRow(t, report, "tool_search", usage.ViaDirect)
	if row.Calls != 1 || int(row.ResponseBytes) != len(text) {
		t.Fatalf("tool_search row = %+v, actual bytes %d", row, len(text))
	}
}

// TestHiddenDiagnosticDoesNotCrowdOutRealTools: a diagnostic must not spend the
// discovery budget gateway mode exists to protect. It ranks below every real
// tool that matched, and still wins on its own exact name.
func TestHiddenDiagnosticDoesNotCrowdOutRealTools(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	names := func(query string) []string {
		isErr, text := callResult(t, server, "tool_search", map[string]any{"query": query})
		if isErr {
			t.Fatalf("tool_search(%q) failed: %s", query, text)
		}
		var envelope struct {
			Data struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(text), &envelope); err != nil {
			t.Fatalf("decode tool_search: %v (%s)", err, text)
		}
		out := make([]string, 0, len(envelope.Data.Tools))
		for _, tool := range envelope.Data.Tools {
			out = append(out, tool.Name)
		}
		return out
	}
	// Wherever it matches, it is LAST -- not merely not-first. Demoting in the
	// ordering rather than the score is what makes that absolute.
	for _, query := range []string{"graph stats", "session context", "architecture overview", "report", "usage tokens"} {
		got := names(query)
		for i, name := range got {
			if name == "usage_stats" && i != len(got)-1 {
				t.Fatalf("tool_search(%q) ranked the diagnostic at %d of %d: %v", query, i, len(got), got)
			}
		}
	}
	// Demotion must not become deletion: a weak match still has to be findable, or
	// the diagnostic is documented as discoverable and is not.
	for _, query := range []string{"usage", "usage tokens", "token usage per tool", "diagnostic"} {
		var found bool
		for _, name := range names(query) {
			if name == "usage_stats" {
				found = true
			}
		}
		if !found {
			t.Fatalf("tool_search(%q) = %v, want the diagnostic among them", query, names(query))
		}
	}
	// Named directly, it is first -- someone who typed usage_stats meant it.
	if got := names("usage_stats"); len(got) == 0 || got[0] != "usage_stats" {
		t.Fatalf("tool_search(usage_stats) = %v, want it first", got)
	}
}

// TestMalformedToolCallParamsAreCounted: a client that cannot serialize its own
// call still burns context on the rejection, so the rejection is booked.
func TestMalformedToolCallParamsAreCounted(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	resp, ok := server.handle(context.Background(), rpcRequest{
		JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: json.RawMessage(`{"name":`),
	})
	if !ok || resp.Error == nil {
		t.Fatalf("malformed params did not produce an error response: %+v", resp)
	}
	report := server.UsageSnapshot()
	row := toolRow(t, report, usage.UnknownTool, usage.ViaDirect)
	if row.Calls != 1 || row.Errors != 1 {
		t.Fatalf("malformed-params row = %+v", row)
	}
	if report.Session.Errors != 1 {
		t.Fatalf("session errors = %d, want 1", report.Session.Errors)
	}
}

// TestUsageReportIsBounded: the meter's own report must not become the most
// expensive response in the session.
func TestUsageReportIsBounded(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	// More distinct dimension tuples than the default row cap.
	for i := 0; i < usage.DefaultToolRows+5; i++ {
		for _, level := range []string{"card", "skeleton", "excerpt", "full"} {
			callResult(t, server, "find_symbol", map[string]any{"query": "Helper", "detail": level})
		}
		callResult(t, server, "graph_stats", map[string]any{})
	}
	report := usageReport(t, server, false)
	if len(report.Tools) > usage.DefaultToolRows {
		t.Fatalf("report rows = %d, want at most %d", len(report.Tools), usage.DefaultToolRows)
	}
	if report.ToolsTotal < len(report.Tools) {
		t.Fatalf("tools_total %d is below the rows returned %d", report.ToolsTotal, len(report.Tools))
	}
	if got := usageReportWithLimit(t, server, 2); len(got.Tools) != 2 {
		t.Fatalf("limit=2 returned %d rows", len(got.Tools))
	}
	// The argument added to bound the report must not be the way to unbound it.
	if got := usageReportWithLimit(t, server, 1_000_000); len(got.Tools) > maxUsageToolRows {
		t.Fatalf("limit=1000000 returned %d rows, want at most %d", len(got.Tools), maxUsageToolRows)
	}
}

func usageReportWithLimit(t *testing.T, server *Server, limit int) usage.Report {
	t.Helper()
	isErr, text := callResult(t, server, "usage_stats", map[string]any{"limit": limit})
	if isErr {
		t.Fatalf("usage_stats failed: %s", text)
	}
	var envelope struct {
		Data usage.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode usage_stats: %v", err)
	}
	return envelope.Data
}

// TestGatewayCanDiscoverThenCallTheDiagnostic walks the documented gateway path
// end to end: search for it, then call it by name.
func TestGatewayCanDiscoverThenCallTheDiagnostic(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	if isErr, text := callResult(t, server, "tool_search", map[string]any{"query": "usage tokens"}); isErr {
		t.Fatalf("tool_search failed: %s", text)
	}
	isErr, text := callResult(t, server, "tool_call", map[string]any{"name": "usage_stats", "arguments": map[string]any{}})
	if isErr {
		t.Fatalf("tool_call usage_stats failed: %s", text)
	}
	if !strings.Contains(text, usage.Schema) {
		t.Fatalf("gateway usage report is not a usage document: %s", text)
	}
}

// TestToolsListAccountingCountsEveryCall: discovery is charged per call, not once
// per session by assumption.
func TestToolsListAccountingCountsEveryCall(t *testing.T) {
	for _, tc := range []struct {
		mode  ToolMode
		bytes int
	}{
		{ToolModeFull, staticToolDefinitionsBytes},
		{ToolModeGateway, gatewayToolDefinitionsBytes},
	} {
		server := newGatewayTestServer(t, tc.mode)
		rpcRoundTrip(t, server, initializeRequest(1), toolsListRequest(2), toolsListRequest(3))
		report := server.UsageSnapshot()
		if report.Discovery.Calls != 2 {
			t.Fatalf("%s discovery calls = %d, want 2", tc.mode, report.Discovery.Calls)
		}
		if report.Discovery.ResponseBytes != int64(2*tc.bytes) {
			t.Fatalf("%s discovery bytes = %d, want %d", tc.mode, report.Discovery.ResponseBytes, 2*tc.bytes)
		}
		if report.Discovery.ResponseEstimatedTokens != tokenest.FromBytes(2*tc.bytes) {
			t.Fatalf("%s discovery tokens = %d", tc.mode, report.Discovery.ResponseEstimatedTokens)
		}
		if report.Session.ToolsListCalls != 2 || report.Session.MCPCalls != 2 {
			t.Fatalf("%s session = %+v", tc.mode, report.Session)
		}
		if report.Session.ToolMode != string(tc.mode) {
			t.Fatalf("%s report tool_mode = %q", tc.mode, report.Session.ToolMode)
		}
	}
	// Gateway discovery is materially cheaper, which is the whole point of P16.
	if gatewayToolDefinitionsBytes >= staticToolDefinitionsBytes {
		t.Fatalf("gateway discovery %d is not smaller than full %d",
			gatewayToolDefinitionsBytes, staticToolDefinitionsBytes)
	}
}

// TestFormatBucketsSeparateJSONFromCompact is the P15 saving made observable.
func TestFormatBucketsSeparateJSONFromCompact(t *testing.T) {
	server := newBulkUsageTestServer(t)
	args := func(format string) map[string]any {
		return map[string]any{"query": "Widget", "limit": 50, "format": format}
	}
	_, jsonText := callResult(t, server, "search_symbols", args("json"))
	_, compactText := callResult(t, server, "search_symbols", args("compact"))

	report := usageReport(t, server, false)
	var jsonRow, compactRow usage.ToolRow
	for _, row := range report.Tools {
		if row.Name != "search_symbols" {
			continue
		}
		switch row.Format {
		case "json":
			jsonRow = row
		case "compact":
			compactRow = row
		}
	}
	if jsonRow.Calls != 1 || compactRow.Calls != 1 {
		t.Fatalf("format buckets = %+v / %+v", jsonRow, compactRow)
	}
	// Parity with an independent oracle, on both encodings.
	if int(jsonRow.ResponseBytes) != len(jsonText) || int(compactRow.ResponseBytes) != len(compactText) {
		t.Fatalf("metered %d/%d, actual %d/%d",
			jsonRow.ResponseBytes, compactRow.ResponseBytes, len(jsonText), len(compactText))
	}
	// The compact document is measured as the document, not as its escaped
	// representation inside a JSON-RPC line.
	if !strings.HasPrefix(compactText, "@codegraph.compact/v1") {
		t.Fatalf("compact call did not return a compact document: %.60s", compactText)
	}
	if strings.Contains(compactText, `\n`) {
		t.Fatalf("compact text was measured after transport escaping: %.80s", compactText)
	}
	if compactRow.ResponseBytes >= jsonRow.ResponseBytes {
		t.Fatalf("compact %d is not smaller than json %d on a multi-row result",
			compactRow.ResponseBytes, jsonRow.ResponseBytes)
	}
}

// TestDetailBucketsOrderByRealCost is the P13 saving made observable: the meter's
// ordering must be the serialized payloads' ordering, measured not assumed.
func TestDetailBucketsOrderByRealCost(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	levels := []string{"card", "skeleton", "excerpt", "full"}
	actual := map[string]int{}
	for _, level := range levels {
		isErr, text := callResult(t, server, "find_symbol", map[string]any{"query": "Helper", "detail": level})
		if isErr {
			t.Fatalf("find_symbol detail=%s failed: %s", level, text)
		}
		actual[level] = len(text)
	}
	report := usageReport(t, server, false)
	metered := map[string]usage.ToolRow{}
	for _, row := range report.Tools {
		if row.Name == "find_symbol" {
			metered[row.Detail] = row
		}
	}
	for _, level := range levels {
		row, ok := metered[level]
		if !ok {
			t.Fatalf("no bucket for detail=%s: %+v", level, report.Tools)
		}
		if row.Calls != 1 {
			t.Fatalf("detail=%s calls = %d, want 1", level, row.Calls)
		}
		if int(row.ResponseBytes) != actual[level] {
			t.Fatalf("detail=%s metered %d, actual %d", level, row.ResponseBytes, actual[level])
		}
	}
	for i := 1; i < len(levels); i++ {
		prev, cur := metered[levels[i-1]], metered[levels[i]]
		if cur.ResponseBytes < prev.ResponseBytes {
			t.Fatalf("detail=%s (%d) is cheaper than detail=%s (%d); the ladder is not monotonic",
				levels[i], cur.ResponseBytes, levels[i-1], prev.ResponseBytes)
		}
	}
}

// TestContextForTaskPagesCountIndependently: P14's own budget is a property of
// the context document; the meter measures the tool envelope, and each page is a
// separate model-visible call.
func TestContextForTaskPagesCountIndependently(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	isErr, first := callResult(t, server, "context_for_task",
		map[string]any{"task": "understand Helper and Caller", "max_tokens": 400})
	if isErr {
		t.Fatalf("context_for_task failed: %s", first)
	}
	var envelope struct {
		Data struct {
			EstimatedTokens int    `json:"estimated_tokens"`
			Cursor          string `json:"cursor"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(first), &envelope); err != nil {
		t.Fatalf("decode context_for_task: %v", err)
	}
	calls := 1
	if envelope.Data.Cursor != "" {
		if isErr, second := callResult(t, server, "context_for_task",
			map[string]any{"task": "understand Helper and Caller", "max_tokens": 400, "cursor": envelope.Data.Cursor}); isErr {
			t.Fatalf("context_for_task page 2 failed: %s", second)
		}
		calls++
	}
	report := usageReport(t, server, false)
	row := toolRow(t, report, "context_for_task", usage.ViaDirect)
	if row.Calls != calls {
		t.Fatalf("context_for_task calls = %d, want %d", row.Calls, calls)
	}
	// The tool has no format or detail argument, so it claims neither bucket.
	if row.Format != "" || row.Detail != "" {
		t.Fatalf("context_for_task claimed dimensions it has no arguments for: %+v", row)
	}
	// P14's estimated_tokens describes the context document; P17 measures the
	// model-visible envelope that carries it. The envelope is the larger of the
	// two, and the difference is the JSON wrapper, not double counting.
	if envelope.Data.EstimatedTokens > row.ResponseEstimatedTokens {
		t.Fatalf("context document estimate %d exceeds the envelope estimate %d",
			envelope.Data.EstimatedTokens, row.ResponseEstimatedTokens)
	}
}

// TestUsageReportRetainsNoPayloads is the retention proof: nothing the caller
// wrote and nothing the tool returned survives into the report.
func TestUsageReportRetainsNoPayloads(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	const secret = "zzsecrettaskzz"
	callResult(t, server, "search_symbols", map[string]any{"query": secret})
	callResult(t, server, "context_for_task", map[string]any{"task": secret})
	callResult(t, server, "find_related_tests", map[string]any{"symbol": secret})

	report := usageReport(t, server, false)
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("usage report retained caller-supplied text: %s", encoded)
	}
	// Only registry names and closed enum values appear as dimensions.
	for _, row := range report.Tools {
		if row.Name != usage.UnknownTool && row.Name != usage.OverflowTool {
			if _, ok := toolByName[row.Name]; !ok {
				t.Fatalf("row name %q is not a canonical tool", row.Name)
			}
		}
		if row.Via != usage.ViaDirect && row.Via != usage.ViaGateway {
			t.Fatalf("row via %q is not an invocation path", row.Via)
		}
		switch row.Format {
		case "", "json", "compact":
		default:
			t.Fatalf("row format %q is not a known encoding", row.Format)
		}
		switch row.Detail {
		case "", "card", "skeleton", "excerpt", "full":
		default:
			t.Fatalf("row detail %q is not a known level", row.Detail)
		}
	}
}

// TestUsageStatsResetIsAtomicAndLocal: reset clears meter counters and nothing
// else, and the pre-reset snapshot is the value that was returned.
func TestUsageStatsResetIsAtomicAndLocal(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	callResult(t, server, "graph_stats", map[string]any{})
	pre := usageReport(t, server, true)
	if !hasRow(pre, "graph_stats") {
		t.Fatalf("pre-reset snapshot lost the call it should report: %+v", pre.Tools)
	}
	if hasRow(pre, "usage_stats") {
		t.Fatalf("the resetting call counted itself: %+v", pre.Tools)
	}
	post := server.UsageSnapshot()
	if post.Session.ToolCalls != 1 || !hasRow(post, "usage_stats") {
		t.Fatalf("after reset the only recorded call should be the resetting one: %+v", post)
	}
	if hasRow(post, "graph_stats") {
		t.Fatalf("reset did not clear counters: %+v", post.Tools)
	}
	// The graph itself is untouched: reset is diagnostic state, not repo state.
	if isErr, text := callResult(t, server, "graph_stats", map[string]any{}); isErr {
		t.Fatalf("graph_stats after reset failed: %s", text)
	}
}

// TestReportIsDeterministicAcrossRenders guards against map-order leakage.
func TestReportIsDeterministicAcrossRenders(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	callResult(t, server, "find_symbol", map[string]any{"query": "Helper"})
	callResult(t, server, "tool_call", map[string]any{"name": "graph_stats", "arguments": map[string]any{}})
	callResult(t, server, "tool_search", map[string]any{"query": "audit"})

	var first string
	for i := 0; i < 5; i++ {
		encoded, err := json.Marshal(server.UsageSnapshot())
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		// elapsed_ms is a clock reading, not a counter; everything else must be
		// stable render to render.
		stable := stripElapsed(t, encoded)
		if i == 0 {
			first = stable
			continue
		}
		if stable != first {
			t.Fatalf("report render %d differs:\n%s\n%s", i, first, stable)
		}
	}
}

func stripElapsed(t *testing.T, encoded []byte) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if session, ok := doc["session"].(map[string]any); ok {
		delete(session, "elapsed_ms")
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(out)
}

// TestConcurrentToolCallsAreMeteredSafely: Serve is serial today, but the meter
// must not be the reason that has to stay true. Run with -race.
func TestConcurrentToolCallsAreMeteredSafely(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	const workers, per = 6, 25
	params := func(name string, args map[string]any) json.RawMessage {
		encoded, err := json.Marshal(map[string]any{"name": name, "arguments": args})
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		return encoded
	}
	direct := params("find_symbol", map[string]any{"query": "Helper"})
	forwarded := params("tool_call", map[string]any{"name": "find_symbol", "arguments": map[string]any{"query": "Helper"}})

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			raw := direct
			if w%2 == 1 {
				raw = forwarded
			}
			for i := 0; i < per; i++ {
				server.handle(context.Background(), rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: raw})
				if i%10 == 0 {
					server.UsageSnapshot()
				}
			}
		}(w)
	}
	wg.Wait()

	report := server.UsageSnapshot()
	if report.Session.ToolCalls != workers*per {
		t.Fatalf("tool calls = %d, want %d", report.Session.ToolCalls, workers*per)
	}
	if got := toolRow(t, report, "find_symbol", usage.ViaDirect).Calls; got != workers/2*per {
		t.Fatalf("direct calls = %d, want %d", got, workers/2*per)
	}
	if got := toolRow(t, report, "find_symbol", usage.ViaGateway).Calls; got != workers/2*per {
		t.Fatalf("gateway calls = %d, want %d", got, workers/2*per)
	}
}

// TestSessionTotalsMatchTheSumOfParts: the report must add up, or none of its
// per-tool figures can be trusted.
func TestSessionTotalsMatchTheSumOfParts(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	rpcRoundTrip(t, server, initializeRequest(1), toolsListRequest(2))
	callResult(t, server, "find_symbol", map[string]any{"query": "Helper"})
	callResult(t, server, "tool_call", map[string]any{"name": "audit", "arguments": map[string]any{}})
	callResult(t, server, "find_symbol", map[string]any{})

	// Summing rows only equals the session totals when nothing was trimmed, so the
	// precondition is asserted rather than assumed: a future edit that adds a sixth
	// call shape must fail here for a stated reason, not silently.
	report := server.usage.Snapshot(false, maxUsageToolRows)
	if report.ToolsTotal != len(report.Tools) {
		t.Fatalf("fixture produced %d rows but the report returned %d; this test needs an untrimmed table",
			report.ToolsTotal, len(report.Tools))
	}
	var reqBytes, respBytes int64
	var calls, errs int
	for _, row := range report.Tools {
		reqBytes += row.RequestBytes
		respBytes += row.ResponseBytes
		calls += row.Calls
		errs += row.Errors
	}
	if calls != report.Session.ToolCalls || errs != report.Session.Errors {
		t.Fatalf("tool rows sum to %d calls / %d errors, session says %d / %d",
			calls, errs, report.Session.ToolCalls, report.Session.Errors)
	}
	if report.Session.RequestBytes != reqBytes+report.Discovery.RequestBytes {
		t.Fatalf("session request bytes %d != tools %d + discovery %d",
			report.Session.RequestBytes, reqBytes, report.Discovery.RequestBytes)
	}
	if report.Session.ResponseBytes != respBytes+report.Discovery.ResponseBytes {
		t.Fatalf("session response bytes %d != tools %d + discovery %d",
			report.Session.ResponseBytes, respBytes, report.Discovery.ResponseBytes)
	}
	if report.Session.MCPCalls != report.Session.ToolCalls+report.Session.ToolsListCalls {
		t.Fatalf("mcp calls %d != tool calls %d + list calls %d",
			report.Session.MCPCalls, report.Session.ToolCalls, report.Session.ToolsListCalls)
	}
	var viaCalls int
	for _, row := range report.Invocation {
		viaCalls += row.Calls
	}
	if viaCalls != report.Session.ToolCalls {
		t.Fatalf("invocation rows sum to %d calls, session says %d", viaCalls, report.Session.ToolCalls)
	}
}

// TestOversizedRequestIsMetered covers the accounting side of P18. A rejected
// request still crossed the wire in both directions, so it still costs context
// and must still be counted -- once, under its own tool name.
func TestOversizedRequestIsMetered(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	before := usageReport(t, server, true)
	if hasRow(before, "search_symbols") {
		t.Fatalf("reset left a search_symbols row: %+v", before.Tools)
	}

	isErr, text := callResult(t, server, "search_symbols", map[string]any{"query": "Helper", "limit": 100000000})
	if !isErr {
		t.Fatalf("oversized limit was accepted: %s", text)
	}

	after := usageReport(t, server, false)
	row := toolRow(t, after, "search_symbols", usage.ViaDirect)
	if row.Calls != 1 || row.Errors != 1 {
		t.Fatalf("oversized-request row = %+v, want 1 call and 1 error", row)
	}
	if row.RequestBytes == 0 || row.RequestEstimatedTokens == 0 {
		t.Fatalf("the rejected request was not charged for its own arguments: %+v", row)
	}
	if int(row.ResponseBytes) != len(text) {
		t.Fatalf("metered error response = %d, actual = %d", row.ResponseBytes, len(text))
	}
}

// TestOversizedRequestThroughTheGatewayIsNotDoubleCounted checks that the extra
// validation P18 adds did not give the gateway a second place to charge for the
// same rejection.
func TestOversizedRequestThroughTheGatewayIsNotDoubleCounted(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	usageReport(t, server, true)

	if isErr, text := callResult(t, server, "tool_call", map[string]any{
		"name":      "find_symbol",
		"arguments": map[string]any{"query": "Helper", "limit": 100000000},
	}); !isErr {
		t.Fatalf("gateway accepted an oversized limit: %s", text)
	}

	report := usageReport(t, server, false)
	var charged int
	for _, row := range report.Tools {
		if row.Name == "find_symbol" {
			charged += row.Calls
		}
	}
	if charged != 1 {
		t.Fatalf("find_symbol was charged %d times for one rejected call: %+v", charged, report.Tools)
	}
}

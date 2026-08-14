package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/isink17/codegraph/internal/usage"
)

// The meter runs on every MCP call, so its own cost has to be small enough to be
// uninteresting. These benchmarks separate the two questions a reviewer should
// ask: what does the bookkeeping cost on its own, and what does it cost as a
// share of a real call?
//
// BenchmarkUsageMeterBookkeeping is the first question with nothing else in the
// frame. The handleUsage* pairs are the second: the same tool call metered and
// unmetered, on the same server, differing only in whether the record is booked.

// benchHandle drives one tools/call through handle, the production path,
// optionally with metering suppressed by pointing the server at a nil meter.
func benchHandle(b *testing.B, metered bool, name string, args json.RawMessage) {
	b.Helper()
	ctx := context.Background()
	server := setupMCPBenchServer(b, ctx)
	if !metered {
		server.usage = nil
	}
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		b.Fatalf("json.Marshal() error = %v", err)
	}
	req := rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		resp, ok := server.handle(ctx, req)
		if !ok {
			b.Fatal("handle() produced no response")
		}
		if isErr, _ := resp.Result["isError"].(bool); isErr {
			b.Fatalf("tool %q failed: %v", name, resp.Result)
		}
	}
}

// BenchmarkUsageMeterCallPath is every instruction P17 adds to one tools/call:
// the per-call record, the context that carries it, both annotations, and the
// aggregate update. The A/B pairs below isolate the aggregate alone, so this is
// the number to quote for total per-call overhead.
func BenchmarkUsageMeterCallPath(b *testing.B) {
	ctx := context.Background()
	meter := usage.New(string(ToolModeGateway), nil)
	raw := json.RawMessage(`{"query":"BenchFn","limit":20,"detail":"card"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := newCallMeter("tool_call")
		callCtx := withCallMeter(ctx, rec)
		inner := callMeterFrom(callCtx)
		inner.markGateway()
		_ = inner.observeTool("find_symbol")
		format, detailLevel := meterDimensions(toolByName["find_symbol"], "json", "card")
		inner.observeDimensions(format, detailLevel)
		meter.RecordToolCall(inner.key(), len(raw), 4096, false)
	}
}

func BenchmarkUsageMeterBookkeeping(b *testing.B) {
	meter := usage.New(string(ToolModeFull), nil)
	key := usage.Key{Tool: "find_symbol", Via: usage.ViaDirect, Format: "json", Detail: "card"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		meter.RecordToolCall(key, 32, 4096, false)
	}
}

// A cheap call is where meter overhead is most visible as a percentage, so it is
// the honest place to look; report the absolute difference, not the ratio.
func BenchmarkHandleGraphStatsMetered(b *testing.B) {
	benchHandle(b, true, "graph_stats", json.RawMessage(`{}`))
}

func BenchmarkHandleGraphStatsUnmetered(b *testing.B) {
	benchHandle(b, false, "graph_stats", json.RawMessage(`{}`))
}

func BenchmarkHandleFindSymbolCardMetered(b *testing.B) {
	benchHandle(b, true, "find_symbol", json.RawMessage(`{"query":"BenchFn","limit":20}`))
}

func BenchmarkHandleFindSymbolCardUnmetered(b *testing.B) {
	benchHandle(b, false, "find_symbol", json.RawMessage(`{"query":"BenchFn","limit":20}`))
}

func BenchmarkHandleSearchSymbolsCompactMetered(b *testing.B) {
	benchHandle(b, true, "search_symbols", json.RawMessage(`{"query":"BenchFn","limit":50,"format":"compact"}`))
}

func BenchmarkHandleSearchSymbolsCompactUnmetered(b *testing.B) {
	benchHandle(b, false, "search_symbols", json.RawMessage(`{"query":"BenchFn","limit":50,"format":"compact"}`))
}

func BenchmarkHandleGatewayToolCallMetered(b *testing.B) {
	ctx := context.Background()
	server := setupMCPBenchServer(b, ctx)
	if err := server.SetToolMode(ToolModeGateway); err != nil {
		b.Fatalf("SetToolMode() error = %v", err)
	}
	params := json.RawMessage(`{"name":"tool_call","arguments":{"name":"graph_stats","arguments":{}}}`)
	req := rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := server.handle(ctx, req); !ok {
			b.Fatal("handle() produced no response")
		}
	}
}

func BenchmarkHandleToolsListMetered(b *testing.B) {
	ctx := context.Background()
	server := setupMCPBenchServer(b, ctx)
	req := rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list", Params: json.RawMessage(`{}`)}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := server.handle(ctx, req); !ok {
			b.Fatal("handle() produced no response")
		}
	}
}

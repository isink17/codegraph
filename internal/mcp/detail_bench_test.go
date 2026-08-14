package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/isink17/codegraph/internal/detail"
)

// BenchmarkServerToolDetailLevels measures the dispatch-and-encode cost of one
// bulk symbol result at each detail level.
//
// It encodes the response the way the transport does, so the reported
// bytes/op is the payload a client would actually receive and the allocations
// include everything the projection had to build to produce it. The question it
// answers is the one progressive disclosure has to get right: a card must be
// cheaper to construct, not merely cheaper to print.
func BenchmarkServerToolDetailLevels(b *testing.B) {
	ctx := context.Background()
	server := setupMCPBenchServer(b, ctx)

	for _, level := range detail.Levels {
		raw := json.RawMessage(fmt.Sprintf(`{"query":"BenchFn","limit":100,"offset":0,"detail":%q}`, level))
		b.Run(string(level), func(b *testing.B) {
			// One warm call to report the payload size alongside the timings.
			result, err := server.callTool(ctx, "search_symbols", raw)
			if err != nil {
				b.Fatalf("callTool(search_symbols) error = %v", err)
			}
			payload, err := json.Marshal(result)
			if err != nil {
				b.Fatalf("json.Marshal() error = %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := server.callTool(ctx, "search_symbols", raw)
				if err != nil {
					b.Fatalf("callTool(search_symbols) error = %v", err)
				}
				if _, err := json.Marshal(out); err != nil {
					b.Fatalf("json.Marshal() error = %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(len(payload)), "payload_bytes")
			b.ReportMetric(float64(len(payload))/4, "est_tokens")
		})
	}
}

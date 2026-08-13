package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/latency"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// MCP end-to-end latency for the local graph tools.
//
// P12's budget is stated for the store, but nothing reaches a user through the
// store: it reaches them through tool dispatch, a handler, and JSON encoding.
// This benchmark measures the same query at three layers against one graph, so
// the report can say where the time actually goes instead of assuming the store
// number is the whole story:
//
//	store    -- the store call on its own
//	dispatch -- argument validation, unmarshal, handler, response map
//	encode   -- marshalling that response to JSON
//
// The fixture is a hub: one symbol with thousands of callers, which is the
// shape where a per-result cost in the MCP layer would show up.

const (
	mcpBenchFiles      = 200
	mcpBenchSymsPerDir = 30
	mcpBenchHubCallers = 5000
)

func setupMCPHubServer(b *testing.B) (*Server, *store.Store, int64) {
	b.Helper()
	ctx := context.Background()
	repoRoot := b.TempDir()
	s, err := store.OpenWithOptions(filepath.Join(b.TempDir(), "mcp-hub.sqlite"), store.OpenOptions{PerformanceProfile: "fast"})
	if err != nil {
		b.Fatalf("store.OpenWithOptions() error = %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}

	sym := func(name string, line int) graph.Symbol {
		return graph.Symbol{
			Language: "go", Kind: "function", Name: name, QualifiedName: "pkg." + name,
			Signature:  fmt.Sprintf("func %s(ctx context.Context) error", name),
			DocSummary: name + " handles a request",
			Range:      graph.Position{StartLine: line, StartCol: 1, EndLine: line + 8, EndCol: 1},
			StableKey:  "go:pkg." + name,
		}
	}

	hub := graph.ParsedFile{Language: "go"}
	hub.Symbols = append(hub.Symbols, sym("Hub", 1))
	inputs := []store.ReplaceFileGraphInput{
		{Path: "hub.go", Language: "go", SizeBytes: 100, ContentHash: "hub", Parsed: hub},
	}

	callerIdx := 0
	for f := 0; f < mcpBenchFiles; f++ {
		pf := graph.ParsedFile{Language: "go"}
		for m := 0; m < mcpBenchSymsPerDir; m++ {
			name := fmt.Sprintf("Fn%05d", f*mcpBenchSymsPerDir+m)
			pf.Symbols = append(pf.Symbols, sym(name, m*10+1))
			if callerIdx < mcpBenchHubCallers {
				pf.Edges = append(pf.Edges, graph.Edge{
					DstName: "pkg.Hub", Kind: "calls", Evidence: "bench", Line: m*10 + 2,
				})
				callerIdx++
			}
		}
		inputs = append(inputs, store.ReplaceFileGraphInput{
			Path: fmt.Sprintf("pkg/file_%03d.go", f), Language: "go", SizeBytes: 1024,
			ContentHash: fmt.Sprintf("f%03d", f), Parsed: pf,
		})
	}
	if _, err := s.ReplaceFileGraphsBatch(ctx, repo.ID, 1, inputs); err != nil {
		b.Fatalf("ReplaceFileGraphsBatch() error = %v", err)
	}
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		b.Fatalf("ResolveEdges() error = %v", err)
	}
	if _, err := s.Analyze(ctx); err != nil {
		b.Fatalf("Analyze() error = %v", err)
	}

	server := NewServer(repoRoot, repoRoot, repo.ID, s, nil, query.New(s, nil), io.Discard)
	return server, s, repo.ID
}

// benchLayer runs one measured layer and reports percentile metrics.
func benchLayer(b *testing.B, run func() error) {
	b.Helper()
	if err := run(); err != nil {
		b.Fatalf("warmup: %v", err)
	}
	samples := make(latency.Samples, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if err := run(); err != nil {
			b.Fatalf("run: %v", err)
		}
		samples = append(samples, time.Since(start))
	}
	b.StopTimer()
	sorted := samples.Sorted()
	b.ReportMetric(latency.Millis(sorted.SortedPercentile(50)), "p50_ms")
	b.ReportMetric(latency.Millis(sorted.SortedPercentile(95)), "p95_ms")
	b.ReportMetric(latency.Millis(sorted.Max()), "max_ms")
}

func BenchmarkMCPQueryLatency(b *testing.B) {
	ctx := context.Background()
	server, s, repoID := setupMCPHubServer(b)

	cases := []struct {
		name     string
		tool     string
		args     string
		storeRun func() error
	}{
		{
			name: "find_callers_hub", tool: "find_callers",
			args: `{"symbol":"pkg.Hub","limit":20,"offset":0}`,
			storeRun: func() error {
				_, err := s.FindCallers(ctx, repoID, "pkg.Hub", 0, 20, 0)
				return err
			},
		},
		{
			name: "search_symbols", tool: "search_symbols",
			args: `{"query":"Fn00001","limit":20,"offset":0}`,
			storeRun: func() error {
				_, err := s.SearchSymbols(ctx, repoID, "Fn00001", 20, 0)
				return err
			},
		},
		{
			name: "graph_stats", tool: "graph_stats", args: `{}`,
			storeRun: func() error {
				_, err := s.Stats(ctx, repoID)
				return err
			},
		},
	}

	for _, tc := range cases {
		raw := json.RawMessage(tc.args)
		b.Run(tc.name+"/store", func(b *testing.B) {
			benchLayer(b, tc.storeRun)
		})
		b.Run(tc.name+"/dispatch", func(b *testing.B) {
			benchLayer(b, func() error {
				_, err := server.callTool(ctx, tc.tool, raw)
				return err
			})
		})
		b.Run(tc.name+"/dispatch_encode", func(b *testing.B) {
			benchLayer(b, func() error {
				out, err := server.callTool(ctx, tc.tool, raw)
				if err != nil {
					return err
				}
				_, err = json.Marshal(out)
				return err
			})
		})
	}
}

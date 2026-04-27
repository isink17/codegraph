package store

import (
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

var benchSinkInt64 int64

func BenchmarkSrcSymbolChooserChoose_NestedMonotonic(b *testing.B) {
	stableToID := map[string]int64{
		"outer": 1,
		"inner": 2,
		"after": 3,
	}
	symbols := []graph.Symbol{
		{Kind: "function", StableKey: "outer", Range: graph.Position{StartLine: 10, EndLine: 100}},
		{Kind: "function", StableKey: "inner", Range: graph.Position{StartLine: 30, EndLine: 40}},
		{Kind: "function", StableKey: "after", Range: graph.Position{StartLine: 120, EndLine: 130}},
	}
	c := newSrcSymbolChooser(stableToID, symbols)

	// Correctness sanity (nested: outside inner but inside outer).
	if got := c.Choose(50); got != 1 {
		b.Fatalf("Choose(50)=%d, want 1 (outer)", got)
	}

	lines := []int{11, 35, 50, 99, 121, 200}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkInt64 = c.Choose(lines[i%len(lines)])
	}
}

func BenchmarkSrcSymbolChooserChoose_NonMonotonic(b *testing.B) {
	stableToID := map[string]int64{
		"a": 1,
		"b": 2,
		"c": 3,
	}
	// Start lines are not monotonic => linear scan path.
	symbols := []graph.Symbol{
		{Kind: "function", StableKey: "b", Range: graph.Position{StartLine: 20, EndLine: 30}},
		{Kind: "function", StableKey: "a", Range: graph.Position{StartLine: 10, EndLine: 100}},
		{Kind: "function", StableKey: "c", Range: graph.Position{StartLine: 120, EndLine: 130}},
	}
	c := newSrcSymbolChooser(stableToID, symbols)

	lines := []int{11, 25, 50, 121, 200}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkInt64 = c.Choose(lines[i%len(lines)])
	}
}

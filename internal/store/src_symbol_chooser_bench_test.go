package store

import (
	"strconv"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

var benchSinkInt64 int64

func BenchmarkSrcSymbolChooserChoose_NestedMonotonic(b *testing.B) {
	symbols := []graph.Symbol{
		{Kind: "function", QualifiedName: "outer", Range: graph.Position{StartLine: 10, EndLine: 100}},
		{Kind: "function", QualifiedName: "inner", Range: graph.Position{StartLine: 30, EndLine: 40}},
		{Kind: "function", QualifiedName: "after", Range: graph.Position{StartLine: 120, EndLine: 130}},
	}
	c := newSrcSymbolChooser([]int64{1, 2, 3}, symbols)

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
	// Start lines are not monotonic => the constructor has to sort.
	symbols := []graph.Symbol{
		{Kind: "function", QualifiedName: "b", Range: graph.Position{StartLine: 20, EndLine: 30}},
		{Kind: "function", QualifiedName: "a", Range: graph.Position{StartLine: 10, EndLine: 100}},
		{Kind: "function", QualifiedName: "c", Range: graph.Position{StartLine: 120, EndLine: 130}},
	}
	c := newSrcSymbolChooser([]int64{2, 1, 3}, symbols)

	lines := []int{11, 25, 50, 121, 200}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkInt64 = c.Choose(lines[i%len(lines)])
	}
}

// methodHeavyFile models a class-per-file source layout: one container plus
// many methods, which is the shape the P20b fix newly has to index.
func methodHeavyFile(methods int) ([]int64, []graph.Symbol) {
	ids := make([]int64, 0, methods+1)
	symbols := make([]graph.Symbol, 0, methods+1)

	ids = append(ids, 1)
	symbols = append(symbols, graph.Symbol{
		Kind:          "class",
		QualifiedName: "Class",
		Range:         graph.Position{StartLine: 1, EndLine: 2 + methods*10},
	})
	for i := 0; i < methods; i++ {
		ids = append(ids, int64(i+2))
		start := 2 + i*10
		symbols = append(symbols, graph.Symbol{
			Kind:          "method",
			QualifiedName: "Class.m" + strconv.Itoa(i),
			Range:         graph.Position{StartLine: start, EndLine: start + 8},
		})
	}
	return ids, symbols
}

func BenchmarkSrcSymbolChooserChoose_MethodHeavy(b *testing.B) {
	ids, symbols := methodHeavyFile(60)
	c := newSrcSymbolChooser(ids, symbols)

	lines := []int{3, 57, 149, 302, 455, 601, 1000}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkInt64 = c.Choose(lines[i%len(lines)])
	}
}

var benchSinkChooser srcSymbolChooser

func BenchmarkNewSrcSymbolChooser_MethodHeavy(b *testing.B) {
	ids, symbols := methodHeavyFile(60)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkChooser = newSrcSymbolChooser(ids, symbols)
	}
}

// Symbols emitted out of positional order take the sorting path, which the Go
// adapters hit on every file that declares a method.
func BenchmarkNewSrcSymbolChooser_UnsortedEmission(b *testing.B) {
	ids, symbols := methodHeavyFile(60)
	for i, j := 1, len(symbols)-1; i < j; i, j = i+1, j-1 {
		symbols[i], symbols[j] = symbols[j], symbols[i]
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkChooser = newSrcSymbolChooser(ids, symbols)
	}
}

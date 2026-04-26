package store

import (
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestSrcSymbolChooserChoose_NestedFunctions(t *testing.T) {
	stableToID := map[string]int64{
		"outer": 1,
		"inner": 2,
	}
	symbols := []graph.Symbol{
		{
			Kind:      "function",
			StableKey: "outer",
			Range:     graph.Position{StartLine: 10, EndLine: 50},
		},
		{
			Kind:      "function",
			StableKey: "inner",
			Range:     graph.Position{StartLine: 20, EndLine: 30},
		},
	}

	c := newSrcSymbolChooser(stableToID, symbols)

	if got := c.Choose(25); got != 2 {
		t.Fatalf("Choose(25) = %d, want %d (inner)", got, 2)
	}
	if got := c.Choose(35); got != 1 {
		t.Fatalf("Choose(35) = %d, want %d (outer)", got, 1)
	}
}

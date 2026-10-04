package store

import (
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

type colOwner struct {
	id                  int64
	kind                string
	startLine, startCol int
	endLine, endCol     int
}

func colChooser(owners ...colOwner) srcSymbolChooser {
	symbols := make([]graph.Symbol, len(owners))
	ids := make([]int64, len(owners))
	for i, o := range owners {
		symbols[i] = graph.Symbol{Kind: o.kind, Range: graph.Position{StartLine: o.startLine, StartCol: o.startCol, EndLine: o.endLine, EndCol: o.endCol}}
		ids[i] = o.id
	}
	return newSrcSymbolChooser(ids, symbols)
}

func TestSrcSymbolChooserColumns(t *testing.T) {
	// Line 1 holds two methods side by side; a field initializer sits between.
	sameLine := colChooser(
		colOwner{1, "method", 1, 10, 1, 30},
		colOwner{2, "method", 1, 40, 1, 60},
	)
	for _, tc := range []struct {
		name      string
		c         srcSymbolChooser
		line, col int
		want      int64
		kind      sourceAttributionKind
	}{
		{"first method", sameLine, 1, 15, 1, sourceAttributionExact},
		{"second method", sameLine, 1, 45, 2, sourceAttributionExact},
		{"field between methods owns nothing", sameLine, 1, 35, 0, sourceAttributionOutsideSpan},
		{"before first method", sameLine, 1, 5, 0, sourceAttributionOutsideSpan},
		{"column zero keeps line-only ambiguity", sameLine, 1, 0, 0, sourceAttributionAmbiguous},
		{"nested: innermost start wins", colChooser(
			colOwner{1, "method", 1, 1, 5, 2},
			colOwner{2, "method", 2, 3, 2, 40},
		), 2, 10, 2, sourceAttributionExact},
		{"nested: outside inner is outer", colChooser(
			colOwner{1, "method", 1, 1, 5, 2},
			colOwner{2, "method", 2, 3, 2, 40},
		), 2, 50, 1, sourceAttributionExact},
		{"nested same start: shortest wins", colChooser(
			colOwner{1, "method", 1, 1, 1, 80},
			colOwner{2, "method", 1, 1, 1, 40},
		), 1, 10, 2, sourceAttributionExact},
		{"identical ranges differing ids are ambiguous", colChooser(
			colOwner{1, "method", 1, 1, 1, 40},
			colOwner{2, "method", 1, 1, 1, 40},
		), 1, 10, 0, sourceAttributionAmbiguous},
		{"identical range, one id is not ambiguous", colChooser(
			colOwner{1, "method", 1, 1, 1, 40},
			colOwner{1, "method", 1, 1, 1, 40},
		), 1, 10, 1, sourceAttributionExact},
		{"class container never owns", colChooser(colOwner{1, "class", 1, 1, 9, 2}), 3, 5, 0, sourceAttributionNoOwner},
	} {
		got := tc.c.attribute(tc.line, tc.col)
		if got.id != tc.want || got.kind != tc.kind {
			t.Errorf("%s: attribute(%d,%d) = %+v, want id %d kind %d", tc.name, tc.line, tc.col, got, tc.want, tc.kind)
		}
	}
}

// With no column every result must equal Choose, the line-only behaviour.
func TestSrcSymbolChooserColumnZeroMatchesLineOnly(t *testing.T) {
	c := colChooser(
		colOwner{1, "method", 1, 1, 9, 2},
		colOwner{2, "method", 3, 3, 5, 4},
		colOwner{3, "method", 7, 1, 7, 20},
		colOwner{4, "method", 7, 30, 7, 50},
	)
	for line := 0; line <= 11; line++ {
		if a, b := c.attribute(line, 0).id, c.Choose(line); a != b {
			t.Errorf("line %d: attribute %d != Choose %d", line, a, b)
		}
	}
}

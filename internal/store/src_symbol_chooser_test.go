package store

import (
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// owner is one body-owning declaration plus the id its row was persisted under.
// The chooser reads only the kind, the line range, and that id.
type owner struct {
	kind  string
	qname string
	start int
	end   int
	id    int64
}

func fn(qname string, start, end int, id int64) owner {
	return owner{kind: "function", qname: qname, start: start, end: end, id: id}
}

func meth(qname string, start, end int, id int64) owner {
	return owner{kind: "method", qname: qname, start: start, end: end, id: id}
}

func chooserFor(owners ...owner) srcSymbolChooser {
	symbols := make([]graph.Symbol, len(owners))
	ids := make([]int64, len(owners))
	for i, o := range owners {
		symbols[i] = graph.Symbol{
			Kind:          o.kind,
			QualifiedName: o.qname,
			Range:         graph.Position{StartLine: o.start, EndLine: o.end},
		}
		ids[i] = o.id
	}
	return newSrcSymbolChooser(ids, symbols)
}

func TestSrcSymbolChooserChoose_NestedFunctions(t *testing.T) {
	c := chooserFor(
		fn("outer", 10, 50, 1),
		fn("inner", 20, 30, 2),
	)

	if got := c.Choose(25); got != 2 {
		t.Fatalf("Choose(25) = %d, want 2 (inner)", got)
	}
	if got := c.Choose(35); got != 1 {
		t.Fatalf("Choose(35) = %d, want 1 (outer)", got)
	}
}

// A call inside a method body belongs to that method, not to the first
// function in the file. This is the P20b defect: kinds other than "function"
// were skipped entirely, so every method-body call fell through to the
// fallback symbol.
func TestSrcSymbolChooserChoose_MethodOwnsItsBody(t *testing.T) {
	c := chooserFor(
		fn("helper", 1, 3, 1),
		owner{kind: "class", qname: "Service", start: 5, end: 11, id: 2},
		meth("Service.noop", 6, 6, 3),
		meth("Service.process", 8, 10, 4),
	)

	if got := c.Choose(9); got != 4 {
		t.Fatalf("Choose(9) = %d, want 4 (Service.process); a method must own its own body", got)
	}
	if got := c.Choose(6); got != 3 {
		t.Fatalf("Choose(6) = %d, want 3 (Service.noop); a one-line method must own its own line", got)
	}
	if got := c.Choose(2); got != 1 {
		t.Fatalf("Choose(2) = %d, want 1 (helper)", got)
	}
}

// A class/type/struct/interface contains its members' line ranges but has no
// body of its own, so it must never become the source of a member's call.
func TestSrcSymbolChooserChoose_ContainerNeverOwnsEdges(t *testing.T) {
	for _, kind := range []string{"class", "type", "struct", "interface", "enum", "trait", "protocol", "actor", "object", "value"} {
		t.Run(kind, func(t *testing.T) {
			c := chooserFor(
				owner{kind: kind, qname: "Container", start: 1, end: 20, id: 1},
				meth("Container.member", 5, 8, 2),
				fn("top", 30, 34, 3),
			)

			if got := c.Choose(6); got != 2 {
				t.Fatalf("Choose(6) = %d, want 2 (member)", got)
			}
			// Line 15 is inside the container but inside no member. The
			// container must not claim it.
			if got := c.Choose(15); got == 1 {
				t.Fatalf("Choose(15) = %d: %q container must never own a source edge", got, kind)
			}
			if got := c.Choose(15); got != 3 {
				t.Fatalf("Choose(15) = %d, want 3 (top-level fallback)", got)
			}
		})
	}
}

// Nested executables: the innermost represented symbol wins, and the enclosing
// one still owns everything outside the nested range.
func TestSrcSymbolChooserChoose_InnermostExecutableWins(t *testing.T) {
	c := chooserFor(
		meth("C.method", 10, 40, 1),
		fn("nested", 20, 25, 2),
	)

	if got := c.Choose(22); got != 2 {
		t.Fatalf("Choose(22) = %d, want 2 (nested function inside a method)", got)
	}
	if got := c.Choose(30); got != 1 {
		t.Fatalf("Choose(30) = %d, want 1 (method, after the nested function)", got)
	}
	if got := c.Choose(12); got != 1 {
		t.Fatalf("Choose(12) = %d, want 1 (method, before the nested function)", got)
	}
}

// Selection must depend only on the semantic span set, never on the order the
// adapter happened to emit symbols in. The Go adapters emit methods after
// top-level declarations, so unsorted input is the normal case, not a corner.
func TestSrcSymbolChooserChoose_IndependentOfEmissionOrder(t *testing.T) {
	base := []owner{
		fn("early", 1, 5, 4),
		fn("outer", 10, 50, 1),
		meth("C.inner", 20, 30, 2),
		fn("later", 60, 70, 3),
	}
	orders := [][]int{
		{0, 1, 2, 3},
		{3, 2, 1, 0},
		{2, 0, 3, 1},
		{1, 3, 0, 2},
	}
	lines := []int{1, 3, 12, 25, 35, 55, 65, 99}

	var want []int64
	for i, order := range orders {
		owners := make([]owner, 0, len(base))
		for _, idx := range order {
			owners = append(owners, base[idx])
		}
		c := chooserFor(owners...)

		got := make([]int64, 0, len(lines))
		for _, line := range lines {
			got = append(got, c.Choose(line))
		}
		if i == 0 {
			want = got
			continue
		}
		for j, line := range lines {
			if got[j] != want[j] {
				t.Fatalf("order %v: Choose(%d) = %d, want %d (order %v); selection must not depend on emission order",
					order, line, got[j], want[j], orders[0])
			}
		}
	}

	// Pin the semantics too, so the invariance test cannot pass on a shared
	// wrong answer.
	for i, line := range lines {
		var expect int64
		switch {
		case line >= 20 && line <= 30:
			expect = 2
		case line >= 10 && line <= 50:
			expect = 1
		case line >= 1 && line <= 5:
			expect = 4
		case line >= 60 && line <= 70:
			expect = 3
		default:
			expect = 4 // fallback: the earliest top-level function
		}
		if want[i] != expect {
			t.Fatalf("Choose(%d) = %d, want %d", line, want[i], expect)
		}
	}
}

// Declarations that share a stable key — overloads, getter/setter pairs, a
// top-level function and a same-named method — are separate rows in `symbols`,
// so each keeps its own id and each owns its own body. Nothing is ambiguous
// and nothing is dropped.
func TestSrcSymbolChooserChoose_SameNamedDeclarationsKeepOwnBodies(t *testing.T) {
	t.Run("overloads", func(t *testing.T) {
		c := chooserFor(
			fn("Calc.helper", 2, 2, 8),
			fn("Calc.add", 3, 3, 5),
			fn("Calc.add", 4, 4, 6),
		)
		if got := c.Choose(3); got != 5 {
			t.Fatalf("Choose(3) = %d, want 5 (first add overload)", got)
		}
		if got := c.Choose(4); got != 6 {
			t.Fatalf("Choose(4) = %d, want 6 (second add overload)", got)
		}
	})

	t.Run("top-level function and same-named method", func(t *testing.T) {
		c := chooserFor(
			fn("m.other", 1, 5, 8),
			meth("m.Agent.request", 20, 28, 7),
			fn("m.request", 60, 64, 9),
		)
		if got := c.Choose(22); got != 7 {
			t.Fatalf("Choose(22) = %d, want 7 (m.Agent.request)", got)
		}
		if got := c.Choose(62); got != 9 {
			t.Fatalf("Choose(62) = %d, want 9 (m.request)", got)
		}
	})

	t.Run("getter and setter", func(t *testing.T) {
		c := chooserFor(
			meth("O.username", 10, 14, 3),
			meth("O.username", 16, 20, 4),
		)
		if got := c.Choose(12); got != 3 {
			t.Fatalf("Choose(12) = %d, want 3 (getter)", got)
		}
		if got := c.Choose(18); got != 4 {
			t.Fatalf("Choose(18) = %d, want 4 (setter)", got)
		}
	})
}

// Two distinct symbols on exactly the same span cannot be told apart: edges
// carry a line but no column. The chooser must not guess between them.
func TestSrcSymbolChooserChoose_IdenticalSpansAbstain(t *testing.T) {
	t.Run("falls out to the enclosing owner", func(t *testing.T) {
		c := chooserFor(
			fn("outer", 1, 20, 1),
			meth("C.a", 10, 10, 2),
			meth("C.b", 10, 10, 3),
		)
		if got := c.Choose(10); got != 1 {
			t.Fatalf("Choose(10) = %d, want 1 (outer): tied spans must yield to an unambiguous enclosing owner", got)
		}
	})

	t.Run("abstains rather than using the fallback", func(t *testing.T) {
		c := chooserFor(
			fn("other", 1, 5, 1),
			meth("C.a", 10, 10, 2),
			meth("C.b", 10, 10, 3),
		)
		if got := c.Choose(10); got != 0 {
			t.Fatalf("Choose(10) = %d, want 0: with no enclosing owner the chooser must abstain, not fall back to an unrelated symbol", got)
		}
		// The fallback still applies to lines no symbol claims.
		if got := c.Choose(30); got != 1 {
			t.Fatalf("Choose(30) = %d, want 1 (fallback)", got)
		}
	})

	t.Run("abstention does not depend on emission order", func(t *testing.T) {
		forward := chooserFor(
			fn("other", 1, 5, 1),
			meth("C.a", 10, 10, 2),
			meth("C.b", 10, 10, 3),
		)
		reverse := chooserFor(
			meth("C.b", 10, 10, 3),
			meth("C.a", 10, 10, 2),
			fn("other", 1, 5, 1),
		)
		if a, b := forward.Choose(10), reverse.Choose(10); a != b {
			t.Fatalf("Choose(10) = %d forward, %d reversed; must match", a, b)
		}
	})
}

// Three declarations on one span, two of which are the same symbol emitted
// twice. Whichever order the sort leaves them in, the group is still tied.
func TestSrcSymbolChooserChoose_TiedGroupLargerThanTwo(t *testing.T) {
	base := []owner{
		fn("m.outer", 1, 30, 1),
		meth("m.A.run", 10, 10, 5),
		meth("m.A.run", 10, 10, 5),
		meth("m.B.run", 10, 10, 7),
	}
	orders := [][]int{{0, 1, 2, 3}, {0, 3, 1, 2}, {3, 2, 1, 0}, {0, 2, 3, 1}}
	for _, order := range orders {
		owners := make([]owner, 0, len(base))
		for _, idx := range order {
			owners = append(owners, base[idx])
		}
		if got := chooserFor(owners...).Choose(10); got != 1 {
			t.Fatalf("order %v: Choose(10) = %d, want 1 (m.outer); a tied group of three must not resolve to one of its members", order, got)
		}
	}
}

// One declaration emitted twice under one id is a duplicate, not a tie.
func TestSrcSymbolChooserChoose_DuplicateEmissionOfOneSymbol(t *testing.T) {
	c := chooserFor(
		fn("m.other", 1, 3, 6),
		fn("m.dup", 10, 20, 5),
		fn("m.dup", 10, 20, 5),
	)
	if got := c.Choose(15); got != 5 {
		t.Fatalf("Choose(15) = %d, want 5 (m.dup); one symbol emitted twice is not an ambiguity", got)
	}
}

// A reference outside every body-owning span keeps the file's fallback symbol.
// Top-level and file-scope calls must not be dropped.
func TestSrcSymbolChooserChoose_TopLevelKeepsFallback(t *testing.T) {
	c := chooserFor(
		fn("alpha", 1, 2, 1),
		fn("beta", 4, 5, 2),
	)

	if got := c.Choose(7); got != 1 {
		t.Fatalf("Choose(7) = %d, want 1 (fallback); a top-level reference must keep an edge", got)
	}
	if got := c.Choose(0); got != 1 {
		t.Fatalf("Choose(0) = %d, want 1 (fallback)", got)
	}
}

// The fallback is the earliest top-level body owner by position, not the first
// one emitted. A method must never be the fallback: a statement at module
// scope does not belong to some arbitrary method of some arbitrary class.
func TestSrcSymbolChooserChoose_FallbackIsEarliestTopLevelFunction(t *testing.T) {
	t.Run("earliest by position, not by emission", func(t *testing.T) {
		c := chooserFor(
			fn("late", 40, 44, 9),
			fn("early", 5, 9, 3),
		)
		if got := c.Choose(100); got != 3 {
			t.Fatalf("Choose(100) = %d, want 3 (earliest by position)", got)
		}
	})

	t.Run("methods are never the fallback", func(t *testing.T) {
		c := chooserFor(
			meth("App.helper", 2, 4, 5),
			meth("App.other", 6, 8, 6),
		)
		if got := c.Choose(20); got != 0 {
			t.Fatalf("Choose(20) = %d, want 0; a module-level reference must not be credited to a method", got)
		}
		// Method bodies are still attributed normally.
		if got := c.Choose(3); got != 5 {
			t.Fatalf("Choose(3) = %d, want 5 (App.helper)", got)
		}
	})

	t.Run("a top-level function outranks an earlier method", func(t *testing.T) {
		c := chooserFor(
			meth("App.helper", 2, 4, 5),
			fn("top", 10, 12, 7),
		)
		if got := c.Choose(50); got != 7 {
			t.Fatalf("Choose(50) = %d, want 7 (top-level function)", got)
		}
	})
}

// A file with no body-owning symbol at all yields no source, and its edges are
// dropped by the caller. Container symbols must not rescue it.
func TestSrcSymbolChooserChoose_NoExecutableSymbols(t *testing.T) {
	c := chooserFor(owner{kind: "class", qname: "C", start: 1, end: 10, id: 1})

	if got := c.Choose(5); got != 0 {
		t.Fatalf("Choose(5) = %d, want 0", got)
	}
}

// Symbols whose row did not persist have no id and must be skipped without
// shifting ownership onto them.
func TestSrcSymbolChooserChoose_SkipsUnpersistedSymbols(t *testing.T) {
	c := chooserFor(
		fn("kept", 1, 100, 1),
		meth("missing", 10, 20, 0),
	)

	if got := c.Choose(15); got != 1 {
		t.Fatalf("Choose(15) = %d, want 1 (kept); an unpersisted symbol must not win", got)
	}
}

// A short id slice must not panic or misalign ids with symbols.
func TestSrcSymbolChooserChoose_ToleratesShortIDSlice(t *testing.T) {
	symbols := []graph.Symbol{
		{Kind: "function", QualifiedName: "a", Range: graph.Position{StartLine: 1, EndLine: 5}},
		{Kind: "function", QualifiedName: "b", Range: graph.Position{StartLine: 10, EndLine: 15}},
	}
	c := newSrcSymbolChooser([]int64{7}, symbols)

	if got := c.Choose(3); got != 7 {
		t.Fatalf("Choose(3) = %d, want 7", got)
	}
	if got := c.Choose(12); got != 7 {
		t.Fatalf("Choose(12) = %d, want 7 (fallback); the second symbol has no id", got)
	}
}

// Zero-width spans (EndLine == StartLine on a multi-line declaration) are
// produced by the non-cgo heuristic adapters and by the cgo-free Python
// adapter. They must not let a declaration own the lines that follow it.
func TestSrcSymbolChooserChoose_ZeroWidthSpansDoNotSwallowFollowingLines(t *testing.T) {
	c := chooserFor(
		fn("first", 1, 1, 1),
		fn("second", 8, 8, 2),
	)

	if got := c.Choose(8); got != 2 {
		t.Fatalf("Choose(8) = %d, want 2 (second, on its own declaration line)", got)
	}
	// Line 9 is inside second's body in the source but outside its recorded
	// span, so it falls back rather than being claimed by second.
	if got := c.Choose(9); got != 1 {
		t.Fatalf("Choose(9) = %d, want 1 (fallback)", got)
	}
}

func TestOwnsSourceEdges(t *testing.T) {
	owners := []string{"function", "method"}
	containers := []string{"class", "type", "struct", "interface", "enum", "trait", "protocol", "actor", "object", "value", "", "calls"}

	for _, kind := range owners {
		if !ownsSourceEdges(kind) {
			t.Errorf("ownsSourceEdges(%q) = false, want true", kind)
		}
	}
	for _, kind := range containers {
		if ownsSourceEdges(kind) {
			t.Errorf("ownsSourceEdges(%q) = true, want false", kind)
		}
	}
}

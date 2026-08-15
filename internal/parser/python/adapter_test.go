package python

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func parseSource(t *testing.T, src string) graph.ParsedFile {
	t.Helper()
	pf, err := New().Parse(context.Background(), "mod.py", []byte(src))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return pf
}

func symbolRange(t *testing.T, pf graph.ParsedFile, qualified string) graph.Position {
	t.Helper()
	for _, sym := range pf.Symbols {
		if sym.QualifiedName == qualified {
			return sym.Range
		}
	}
	t.Fatalf("symbol %q not found in %v", qualified, func() []string {
		out := make([]string, 0, len(pf.Symbols))
		for _, s := range pf.Symbols {
			out = append(out, s.QualifiedName)
		}
		return out
	}())
	return graph.Position{}
}

func assertSpan(t *testing.T, pf graph.ParsedFile, qualified string, startLine, endLine int) {
	t.Helper()
	r := symbolRange(t, pf, qualified)
	if r.StartLine != startLine || r.EndLine != endLine {
		t.Fatalf("%s span = [%d,%d], want [%d,%d]", qualified, r.StartLine, r.EndLine, startLine, endLine)
	}
}

// The range contract for executable symbols: StartLine is the declaration
// line, EndLine is the last content line of the body (inclusive, matching the
// chooser's start <= line <= end containment test). Blank lines, comments and
// decorator lines after a body do not extend it.
func TestExecutableSymbolRanges(t *testing.T) {
	const src = `class A:
    def first(self):
        helper_a()

    def second(self):
        helper_b()


class B:
    def only(self):
        pass
`
	pf := parseSource(t, src)

	assertSpan(t, pf, "mod.A", 1, 6)
	assertSpan(t, pf, "mod.A.first", 2, 3)
	assertSpan(t, pf, "mod.A.second", 5, 6)
	assertSpan(t, pf, "mod.B", 9, 11)
	// Final method at EOF keeps its body.
	assertSpan(t, pf, "mod.B.only", 10, 11)
}

func TestOneLineAndMultilineBodies(t *testing.T) {
	const src = `def one_liner(): return 1


def multi():
    a = helper(
        1,
    )
    return a
`
	pf := parseSource(t, src)

	assertSpan(t, pf, "mod.one_liner", 1, 1)
	// Indented continuation lines of a call stay inside the body.
	assertSpan(t, pf, "mod.multi", 4, 8)
}

func TestDecoratorsAndCommentsBetweenMethods(t *testing.T) {
	const src = `class S:
    def a(self):
        helper()

    # a comment

    @staticmethod
    def b():
        helper()
`
	pf := parseSource(t, src)

	// a's span ends at its last body line; the comment and decorator that
	// follow belong to no method, and the decorator line stays outside b.
	assertSpan(t, pf, "mod.S.a", 2, 3)
	assertSpan(t, pf, "mod.S.b", 8, 9)
}

func TestNestedDefRanges(t *testing.T) {
	const src = `def outer():
    def inner():
        helper_inner()
    helper_outer()
`
	pf := parseSource(t, src)

	assertSpan(t, pf, "mod.outer", 1, 4)
	assertSpan(t, pf, "mod.inner", 2, 3)
}

// Every edge the adapter emits must land inside the span of some emitted
// executable symbol, so the store's chooser never needs the file-level
// fallback for a call the adapter itself scoped to a function.
func TestEveryEdgeLandsInsideAnExecutableSpan(t *testing.T) {
	const src = `class Service:
    def noop(self):
        pass

    def process(self):
        helper()
        value = other(
            1,
        )


def helper():
    return once()
`
	pf := parseSource(t, src)

	for _, edge := range pf.Edges {
		owned := false
		for _, sym := range pf.Symbols {
			if sym.Kind != "function" {
				continue
			}
			if edge.Line >= sym.Range.StartLine && edge.Line <= sym.Range.EndLine {
				owned = true
				break
			}
		}
		if !owned {
			t.Fatalf("edge to %q at line %d is inside no function span", edge.DstName, edge.Line)
		}
	}
}

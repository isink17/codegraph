package python

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"unicode"

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

func TestCRLFRangesMatchLFRanges(t *testing.T) {
	lf := parseSource(t, "def f():\n    return 1\n")
	crlf := parseSource(t, "def f():\r\n    return 1\r\n")
	want := symbolRange(t, lf, "mod.f")
	got := symbolRange(t, crlf, "mod.f")
	if got != want {
		t.Fatalf("CRLF range = %+v, LF range = %+v", got, want)
	}
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
	assertSpan(t, pf, "mod.outer.inner", 2, 3)
}

func TestAsyncAndNestedNames(t *testing.T) {
	const src = `class Outer:
    async def fetch(self):
        work()
        def inner():
            leaf()

def outer():
    def inner():
        leaf()
`
	p := parseSource(t, src)
	assertSpan(t, p, "mod.Outer.fetch", 2, 5)
	assertSpan(t, p, "mod.Outer.fetch.inner", 4, 5)
	assertSpan(t, p, "mod.outer", 7, 9)
	assertSpan(t, p, "mod.outer.inner", 8, 9)
	for _, sym := range p.Symbols {
		if sym.QualifiedName == "mod.Outer.fetch.inner" && sym.Kind == "method" {
			t.Fatal("nested function was classified as a method")
		}
	}
}

func TestKindFollowsImmediateOwner(t *testing.T) {
	const src = `def top():
    pass

class C:
    def method(self):
        pass

    async def fetch(self):
        pass

class Outer:
    class Inner:
        def method(self):
            pass

def outer():
    def inner():
        pass
    class Local:
        def method(self):
            pass
`
	p := parseSource(t, src)
	want := map[string]string{
		"mod.top":                "function",
		"mod.C.method":           "method",
		"mod.C.fetch":            "method",
		"mod.Outer.Inner.method": "method",
		"mod.outer":              "function",
		"mod.outer.inner":        "function",
		"mod.outer.Local.method": "method",
	}
	for qname, kind := range want {
		found := false
		for _, sym := range p.Symbols {
			if sym.QualifiedName == qname {
				found = true
				if sym.Kind != kind {
					t.Fatalf("%s kind = %q, want %q", qname, sym.Kind, kind)
				}
			}
		}
		if !found {
			t.Fatalf("missing symbol %s", qname)
		}
	}
}

func TestCallsIgnoreStringsCommentsAndTripleStrings(t *testing.T) {
	const src = `def f():
	value = "fake_call()"
	other = 'also_fake()'
	x = 1  # comment_fake()
	"""
	def fake():
	triple_fake()
	"""
	real_call()
`
	p := parseSource(t, src)
	for _, sym := range p.Symbols {
		if sym.Name == "fake" {
			t.Fatal("fabricated declaration from triple string")
		}
	}
	for _, edge := range p.Edges {
		switch edge.DstName {
		case "fake_call", "also_fake", "comment_fake", "triple_fake":
			t.Fatalf("fabricated call %q", edge.DstName)
		case "real_call":
			return
		}
	}
	t.Fatal("real_call was not emitted")
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
			if sym.Kind != "function" && sym.Kind != "method" {
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

// Python identifiers are Unicode (PEP 3131). Every verdict below is what
// Python's own str.isidentifier() says about the name; the adapter must declare
// exactly the valid ones, under exactly that name, and never a fragment of an
// invalid one. The name is the source spelling: CPython binds its NFKC form
// (`ｆｕｌｌ` binds `full`), which neither Python adapter models.
func TestUnicodeDefinitionNamesFollowPython(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"café", true},
		{"Ñandú", true},
		{"变量", true},
		{"℘x", true},      // Other_ID_Start
		{"x٣", true},      // Arabic-Indic digit continues a name
		{"e\u0301", true}, // combining mark continues a name
		{"x·y", true},     // Other_ID_Continue
		{"ｆｕｌｌ", true},    // fullwidth letters
		{"_ñ", true},
		{"€uro", false},
		{"∑", false},
		{"😀", false},
		{"a\u00a0b", false}, // no-break space
		{"·x", false},       // continue-only rune cannot start a name
		{"٣x", false},
		{"\u0301e", false},
		// Python (Unicode 16) accepts U+1C89; Go's tables predate it, so the
		// adapter declares nothing rather than the fragment before it. See
		// TestPythonNamesFollowGoUnicodeTables before changing this case.
		{"x\u1c89y", false},
	}
	for _, tc := range cases {
		src := "def " + tc.name + "():\n    pass\n\nclass " + tc.name + "_k:\n    pass\n"
		want := []string(nil)
		if tc.valid {
			want = []string{tc.name, tc.name + "_k"}
		}
		var got []string
		for _, s := range parseSource(t, src).Symbols {
			got = append(got, s.Name)
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%q: symbols = %q, want %q", tc.name, got, want)
		}
	}
}

// Calls to Unicode names keep their whole name. An ASCII-only identifier class
// used to drop them, or worse emit the ASCII tail of the name as a call to a
// different function (`naïve_func()` became `ve_func`).
func TestUnicodeCallNamesAreWhole(t *testing.T) {
	const src = `class Café:
    def naïve_méthode(self):
        return 变量()

def 变量():
    return ℘x٣()

def ℘x٣():
    pass

def caller():
    Café().naïve_méthode()
    café_obj.métode()
    naïve_func()
    xᲉy()
`
	p := parseSource(t, src)
	assertSpan(t, p, "mod.Café", 1, 3)
	assertSpan(t, p, "mod.Café.naïve_méthode", 2, 3)
	assertSpan(t, p, "mod.变量", 5, 6)
	assertSpan(t, p, "mod.℘x٣", 8, 9)
	if len(p.Symbols) != 5 {
		t.Fatalf("symbols = %v, want exactly Café, naïve_méthode, 变量, ℘x٣, caller", p.Symbols)
	}
	var got []string
	for _, e := range p.Edges {
		got = append(got, e.DstName+"@"+strconv.Itoa(e.Line))
	}
	want := "变量@3|℘x٣@6|Café@12|café_obj.métode@13|naïve_func@14"
	if strings.Join(got, "|") != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

// Which runes are Python names here comes from Go's unicode tables, so a Go
// toolchain with newer tables changes what both Python adapters persist for
// unchanged files. When this fails after a Go upgrade, bump treesitter:python
// and python-regex:python, then update the expected version.
func TestPythonNamesFollowGoUnicodeTables(t *testing.T) {
	if unicode.Version != "15.0.0" {
		t.Fatalf("unicode.Version = %s; bump both Python parser profiles", unicode.Version)
	}
}

// pythonKeywordCallSource holds every place a keyword or a soft keyword sits
// right before `(` without being a call, and case guards however they are
// spaced. The calls it does make are exactly what CPython's ast reports for it.
const pythonKeywordCallSource = `def check(x):
    return x


class Point:
    pass


def run(value, items, cm):
    with cm as (a, b):
        pass
    match check(value):
        case (1, 2):
            pass
        case Point(x=0) | Point(x=1):
            pass
        case [a, b] if check(a):
            pass
        case [a] if(check(a)):
            pass
        case (a)if check(a):
            pass
        case "s"if check(value):
            pass
        case 1	if check(value):
            pass
        case Point(x=0): check(value)
    match (
        value
    ):
        case _:
            pass
    match(value, items)
    case(value)
    return value


def match(*args):
    return args


def case(*args):
    return args
`

// Keyword syntax is not a call: `with x as (a, b)` does not call `as`, a match
// statement does not call `match`, and a case pattern -- `Point(x=0)` is a
// class pattern -- calls nothing, though its guard, a same-line body and the
// match subject can.
// `match` and `case` stay ordinary names everywhere else.
func TestKeywordSyntaxIsNotACall(t *testing.T) {
	var got []string
	for _, e := range parseSource(t, pythonKeywordCallSource).Edges {
		got = append(got, e.DstName+"@"+strconv.Itoa(e.Line))
	}
	want := "check@12 check@17 check@19 check@21 check@23 check@25 check@27 match@33 case@34"
	if strings.Join(got, " ") != want {
		t.Fatalf("calls = %q, want %q", strings.Join(got, " "), want)
	}
}

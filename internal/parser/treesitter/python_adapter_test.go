//go:build cgo

package treesitter

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser/python"
)

func TestPythonNestedScopesAndAsync(t *testing.T) {
	const src = `class Outer:
    class Inner:
        class Deep:
            async def fetch(self):
                work()
                def inner():
                    leaf()
`
	p, err := NewPython().Parse(context.Background(), "mod.py", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"mod.Outer":                        "class",
		"mod.Outer.Inner":                  "class",
		"mod.Outer.Inner.Deep":             "class",
		"mod.Outer.Inner.Deep.fetch":       "method",
		"mod.Outer.Inner.Deep.fetch.inner": "function",
	}
	for qname, kind := range want {
		found := false
		for _, sym := range p.Symbols {
			if sym.QualifiedName != qname {
				continue
			}
			found = true
			if sym.Kind != kind {
				t.Fatalf("%s kind = %q, want %q", qname, sym.Kind, kind)
			}
			if sym.Range.StartLine <= 0 || sym.Range.EndLine < sym.Range.StartLine {
				t.Fatalf("%s invalid range %+v", qname, sym.Range)
			}
			if qname == "mod.Outer.Inner.Deep.fetch" && sym.Signature != "async def fetch(self)" {
				t.Fatalf("async signature = %q", sym.Signature)
			}
		}
		if !found {
			t.Fatalf("missing symbol %s", qname)
		}
	}
	for _, edge := range p.Edges {
		if edge.DstName == "leaf" || edge.DstName == "work" {
			if edge.DstName == "leaf" && edge.Line < 6 {
				t.Fatalf("leaf line = %d", edge.Line)
			}
		}
	}
}

func TestPythonCalleesFailClosed(t *testing.T) {
	const src = `def f():
    foo()
    obj.method()
    a.b.c()
    factory()()
    factory().run()
    items[0]()
`
	p, err := NewPython().Parse(context.Background(), "mod.py", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, edge := range p.Edges {
		got[edge.DstName] = true
	}
	for _, name := range []string{"foo", "obj.method", "a.b.c", "factory"} {
		if !got[name] {
			t.Fatalf("missing supported callee %q: %v", name, got)
		}
	}
	for _, name := range []string{"factory()", "factory().run", "items[0]"} {
		if got[name] {
			t.Fatalf("unsupported callee emitted as %q", name)
		}
	}
}

func TestPythonDecoratedAsyncSignature(t *testing.T) {
	p, err := NewPython().Parse(context.Background(), "mod.py", []byte("@retry\nasync def fetch():\n    work()\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Symbols) != 1 || p.Symbols[0].Signature != "@retry\nasync def fetch()" {
		t.Fatalf("symbols = %+v", p.Symbols)
	}
}

// The two Python adapters must record the same scope evidence for the same
// source: which import is written in which lexical scope, and what each scope
// binds itself. A resolver that trusts one and not the other would decide
// differently under cgo and without it.
func TestPythonScopeEvidenceMatchesRegexAdapter(t *testing.T) {
	const src = `import pkg.helpers as h
from . import sibling
from pkg.helpers import load as read


CONFIG = {}


def f(arg):
    import other.helpers as h
    value = 1
    for item in arg:
        pass
    return h.load()


class Service:
    attr = 1

    def method(self, name):
        with open(name) as fh:
            return read(fh)
`
	tree, err := NewPython().Parse(context.Background(), "mod.py", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	regex, err := python.New().Parse(context.Background(), "mod.py", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	render := func(pf graph.ParsedFile) []string {
		out := make([]string, 0, len(pf.Scope.Imports))
		for _, b := range pf.Scope.Imports {
			out = append(out, strings.Join([]string{b.Kind, b.OwnerModule, b.LocalName, b.ImportedName, b.SourceSpecifier}, "|"))
		}
		sort.Strings(out)
		return out
	}
	got, want := render(tree), render(regex)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree-sitter scope evidence:\n%v\nregex:\n%v", got, want)
	}
	if len(got) == 0 {
		t.Fatal("no scope evidence recorded")
	}
}

// pythonConditionalDefsSource defines functions under every compound statement
// Python has. None of them opens a scope, so each def belongs to the scope the
// statement is written in.
const pythonConditionalDefsSource = `import sys

if sys.platform == "win32":
    def pick():
        return 1
elif sys.platform == "darwin":
    def pick():
        return 2
else:
    def pick():
        return 3

try:
    from fast import speed
except ImportError:
    def speed():
        return 0
else:
    def ready():
        return True
finally:
    def cleanup():
        return None

with open(__file__) as fh:
    def reader():
        return fh

for _ in range(1):
    def looped():
        return 1
else:
    def after_loop():
        return 2

while False:
    def never():
        pass
else:
    def after_while():
        pass

match sys.platform:
    case "linux":
        def linux_only():
            pass
    case _:
        def other():
            pass


class Service:
    if sys.version_info >= (3, 11):
        def run(self):
            return helper()
    else:
        @staticmethod
        def run():
            return helper()


def outer(flag):
    if flag:
        def inner():
            return helper()
        return inner()
    try:
        pass
    except* ValueError:
        def handler():
            return helper()
    return handler


def helper():
    return 1
`

// A def nested under if/elif/else, try/except/except*/else/finally, with,
// for/else, while/else or match/case is a real definition in the enclosing
// scope. The expected list is CPython's ast for the source: every def and
// class with its scope-qualified name, kind and line.
func TestPythonConditionalDefinitionsAreSymbols(t *testing.T) {
	p, err := NewPython().Parse(context.Background(), "mod.py", []byte(pythonConditionalDefsSource))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range p.Symbols {
		got = append(got, s.QualifiedName+"|"+s.Kind+"|"+strconv.Itoa(s.Range.StartLine))
	}
	sort.SliceStable(got, func(i, j int) bool { return lineOf(got[i]) < lineOf(got[j]) })
	want := "mod.pick|function|4 mod.pick|function|7 mod.pick|function|10 mod.speed|function|16 mod.ready|function|19 mod.cleanup|function|22 mod.reader|function|26 mod.looped|function|30 mod.after_loop|function|33 mod.never|function|37 mod.after_while|function|40 mod.linux_only|function|45 mod.other|function|48 mod.Service|class|52 mod.Service.run|method|54 mod.Service.run|method|58 mod.outer|function|62 mod.outer.inner|function|64 mod.outer.handler|function|70 mod.helper|function|75"
	if strings.Join(got, " ") != want {
		t.Fatalf("symbols:\n%s\nwant (CPython ast):\n%s", strings.Join(got, " "), want)
	}
}

func lineOf(row string) int {
	n, _ := strconv.Atoi(row[strings.LastIndexByte(row, '|')+1:])
	return n
}

// Both Python adapters record the same NFKC names (PEP 3131) for the same
// source: symbols, calls and scope evidence over every file of the
// CPython-checked python_nfkc_scope fixture.
func TestPythonNFKCNamesMatchRegexAdapter(t *testing.T) {
	dir := filepath.Join("..", "..", "indexer", "testdata", "python_nfkc_scope")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	render := func(pf graph.ParsedFile) []string {
		var out []string
		for _, s := range pf.Symbols {
			out = append(out, "sym|"+s.Kind+"|"+s.Name+"|"+s.QualifiedName+"|"+s.ContainerName+"|"+s.StableKey)
		}
		for _, e := range pf.Edges {
			out = append(out, "call|"+e.DstName+"@"+strconv.Itoa(e.Line))
		}
		for _, b := range pf.Scope.Imports {
			out = append(out, "scope|"+strings.Join([]string{b.Kind, b.OwnerModule, b.LocalName, b.ImportedName, b.SourceSpecifier}, "|"))
		}
		out = append(out, "imports|"+strings.Join(pf.Imports, ","))
		sort.Strings(out)
		return out
	}
	for _, e := range entries {
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		tree, err := NewPython().Parse(context.Background(), e.Name(), src)
		if err != nil {
			t.Fatal(err)
		}
		regex, err := python.New().Parse(context.Background(), e.Name(), src)
		if err != nil {
			t.Fatal(err)
		}
		got, want := render(tree), render(regex)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: tree-sitter:\n%v\nregex:\n%v", e.Name(), got, want)
		}
		for _, row := range got {
			if strings.ContainsFunc(row, func(r rune) bool { return r >= 0xFF00 && r <= 0xFFEF }) {
				t.Errorf("%s: fullwidth spelling persisted: %s", e.Name(), row)
			}
		}
	}
}

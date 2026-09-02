//go:build cgo

package treesitter

import (
	"context"
	"testing"
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

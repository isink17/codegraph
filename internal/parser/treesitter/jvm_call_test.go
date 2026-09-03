//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"testing"
)

func TestJavaChainedCallPreservesInnerCall(t *testing.T) {
	p, err := NewJava().Parse(context.Background(), "C.java", []byte("class C {\n void f() {\n  factory().run();\n }\n}"))
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	for _, e := range p.Edges {
		if e.Kind != "calls" {
			continue
		}
		calls++
		if e.DstName != "factory" || e.Evidence != "factory()" || e.Line != 3 {
			t.Fatalf("call = %+v", e)
		}
		if strings.ContainsAny(e.DstName, "().") {
			t.Fatalf("malformed call = %+v", e)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestJavaConstructorInvocationIsDistinct(t *testing.T) {
	p, err := NewJava().Parse(context.Background(), "C.java", []byte(`class C { void f() { new a.b.Foo(1); } }`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Edges) != 1 || p.Edges[0].Kind != "constructs" || p.Edges[0].DstName != "a.b.Foo" {
		t.Fatalf("constructor edges = %+v", p.Edges)
	}
}

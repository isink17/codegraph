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

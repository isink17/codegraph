package golang

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestLinkTests_SkipsTestMain: TestMain is the harness hook, not a test of a
// function called Main -- linking it would let a repo's exported Main steal a
// wrong test edge (P22.2).
func TestLinkTests_SkipsTestMain(t *testing.T) {
	src := `package pkg

import "testing"

func TestMain(m *testing.M) {}

func TestHelper(t *testing.T) {}
`
	pf, err := New().Parse(context.Background(), "pkg_test.go", []byte(src))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(pf.TestLinks) != 1 {
		t.Fatalf("TestLinks = %+v, want only TestHelper's link", pf.TestLinks)
	}
	link := pf.TestLinks[0]
	if link.TargetStableKey != "func:pkg::Helper" {
		t.Fatalf("TargetStableKey = %q, want func:pkg::Helper", link.TargetStableKey)
	}
	if link.Reason != "test_name_match" {
		t.Fatalf("Reason = %q, want test_name_match", link.Reason)
	}
}

func TestParserHygiene_GenericReceiversAndCalls(t *testing.T) {
	src := `package pkg
type Pair[T any] struct{}
type Duo[K, V any] struct{}
func (p *Pair[T]) Get() {}
func (d Duo[K, V]) Put() {}
func helper() {}
func run() {
	helper()
	Pair[A, B]()
	func() { helper() }()
	factory()()
	items[0]()
}
`
	pf, err := New().Parse(context.Background(), "pkg.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, sym := range pf.Symbols {
		if sym.Name == "Get" && (sym.Kind != "method" || sym.ContainerName != "Pair" || sym.QualifiedName != "pkg.Pair.Get" || sym.StableKey != "func:pkg:Pair:Get") {
			t.Fatalf("generic receiver identity = %+v", sym)
		}
		if sym.Name == "Put" && (sym.ContainerName != "Duo" || sym.QualifiedName != "pkg.Duo.Put" || sym.StableKey != "func:pkg:Duo:Put") {
			t.Fatalf("multi-type receiver identity = %+v", sym)
		}
		if strings.Contains(sym.ContainerName+sym.QualifiedName+sym.StableKey, "ast.") {
			t.Fatalf("AST implementation name in symbol = %+v", sym)
		}
	}
	var calls []string
	for _, edge := range pf.Edges {
		if edge.Kind == "calls" {
			calls = append(calls, edge.DstName)
		}
	}
	if !reflect.DeepEqual(calls, []string{"helper", "Pair", "helper", "factory"}) {
		t.Fatalf("call destinations = %q, want [helper Pair helper factory]", calls)
	}
}

func TestCallOccurrenceColumnsCoverUnicodeCRLFAndNestedCalls(t *testing.T) {
	src := "package p\r\nfunc caller() { café(); target(target()); target() }\r\n"
	pf, err := New().Parse(context.Background(), "pkg.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Split(src, "\r\n")[1]
	firstTarget := strings.Index(line, "target")
	secondTarget := firstTarget + len("target") + strings.Index(line[firstTarget+len("target"):], "target")
	wants := []struct {
		name string
		col  int
	}{
		{"café", strings.Index(line, "café") + 1},
		{"target", firstTarget + 1},
		{"target", secondTarget + 1},
		{"target", strings.LastIndex(line, "target") + 1},
	}
	if len(pf.Edges) != len(wants) || len(pf.References) != len(wants) {
		t.Fatalf("calls: edges=%d refs=%d, want %d", len(pf.Edges), len(pf.References), len(wants))
	}
	for i, want := range wants {
		edge, ref := pf.Edges[i], pf.References[i]
		if edge.DstName != want.name || edge.Line != 2 || edge.Col != want.col {
			t.Errorf("edge %d = (%q,%d,%d), want (%q,2,%d)", i, edge.DstName, edge.Line, edge.Col, want.name, want.col)
		}
		if ref.Name != want.name || ref.Range.StartLine != 2 || ref.Range.StartCol != want.col {
			t.Errorf("reference %d = (%q,%d,%d), want (%q,2,%d)", i, ref.Name, ref.Range.StartLine, ref.Range.StartCol, want.name, want.col)
		}
	}
}

//go:build cgo

package treesitter

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestGoParserHygiene_GenericReceiversAndCalls(t *testing.T) {
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
	pf, err := NewGo().Parse(context.Background(), "pkg.go", []byte(src))
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

//go:build cgo

package indexer

import (
	"testing"

	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

func TestDotSuffixIsReachedByProductionJavaParser(t *testing.T) {
	const src = `class A { class B { class C { static void m() {} } } }
class Caller { void run() { A.B.C.m(); } }
`
	s, repoID := indexSource(t, tsparser.NewJava(), "Sample.java", src)
	edges, err := s.ExportEdgesPage(t.Context(), repoID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.DstName == "A.B.C.m" {
			if edge.DstSymbolID == nil || edge.ResolutionStrategy != "dot_suffix" || edge.ResolutionConfidence != "low" {
				t.Fatalf("dot-suffix edge = %+v", edge)
			}
			return
		}
	}
	t.Fatal("production parser did not emit A.B.C.m call")
}

func TestBareGoMethodCallDoesNotMintReceiverMethod(t *testing.T) {
	const src = `package p

type T struct{}
func (T) Run() {}
func f() { Run() }
`
	s, repoID := indexSource(t, tsparser.NewGo(), "main.go", src)
	edges, err := s.ExportEdgesPage(t.Context(), repoID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		if edge.DstName == "Run" {
			if edge.DstSymbolID != nil || edge.ResolutionStrategy == "receiver_method" {
				t.Fatalf("bare method call = %+v", edge)
			}
			return
		}
	}
	t.Fatal("production parser did not emit bare Run call")
}

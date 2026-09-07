package store

import (
	"database/sql"
	"testing"
)

func TestCSharpBareCallUnknownSourceStaticnessKeepsOnlyStaticCandidates(t *testing.T) {
	zero := sql.NullInt64{Int64: 0, Valid: true}
	static := sql.NullInt64{Int64: 1, Valid: true}
	unknownSource := csharpScopeEdge{name: "Run", srcContainer: "C", callArity: zero}
	instance := csharpScopeSymbol{name: "Run", qname: "C.Run", container: "C", kind: "function", visibility: "public", static: zero, arityMin: zero, arityMax: zero}
	staticRun := csharpScopeSymbol{id: 2, name: "Run", qname: "C.Run", container: "C", kind: "function", visibility: "public", static: static, arityMin: zero, arityMax: zero}

	got, strategy, ok := csharpResolveEdge(unknownSource, map[string][]csharpScopeSymbol{"Run": {instance, staticRun}}, nil, nil, csharpScopeBindings{unknown: map[string]map[string]struct{}{}, typed: map[string]map[string]map[string]struct{}{}})
	if !ok || got.id != 2 || strategy != "csharp_same_type" {
		t.Fatalf("got id=%d strategy=%q ok=%v, want static candidate", got.id, strategy, ok)
	}
}

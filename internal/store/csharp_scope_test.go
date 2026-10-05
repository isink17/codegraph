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

	got, strategy, ok := csharpResolveEdge(unknownSource, map[string][]csharpScopeSymbol{"Run": {instance, staticRun}}, nil, nil, nil, csharpScopeBindings{unknown: map[string]map[string]struct{}{}, typed: map[string]map[string]map[string]struct{}{}})
	if !ok || got.id != 2 || strategy != "csharp_same_type" {
		t.Fatalf("got id=%d strategy=%q ok=%v, want static candidate", got.id, strategy, ok)
	}
}

func TestCSharpResolveTypeIdentityRefusesNamespaceAliasPrefix(t *testing.T) {
	byQName := map[string][]csharpScopeSymbol{
		"a.N.Util":   {{qname: "a.N.Util", container: "a.N", kind: "type", visibility: "public"}},
		"other.Util": {{qname: "other.Util", container: "other", kind: "type", visibility: "public"}},
	}
	alias := func(owner, local, source string) csharpScopeImport {
		return csharpScopeImport{owner: owner, kind: "alias", local: local, source: source}
	}
	cases := []struct {
		name    string
		imports []csharpScopeImport
		global  bool
		want    string
	}{
		{"no alias binds through the namespace", nil, false, "a.N.Util"},
		{"own-level alias prefix", []csharpScopeImport{alias("a.b", "N", "other")}, false, ""},
		{"ancestor alias prefix", []csharpScopeImport{alias("a", "N", "other")}, false, ""},
		{"root alias prefix", []csharpScopeImport{alias("", "N", "other")}, false, ""},
		{"alias to an unindexed namespace", []csharpScopeImport{alias("a.b", "N", "ext.Lib")}, false, ""},
		{"alias of another name", []csharpScopeImport{alias("a.b", "M", "other")}, false, "a.N.Util"},
		{"alias outside the lookup chain", []csharpScopeImport{alias("x", "N", "other")}, false, "a.N.Util"},
		{"global qualification", []csharpScopeImport{alias("a.b", "N", "other")}, true, ""},
	}
	for _, tc := range cases {
		qualifier := "N.Util"
		if tc.global {
			qualifier = "a.N.Util"
		}
		got, _, ok := csharpResolveTypeIdentity(qualifier, "a.b", tc.imports, byQName, "a.b.Caller", tc.global)
		if tc.global {
			tc.want = "a.N.Util"
		}
		if (got != "") != ok || got != tc.want {
			t.Fatalf("%s: got %q ok=%v, want %q", tc.name, got, ok, tc.want)
		}
	}
}

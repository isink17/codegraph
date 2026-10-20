//go:build cgo

package indexer

import (
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// A cgo and a non-cgo index of type aliases store the same declarations and
// the same edges: the conversion `Plain(...)` binds to the alias in both.
func TestGoTypeAliasFamilyParity(t *testing.T) {
	native, _ := newGoRepo(t, goNativeRegistry(), goTypeAliasFiles).semanticJSON(t)
	g, _ := newGoRepo(t, parser.NewRegistry(tsparser.NewGo()), goTypeAliasFiles).semanticJSON(t)
	nd, ne := canonicalJSON(native.Declarations), canonicalJSON(native.Edges)
	td, te := canonicalJSON(g.Declarations), canonicalJSON(g.Edges)
	if nd != td {
		t.Fatalf("declarations differ:\n go/ast:      %s\n tree-sitter: %s", nd, td)
	}
	if ne != te {
		t.Fatalf("edges differ:\n go/ast:      %s\n tree-sitter: %s", ne, te)
	}
	for _, want := range []string{`"qualified_name":"p.Plain"`, `"qualified_name":"p.Grouped"`, `"qualified_name":"p.Remote"`, `"qualified_name":"p.GenAlias"`} {
		if !strings.Contains(td, want) {
			t.Errorf("declarations lack %s: %s", want, td)
		}
	}
	bound := false
	for _, e := range g.Edges {
		bound = bound || (e.Source.QualifiedName == "p.Conv" && e.Target.QualifiedName == "p.Plain")
	}
	if !bound {
		t.Errorf("conversion Plain(...) is not bound to the alias: %s", te)
	}
	if strings.Contains(td, "LocalAlias") {
		t.Errorf("a function-local alias became a declaration: %s", td)
	}
}

// A tree-sitter index written before aliases were symbols has no alias rows
// and an unbound conversion.
func TestGoTypeAliasProfileUpgradeTreeSitter(t *testing.T) {
	aliases := `SELECT id FROM symbols WHERE language='go' AND qualified_name IN ('p.Plain','p.Grouped','p.Remote')`
	runGoProfileUpgrade(t, parser.NewRegistry(tsparser.NewGo()), goTypeAliasFiles, "treesitter:go:v4", "treesitter:go:v6",
		`UPDATE edges SET dst_symbol_id=NULL, resolution_strategy='', resolution_confidence='' WHERE dst_symbol_id IN (`+aliases+`)`,
		`DELETE FROM symbols WHERE id IN (`+aliases+`)`)
}

package python

import (
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestImportBindings(t *testing.T) {
	tests := []struct {
		name, stmt string
		want       []graph.ScopeImport
	}{
		{
			name: "from import",
			stmt: "from pkg.helpers import load",
			want: []graph.ScopeImport{{SourceSpecifier: "pkg.helpers", ImportedName: "load", LocalName: "load", Kind: graph.ScopeImportNamed}},
		},
		{
			name: "from import aliased",
			stmt: "from pkg.helpers import load as read_config",
			want: []graph.ScopeImport{{SourceSpecifier: "pkg.helpers", ImportedName: "load", LocalName: "read_config", Kind: graph.ScopeImportNamed}},
		},
		{
			name: "from import several",
			stmt: "from pkg.helpers import load, dump as write",
			want: []graph.ScopeImport{
				{SourceSpecifier: "pkg.helpers", ImportedName: "load", LocalName: "load", Kind: graph.ScopeImportNamed},
				{SourceSpecifier: "pkg.helpers", ImportedName: "dump", LocalName: "write", Kind: graph.ScopeImportNamed},
			},
		},
		{
			name: "parenthesised",
			stmt: "from pkg.helpers import ( load , dump )",
			want: []graph.ScopeImport{
				{SourceSpecifier: "pkg.helpers", ImportedName: "load", LocalName: "load", Kind: graph.ScopeImportNamed},
				{SourceSpecifier: "pkg.helpers", ImportedName: "dump", LocalName: "dump", Kind: graph.ScopeImportNamed},
			},
		},
		{
			name: "relative keeps its level",
			stmt: "from ..common.helpers import load",
			want: []graph.ScopeImport{{SourceSpecifier: "..common.helpers", ImportedName: "load", LocalName: "load", Kind: graph.ScopeImportNamed}},
		},
		{
			name: "relative package only",
			stmt: "from . import helpers",
			want: []graph.ScopeImport{{SourceSpecifier: ".", ImportedName: "helpers", LocalName: "helpers", Kind: graph.ScopeImportNamed}},
		},
		{
			name: "wildcard binds no local name",
			stmt: "from pkg.helpers import *",
			want: []graph.ScopeImport{{SourceSpecifier: "pkg.helpers", Kind: graph.ScopeImportNamed, Wildcard: true}},
		},
		{
			name: "module import binds the dotted path",
			stmt: "import pkg.helpers",
			want: []graph.ScopeImport{{SourceSpecifier: "pkg.helpers", LocalName: "pkg.helpers", Kind: graph.ScopeImportNamespace}},
		},
		{
			name: "module import aliased",
			stmt: "import pkg.helpers as helpers",
			want: []graph.ScopeImport{{SourceSpecifier: "pkg.helpers", LocalName: "helpers", Kind: graph.ScopeImportNamespace}},
		},
		{
			name: "several modules",
			stmt: "import os, pkg.helpers as helpers",
			want: []graph.ScopeImport{
				{SourceSpecifier: "os", LocalName: "os", Kind: graph.ScopeImportNamespace},
				{SourceSpecifier: "pkg.helpers", LocalName: "helpers", Kind: graph.ScopeImportNamespace},
			},
		},
		{name: "not an import", stmt: "load()", want: nil},
		{name: "from without import", stmt: "from pkg.helpers", want: nil},
		{name: "malformed module", stmt: "from 9bad import load", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImportBindings(tc.stmt); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ImportBindings(%q) = %+v, want %+v", tc.stmt, got, tc.want)
			}
		})
	}
}

func TestAdapterRecordsImportEvidence(t *testing.T) {
	const src = `import pkg.helpers as helpers
from .siblings import (
    one,
    two as second,
)
from pkg.helpers import load


def run():
    return helpers.load()
`
	pf := parseSource(t, src)
	want := []graph.ScopeImport{
		{SourceSpecifier: "pkg.helpers", LocalName: "helpers", Kind: graph.ScopeImportNamespace},
		{SourceSpecifier: ".siblings", ImportedName: "one", LocalName: "one", Kind: graph.ScopeImportNamed},
		{SourceSpecifier: ".siblings", ImportedName: "two", LocalName: "second", Kind: graph.ScopeImportNamed},
		{SourceSpecifier: "pkg.helpers", ImportedName: "load", LocalName: "load", Kind: graph.ScopeImportNamed},
	}
	if !reflect.DeepEqual(pf.Scope.Imports, want) {
		t.Fatalf("scope imports = %+v, want %+v", pf.Scope.Imports, want)
	}
	if got, expect := pf.Imports, []string{"pkg.helpers", ".siblings"}; !reflect.DeepEqual(got, expect) {
		t.Fatalf("file imports = %v, want %v", got, expect)
	}
}

// The dotted receiver a call site wrote is evidence. Dropping it would let a
// bare same-name symbol elsewhere answer `helpers.load()`, and would also make
// this adapter disagree with the tree-sitter one.
func TestCallEdgesKeepDottedReceivers(t *testing.T) {
	const src = `def f():
    foo()
    obj.method()
    a.b.c()
    factory()()
    factory().run()
    items[0]()
`
	pf := parseSource(t, src)
	got := map[string]bool{}
	for _, edge := range pf.Edges {
		got[edge.DstName] = true
	}
	for _, name := range []string{"foo", "obj.method", "a.b.c", "factory"} {
		if !got[name] {
			t.Fatalf("missing callee %q: %v", name, got)
		}
	}
	for _, name := range []string{"run", "items", "0"} {
		if got[name] {
			t.Fatalf("call on an unnameable receiver emitted %q: %v", name, got)
		}
	}
}

// Import syntax is not a call, whether or not it carries parentheses.
func TestImportSyntaxEmitsNoCallEdge(t *testing.T) {
	const src = `from pkg.helpers import (
    load,
)


def run():
    return load()
`
	pf := parseSource(t, src)
	for _, edge := range pf.Edges {
		if edge.DstName != "load" {
			t.Fatalf("import syntax produced call edge %q", edge.DstName)
		}
	}
}

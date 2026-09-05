package python

import (
	"reflect"
	"sort"
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
			// `import a.b` binds `a`. ImportedName records what the local name
			// is bound to, which is what separates this from `import a.b as a`.
			name: "module import binds the top package",
			stmt: "import pkg.helpers",
			want: []graph.ScopeImport{{SourceSpecifier: "pkg.helpers", ImportedName: "pkg", LocalName: "pkg", Kind: graph.ScopeImportNamespace}},
		},
		{
			name: "module import aliased binds the alias to the module",
			stmt: "import pkg.helpers as helpers",
			want: []graph.ScopeImport{{SourceSpecifier: "pkg.helpers", ImportedName: "pkg.helpers", LocalName: "helpers", Kind: graph.ScopeImportNamespace}},
		},
		{
			name: "several modules",
			stmt: "import os, pkg.helpers as helpers",
			want: []graph.ScopeImport{
				{SourceSpecifier: "os", ImportedName: "os", LocalName: "os", Kind: graph.ScopeImportNamespace},
				{SourceSpecifier: "pkg.helpers", ImportedName: "pkg.helpers", LocalName: "helpers", Kind: graph.ScopeImportNamespace},
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
		{SourceSpecifier: "pkg.helpers", ImportedName: "pkg.helpers", LocalName: "helpers", Kind: graph.ScopeImportNamespace},
		{SourceSpecifier: ".siblings", ImportedName: "one", LocalName: "one", Kind: graph.ScopeImportNamed},
		{SourceSpecifier: ".siblings", ImportedName: "two", LocalName: "second", Kind: graph.ScopeImportNamed},
		{SourceSpecifier: "pkg.helpers", ImportedName: "load", LocalName: "load", Kind: graph.ScopeImportNamed},
	}
	var got []graph.ScopeImport
	for _, binding := range pf.Scope.Imports {
		if binding.Kind != graph.ScopeImportLocalBinding {
			got = append(got, binding)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scope imports = %+v, want %+v", got, want)
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

func TestLocalBindings(t *testing.T) {
	tests := []struct {
		name, src string
		function  bool
		want      []string
	}{
		{
			name:     "parameters bind",
			src:      "def run(a, b=1, *rest, **kw):\n    return a\n",
			function: true,
			want:     []string{"a", "b", "rest", "kw"},
		},
		{
			name:     "annotated parameters drop their annotation",
			src:      "def run(cfg: Config = None) -> int:\n    return 1\n",
			function: true,
			want:     []string{"cfg"},
		},
		{
			name:     "a signature spanning lines still binds every parameter",
			src:      "def run(\n    first,\n    second,\n):\n    return first\n",
			function: true,
			want:     []string{"first", "second"},
		},
		{
			name:     "assignments, loops, with, except and walrus bind",
			src:      "def run():\n    h = make()\n    x: int = 1\n    y += 2\n    for item in items:\n        pass\n    with open(p) as fh:\n        pass\n    try:\n        pass\n    except Error as err:\n        pass\n    if (n := size()) > 0:\n        pass\n",
			function: true,
			want:     []string{"h", "x", "y", "item", "fh", "err", "n"},
		},
		{
			name:     "attribute and subscript targets bind nothing",
			src:      "def run(self):\n    self.value = 1\n    items[0] = 2\n",
			function: true,
			want:     []string{"self"},
		},
		{
			name:     "comparisons are not assignments",
			src:      "def run():\n    if a == b:\n        pass\n    while c != d:\n        pass\n",
			function: true,
			want:     nil,
		},
		{
			name:     "keyword arguments are not assignments",
			src:      "def run():\n    return call(timeout=1, retries=2)\n",
			function: true,
			want:     nil,
		},
		{
			name:     "a nested scope keeps its own bindings",
			src:      "def outer():\n    kept = 1\n    def inner(hidden):\n        also_hidden = 2\n        return hidden\n    return inner\n",
			function: true,
			want:     []string{"kept"},
		},
		{
			name: "module scope skips declarations and their bodies",
			src:  "CONFIG = {}\n\n\ndef run(param):\n    local = 1\n    return local\n\n\nclass Thing:\n    attr = 1\n",
			want: []string{"CONFIG"},
		},
		{
			name: "imports are not local bindings",
			src:  "import pkg.helpers as h\nfrom x import y as z\n",
			want: nil,
		},
		{
			name:     "strings and comments bind nothing",
			src:      "def run():\n    text = \"fake = 1\"\n    # comment = 2\n    return text\n",
			function: true,
			want:     []string{"text"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := LocalBindings(tc.src, tc.function)
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("LocalBindings() = %v, want %v", got, want)
			}
		})
	}
}

// A module-level import is owned by the module; one written inside a function
// is owned by that function and reaches nothing outside it.
func TestAdapterRecordsLexicalOwners(t *testing.T) {
	const src = `import pkg.helpers as h


def f():
    import other.helpers as h
    return h.load()


class Service:
    def method(self, arg):
        value = 1
        return h.load()
`
	pf := parseSource(t, src)
	type row struct{ kind, owner, local, source string }
	var got []row
	for _, binding := range pf.Scope.Imports {
		got = append(got, row{binding.Kind, binding.OwnerModule, binding.LocalName, binding.SourceSpecifier})
	}
	want := []row{
		{graph.ScopeImportNamespace, "", "h", "pkg.helpers"},
		{graph.ScopeImportNamespace, "f", "h", "other.helpers"},
		{graph.ScopeImportLocalBinding, "Service.method", "self", ""},
		{graph.ScopeImportLocalBinding, "Service.method", "arg", ""},
		{graph.ScopeImportLocalBinding, "Service.method", "value", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scope evidence = %+v, want %+v", got, want)
	}
}

// A compound header carrying its body inline still binds what the body binds,
// and `global`/`nonlocal` bind nothing at all -- they name an outer scope's
// binding, which is recorded under that scope's own owner.
func TestLocalBindingsCompoundAndGlobal(t *testing.T) {
	tests := []struct {
		name, src string
		want      []string
	}{
		{"inline if body", "def run(flag):\n    if flag: h = 1\n    return h\n", []string{"flag", "h"}},
		{"inline else body", "def run(flag):\n    if flag:\n        pass\n    else: h = 1\n", []string{"flag", "h"}},
		{"inline for body", "def run(xs):\n    for x in xs: seen = x\n", []string{"xs", "x", "seen"}},
		{"inline with body", "def run(p):\n    with open(p) as fh: data = fh\n", []string{"p", "fh", "data"}},
		{"global binds nothing here", "def run():\n    global h\n", nil},
		{"nonlocal binds nothing here", "def run():\n    nonlocal h\n", nil},
		{"a dict literal is not a compound body", "def run():\n    m = {\"k\": 1}\n", []string{"m"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := LocalBindings(tc.src, true)
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("LocalBindings() = %v, want %v", got, want)
			}
		})
	}
}

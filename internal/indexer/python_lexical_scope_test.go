package indexer

import (
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
)

// pythonLexicalTree exercises the two ways P22.26's first cut could produce a
// false edge: treating a caller's own package directory as an import root, and
// treating every import in a file as visible everywhere in it.
func pythonLexicalTree() map[string]string {
	return map[string]string{
		// A: `import helpers` inside a package is NOT `pkg.helpers` in Python 3.
		"pkg/__init__.py":          "",
		"pkg/helpers.py":           "def load():\n    return 1\n",
		"pkg/implicit_relative.py": "import helpers\n\n\ndef run():\n    return helpers.load()\n",
		// B: the explicit relative form is what means that, and must bind.
		"pkg/explicit_relative.py": "from . import helpers\n\n\ndef run():\n    return helpers.load()\n",

		// C: a top-level module of the same name is what `import helpers`
		// reaches from anywhere in the repository.
		"toplevel/__init__.py": "",
		"toplevel/mod.py":      "def only_top():\n    return 1\n",
		"pkg/absolute.py":      "from toplevel.mod import only_top\n\n\ndef run():\n    return only_top()\n",

		// D: a function-local import must not leak into a sibling function.
		"local_import.py": "def f():\n    import pkg.helpers as h\n    return h.load()\n\n\ndef g():\n    return h.load()\n",

		// E: two functions importing the same alias from different modules.
		"other/__init__.py": "",
		"other/helpers.py":  "def load():\n    return 2\n",
		"two_locals.py": "def f():\n    import pkg.helpers as h\n    return h.load()\n\n\n" +
			"def g():\n    import other.helpers as h\n    return h.load()\n",

		// F: a parameter shadows a module-level import alias.
		"param_shadow.py": "import pkg.helpers as h\n\n\ndef run(h):\n    return h.load()\n",

		// G: a local assignment shadows a module-level import alias.
		"assign_shadow.py": "import pkg.helpers as h\n\n\ndef factory():\n    return 1\n\n\n" +
			"def run():\n    h = factory()\n    return h.load()\n",

		// H: the shadow is in a nested function.
		"nested_shadow.py": "import pkg.helpers as h\n\n\ndef outer():\n    def inner(h):\n        return h.load()\n    return inner\n",

		// H2: the enclosing function shadows, the nested one does not rebind.
		"nested_outer_shadow.py": "import pkg.helpers as h\n\n\ndef outer(h):\n    def inner():\n        return h.load()\n    return inner\n",

		// J: a bare name that is a parameter is not the module-level function
		// of that name.
		"bare_param_shadow.py": "def load():\n    return 1\n\n\ndef run(load):\n    return load()\n",

		// The module-level import still reaches the nested scopes below it.
		"visible_nested.py": "import pkg.helpers as h\n\n\ndef outer():\n    def inner():\n        return h.load()\n    return inner\n",
	}
}

func pythonLexicalCases() []pythonScopeCase {
	return []pythonScopeCase{
		{"implicit_relative_is_not_a_package_import", "pkg/implicit_relative.py", "helpers.load", "<unresolved> []"},
		{"explicit_relative_binds", "pkg/explicit_relative.py", "helpers.load", "pkg/helpers.py:helpers.load [python_import_scope]"},
		{"absolute_from_inside_a_package", "pkg/absolute.py", "only_top", "toplevel/mod.py:mod.only_top [python_import_scope]"},
		{"local_import_does_not_leak_to_sibling", "local_import.py", "h.load", "<unresolved> []"},
		{"parameter_shadows_module_alias", "param_shadow.py", "h.load", "<unresolved> []"},
		{"assignment_shadows_module_alias", "assign_shadow.py", "h.load", "<unresolved> []"},
		{"nested_parameter_shadows", "nested_shadow.py", "h.load", "<unresolved> []"},
		{"enclosing_parameter_shadows", "nested_outer_shadow.py", "h.load", "<unresolved> []"},
		{"bare_parameter_shadows_module_function", "bare_param_shadow.py", "load", "<unresolved> []"},
		{"module_import_is_visible_in_nested_scopes", "visible_nested.py", "h.load", "pkg/helpers.py:helpers.load [python_import_scope]"},
	}
}

func runPythonLexicalCases(t *testing.T, reg *parser.Registry) {
	t.Helper()
	r := newPyRepo(t, reg, pythonLexicalTree())
	for _, tc := range pythonLexicalCases() {
		if got := r.edgeState(t, tc.file, tc.dst); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %s -> %q = %s; want %s", tc.name, tc.file, tc.dst, got, tc.want)
		}
	}
	// Two functions binding the same alias to different modules: neither call
	// may take the other's import, and a function-local import binds nothing on
	// its own, so both stay unresolved. The pairing is asserted by calling
	// symbol, not by row order.
	byCaller := r.callEdgeTargets(t, "two_locals.py")
	for _, caller := range []string{"two_locals.f", "two_locals.g"} {
		if got := byCaller[caller]; !strings.Contains(got, "<unresolved>") {
			t.Errorf("%s must not bind from a function-local import: %s", caller, got)
		}
	}
}

func TestPythonLexicalScopeRegexAdapter(t *testing.T) {
	runPythonLexicalCases(t, parser.NewRegistry(pyparser.New()))
}

// A lexical edit changes which import a call site can see, so the incremental
// resolver has to reconsider on exactly the same evidence a fresh index would.
func TestPythonLexicalScopeLifecycleParity(t *testing.T) {
	reg := parser.NewRegistry(pyparser.New())
	r := newPyRepo(t, reg, map[string]string{
		"pkg/__init__.py":   "",
		"pkg/helpers.py":    "def load():\n    return 1\n",
		"other/__init__.py": "",
		"other/helpers.py":  "def load():\n    return 2\n",
		"app.py":            "import pkg.helpers as h\n\n\ndef run():\n    return h.load()\n",
	})
	const inPkg = "pkg/helpers.py:helpers.load [python_import_scope]"
	const inOther = "other/helpers.py:helpers.load [python_import_scope]"

	steps := []struct {
		name, source, want string
	}{
		{
			name:   "module scope import binds",
			source: "import pkg.helpers as h\n\n\ndef run():\n    return h.load()\n",
			want:   inPkg,
		},
		{
			// A function-local import binds the name for the whole function,
			// but nothing here proves the import ran before the call.
			name:   "moving the import into the function stops it binding",
			source: "def run():\n    import pkg.helpers as h\n    return h.load()\n",
			want:   "<unresolved>",
		},
		{
			name:   "moving it into a sibling function takes it out of scope",
			source: "def other():\n    import pkg.helpers as h\n    return 1\n\n\ndef run():\n    return h.load()\n",
			want:   "<unresolved>",
		},
		{
			name:   "moving it back to module scope restores it",
			source: "import pkg.helpers as h\n\n\ndef run():\n    return h.load()\n",
			want:   inPkg,
		},
		{
			name:   "a parameter of that name shadows it",
			source: "import pkg.helpers as h\n\n\ndef run(h):\n    return h.load()\n",
			want:   "<unresolved>",
		},
		{
			name:   "removing the parameter restores it",
			source: "import pkg.helpers as h\n\n\ndef run():\n    return h.load()\n",
			want:   inPkg,
		},
		{
			name:   "a local assignment of that name shadows it",
			source: "import pkg.helpers as h\n\n\ndef run():\n    h = 1\n    return h.load()\n",
			want:   "<unresolved>",
		},
		{
			name:   "removing the assignment restores it",
			source: "import pkg.helpers as h\n\n\ndef run():\n    return h.load()\n",
			want:   inPkg,
		},
		{
			name:   "changing the alias moves the binding with it",
			source: "import pkg.helpers as mod\n\n\ndef run():\n    return mod.load()\n",
			want:   inPkg,
		},
		{
			name:   "changing the module moves the destination",
			source: "import other.helpers as mod\n\n\ndef run():\n    return mod.load()\n",
			want:   inOther,
		},
	}
	for _, step := range steps {
		r.write(t, "app.py", step.source)
		r.update(t)
		dst := "h.load"
		if strings.Contains(step.source, "mod.load") {
			dst = "mod.load"
		}
		if got := r.edgeState(t, "app.py", dst); !strings.Contains(got, step.want) {
			t.Fatalf("%s: %s; want %s", step.name, got, step.want)
		}
		fresh := newPyRepo(t, reg, readTree(t, r.root)).projection(t)
		if diff := pythonProjectionDiff(fresh, r.projection(t)); diff != "" {
			t.Fatalf("%s: update diverges from a fresh index of the same tree:\n%s", step.name, diff)
		}
	}

	// Deleting the imported module leaves the call unresolved rather than
	// letting the other module's same-named function answer it.
	r.remove(t, "other/helpers.py")
	r.update(t)
	if got := r.edgeState(t, "app.py", "mod.load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("target deleted: %s; want unresolved", got)
	}
}

// A class body is not a scope Python name lookup passes through, so an import
// written in one reaches neither its own methods nor anything else.
func TestPythonClassBodyImportIsNotVisible(t *testing.T) {
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
		"pkg/__init__.py": "",
		"pkg/helpers.py":  "def load():\n    return 1\n",
		"app.py":          "class Service:\n    import pkg.helpers as h\n\n    def method(self):\n        return h.load()\n",
	})
	if got := r.edgeState(t, "app.py", "h.load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("class-body import reached a method: %s", got)
	}
}

// `import pkg.helpers` binds `pkg` and reaches `pkg.helpers`. Both spellings
// are resolvable, and neither may answer for the other.
func TestPythonDottedNamespaceImportBindsBothSpellings(t *testing.T) {
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
		"pkg/__init__.py": "def package_level():\n    return 0\n",
		"pkg/helpers.py":  "def load():\n    return 1\n",
		"pkg/other.py":    "def load():\n    return 2\n",
		"app.py": "import pkg.helpers\n\n\ndef run():\n" +
			"    pkg.helpers.load()\n    pkg.package_level()\n    return pkg.other.load()\n",
	})
	for _, tc := range []struct{ dst, want string }{
		{"pkg.helpers.load", "pkg/helpers.py:helpers.load [python_import_scope]"},
		{"pkg.package_level", "pkg/__init__.py:__init__.package_level [python_import_scope]"},
		// `pkg.other` was never imported, so nothing proves this reaches
		// pkg/other.py -- and the claim keeps a weaker strategy off it.
		{"pkg.other.load", "<unresolved>"},
	} {
		if got := r.edgeState(t, "app.py", tc.dst); !strings.Contains(got, tc.want) {
			t.Errorf("%s = %s; want %s", tc.dst, got, tc.want)
		}
	}
}

// An absolute import is anchored at the repository root and nowhere else, so a
// sibling module never answers one -- with or without an `__init__.py`. Adding
// or removing that marker still has to reach the files below it on an
// incremental update, because it decides whether the directory is an importable
// package at all.
func TestPythonAbsoluteRootLifecycleParity(t *testing.T) {
	reg := parser.NewRegistry(pyparser.New())
	r := newPyRepo(t, reg, map[string]string{
		"pkg/helpers.py": "def load():\n    return 1\n",
		"pkg/main.py":    "import helpers\n\n\ndef run():\n    return helpers.load()\n",
	})
	// Case A: `import helpers` inside `pkg/` is top-level `helpers`, which does
	// not exist. Whether `pkg/` happens to be `sys.path[0]` at runtime is not a
	// repository fact, so nothing binds.
	if got := r.edgeState(t, "pkg/main.py", "helpers.load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("sibling module answered an absolute import: %s", got)
	}
	assertPythonFreshParity(t, reg, r, "no top-level module")

	// Case B: a top-level `helpers.py` is what the import names, and the
	// sibling still never is.
	r.write(t, "helpers.py", "def load():\n    return 2\n")
	r.update(t)
	got := r.edgeState(t, "pkg/main.py", "helpers.load")
	if !strings.Contains(got, "helpers.py:helpers.load [python_import_scope]") || strings.Contains(got, "pkg/helpers.py") {
		t.Fatalf("absolute import must bind the repository-root module: %s", got)
	}
	assertPythonFreshParity(t, reg, r, "top-level module added")

	// The package marker changes nothing about which module that is.
	r.write(t, "pkg/__init__.py", "")
	r.update(t)
	if got := r.edgeState(t, "pkg/main.py", "helpers.load"); !strings.Contains(got, "helpers.py:helpers.load") || strings.Contains(got, "pkg/helpers.py") {
		t.Fatalf("after __init__.py: %s", got)
	}
	assertPythonFreshParity(t, reg, r, "package marker added")

	r.remove(t, "helpers.py")
	r.update(t)
	if got := r.edgeState(t, "pkg/main.py", "helpers.load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("after removing the top-level module: %s; want unresolved", got)
	}
	assertPythonFreshParity(t, reg, r, "top-level module removed")
}

// A `src/` layout is a packaging convention, not a repository-provable import
// root, so an import that only such a root could satisfy stays unresolved.
func TestPythonSrcLayoutRootIsNotAssumed(t *testing.T) {
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
		"src/pkg/__init__.py": "",
		"src/pkg/helpers.py":  "def load():\n    return 1\n",
		"src/app.py":          "import pkg.helpers\n\n\ndef run():\n    return pkg.helpers.load()\n",
	})
	if got := r.edgeState(t, "src/app.py", "pkg.helpers.load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("src layout was treated as an import root: %s", got)
	}
}

// `from pkg import name` binds whatever `pkg` exposes as `name`. A package that
// binds the name itself wins over its own same-named submodule, and no
// syntax proves what that object is.
func TestPythonPackageAttributeShadowsSubmodule(t *testing.T) {
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
		"attr/__init__.py":     "class Obj:\n    def load(self):\n        return 0\n\n\nhelpers = Obj()\n",
		"attr/helpers.py":      "def load():\n    return 1\n",
		"decl/__init__.py":     "def helpers():\n    return 0\n",
		"decl/helpers.py":      "def load():\n    return 1\n",
		"alias/__init__.py":    "from other.mod import helpers\n",
		"alias/helpers.py":     "def load():\n    return 1\n",
		"other/__init__.py":    "",
		"other/mod.py":         "def helpers():\n    return 0\n",
		"reexport/__init__.py": "from . import helpers\n",
		"reexport/helpers.py":  "def load():\n    return 1\n",
		"plain/__init__.py":    "",
		"plain/helpers.py":     "def load():\n    return 1\n",
		"star/__init__.py":     "from .base import *\n",
		"star/base.py":         "helpers = 1\n",
		"star/helpers.py":      "def load():\n    return 1\n",
		"app.py": "from attr import helpers as a\n" +
			"from decl import helpers as d\n" +
			"from alias import helpers as x\n" +
			"from reexport import helpers as r\n" +
			"from plain import helpers as p\n" +
			"from star import helpers as st\n\n\n" +
			"def run():\n    a.load()\n    d.load()\n    x.load()\n    r.load()\n    st.load()\n    return p.load()\n",
	})
	for _, tc := range []struct{ dst, want string }{
		{"a.load", "<unresolved>"},
		{"d.load", "<unresolved>"},
		{"x.load", "<unresolved>"},
		// An `__init__.py` that imports the submodule itself is exactly the
		// re-export that proves the submodule reading.
		{"r.load", "reexport/helpers.py:helpers.load [python_import_scope]"},
		{"p.load", "plain/helpers.py:helpers.load [python_import_scope]"},
		// A star import in the package can bind the name to anything.
		{"st.load", "<unresolved>"},
	} {
		if got := r.edgeState(t, "app.py", tc.dst); !strings.Contains(got, tc.want) {
			t.Errorf("%s = %s; want %s", tc.dst, got, tc.want)
		}
	}
}

// A nested `def` or `class` binds its name in the enclosing function for the
// whole function, so it shadows an outer import wherever it is written.
func TestPythonNestedDeclarationShadowsImport(t *testing.T) {
	tree := map[string]string{
		"pkg/__init__.py": "",
		"pkg/helpers.py":  "def load():\n    return 1\n\n\ndef Client():\n    return 2\n",
	}
	cases := []struct{ file, source, dst, want string }{
		{
			"nested_def_before.py",
			"from pkg.helpers import load\n\n\ndef run():\n    def load():\n        return 1\n    return load()\n",
			"load", "<unresolved>",
		},
		{
			// Python classifies the name local to the whole function, so the
			// call before the nested `def` is not the outer import either.
			"nested_def_after.py",
			"from pkg.helpers import load\n\n\ndef run():\n    value = load()\n\n    def load():\n        return 1\n    return value\n",
			"load", "<unresolved>",
		},
		{
			"nested_class.py",
			"from pkg.helpers import Client\n\n\ndef run():\n    class Client:\n        pass\n    return Client()\n",
			"Client", "<unresolved>",
		},
		{
			// A nested declaration in a sibling function binds nothing here.
			"nested_sibling.py",
			"from pkg.helpers import load\n\n\ndef other():\n    def load():\n        return 1\n    return 0\n\n\ndef run():\n    return load()\n",
			"load", "pkg/helpers.py:helpers.load [python_import_scope]",
		},
	}
	for _, tc := range cases {
		tree[tc.file] = tc.source
	}
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), tree)
	for _, tc := range cases {
		if got := r.edgeState(t, tc.file, tc.dst); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q = %s; want %s", tc.file, tc.dst, got, tc.want)
		}
	}
}

// A nested declaration shadows an import, but with no import to shadow it is
// the call's own target -- refusing it would trade a false edge for a lost one.
func TestPythonNestedDeclarationStillAnswersItsOwnCall(t *testing.T) {
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
		"app.py": "def run():\n    def inner():\n        return 1\n    return inner()\n",
	})
	if got := r.edgeState(t, "app.py", "inner"); strings.Contains(got, "<unresolved>") {
		t.Fatalf("a nested declaration must answer its own call: %s", got)
	}
}

// `pkg.py` and `pkg/__init__.py` both existing makes `pkg` undecidable, so what
// it exposes as a name is undecidable too.
func TestPythonAmbiguousPackageLayoutRefusesSubmodule(t *testing.T) {
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
		"pkg.py":          "helpers = 1\n",
		"pkg/__init__.py": "",
		"pkg/helpers.py":  "def load():\n    return 1\n",
		"app.py":          "from pkg import helpers\n\n\ndef run():\n    return helpers.load()\n",
	})
	if got := r.edgeState(t, "app.py", "helpers.load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("ambiguous package layout bound anyway: %s", got)
	}
}

// `from pkg import helpers` resolves against `pkg.helpers`, so that submodule
// appearing later has to reach the caller on an incremental update.
func TestPythonSubmoduleAppearingInvalidatesCaller(t *testing.T) {
	reg := parser.NewRegistry(pyparser.New())
	r := newPyRepo(t, reg, map[string]string{
		"pkg/__init__.py": "",
		"app.py":          "from pkg import helpers\n\n\ndef run():\n    return helpers.load()\n",
	})
	if got := r.edgeState(t, "app.py", "helpers.load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("no submodule yet: %s", got)
	}
	r.write(t, "pkg/helpers.py", "def load():\n    return 1\n")
	r.update(t)
	if got := r.edgeState(t, "app.py", "helpers.load"); !strings.Contains(got, "pkg/helpers.py:helpers.load [python_import_scope]") {
		t.Fatalf("submodule added: %s; want the caller rebound", got)
	}
	assertPythonFreshParity(t, reg, r, "submodule added")

	r.remove(t, "pkg/helpers.py")
	r.update(t)
	if got := r.edgeState(t, "app.py", "helpers.load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("submodule removed: %s; want unresolved", got)
	}
	assertPythonFreshParity(t, reg, r, "submodule removed")
}

func assertPythonFreshParity(t *testing.T, reg *parser.Registry, r *pyRepo, step string) {
	t.Helper()
	fresh := newPyRepo(t, reg, readTree(t, r.root)).projection(t)
	if diff := pythonProjectionDiff(fresh, r.projection(t)); diff != "" {
		t.Fatalf("%s: update diverges from a fresh index of the same tree:\n%s", step, diff)
	}
}

// A function-local import binds the name for the whole function, but nothing in
// the evidence says whether the import statement ran before the call. It is
// claim and veto evidence only: it never binds, and it keeps every weaker
// strategy off the name.
func TestPythonFunctionLocalImportNeverBinds(t *testing.T) {
	tree := map[string]string{
		"pkg/__init__.py":   "",
		"pkg/helpers.py":    "def load():\n    return 1\n",
		"other/__init__.py": "",
		"other/helpers.py":  "def load():\n    return 2\n",
	}
	cases := []struct{ file, source, dst string }{
		{"before.py", "def run():\n    value = load()\n    from pkg.helpers import load\n    return value\n", "load"},
		{"after.py", "def run():\n    from pkg.helpers import load\n    return load()\n", "load"},
		{
			// The function-local import makes the name local for the whole
			// function, so the module-level one may not answer either.
			"over_module.py",
			"from pkg.helpers import load\n\n\ndef run():\n    value = load()\n    from other.helpers import load\n    return value\n",
			"load",
		},
		{
			"twice.py",
			"def run():\n    from pkg.helpers import load\n    from other.helpers import load\n    return load()\n",
			"load",
		},
	}
	for _, tc := range cases {
		tree[tc.file] = tc.source
	}
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), tree)
	for _, tc := range cases {
		if got := r.edgeState(t, tc.file, tc.dst); !strings.Contains(got, "<unresolved>") {
			t.Errorf("%s: %q = %s; want unresolved", tc.file, tc.dst, got)
		}
	}
}

// A compound statement carrying its body on one line still binds, so the import
// it shadows must not answer the call.
func TestPythonInlineCompoundAssignmentShadows(t *testing.T) {
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
		"pkg/__init__.py": "",
		"pkg/helpers.py":  "def load():\n    return 1\n",
		"app.py":          "import pkg.helpers as h\n\n\ndef run(flag):\n    if flag: h = object()\n    return h.load()\n",
	})
	if got := r.edgeState(t, "app.py", "h.load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("inline compound assignment did not shadow: %s", got)
	}
}

// A module whose basename holds a dot still has a lexical scope both the parser
// and the resolver spell the same way.
func TestPythonDottedModuleBasenameKeepsItsScopes(t *testing.T) {
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
		"pkg/__init__.py":   "",
		"pkg/helpers.py":    "def load():\n    return 1\n",
		"settings.local.py": "import pkg.helpers as h\n\n\ndef run(h):\n    return h.load()\n",
	})
	if got := r.edgeState(t, "settings.local.py", "h.load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("a parameter in a dotted-basename module must still shadow: %s", got)
	}
}

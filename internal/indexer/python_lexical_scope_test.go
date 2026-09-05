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
	// Two functions binding the same alias to different modules: each call takes
	// the import written in its own scope, and neither takes the other's. The
	// pairing is asserted by calling symbol, not by row order.
	byCaller := r.callEdgeTargets(t, "two_locals.py")
	if got := byCaller["two_locals.f"]; !strings.Contains(got, "pkg/helpers.py") {
		t.Errorf("f() must take its own import: %s", got)
	}
	if got := byCaller["two_locals.g"]; !strings.Contains(got, "other/helpers.py") {
		t.Errorf("g() must take its own import: %s", got)
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
			name:   "moving the import into the function keeps it visible",
			source: "def run():\n    import pkg.helpers as h\n    return h.load()\n",
			want:   inPkg,
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

// Whether a directory is a package decides which roots an absolute import in
// it resolves against, so adding or removing an `__init__.py` has to reach the
// files below it on an incremental update.
func TestPythonPackageMarkerLifecycleParity(t *testing.T) {
	reg := parser.NewRegistry(pyparser.New())
	files := map[string]string{
		"pkg/helpers.py": "def load():\n    return 1\n",
		"pkg/main.py":    "import helpers\n\n\ndef run():\n    return helpers.load()\n",
	}
	r := newPyRepo(t, reg, files)
	// With no `__init__.py`, `pkg/` is a plain directory: it is the importing
	// file's own root, and `import helpers` reaches its sibling.
	if got := r.edgeState(t, "pkg/main.py", "helpers.load"); !strings.Contains(got, "pkg/helpers.py:helpers.load [python_import_scope]") {
		t.Fatalf("script directory root: %s", got)
	}
	assertPythonFreshParity(t, reg, r, "no package marker")

	// Adding one makes `pkg/` a package, and an implicit-relative import stops
	// meaning anything.
	r.write(t, "pkg/__init__.py", "")
	r.update(t)
	if got := r.edgeState(t, "pkg/main.py", "helpers.load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("after __init__.py: %s; want unresolved", got)
	}
	assertPythonFreshParity(t, reg, r, "package marker added")

	// Removing it puts the root back.
	r.remove(t, "pkg/__init__.py")
	r.update(t)
	if got := r.edgeState(t, "pkg/main.py", "helpers.load"); !strings.Contains(got, "pkg/helpers.py") {
		t.Fatalf("after removing __init__.py: %s", got)
	}
	assertPythonFreshParity(t, reg, r, "package marker removed")
}

func assertPythonFreshParity(t *testing.T, reg *parser.Registry, r *pyRepo, step string) {
	t.Helper()
	fresh := newPyRepo(t, reg, readTree(t, r.root)).projection(t)
	if diff := pythonProjectionDiff(fresh, r.projection(t)); diff != "" {
		t.Fatalf("%s: update diverges from a fresh index of the same tree:\n%s", step, diff)
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

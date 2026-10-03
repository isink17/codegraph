package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
)

// asciiTwin rewrites the Unicode fixture into the same program with ASCII
// names, the control every Unicode case must match.
var asciiTwin = strings.NewReplacer("café", "cafe", "thé", "the", "naïve", "naive", "ñ", "n_")

// runPythonUnicodeScopeCases indexes testdata/python_unicode_scope -- every
// file of which CPython runs; run() returns what each call reaches -- and its
// ASCII twin. A Unicode name binds, shadows and imports exactly like an ASCII
// one, and never as a fragment of itself (`def café` is not a binding of `caf`,
// `naïve :=` is not one of `ve`).
func runPythonUnicodeScopeCases(t *testing.T, reg *parser.Registry) {
	t.Helper()
	dir := filepath.Join("testdata", "python_unicode_scope")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ file, dst, want string }{
		// CPython: the local object. A local binding refuses the module def.
		{"u_assign.py", "café", "<unresolved>"},
		{"u_param.py", "café", "<unresolved>"},
		{"u_walrus.py", "café", "<unresolved>"},
		{"u_for.py", "café", "<unresolved>"},
		{"u_with.py", "café", "<unresolved>"},
		{"u_except.py", "café", "<unresolved>"},
		// CPython: the nested function. The nested declaration shadows the
		// module def of the same name and the resolver refuses rather than
		// choose; the ASCII twin is refused the same way.
		{"u_nested.py", "café", "<unresolved>"},
		// CPython: the import. No fragment of a Unicode local shadows it.
		{"u_fragment.py", "caf", "lib.py:lib.caf [python_import_scope]"},
		{"u_walrus_fragment.py", "ve", "lib.py:lib.ve [python_import_scope]"},
		// CPython: the import, which also picks lib.thé over other.thé.
		{"u_import.py", "thé", "lib.py:lib.thé [python_import_scope]"},
		{"u_alias.py", "ñ", "lib.py:lib.thé [python_import_scope]"},
		// CPython: lib.thé. A function-local import never binds, but it still
		// shadows the module def it rebinds.
		{"u_local_import.py", "café", "<unresolved>"},
	}
	for _, twin := range []struct {
		name    string
		rewrite func(string) string
	}{{"unicode", func(s string) string { return s }}, {"ascii", asciiTwin.Replace}} {
		files := map[string]string{}
		for _, e := range entries {
			src, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files[e.Name()] = twin.rewrite(string(src))
		}
		r := newPyRepo(t, reg, files)
		for _, tc := range cases {
			want := twin.rewrite(tc.want)
			if got := r.edgeState(t, tc.file, twin.rewrite(tc.dst)); !strings.Contains(got, want) {
				t.Errorf("%s %s: %q = %s; want %s", twin.name, tc.file, twin.rewrite(tc.dst), got, want)
			}
		}
	}
}

func TestPythonUnicodeScopeRegexAdapter(t *testing.T) {
	runPythonUnicodeScopeCases(t, parser.NewRegistry(pyparser.New()))
}

// runPythonNestedVisibilityCases indexes testdata/python_nested_visibility,
// which CPython runs (cg15 visibility oracle): a bare name reaches the module's
// own names and the enclosing functions' locals, never a def nested in another
// function or in a class body, and a module whose name may come from an import
// does not answer for it with its fallback def.
func runPythonNestedVisibilityCases(t *testing.T, reg *parser.Registry) {
	t.Helper()
	dir := filepath.Join("testdata", "python_nested_visibility")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range entries {
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(src)
	}
	r := newPyRepo(t, reg, files)
	for _, tc := range []struct{ file, dst, want string }{
		// CPython: NameError. `h` is local to a(); `run` lives in the class body.
		{"mod.py", "h", "<unresolved>"},
		{"mod.py", "run", "<unresolved>"},
		// CPython: the nested def the calling function encloses.
		{"mod.py", "inner", "mod.py:mod.c.inner"},
		// CPython: one of two platform defs; which one is not in the source.
		{"mod.py", "pick", "<unresolved>"},
		{"other.py", "pick", "<unresolved>"},
		// CPython: fast.speed. mod binds `speed` by import before its fallback.
		{"other.py", "speed", "<unresolved>"},
		// CPython: lib.helper, defined after lib's `from base import *`. Which
		// binding wins depends on statement order the evidence does not keep,
		// so the call is refused: conservative, not wrong.
		{"other.py", "helper", "<unresolved>"},
		// A module-level def of a dotted-basename module is still module level.
		{"settings.local.py", "configure", "settings.local.py:settings.local.configure [python_module_scope]"},
	} {
		if got := r.edgeState(t, tc.file, tc.dst); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q = %s; want %s", tc.file, tc.dst, got, tc.want)
		}
	}
}

func TestPythonNestedVisibilityRegexAdapter(t *testing.T) {
	runPythonNestedVisibilityCases(t, parser.NewRegistry(pyparser.New()))
}

// runPythonNFKCScopeCases indexes testdata/python_nfkc_scope, every file of
// which CPython runs; run() returns what its call reaches. CPython binds the
// NFKC form of a name (PEP 3131): `ｆｕｌｌ` is `full`, `ﬁnd` is `find`, `𝐟` is
// `f`, `Ⅻ` is `XII`, in definitions, calls, attributes and imports alike, but
// never in string or comment text. Expected states are CPython's verdicts.
func runPythonNFKCScopeCases(t *testing.T, reg *parser.Registry) {
	t.Helper()
	dir := filepath.Join("testdata", "python_nfkc_scope")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range entries {
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(src)
	}
	r := newPyRepo(t, reg, files)
	for _, tc := range []struct{ file, dst, want string }{
		// CPython: the local. A compatibility spelling of the imported name
		// shadows the import.
		{"n_local.py", "full", "<unresolved>"},
		{"n_ligature.py", "find", "<unresolved>"},
		{"n_param.py", "full", "<unresolved>"},
		{"n_nested.py", "full", "<unresolved>"},
		// CPython: lib.full, imported under a compatibility spelling.
		{"n_import.py", "full", "lib.py:lib.full [python_import_scope]"},
		{"n_alias.py", "my", "lib.py:lib.full [python_import_scope]"},
		{"n_from_module.py", "full", "lib.py:lib.full [python_import_scope]"},
		{"n_module.py", "lib.full", "lib.py:lib.full"},
		{"n_attr.py", "lib.full", "lib.py:lib.full"},
		// CPython: the module's own def, declared or called under a
		// compatibility spelling.
		{"n_def.py", "f", "n_def.py:n_def.f"},
		{"n_call.py", "f", "n_call.py:n_call.f"},
		{"n_roman.py", "XII", "n_roman.py:n_roman.XII"},
		// CPython: lib.full. String and comment text binds nothing.
		{"n_text.py", "full", "lib.py:lib.full [python_import_scope]"},
	} {
		if got := r.edgeState(t, tc.file, tc.dst); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q = %s; want %s", tc.file, tc.dst, got, tc.want)
		}
	}
}

func TestPythonNFKCScopeRegexAdapter(t *testing.T) {
	runPythonNFKCScopeCases(t, parser.NewRegistry(pyparser.New()))
}

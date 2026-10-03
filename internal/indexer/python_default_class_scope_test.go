package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
)

// runPythonDefaultClassScopeCases indexes testdata/python_default_class_scope,
// every file of which CPython 3.14 runs; the comment on each case is what
// run() returned. A lambda written in a `def` header's default value binds its
// parameters, and a class body binds the names it assigns for the code that
// runs directly in it -- not for its methods, which skip the class scope.
// No case may bind an import CPython does not call; where the graph cannot
// name what CPython calls, the call stays unresolved. headerCalls says whether
// the adapter reads calls written in a `def` header at all; the regex adapter
// does not, so it records no edge there.
func runPythonDefaultClassScopeCases(t *testing.T, reg *parser.Registry, headerCalls bool) {
	t.Helper()
	dir := filepath.Join("testdata", "python_default_class_scope")
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
	const imported = "lib.py:lib.full [python_import_scope]"
	for _, tc := range []struct{ file, dst string }{
		// CPython calls the default lambda's parameter, never lib.full.
		{"d_single.py", "full"}, // 'param'
		{"d_multi.py", "full"},  // 'param'
		{"d_method.py", "full"}, // 'param'
		{"d_nested.py", "full"}, // 'param'
		// A class body nested in a function binds its own names for the code
		// that runs in it.
		{"c_lambda.py", "full"},       // 'param'
		{"c_assign.py", "full"},       // 'classattr'
		{"c_def.py", "full"},          // 'classdef'
		{"c_import.py", "full.upper"}, // 'X'
		{"c_decorated.py", "full"},    // 'decorated'
		{"c_nested.py", "full"},       // 'nested'
		// A class outside every function: no call in its body is attributed
		// to any symbol, so there is no edge to bind.
		{"c_module.py", "full"}, // 'classattr'
	} {
		want := "<unresolved>"
		if tc.file == "c_module.py" || !headerCalls && strings.HasPrefix(tc.file, "d_") {
			want = "<no edge>"
		}
		if got := r.edgeState(t, tc.file, tc.dst); !strings.HasPrefix(got, want) {
			t.Errorf("%s: %q = %s; want %s, CPython does not call the import", tc.file, tc.dst, got, want)
		}
	}
	// CPython calls lib.full: nothing between the call and the import binds
	// `full`, and a method skips its class's scope.
	for _, file := range []string{"d_noparam.py", "c_method.py", "c_module_method.py", "c_unbound.py"} {
		want := imported
		if file == "d_noparam.py" && !headerCalls {
			want = "<no edge>"
		}
		if got := r.edgeState(t, file, "full"); !strings.Contains(got, want) {
			t.Errorf("%s: %q = %s; want %s", file, "full", got, want)
		}
	}
}

func TestPythonDefaultClassScopeRegexAdapter(t *testing.T) {
	runPythonDefaultClassScopeCases(t, parser.NewRegistry(pyparser.New()), false)
}

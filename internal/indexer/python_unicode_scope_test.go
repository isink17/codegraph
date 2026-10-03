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

package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
)

// runPythonLambdaMatchScopeCases indexes testdata/python_lambda_match_scope,
// every file of which CPython 3.14 runs; the comment on each case is what
// run() returned. A lambda parameter and a match capture are names their
// scope binds, so a call through them must not bind an import of the same
// name. Expected states are CPython's verdicts, except where the graph refuses
// more than CPython would: lambdas and comprehensions are not lexical owners
// here, so a lambda parameter is a binding of the enclosing scope as a whole.
func runPythonLambdaMatchScopeCases(t *testing.T, reg *parser.Registry) {
	t.Helper()
	dir := filepath.Join("testdata", "python_lambda_match_scope")
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
	const unresolved, imported = "<unresolved>", "lib.py:lib.full [python_import_scope]"
	for _, tc := range []struct{ file, dst, want string }{
		// CPython calls the lambda parameter, never lib.full.
		{"l_param.py", "full", unresolved},   // 'param'
		{"l_star.py", "full", unresolved},    // TypeError: 'tuple' object is not callable
		{"l_kwstar.py", "full", unresolved},  // TypeError: 'dict' object is not callable
		{"l_default.py", "full", unresolved}, // 'default'
		{"l_kwonly.py", "full", unresolved},  // 'kwonly'
		{"l_nested.py", "full", unresolved},  // 'inner'
		{"l_noparam.py", "full", imported},   // 'lib.full': no parameter, the import
		// CPython: a capture pattern binds a local of the enclosing function.
		{"m_capture.py", "full", unresolved},   // 'capture'
		{"m_inline.py", "full", unresolved},    // 'capture'
		{"m_seq.py", "full", unresolved},       // 'seq'
		{"m_star.py", "full", unresolved},      // TypeError: 'list' object is not callable
		{"m_map.py", "full", unresolved},       // 'map'
		{"m_map_rest.py", "full", unresolved},  // TypeError: 'dict' object is not callable
		{"m_class_kw.py", "full", unresolved},  // 'keyword'
		{"m_class_pos.py", "full", unresolved}, // 'positional'
		{"m_as.py", "full", unresolved},        // 'as'
		{"m_or.py", "full", unresolved},        // 'or'
		// `case` followed directly by a bracket or a tab is still a case clause.
		{"m_paren.py", "full", unresolved},   // 'paren'
		{"m_bracket.py", "full", unresolved}, // 'bracket'
		{"m_brace.py", "full", unresolved},   // 'brace'
		{"m_tab.py", "full", unresolved},     // 'tab'
		// UnboundLocalError: the capture makes `full` local to the whole
		// function, so a call before the match is not the import either.
		{"m_before.py", "full", unresolved},
		// TypeError: 'int' object is not callable: a module-level capture
		// rebinds the module global every function reads.
		{"m_module.py", "full", unresolved},
		// CPython: lib.full. A guard, a value pattern, a class name and a
		// keyword-pattern attribute name bind nothing.
		{"m_guard.py", "full", imported},              // 'lib.full'
		{"m_value.py", "lib.full", "lib.py:lib.full"}, // 'lib.full'
		{"m_class_name.py", "full", imported},         // 'lib.full'
		{"m_class_name.py", "Box", "lib.py:lib.Box [python_import_scope]"},
	} {
		if got := r.edgeState(t, tc.file, tc.dst); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q = %s; want %s", tc.file, tc.dst, got, tc.want)
		}
	}
	// l_sibling.py: run() returns 'lib.full'; other() calls its own lambda's
	// parameter. The parameter refuses only the function it is written in.
	var states []string
	for _, line := range r.projection(t) {
		if strings.HasPrefix(line, `edge l_sibling.py "full" => `) {
			states = append(states, strings.TrimPrefix(line, `edge l_sibling.py "full" => `))
		}
	}
	if got, want := strings.Join(states, " | "), unresolved+" [] | "+imported; got != want {
		t.Errorf("l_sibling.py: %q = %s; want %s", "full", got, want)
	}
}

func TestPythonLambdaMatchScopeRegexAdapter(t *testing.T) {
	runPythonLambdaMatchScopeCases(t, parser.NewRegistry(pyparser.New()))
}

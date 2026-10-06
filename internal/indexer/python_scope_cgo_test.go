//go:build cgo

package indexer

import (
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// The tree-sitter adapter must reach the same decisions as the regex adapter on
// the same tree: import evidence and call spellings are the contract between
// the two parser paths, not an implementation detail of either.
func TestPythonImportScopeTreeSitterAdapter(t *testing.T) {
	runPythonScopeCases(t, parser.NewRegistry(tsparser.NewPython()))
}

func TestPythonUnstableModuleBindingsTreeSitter(t *testing.T) {
	runPythonUnstableBindingCases(t, parser.NewRegistry(tsparser.NewPython()))
}

func TestPythonDottedLocalClassTreeSitterAdapter(t *testing.T) {
	files := map[string]string{
		"a.py": "def run():\n    class C:\n        def full(self): return 'a'\n",
		"b.py": "def run():\n    class C:\n        full = lambda: 'b'\n    return C.full()\n",
	}
	r := newPyRepo(t, parser.NewRegistry(tsparser.NewPython()), files)
	assert := func() {
		t.Helper()
		got := strings.Join(r.projection(t), "\n")
		if !strings.Contains(got, `edge b.py "C.full" => <unresolved>`) {
			t.Fatalf("cross-module local class edge was not refused:\n%s", got)
		}
	}
	assert()
	positive := newPyRepo(t, parser.NewRegistry(tsparser.NewPython()), map[string]string{
		"positive.py": "def run():\n    class C:\n        def full(self): return 'local'\n    return C.full()\n",
	})
	positiveAssert := func(want string) {
		t.Helper()
		got := strings.Join(positive.projection(t), "\n")
		if !strings.Contains(got, want) {
			t.Fatalf("same-scope class control: want %q in\n%s", want, got)
		}
	}
	positiveAssert(`edge positive.py "C.full" => positive.py:positive.run.C.full [python_local_class_scope]`)
	positive.write(t, "positive.py", "def run():\n    class C:\n        full = lambda: 'local'\n    return C.full()\n")
	positive.update(t)
	positiveAssert(`edge positive.py "C.full" => <unresolved>`)
	fresh := newPyRepo(t, parser.NewRegistry(tsparser.NewPython()), map[string]string{
		"positive.py": "def run():\n    class C:\n        full = lambda: 'local'\n    return C.full()\n",
	})
	if got, want := strings.Join(positive.projection(t), "\n"), strings.Join(fresh.projection(t), "\n"); got != want {
		t.Fatalf("changed incremental graph differs from fresh:\nfresh:\n%s\nupdate:\n%s", want, got)
	}
	r.update(t)
	assert()
	freshOriginal := newPyRepo(t, parser.NewRegistry(tsparser.NewPython()), files)
	if got, want := strings.Join(freshOriginal.projection(t), "\n"), strings.Join(r.projection(t), "\n"); got != want {
		t.Fatalf("fresh/update graph differs:\nfresh:\n%s\nupdate:\n%s", got, want)
	}
}

func TestPythonLocalClassScopeCasesTreeSitter(t *testing.T) {
	runPythonLocalClassCases(t, func() *parser.Registry { return parser.NewRegistry(tsparser.NewPython()) })
}

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
	"github.com/isink17/codegraph/internal/store"
)

// pyRepo drives the real indexer over a real temp tree with a chosen Python
// adapter, so the same expectations can be asserted against both parser paths.
type pyRepo struct {
	ctx    context.Context
	root   string
	store  *store.Store
	idx    *Indexer
	repoID int64
}

func newPyRepo(t *testing.T, reg *parser.Registry, files map[string]string) *pyRepo {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "codegraph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &pyRepo{ctx: ctx, root: t.TempDir(), store: s, idx: New(s, reg, nil)}
	for rel, content := range files {
		r.write(t, rel, content)
	}
	if _, err := r.idx.Index(ctx, Options{RepoRoot: r.root, ScanKind: "index"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo, err := s.UpsertRepo(ctx, r.root)
	if err != nil {
		t.Fatalf("upsert repo: %v", err)
	}
	r.repoID = repo.ID
	return r
}

func (r *pyRepo) write(t *testing.T, rel, content string) {
	t.Helper()
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func (r *pyRepo) remove(t *testing.T, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(r.root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("remove %s: %v", rel, err)
	}
}

func (r *pyRepo) update(t *testing.T) {
	t.Helper()
	if _, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"}); err != nil {
		t.Fatalf("update: %v", err)
	}
}

// projection renders the repository's call edges as sorted, id-free text.
func (r *pyRepo) projection(t *testing.T) []string {
	t.Helper()
	symbols, err := r.store.ExportSymbolsPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatalf("export symbols: %v", err)
	}
	symbolFile := make(map[int64]string, len(symbols))
	for _, sym := range symbols {
		symbolFile[sym.ID] = filepath.ToSlash(sym.FilePath)
	}
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatalf("export edges: %v", err)
	}
	lines := make([]string, 0, len(edges))
	for _, e := range edges {
		dst := "<unresolved>"
		if e.DstSymbolID != nil {
			dst = symbolFile[*e.DstSymbolID] + ":" + e.DstQualifiedName
		}
		lines = append(lines, "edge "+filepath.ToSlash(e.FilePath)+` "`+e.DstName+`" => `+
			dst+" ["+e.ResolutionStrategy+"]")
	}
	sort.Strings(lines)
	return lines
}

// edgeState renders one caller's outgoing edge for a dst_name.
func (r *pyRepo) edgeState(t *testing.T, srcPath, dstName string) string {
	t.Helper()
	prefix := "edge " + srcPath + " "
	needle := `"` + dstName + `" => `
	for _, line := range r.projection(t) {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, needle) {
			return line[strings.Index(line, needle)+len(needle):]
		}
	}
	return "<no edge>"
}

// pythonScopeTree is the fixture every scope case is asserted against. Module
// basenames repeat across packages on purpose (`one/mod.py` vs `two/mod.py`,
// `alpha/utils.py` vs `beta/utils.py`) so no expectation can be satisfied by a
// basename match.
func pythonScopeTree() map[string]string {
	return map[string]string{
		"pkg/__init__.py": "",
		"pkg/helpers.py":  "def load():\n    return 1\n",

		"from_import.py":  "from pkg.helpers import load\n\n\ndef run():\n    return load()\n",
		"symbol_alias.py": "from pkg.helpers import load as read_config\n\n\ndef run():\n    return read_config()\n",
		"module_alias.py": "import pkg.helpers as helpers\n\n\ndef run():\n    return helpers.load()\n",
		"dotted.py":       "import pkg.helpers\n\n\ndef run():\n    return pkg.helpers.load()\n",
		"parenthesised.py": "from pkg.helpers import (\n    load,\n)\n\n\ndef run():\n" +
			"    return load()\n",
		"pkg/relative.py": "from .helpers import load\n\n\ndef run():\n    return load()\n",
		// The submodule reading of `from <package> import <name>`: `helpers` is
		// a module in `pkg`, not an object in `pkg/__init__.py`.
		"pkg/sibling.py":     "from . import helpers\n\n\ndef run():\n    return helpers.load()\n",
		"absolute_pkg.py":    "from pkg import helpers\n\n\ndef run():\n    return helpers.load()\n",
		"submodule_alias.py": "from pkg import helpers as h\n\n\ndef run():\n    return h.load()\n",

		"pkg/common/__init__.py": "",
		"pkg/common/helpers.py":  "def load_common():\n    return 3\n",
		"pkg/deep/__init__.py":   "",
		"pkg/deep/relative2.py":  "from ..common.helpers import load_common as read_config\n\n\ndef run():\n    return read_config()\n",

		"one/__init__.py": "",
		"one/mod.py":      "def dup_load():\n    return 1\n",
		"two/__init__.py": "",
		"two/mod.py":      "def dup_load():\n    return 2\n",
		"picks_one.py":    "from one.mod import dup_load\n\n\ndef run():\n    return dup_load()\n",

		"alpha/__init__.py": "",
		"alpha/utils.py":    "def shared_name():\n    return 1\n",
		"beta/__init__.py":  "",
		"beta/utils.py":     "def shared_name():\n    return 2\n",
		"picks_beta.py":     "from beta.utils import shared_name\n\n\ndef run():\n    return shared_name()\n",

		// The veto case: foo.config is a repository module without `missing_one`,
		// and bar/config.py declares one. The call must not reach it.
		"foo/__init__.py": "",
		"foo/config.py":   "def other():\n    return 1\n",
		"bar/__init__.py": "",
		"bar/config.py":   "def missing_one():\n    return 2\n",
		"veto.py":         "from foo.config import missing_one\n\n\ndef run():\n    return missing_one()\n",

		"external.py":  "from external_package import ext_only\n\n\ndef run():\n    return ext_only()\n",
		"dynamic.py":   "import importlib\n\n\ndef run():\n    mod = importlib.import_module(name)\n    return mod.load()\n",
		"builtin.py":   "def run(items):\n    return sorted(items)\n",
		"local.py":     "def local_helper():\n    return 1\n\n\ndef run():\n    return local_helper()\n",
		"ambiguous.py": "def twice():\n    return 1\n\n\ndef twice():\n    return 2\n\n\ndef run():\n    return twice()\n",

		// Two plausible absolute roots for `layout.mod`, so the layout itself is
		// undecidable and nothing may be bound.
		"layout/__init__.py":      "",
		"layout/mod.py":           "def ambiguous_root():\n    return 1\n",
		"src/layout/__init__.py":  "",
		"src/layout/mod.py":       "def ambiguous_root():\n    return 2\n",
		"src/ambiguous_layout.py": "from layout.mod import ambiguous_root\n\n\ndef run():\n    return ambiguous_root()\n",

		// Import syntax and quoted or commented text must not become call edges.
		"noise.py": "from pkg.helpers import load\n\n\ndef run():\n" +
			"    text = \"fake_call()\"\n" +
			"    # comment_call()\n" +
			"    return load()\n",
	}
}

type pythonScopeCase struct {
	name, file, dst string
	// want is a substring the rendered edge must contain; unresolved cases
	// assert the exact unresolved rendering.
	want string
}

func pythonScopeCases() []pythonScopeCase {
	return []pythonScopeCase{
		{"from_import", "from_import.py", "load", "pkg/helpers.py:helpers.load [python_import_scope]"},
		{"symbol_alias", "symbol_alias.py", "read_config", "pkg/helpers.py:helpers.load [python_import_scope]"},
		{"module_alias", "module_alias.py", "helpers.load", "pkg/helpers.py:helpers.load [python_import_scope]"},
		{"dotted_module", "dotted.py", "pkg.helpers.load", "pkg/helpers.py:helpers.load [python_import_scope]"},
		{"parenthesised_import", "parenthesised.py", "load", "pkg/helpers.py:helpers.load [python_import_scope]"},
		{"relative_one_level", "pkg/relative.py", "load", "pkg/helpers.py:helpers.load [python_import_scope]"},
		{"relative_submodule", "pkg/sibling.py", "helpers.load", "pkg/helpers.py:helpers.load [python_import_scope]"},
		{"absolute_submodule", "absolute_pkg.py", "helpers.load", "pkg/helpers.py:helpers.load [python_import_scope]"},
		{"aliased_submodule", "submodule_alias.py", "h.load", "pkg/helpers.py:helpers.load [python_import_scope]"},
		{"relative_two_levels_aliased", "pkg/deep/relative2.py", "read_config", "pkg/common/helpers.py:helpers.load_common [python_import_scope]"},
		{"duplicate_module_basename", "picks_one.py", "dup_load", "one/mod.py:mod.dup_load [python_import_scope]"},
		{"duplicate_symbol_name", "picks_beta.py", "shared_name", "beta/utils.py:utils.shared_name [python_import_scope]"},
		{"same_module_call", "local.py", "local_helper", "local.py:local.local_helper [python_module_scope]"},
		{"missing_imported_symbol_veto", "veto.py", "missing_one", "<unresolved> []"},
		{"external_import", "external.py", "ext_only", "<unresolved> []"},
		{"dynamic_import", "dynamic.py", "mod.load", "<unresolved> []"},
		{"builtin", "builtin.py", "sorted", "<unresolved> []"},
		{"same_module_ambiguous", "ambiguous.py", "twice", "<unresolved> []"},
		{"ambiguous_layout_root", "src/ambiguous_layout.py", "ambiguous_root", "<unresolved> []"},
		{"import_line_is_not_a_call", "noise.py", "import", "<no edge>"},
		{"string_is_not_a_call", "noise.py", "fake_call", "<no edge>"},
		{"comment_is_not_a_call", "noise.py", "comment_call", "<no edge>"},
	}
}

func runPythonScopeCases(t *testing.T, reg *parser.Registry) {
	t.Helper()
	r := newPyRepo(t, reg, pythonScopeTree())
	for _, tc := range pythonScopeCases() {
		if got := r.edgeState(t, tc.file, tc.dst); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %s -> %q = %s; want %s", tc.name, tc.file, tc.dst, got, tc.want)
		}
	}
}

func TestPythonImportScopeRegexAdapter(t *testing.T) {
	runPythonScopeCases(t, parser.NewRegistry(pyparser.New()))
}

// The wildcard form is deliberately not supported: `from x import *` publishes
// whatever `__all__` says, which the syntax does not state, so the pass neither
// binds nor vetoes and the generic strategies keep whatever they had.
func TestPythonWildcardImportIsNotImportScope(t *testing.T) {
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
		"pkg/__init__.py": "",
		"pkg/helpers.py":  "def only_here():\n    return 1\n",
		"star.py":         "from pkg.helpers import *\n\n\ndef run():\n    return only_here()\n",
	})
	if got := r.edgeState(t, "star.py", "only_here"); strings.Contains(got, "python_import_scope") {
		t.Fatalf("wildcard import must not claim import scope, got %s", got)
	}
}

// A Python import may never reach another language, however well the names line
// up.
func TestPythonImportScopeStaysWithinPython(t *testing.T) {
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
		"pkg/__init__.py": "",
		"pkg/helpers.py":  "def other():\n    return 1\n",
		"pkg/helpers.go":  "package pkg\n\nfunc load() int { return 1 }\n",
		"app.py":          "from pkg.helpers import load\n\n\ndef run():\n    return load()\n",
	})
	if got := r.edgeState(t, "app.py", "load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("cross-language bind: %s", got)
	}
}

// The lifecycle cases: every one of them must land on the same answer a fresh
// index of the final tree would give.
func TestPythonImportScopeLifecycle(t *testing.T) {
	base := map[string]string{
		"pkg/__init__.py": "",
		"pkg/helpers.py":  "def load():\n    return 1\n",
		"pkg/other.py":    "def load():\n    return 2\n",
		"decoy.py":        "def loaded_elsewhere():\n    return 3\n",
		"app.py":          "from pkg.helpers import load\n\n\ndef run():\n    return load()\n",
	}
	reg := parser.NewRegistry(pyparser.New())
	r := newPyRepo(t, reg, base)
	const bound = "pkg/helpers.py:helpers.load [python_import_scope]"
	if got := r.edgeState(t, "app.py", "load"); !strings.Contains(got, bound) {
		t.Fatalf("initial: %s", got)
	}

	// Touching the caller alone must not move the answer.
	r.write(t, "app.py", "from pkg.helpers import load\n\n\ndef run():\n    # touched\n    return load()\n")
	r.update(t)
	if got := r.edgeState(t, "app.py", "load"); !strings.Contains(got, bound) {
		t.Fatalf("caller touched: %s", got)
	}

	// Touching the imported module alone must not move it either.
	r.write(t, "pkg/helpers.py", "def load():\n    return 11\n")
	r.update(t)
	if got := r.edgeState(t, "app.py", "load"); !strings.Contains(got, bound) {
		t.Fatalf("target touched: %s", got)
	}

	// Renaming the imported symbol away leaves the edge unresolved -- and must
	// not let a weaker strategy reach `pkg/other.py`, which still declares one.
	r.write(t, "pkg/helpers.py", "def renamed():\n    return 11\n")
	r.update(t)
	if got := r.edgeState(t, "app.py", "load"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("target renamed: %s; want unresolved", got)
	}

	// Restoring it re-binds.
	r.write(t, "pkg/helpers.py", "def load():\n    return 12\n")
	r.update(t)
	if got := r.edgeState(t, "app.py", "load"); !strings.Contains(got, bound) {
		t.Fatalf("target restored: %s", got)
	}

	// Changing the alias keeps the binding under the new local name.
	r.write(t, "app.py", "from pkg.helpers import load as alias\n\n\ndef run():\n    return alias()\n")
	r.update(t)
	if got := r.edgeState(t, "app.py", "alias"); !strings.Contains(got, bound) {
		t.Fatalf("alias changed: %s", got)
	}

	// Changing the imported module moves the destination with it.
	r.write(t, "app.py", "from pkg.other import load as alias\n\n\ndef run():\n    return alias()\n")
	r.update(t)
	if got := r.edgeState(t, "app.py", "alias"); !strings.Contains(got, "pkg/other.py:other.load [python_import_scope]") {
		t.Fatalf("module changed: %s", got)
	}

	// Deleting the imported module leaves the edge unresolved.
	r.remove(t, "pkg/other.py")
	r.update(t)
	if got := r.edgeState(t, "app.py", "alias"); !strings.Contains(got, "<unresolved>") {
		t.Fatalf("target deleted: %s; want unresolved", got)
	}
}

// Every step above is also checked for parity against a fresh index of the same
// final tree, which is the only guarantee that an incremental answer is not
// merely stale.
func TestPythonImportScopeFullIncrementalParity(t *testing.T) {
	reg := parser.NewRegistry(pyparser.New())
	r := newPyRepo(t, reg, pythonScopeTree())
	steps := []struct {
		name  string
		apply func()
	}{
		{"touch caller", func() {
			r.write(t, "from_import.py", "from pkg.helpers import load\n\n\ndef run():\n    # touched\n    return load()\n")
		}},
		{"touch target", func() { r.write(t, "pkg/helpers.py", "def load():\n    return 99\n") }},
		{"rename target", func() { r.write(t, "pkg/helpers.py", "def renamed():\n    return 99\n") }},
		{"change alias", func() {
			r.write(t, "symbol_alias.py", "from pkg.helpers import renamed as read_config\n\n\ndef run():\n    return read_config()\n")
		}},
		{"change module", func() {
			r.write(t, "picks_one.py", "from two.mod import dup_load\n\n\ndef run():\n    return dup_load()\n")
		}},
		{"delete target", func() { r.remove(t, "beta/utils.py") }},
	}
	for _, step := range steps {
		step.apply()
		r.update(t)
		got := r.projection(t)
		fresh := newPyRepo(t, reg, readTree(t, r.root)).projection(t)
		if diff := pythonProjectionDiff(fresh, got); diff != "" {
			t.Fatalf("%s: update diverges from a fresh index of the same tree:\n%s", step.name, diff)
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("read tree: %v", err)
	}
	return files
}

func pythonProjectionDiff(want, got []string) string {
	remaining := map[string]int{}
	for _, line := range want {
		remaining[line]++
	}
	var only []string
	for _, line := range got {
		if remaining[line] > 0 {
			remaining[line]--
			continue
		}
		only = append(only, "update-only: "+line)
	}
	for line, n := range remaining {
		for i := 0; i < n; i++ {
			only = append(only, "fresh-only:  "+line)
		}
	}
	sort.Strings(only)
	return strings.Join(only, "\n")
}

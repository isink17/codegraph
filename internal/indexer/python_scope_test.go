package indexer

import (
	"context"
	"database/sql"
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
	dbPath string
	store  *store.Store
	idx    *Indexer
	repoID int64
}

func newPyRepo(t *testing.T, reg *parser.Registry, files map[string]string) *pyRepo {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codegraph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &pyRepo{ctx: ctx, root: t.TempDir(), dbPath: dbPath, store: s, idx: New(s, reg, nil)}
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

func (r *pyRepo) raw(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
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

		// An absolute import is anchored at the repository root only, so
		// `layout.mod` is `layout/mod.py` and never the src-layout twin.
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
		{"absolute_root_only", "src/ambiguous_layout.py", "ambiguous_root", "layout/mod.py:mod.ambiguous_root [python_import_scope]"},
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

func runPythonUnstableBindingCases(t *testing.T, reg *parser.Registry) {
	t.Helper()
	r := newPyRepo(t, reg, map[string]string{
		"pkg/__init__.py":               "",
		"pkg/decorators.py":             "def plain(fn):\n    return replacement\n\ndef factory():\n    return plain\n\ndef replacement():\n    return 0\n",
		"pkg/decorated.py":              "from pkg.decorators import plain, factory\n\n@plain\ndef f():\n    return 1\n\n@factory()\ndef g():\n    return 1\n\n@plain\n@factory\ndef h():\n    return 1\n\n@plain\ndef unrelated():\n    return 1\n\ndef stable():\n    return 1\n\ndef call_f():\n    return f()\ndef call_g():\n    return g()\ndef call_h():\n    return h()\ndef call_unrelated():\n    return unrelated()\ndef call_stable():\n    return stable()\n",
		"pkg/globals_case.py":           "def f():\n    return 1\ndef g():\n    return 2\nglobals()[\"f\"] = g\ndef call_f():\n    return f()\ndef call_g():\n    return g()\n",
		"pkg/computed_globals.py":       "def f():\n    return 1\nkey = \"f\"\nglobals()[key] = lambda: 2\ndef call_f():\n    return f()\n",
		"pkg/lib.py":                    "def helper():\n    return 1\ndef other():\n    return 2\ndef target():\n    return 3\n",
		"pkg/patch_setattr.py":          "from pkg import lib\ndef replacement():\n    return 0\nsetattr(lib, \"helper\", replacement)\ndef call_helper():\n    return lib.helper()\ndef call_other():\n    return lib.other()\n",
		"pkg/patch_shadowed_setattr.py": "from pkg import lib\ndef setattr(obj, name, value):\n    return None\ndef fake():\n    return 0\nsetattr(lib, \"helper\", fake)\ndef call_helper():\n    return lib.helper()\n",
		"pkg/custom_setter.py":          "def setattr(obj, name, value):\n    return None\n",
		"pkg/patch_imported_setattr.py": "from pkg import lib\nfrom pkg.custom_setter import setattr\ndef fake():\n    return 0\nsetattr(lib, \"helper\", fake)\ndef call_helper():\n    return lib.helper()\n",
		"pkg/patch_setter_after.py":     "from pkg import lib\nsetattr(lib, \"helper\", fake)\ndef setattr(obj, name, value):\n    return None\ndef fake():\n    return 0\ndef call_helper():\n    return lib.helper()\n",
		"pkg/patch_assigned_setattr.py": "from pkg import lib\ndef custom(obj, name, value):\n    return None\nsetattr = custom\ndef fake():\n    return 0\nsetattr(lib, \"helper\", fake)\ndef call_helper():\n    return lib.helper()\n",
		"pkg/patch_class_setattr.py":    "from pkg import lib\nclass setattr:\n    def __init__(self, obj, name, value):\n        pass\ndef fake():\n    return 0\nsetattr(lib, \"helper\", fake)\ndef call_helper():\n    return lib.helper()\n",
		"pkg/patch_direct.py":           "from pkg import lib\ndef replacement():\n    return 0\nlib.other = replacement\ndef call_other():\n    return lib.other()\ndef call_helper():\n    return lib.helper()\n",
		"pkg/patch_receiver.py":         "from pkg import lib, decorators\ndef replacement():\n    return 0\nsetattr(decorators, \"helper\", replacement)\ndef call_helper():\n    return lib.helper()\n",
		"pkg/patch_computed.py":         "from pkg import lib\ndef replacement():\n    return 0\nname = \"helper\"\nsetattr(lib, name, replacement)\ndef call_helper():\n    return lib.helper()\n",
	})
	for _, tc := range []struct{ file, name, want string }{
		{"pkg/decorated.py", "f", "<unresolved>"},
		{"pkg/decorated.py", "g", "<unresolved>"},
		{"pkg/decorated.py", "h", "<unresolved>"},
		{"pkg/decorated.py", "unrelated", "<unresolved>"},
		{"pkg/decorated.py", "stable", "pkg/decorated.py:decorated.stable [python_module_scope]"},
		{"pkg/globals_case.py", "f", "<unresolved>"},
		{"pkg/globals_case.py", "g", "pkg/globals_case.py:globals_case.g [python_module_scope]"},
		{"pkg/computed_globals.py", "f", "pkg/computed_globals.py:computed_globals.f [python_module_scope]"},
		{"pkg/patch_setattr.py", "lib.helper", "<unresolved>"},
		{"pkg/patch_shadowed_setattr.py", "lib.helper", "pkg/lib.py:lib.helper [python_import_scope]"},
		{"pkg/patch_imported_setattr.py", "lib.helper", "pkg/lib.py:lib.helper [python_import_scope]"},
		{"pkg/patch_setter_after.py", "lib.helper", "<unresolved>"},
		{"pkg/patch_assigned_setattr.py", "lib.helper", "pkg/lib.py:lib.helper [python_import_scope]"},
		{"pkg/patch_class_setattr.py", "lib.helper", "pkg/lib.py:lib.helper [python_import_scope]"},
		{"pkg/patch_setattr.py", "lib.other", "pkg/lib.py:lib.other [python_import_scope]"},
		{"pkg/patch_direct.py", "lib.other", "<unresolved>"},
		{"pkg/patch_direct.py", "lib.helper", "pkg/lib.py:lib.helper [python_import_scope]"},
		{"pkg/patch_receiver.py", "lib.helper", "pkg/lib.py:lib.helper [python_import_scope]"},
		{"pkg/patch_computed.py", "lib.helper", "pkg/lib.py:lib.helper [python_import_scope]"},
	} {
		if got := r.edgeState(t, tc.file, tc.name); !strings.Contains(got, tc.want) {
			t.Errorf("%s %s: got %s; want %s", tc.file, tc.name, got, tc.want)
		}
	}
}

func TestPythonUnstableModuleBindingsRegex(t *testing.T) {
	runPythonUnstableBindingCases(t, parser.NewRegistry(pyparser.New()))
}

func TestPythonUnstableBindingLifecycle(t *testing.T) {
	reg := parser.NewRegistry(pyparser.New())
	base := map[string]string{
		"pkg/__init__.py":     "",
		"pkg/decorators.py":   "def plain(fn):\n    return replacement\n\ndef factory():\n    return plain\n\ndef replacement():\n    return 0\n",
		"pkg/decorated.py":    "def f():\n    return 1\ndef g():\n    return 2\ndef stable():\n    return 3\ndef call_f():\n    return f()\ndef call_g():\n    return g()\ndef call_stable():\n    return stable()\n",
		"pkg/globals_case.py": "def f():\n    return 1\ndef g():\n    return 2\ndef call_f():\n    return f()\ndef call_g():\n    return g()\n",
		"pkg/lib.py":          "def helper():\n    return 1\ndef other():\n    return 2\ndef target():\n    return 3\n",
		"pkg/patch.py":        "from pkg import lib\ndef replacement():\n    return 0\ndef call_helper():\n    return lib.helper()\ndef call_other():\n    return lib.other()\ndef call_target():\n    return lib.target()\n",
	}
	r := newPyRepo(t, reg, base)
	assertFresh := func(step string) {
		t.Helper()
		want := newPyRepo(t, reg, readTree(t, r.root)).projection(t)
		if diff := pythonProjectionDiff(want, r.projection(t)); diff != "" {
			t.Fatalf("%s: update differs from fresh index:\n%s", step, diff)
		}
	}
	assertTarget := func(file, name, want string) {
		t.Helper()
		if got := r.edgeState(t, file, name); !strings.Contains(got, want) {
			t.Fatalf("%s %s: got %s; want %s", file, name, got, want)
		}
	}
	assertTarget("pkg/decorated.py", "f", "pkg/decorated.py:decorated.f [python_module_scope]")
	assertTarget("pkg/globals_case.py", "f", "pkg/globals_case.py:globals_case.f [python_module_scope]")
	assertTarget("pkg/patch.py", "lib.helper", "pkg/lib.py:lib.helper [python_import_scope]")

	// Seed the prior high-confidence edges and reference identities as if this
	// source had been indexed before the mutation semantics were understood.
	for _, stale := range []struct{ file, name, target string }{
		{"pkg/decorated.py", "f", "decorated.f"},
		{"pkg/globals_case.py", "f", "globals_case.f"},
		{"pkg/patch.py", "lib.helper", "lib.helper"},
	} {
		var id int64
		if err := r.raw(t).QueryRowContext(r.ctx, `SELECT id FROM symbols WHERE repo_id=? AND qualified_name=?`, r.repoID, stale.target).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := r.raw(t).ExecContext(r.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy='python_module_scope',resolution_confidence='high' WHERE repo_id=? AND dst_name=? AND file_id=(SELECT id FROM files WHERE repo_id=? AND path=?)`, id, r.repoID, stale.name, r.repoID, stale.file); err != nil {
			t.Fatal(err)
		}
		if _, err := r.raw(t).ExecContext(r.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=? AND ref_kind='call' AND name=? AND file_id=(SELECT id FROM files WHERE repo_id=? AND path=?)`, id, r.repoID, stale.name, r.repoID, stale.file); err != nil {
			t.Fatal(err)
		}
	}

	r.write(t, "pkg/decorated.py", "from pkg.decorators import plain, factory\n@plain\ndef f():\n    return 1\n@factory()\ndef g():\n    return 2\ndef stable():\n    return 3\ndef call_f():\n    return f()\ndef call_g():\n    return g()\ndef call_stable():\n    return stable()\n")
	r.write(t, "pkg/globals_case.py", "def f():\n    return 1\ndef g():\n    return 2\nglobals()[\"f\"] = g\ndef call_f():\n    return f()\ndef call_g():\n    return g()\n")
	r.write(t, "pkg/patch.py", "from pkg import lib\ndef replacement():\n    return 0\nsetattr(lib, \"helper\", replacement)\nlib.other = replacement\ndef call_helper():\n    return lib.helper()\ndef call_other():\n    return lib.other()\ndef call_target():\n    return lib.target()\n")
	r.update(t)
	for _, unresolved := range []struct{ file, name string }{{"pkg/decorated.py", "f"}, {"pkg/decorated.py", "g"}, {"pkg/globals_case.py", "f"}, {"pkg/patch.py", "lib.helper"}, {"pkg/patch.py", "lib.other"}} {
		assertTarget(unresolved.file, unresolved.name, "<unresolved>")
		var edge, ref sql.NullInt64
		if err := r.raw(t).QueryRowContext(r.ctx, `SELECT e.dst_symbol_id,r.symbol_id FROM edges e JOIN files f ON f.id=e.file_id JOIN references_tbl r ON r.file_id=e.file_id AND r.ref_kind='call' AND r.name=e.dst_name WHERE e.repo_id=? AND f.path=? AND e.dst_name=?`, r.repoID, unresolved.file, unresolved.name).Scan(&edge, &ref); err != nil {
			t.Fatalf("read cleared identities for %s %s: %v", unresolved.file, unresolved.name, err)
		}
		if edge.Valid || ref.Valid {
			t.Fatalf("%s %s retained edge/reference identities: %v / %v", unresolved.file, unresolved.name, edge, ref)
		}
	}
	assertTarget("pkg/decorated.py", "stable", "pkg/decorated.py:decorated.stable [python_module_scope]")
	assertTarget("pkg/globals_case.py", "g", "pkg/globals_case.py:globals_case.g [python_module_scope]")
	assertTarget("pkg/patch.py", "lib.target", "pkg/lib.py:lib.target [python_import_scope]")
	assertFresh("add mutations")

	// Recreate the v2.0 profile/evidence shape of an existing graph, including
	// its old high-confidence targets, then let ordinary update reparse it.
	legacy := newPyRepo(t, reg, readTree(t, r.root))
	for _, stale := range []struct{ file, name, target string }{
		{"pkg/decorated.py", "f", "decorated.f"},
		{"pkg/decorated.py", "g", "decorated.g"},
		{"pkg/globals_case.py", "f", "globals_case.f"},
		{"pkg/patch.py", "lib.helper", "lib.helper"},
		{"pkg/patch.py", "lib.other", "lib.other"},
	} {
		var id int64
		if err := legacy.raw(t).QueryRowContext(legacy.ctx, `SELECT id FROM symbols WHERE repo_id=? AND qualified_name=?`, legacy.repoID, stale.target).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.raw(t).ExecContext(legacy.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy='python_module_scope',resolution_confidence='high' WHERE repo_id=? AND dst_name=? AND file_id=(SELECT id FROM files WHERE repo_id=? AND path=?)`, id, legacy.repoID, stale.name, legacy.repoID, stale.file); err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.raw(t).ExecContext(legacy.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=? AND ref_kind='call' AND name=? AND file_id=(SELECT id FROM files WHERE repo_id=? AND path=?)`, id, legacy.repoID, stale.name, legacy.repoID, stale.file); err != nil {
			t.Fatal(err)
		}
	}
	for _, evidence := range []struct{ file, name string }{
		{"pkg/decorated.py", "f"}, {"pkg/decorated.py", "g"},
		{"pkg/globals_case.py", "f"}, {"pkg/patch.py", "lib.helper"}, {"pkg/patch.py", "lib.other"},
	} {
		if _, err := legacy.raw(t).ExecContext(legacy.ctx, `DELETE FROM scope_import_evidence WHERE repo_id=? AND language='python' AND import_kind='local_binding' AND local_name=? AND owner_module='' AND file_id=(SELECT id FROM files WHERE repo_id=? AND path=?)`, legacy.repoID, evidence.name, legacy.repoID, evidence.file); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := legacy.raw(t).ExecContext(legacy.ctx, `UPDATE files SET parser_profile='python-regex:python:v8' WHERE repo_id=? AND language='python'`, legacy.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.idx.Update(legacy.ctx, Options{RepoRoot: legacy.root, ScanKind: "update"}); err != nil {
		t.Fatalf("ordinary update of legacy v8 graph: %v", err)
	}
	for _, unresolved := range []struct{ file, name string }{
		{"pkg/decorated.py", "f"}, {"pkg/decorated.py", "g"}, {"pkg/globals_case.py", "f"}, {"pkg/patch.py", "lib.helper"}, {"pkg/patch.py", "lib.other"},
	} {
		if got := legacy.edgeState(t, unresolved.file, unresolved.name); !strings.Contains(got, "<unresolved>") {
			t.Fatalf("legacy upgrade %s %s: %s", unresolved.file, unresolved.name, got)
		}
		var edge, ref sql.NullInt64
		if err := legacy.raw(t).QueryRowContext(legacy.ctx, `SELECT e.dst_symbol_id,r.symbol_id FROM edges e JOIN files f ON f.id=e.file_id JOIN references_tbl r ON r.file_id=e.file_id AND r.ref_kind='call' AND r.name=e.dst_name WHERE e.repo_id=? AND f.path=? AND e.dst_name=?`, legacy.repoID, unresolved.file, unresolved.name).Scan(&edge, &ref); err != nil {
			t.Fatal(err)
		}
		if edge.Valid || ref.Valid {
			t.Fatalf("legacy upgrade retained %s %s identities: %v / %v", unresolved.file, unresolved.name, edge, ref)
		}
	}
	wantLegacy := newPyRepo(t, reg, readTree(t, legacy.root)).projection(t)
	if diff := pythonProjectionDiff(wantLegacy, legacy.projection(t)); diff != "" {
		t.Fatalf("legacy upgrade differs from fresh index:\n%s", diff)
	}

	// Change only the literal mutation names: calls to the old names recover,
	// and the new names become unresolved.
	r.write(t, "pkg/globals_case.py", "def f():\n    return 1\ndef g():\n    return 2\nglobals()[\"g\"] = f\ndef call_f():\n    return f()\ndef call_g():\n    return g()\n")
	r.write(t, "pkg/patch.py", "from pkg import lib\ndef replacement():\n    return 0\nsetattr(lib, \"target\", replacement)\nlib.helper = replacement\ndef call_helper():\n    return lib.helper()\ndef call_other():\n    return lib.other()\ndef call_target():\n    return lib.target()\n")
	r.update(t)
	assertTarget("pkg/globals_case.py", "f", "pkg/globals_case.py:globals_case.f [python_module_scope]")
	assertTarget("pkg/globals_case.py", "g", "<unresolved>")
	assertTarget("pkg/patch.py", "lib.helper", "<unresolved>")
	assertTarget("pkg/patch.py", "lib.target", "<unresolved>")
	assertTarget("pkg/patch.py", "lib.other", "pkg/lib.py:lib.other [python_import_scope]")
	assertFresh("change mutation names")

	// Removing the mutations and decorators restores the original bindings.
	for path, content := range base {
		r.write(t, path, content)
	}
	r.update(t)
	assertTarget("pkg/decorated.py", "f", "pkg/decorated.py:decorated.f [python_module_scope]")
	assertTarget("pkg/globals_case.py", "g", "pkg/globals_case.py:globals_case.g [python_module_scope]")
	assertTarget("pkg/patch.py", "lib.helper", "pkg/lib.py:lib.helper [python_import_scope]")
	assertFresh("remove mutations")
	before := strings.Join(r.projection(t), "\n")
	summary, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 0 || summary.FilesIndexed != 0 || strings.Join(r.projection(t), "\n") != before {
		t.Fatalf("second update was not a no-op: summary=%+v", summary)
	}
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

func TestPythonDottedLocalClassDoesNotBindCrossModule(t *testing.T) {
	files := map[string]string{
		"a.py": "def run():\n    class C:\n        def full(self): return 'a'\n",
		"b.py": "def run():\n    class C:\n        full = lambda: 'b'\n    return C.full()\n",
	}
	r := newPyRepo(t, parser.NewRegistry(pyparser.New()), files)
	assert := func() {
		t.Helper()
		got := strings.Join(r.projection(t), "\n")
		if !strings.Contains(got, `edge b.py "C.full" => <unresolved>`) {
			syms, _ := r.store.ExportSymbolsPage(r.ctx, r.repoID, 100000, 0)
			t.Fatalf("cross-module local class edge was not refused:\n%s\nsymbols: %+v", got, syms)
		}
	}
	assert()
	positive := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
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
	fresh := newPyRepo(t, parser.NewRegistry(pyparser.New()), map[string]string{
		"positive.py": "def run():\n    class C:\n        full = lambda: 'local'\n    return C.full()\n",
	})
	if got, want := strings.Join(positive.projection(t), "\n"), strings.Join(fresh.projection(t), "\n"); got != want {
		t.Fatalf("changed incremental graph differs from fresh:\nfresh:\n%s\nupdate:\n%s", want, got)
	}
	r.update(t)
	assert()
	freshOriginal := newPyRepo(t, parser.NewRegistry(pyparser.New()), files)
	if got, want := strings.Join(freshOriginal.projection(t), "\n"), strings.Join(r.projection(t), "\n"); got != want {
		t.Fatalf("fresh/update graph differs:\nfresh:\n%s\nupdate:\n%s", got, want)
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

// callEdgeTargets renders one file's call edges keyed by the calling symbol, so
// a test can assert which call site reached which destination rather than
// relying on projection order.
func (r *pyRepo) callEdgeTargets(t *testing.T, srcPath string) map[string]string {
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
	out := map[string]string{}
	for _, e := range edges {
		if filepath.ToSlash(e.FilePath) != srcPath {
			continue
		}
		dst := "<unresolved>"
		if e.DstSymbolID != nil {
			dst = symbolFile[*e.DstSymbolID] + ":" + e.DstQualifiedName
		}
		out[e.SrcQualifiedName] = dst + " [" + e.ResolutionStrategy + "]"
	}
	return out
}

// runPythonLocalClassCases pins which class a dotted `C.member()` call may
// bind to. Each case is a CPython 3.14.5 fact (recovery/a/cg86_oracle.py).
func runPythonLocalClassCases(t *testing.T, reg func() *parser.Registry) {
	const bound = `python_local_class_scope`
	cases := []struct {
		name    string
		files   map[string]string
		src     string
		want    string // substring of the edge state
		notWant string
	}{
		{"enclosing function class is visible to a nested function",
			map[string]string{"m.py": "def run():\n    class C:\n        def full(self): return 1\n    def inner():\n        return C.full(None)\n    return inner()\n"},
			"m.py", "m.py:m.run.C.full [" + bound + "]", ""},
		{"sibling functions bind their own class",
			map[string]string{"m.py": "def a():\n    class C:\n        def full(self): return 1\n    return C.full(None)\n\n\ndef b():\n    class C:\n        def full(self): return 2\n    return C.full(None)\n"},
			"m.py", "m.py:m.a.C.full [" + bound + "]", ""},
		{"method does not see a class declared in its own class body",
			map[string]string{"m.py": "class Outer:\n    class C:\n        def full(self): return 1\n\n    def method(self):\n        return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
		{"rebound name is not the class",
			map[string]string{"m.py": "def run():\n    class C:\n        def full(self): return 1\n    C = make()\n    return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
		{"import of the same name is not the local class",
			map[string]string{"x.py": "class C:\n    def full(self): return 0\n", "m.py": "from x import C\n\n\ndef run():\n    class C:\n        def full(self): return 1\n    return C.full(None)\n"},
			"m.py", "", "x.py:"},
		{"F1 nearer def of the same name is not the module class",
			map[string]string{"m.py": "class C:\n    def full(self): return 1\n\n\ndef run():\n    def C(): pass\n    return C.full()\n"},
			"m.py", "<unresolved>", bound},
		{"F1 module def after the class wins",
			map[string]string{"m.py": "class C:\n    def full(self): return 1\n\n\ndef C(): pass\n\n\ndef run():\n    return C.full()\n"},
			"m.py", "<unresolved>", bound},
		{"F2 wildcard import may rebind the class",
			map[string]string{"pkg/__init__.py": "", "pkg/star.py": "class C:\n    def full(self): return 0\n", "m.py": "class C:\n    def full(self): return 1\n\n\nfrom pkg.star import *\n\n\ndef run():\n    return C.full()\n"},
			"m.py", "", "m.py:m.C.full"},
		{"F3 del makes the name local",
			map[string]string{"m.py": "class C:\n    def full(self): return 1\n\n\ndef run():\n    del C\n    return C.full()\n"},
			"m.py", "<unresolved>", bound},
		{"F4 nonlocal rebinding from a sibling closure",
			map[string]string{"m.py": "def run():\n    class C:\n        def full(self): return 1\n    def inner():\n        return C.full(None)\n    def other():\n        nonlocal C\n        C = 2\n    return inner\n"},
			"m.py", "<unresolved>", bound},
		{"F4 global rebinding of a module class",
			map[string]string{"m.py": "class C:\n    def full(self): return 1\n\n\ndef swap():\n    global C\n    C = 2\n\n\ndef run():\n    return C.full()\n"},
			"m.py", "<unresolved>", bound},
		{"module class is left to the other strategies",
			map[string]string{"m.py": "class C:\n    def full(self): return 1\n\n\ndef run():\n    return C.full()\n"},
			"m.py", "m.py:m.C.full", bound},
		{"B2 global declares a class",
			map[string]string{"m.py": "def run():\n    class C:\n        def full(self): return 1\n    return C.full(None)\n\n\ndef swap():\n    global C\n    class C: pass\n"},
			"m.py", "<unresolved>", bound},
		{"B2 global declares a def",
			map[string]string{"m.py": "class C:\n    def full(self): return 1\n\n\ndef swap():\n    global C\n    def C(): pass\n\n\ndef run():\n    return C.full()\n"},
			"m.py", "<unresolved>", bound},
		{"B2 global imports the name",
			map[string]string{"m.py": "class C:\n    def full(self): return 1\n\n\ndef swap():\n    global C\n    import fastimpl as C\n\n\ndef run():\n    return C.full()\n"},
			"m.py", "<unresolved>", bound},
		{"B2 global and assignment on one line",
			map[string]string{"m.py": "class C:\n    def full(self): return 1\n\n\ndef swap():\n    global C; C = 2\n\n\ndef run():\n    return C.full()\n"},
			"m.py", "<unresolved>", bound},
		{"B2 nonlocal declares a class in a sibling closure",
			map[string]string{"m.py": "def run():\n    class C:\n        def full(self): return 1\n    def inner():\n        return C.full(None)\n    def other():\n        nonlocal C\n        class C: pass\n    return inner\n"},
			"m.py", "<unresolved>", bound},
		{"B3 class body rebinds the member",
			map[string]string{"m.py": "def run():\n    class C:\n        def full(self): return 1\n        full = 3\n    return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
		{"B3 class body rebinds the member by import",
			map[string]string{"m.py": "def run():\n    class C:\n        def full(self): return 1\n        from os import getcwd as full\n    return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
		{"B4 decorated class",
			map[string]string{"m.py": "def deco(cls): return 7\n\n\ndef run():\n    @deco\n    class C:\n        def full(self): return 1\n    return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
		{"B4 multi-line decorator",
			map[string]string{"m.py": "def deco(a): return lambda cls: 7\n\n\ndef run():\n    @deco(\n        1,\n    )\n    class C:\n        def full(self): return 1\n    return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
		{"decorated member (@property) is not provably the def",
			map[string]string{"m.py": "def run():\n    class C:\n        @property\n        def full(self): return 1\n    return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
		{"decorated member (@staticmethod) is refused conservatively",
			map[string]string{"m.py": "def run():\n    class C:\n        @staticmethod\n        def full(): return 1\n    return C.full()\n"},
			"m.py", "<unresolved>", bound},
		{"literal attribute assignment replaces the member",
			map[string]string{"m.py": "def run():\n    class C:\n        def full(self): return 1\n    C.full = 3\n    return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
		{"annotated attribute assignment replaces the member",
			map[string]string{"m.py": "def run():\n    class C:\n        def full(self): return 1\n    C.full: int = 3\n    return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
		{"chained attribute assignment replaces the member",
			map[string]string{"m.py": "def run(o):\n    class C:\n        def full(self): return 1\n    o.x = C.full = 3\n    return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
		{"attribute assignment from a nested function replaces the member",
			map[string]string{"m.py": "def run():\n    class C:\n        def full(self): return 1\n    def patch():\n        C.full = 3\n    return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
		{"assigning another attribute or another receiver does not withhold",
			map[string]string{"m.py": "def run(o):\n    class C:\n        def full(self): return 1\n    C.other = 3\n    o.full = 3\n    return C.full(None)\n"},
			"m.py", "m.py:m.run.C.full [" + bound + "]", ""},
		{"undecorated sibling after a decorated class still binds",
			map[string]string{"m.py": "def deco(cls): return 7\n\n\ndef run():\n    @deco\n    class D:\n        pass\n    class C:\n        def full(self): return 1\n    return C.full(None)\n"},
			"m.py", "m.py:m.run.C.full [" + bound + "]", ""},
		{"global without a rebinding of another name does not withhold",
			map[string]string{"m.py": "counter = 0\n\n\ndef bump():\n    global counter\n    counter += 1\n\n\ndef run():\n    class C:\n        def full(self): return 1\n    return C.full(None)\n"},
			"m.py", "m.py:m.run.C.full [" + bound + "]", ""},
		{"member defined twice is undecidable",
			map[string]string{"m.py": "def run():\n    class C:\n        def full(self): return 1\n        def full(self, x): return 2\n    return C.full(None)\n"},
			"m.py", "<unresolved>", bound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPyRepo(t, reg(), tc.files)
			got := r.edgeState(t, tc.src, "C.full")
			if !strings.Contains(got, tc.want) || (tc.notWant != "" && strings.Contains(got, tc.notWant)) {
				t.Fatalf("C.full => %q; want contains %q, not %q\n%s", got, tc.want, tc.notWant, strings.Join(r.projection(t), "\n"))
			}
		})
	}
}

func TestPythonLocalClassScopeCases(t *testing.T) {
	runPythonLocalClassCases(t, func() *parser.Registry { return parser.NewRegistry(pyparser.New()) })
}

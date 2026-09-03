//go:build cgo

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// Full-index / incremental parity across BOTH directions of a uniqueness change
// (P22.12).
//
// P22.8 closed the "a declaration arrived" direction: a binding made while a
// name was unique is invalidated when a second declaration shows up. The other
// direction was still open, because the indexer derived the batch's names from
// the NEW parse only -- so a name a file STOPPED declaring appeared in no batch,
// and an edge that was unresolved *because* that declaration made the name
// ambiguous stayed unresolved forever while a fresh index of the same tree
// resolved it.
//
// The oracle here is the P22.8 semantic projection (SemanticGraphForTest): file
// paths, qualified names, strategies and confidences, never row ids. Every test
// compares an incrementally-updated database against a fresh index of the exact
// same final tree.

// tree is a repository's complete source content, keyed by repo-relative path.
type tree map[string]string

// lifecycleRepo drives the real indexer over a real temp tree.
type lifecycleRepo struct {
	ctx    context.Context
	root   string
	store  *store.Store
	idx    *Indexer
	repoID int64
}

// Built on the tree-sitter adapters, which is why this file is cgo-only: the
// Python and C++ fixtures below need the adapters that actually emit call
// edges, the same reason cpp_callgraph_test.go carries the tag.
func lifecycleRegistry() *parser.Registry {
	return parser.NewRegistry(goparser.New(), tsparser.NewPython(), tsparser.NewCpp(), tsparser.NewJava())
}

func newLifecycleRepo(t *testing.T, files tree) *lifecycleRepo {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	s, err := store.Open(filepath.Join(t.TempDir(), "codegraph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &lifecycleRepo{ctx: ctx, root: root, store: s, idx: New(s, lifecycleRegistry(), nil)}
	for rel, content := range files {
		r.write(t, rel, content)
	}
	if _, err := r.idx.Index(ctx, Options{RepoRoot: root, ScanKind: "index"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatalf("upsert repo: %v", err)
	}
	r.repoID = repo.ID
	return r
}

func (r *lifecycleRepo) write(t *testing.T, rel, content string) {
	t.Helper()
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func (r *lifecycleRepo) remove(t *testing.T, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(r.root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("remove %s: %v", rel, err)
	}
}

// update runs an ordinary incremental update. With no paths it is a whole-tree
// update scan (the `codegraph update` shape, deletions via MarkMissingDeleted);
// with paths it is the path-scoped shape (deletions via MarkFilesDeletedBatch).
func (r *lifecycleRepo) update(t *testing.T, paths ...string) store.ScanSummary {
	t.Helper()
	summary, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Paths: paths})
	if err != nil {
		t.Fatalf("update(%v): %v", paths, err)
	}
	return summary
}

// projection renders the repository's relationships as sorted, id-free text --
// the P22.8 semantic projection, rebuilt here on the exported export API
// because the store's own helper is package-private to its tests.
//
// Every field is a semantic identity (file path, qualified name, source line,
// strategy, confidence). No row ids and no timestamps, so two databases that
// hold the same graph render identically however they were built.
func (r *lifecycleRepo) projection(t *testing.T) []string {
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
		dst := "::"
		if e.DstSymbolID != nil {
			dst = symbolFile[*e.DstSymbolID] + ":" + e.DstQualifiedName
		}
		lines = append(lines, "edge "+filepath.ToSlash(e.FilePath)+":"+e.SrcQualifiedName+
			" -"+e.Kind+`-> "`+e.DstName+`" => `+dst+
			" ["+e.ResolutionStrategy+"/"+e.ResolutionConfidence+"]")
	}
	sort.Strings(lines)
	return lines
}

// currentTree reads the repository back off disk, so a fresh index can be run
// over exactly what the incremental run ended up with.
func (r *lifecycleRepo) currentTree(t *testing.T) tree {
	t.Helper()
	files := tree{}
	err := filepath.WalkDir(r.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(r.root, path)
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

// freshProjection indexes the given tree from scratch in its own database.
func freshProjection(t *testing.T, files tree) []string {
	t.Helper()
	return newLifecycleRepo(t, files).projection(t)
}

// assertFreshParity is the whole point: the updated database must project
// exactly what a fresh index of the same final tree projects.
func (r *lifecycleRepo) assertFreshParity(t *testing.T, step string) {
	t.Helper()
	got := r.projection(t)
	want := freshProjection(t, r.currentTree(t))
	if diff := projectionDiff(want, got); diff != "" {
		t.Fatalf("%s: update diverges from a fresh index of the same tree:\n%s", step, diff)
	}
}

func projectionDiff(want, got []string) string {
	inWant := map[string]int{}
	for _, line := range want {
		inWant[line]++
	}
	var only []string
	for _, line := range got {
		if inWant[line] > 0 {
			inWant[line]--
			continue
		}
		only = append(only, "update-only: "+line)
	}
	for line, n := range inWant {
		for i := 0; i < n; i++ {
			only = append(only, "fresh-only:  "+line)
		}
	}
	sort.Strings(only)
	return strings.Join(only, "\n")
}

// edgeState renders one caller's outgoing edge for a dst_name, so a test can
// assert the decision itself rather than only parity.
func (r *lifecycleRepo) edgeState(t *testing.T, srcPath, dstName string) string {
	t.Helper()
	prefix := "edge " + srcPath + ":"
	needle := `-calls-> "` + dstName + `"`
	for _, line := range r.projection(t) {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, needle) {
			return line[strings.Index(line, needle)+len(needle):]
		}
	}
	return "<no edge>"
}

// -- removed declaration ------------------------------------------------------

const pyCaller = "from helpers_a import *\n\n\ndef run():\n    return Foo()\n"

func ambiguousPythonTree() tree {
	return tree{
		"helpers_a.py": "def Foo():\n    return 1\n",
		"helpers_b.py": "def Foo():\n    return 2\n",
		"main.py":      pyCaller,
	}
}

// The reproduction: two declarations make the bare call undecidable, removing
// one makes it decidable again, and an ordinary update must notice.
func TestUpdateReconsidersEdgeAfterCompetingDeclarationRemoved(t *testing.T) {
	r := newLifecycleRepo(t, ambiguousPythonTree())
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("initial state: want unresolved, got %s", got)
	}
	r.assertFreshParity(t, "initial")

	// helpers_b keeps existing; it just stops declaring Foo.
	r.write(t, "helpers_b.py", "def Bar():\n    return 2\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("after removing the competing declaration: want a binding to helpers_a.Foo, got %s", got)
	}
	r.assertFreshParity(t, "after removal")
}

// Removal is not, by itself, evidence that the survivor wins: a third
// declaration keeps the name undecidable.
func TestUpdateKeepsEdgeUnresolvedWhenAnotherDeclarationRemains(t *testing.T) {
	files := ambiguousPythonTree()
	files["helpers_c.py"] = "def Foo():\n    return 3\n"
	r := newLifecycleRepo(t, files)

	r.write(t, "helpers_b.py", "def Bar():\n    return 2\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("two declarations still remain; want unresolved, got %s", got)
	}
	r.assertFreshParity(t, "after removal with a third declaration")
}

// Deleting the whole file must mean the same thing as deleting the declaration,
// on both update shapes: a path-scoped update (MarkFilesDeletedBatch) and a
// whole-tree update scan (MarkMissingDeleted).
func TestUpdateReconsidersEdgeAfterCompetingFileDeleted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string
	}{
		{"whole-tree update scan", nil},
		{"path-scoped update", []string{"helpers_b.py"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, ambiguousPythonTree())
			r.remove(t, "helpers_b.py")
			summary := r.update(t, tc.paths...)

			// The summary has to admit that edge resolution ran: a deletion-only
			// run used to report `test_links`, and a mode that denies the work
			// is how this class stayed invisible.
			if summary.ResolveMode != "paths+names" {
				t.Fatalf("ResolveMode = %q, want paths+names", summary.ResolveMode)
			}
			if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
				t.Fatalf("after deleting the competing file: want a binding to helpers_a.Foo, got %s", got)
			}
			r.assertFreshParity(t, "after file deletion")
		})
	}
}

// A rename removes one name and adds another in the same write. Both sides have
// to reach the invalidation/reconsideration pass.
func TestUpdateCarriesBothSidesOfARename(t *testing.T) {
	files := ambiguousPythonTree()
	// A second caller, for the name the rename introduces. It is unresolved to
	// begin with (nothing declares Bar) and must bind after the rename.
	files["other.py"] = "from helpers_b import *\n\n\ndef go():\n    return Bar()\n"
	r := newLifecycleRepo(t, files)
	if got := r.edgeState(t, "other.py", "Bar"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("initial Bar: want unresolved, got %s", got)
	}

	r.write(t, "helpers_b.py", "def Bar():\n    return 2\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("rename removed the competing Foo; want a binding, got %s", got)
	}
	if got := r.edgeState(t, "other.py", "Bar"); !strings.Contains(got, "helpers_b.py") {
		t.Fatalf("rename added Bar; want a binding, got %s", got)
	}
	r.assertFreshParity(t, "after rename")
}

// The direction P22.8 fixed, kept as a direct regression: P22.12 must not fix
// removal by breaking addition.
func TestUpdateInvalidatesWhenCompetingDeclarationArrives(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"helpers_a.py": "def Foo():\n    return 1\n",
		"main.py":      pyCaller,
	})
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("initial state: want a binding, got %s", got)
	}

	r.write(t, "helpers_b.py", "def Foo():\n    return 2\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("a second declaration arrived; want unresolved, got %s", got)
	}
	r.assertFreshParity(t, "after arrival")
}

// Resolved and unresolved must be able to alternate indefinitely, with no
// sticky state in either direction, entirely through incremental updates.
func TestAmbiguityOscillatesWithFreshParity(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"helpers_a.py": "def Foo():\n    return 1\n",
		"main.py":      pyCaller,
	})
	competing := "def Foo():\n    return 2\n"
	absent := "def Bar():\n    return 2\n"

	for round := 0; round < 3; round++ {
		if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
			t.Fatalf("round %d: one declaration, want a binding, got %s", round, got)
		}
		r.assertFreshParity(t, "one declaration")

		r.write(t, "helpers_b.py", competing)
		r.update(t)
		if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
			t.Fatalf("round %d: two declarations, want unresolved, got %s", round, got)
		}
		r.assertFreshParity(t, "two declarations")

		r.write(t, "helpers_b.py", absent)
		r.update(t)
	}
}

// Deleting and recreating the destination itself, with a competitor appearing
// in between. Every step must match a fresh index of that exact tree.
func TestTargetDeleteAndRecreateLifecycle(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"helpers_a.py": "def Foo():\n    return 1\n",
		"main.py":      pyCaller,
	})
	r.assertFreshParity(t, "initial")

	r.remove(t, "helpers_a.py")
	r.update(t)
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("destination deleted; want unresolved, got %s", got)
	}
	r.assertFreshParity(t, "destination deleted")

	r.write(t, "helpers_b.py", "def Foo():\n    return 2\n")
	r.update(t)
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_b.py") {
		t.Fatalf("a new sole declaration; want a binding to helpers_b.Foo, got %s", got)
	}
	r.assertFreshParity(t, "replacement declared")

	r.write(t, "helpers_a.py", "def Foo():\n    return 1\n")
	r.update(t)
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("both declarations back; want unresolved, got %s", got)
	}
	r.assertFreshParity(t, "both declared")

	r.remove(t, "helpers_b.py")
	r.update(t)
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("one declaration left; want a binding to helpers_a.Foo, got %s", got)
	}
	r.assertFreshParity(t, "back to one")
}

// -- gates that removal must not weaken ---------------------------------------

// P22.6: a bare Go call is answered by its own package. Removing a package-local
// declaration must leave the edge unresolved, never hand it to an unrelated
// package's symbol of the same name.
func TestRemovedGoDeclarationDoesNotEscapePackageScope(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"go.mod":         "module example.com/proj\n",
		"pkga/a.go":      "package pkga\n\nfunc Foo() int { return 1 }\n",
		"pkga/dup.go":    "package pkga\n\nfunc Foo2() int { return Foo() }\n",
		"pkgb/b.go":      "package pkgb\n\nfunc Foo() int { return 2 }\n",
		"pkgb/caller.go": "package pkgb\n\nfunc Call() int { return Foo() }\n",
	})
	r.assertFreshParity(t, "initial")

	// pkgb stops declaring Foo. pkga's Foo is now the repository's only Foo, and
	// must still not be reachable from pkgb.
	r.write(t, "pkgb/b.go", "package pkgb\n\nfunc Bar() int { return 2 }\n")
	r.update(t)

	if got := r.edgeState(t, filepath.ToSlash(filepath.Join("pkgb", "caller.go")), "Foo"); strings.Contains(got, "pkga") {
		t.Fatalf("a removed package-local Foo let pkgb bind pkga's: %s", got)
	}
	r.assertFreshParity(t, "after package-local removal")
}

// P2/P22.7: an old name is a trigger key, not permission to widen candidate
// semantics. Removing a Python Foo must not wake a Go Foo for a Python caller.
func TestRemovedNameDoesNotCrossLanguages(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"go.mod":       "module example.com/proj\n",
		"pkga/a.go":    "package pkga\n\nfunc Foo() int { return 1 }\n",
		"helpers_a.py": "def Foo():\n    return 1\n",
		"helpers_b.py": "def Foo():\n    return 2\n",
		"main.py":      pyCaller,
	})
	r.write(t, "helpers_b.py", "def Bar():\n    return 2\n")
	r.update(t)

	got := r.edgeState(t, "main.py", "Foo")
	if strings.Contains(got, "pkga") {
		t.Fatalf("a Python call bound a Go symbol after a removal: %s", got)
	}
	r.assertFreshParity(t, "after cross-language removal")
}

// P22.11: a receiver-qualified C++ spelling matches nothing at any evidence
// level, and a removal that makes a bare `size` unique must not change that.
func TestRemovedDeclarationDoesNotWakeCppReceiverCall(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a.cc":      "struct Holder {\n  int size() const { return 1; }\n};\n",
		"b.cc":      "struct Other {\n  int size() const { return 2; }\n};\n",
		"caller.cc": "#include <vector>\nint use(const std::vector<int>& v) {\n  return v.size();\n}\n",
	})
	r.assertFreshParity(t, "initial")

	r.write(t, "b.cc", "struct Other {\n  int capacity() const { return 2; }\n};\n")
	r.update(t)

	if got := r.edgeState(t, "caller.cc", "v.size"); strings.Contains(got, ".cc:") {
		t.Fatalf("a receiver-qualified call bound a project symbol after a removal: %s", got)
	}
	r.assertFreshParity(t, "after removal")
}

// P7: reconsideration goes through the resolver, so the production/test
// preference still applies to a name a removal just woke up.
func TestRemovedDeclarationKeepsTestShadowPreference(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"helpers_a.py":      "def Foo():\n    return 1\n",
		"helpers_b.py":      "def Foo():\n    return 2\n",
		"test_helpers.py":   "def Foo():\n    return 3\n",
		"main.py":           pyCaller,
		"tests/test_use.py": "from helpers_a import *\n\n\ndef test_run():\n    return Foo()\n",
	})
	r.write(t, "helpers_b.py", "def Bar():\n    return 2\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("a production caller facing one production and one test Foo must bind the production one, got %s", got)
	}
	if got := r.edgeState(t, filepath.ToSlash(filepath.Join("tests", "test_use.py")), "Foo"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("a test caller facing two candidates must stay unresolved, got %s", got)
	}
	r.assertFreshParity(t, "after removal with a test declaration")
}

// P22.9: uniqueness and visibility are separate questions. Removing a competing
// class declaration makes the name unique, but a caller that neither declares
// nor imports the survivor still may not bind it.
func TestRemovedDeclarationDoesNotBypassTypeScope(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"models_a.py": "class Widget:\n    pass\n",
		"models_b.py": "class Widget:\n    pass\n",
		// No import of either module: repository-global uniqueness is not scope
		// evidence for a class name.
		"main.py": "def run():\n    return Widget()\n",
	})
	r.assertFreshParity(t, "initial")

	r.write(t, "models_b.py", "class Gadget:\n    pass\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Widget"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("an unimported class bound after a removal made it unique: %s", got)
	}
	r.assertFreshParity(t, "after removal")
}

// The positive control for the test above: with the import present, the removal
// is exactly what makes the edge decidable, and the update must notice.
func TestRemovedDeclarationResolvesImportedTypeTarget(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"models_a.py": "class Widget:\n    pass\n",
		"models_b.py": "class Widget:\n    pass\n",
		"main.py":     "from models_a import Widget\n\n\ndef run():\n    return Widget()\n",
	})
	if got := r.edgeState(t, "main.py", "Widget"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("initial state: want unresolved, got %s", got)
	}

	r.write(t, "models_b.py", "class Gadget:\n    pass\n")
	r.update(t)

	if got := r.edgeState(t, "main.py", "Widget"); !strings.Contains(got, "models_a.py") {
		t.Fatalf("an imported, now-unique class must bind: %s", got)
	}
	r.assertFreshParity(t, "after removal")
}

// A run whose only change is a deletion has no changed path to dispatch on. It
// must still reconsider, on the full-index shape as well as the update shapes --
// otherwise `codegraph index` over a tree that only lost a file would leave the
// graph where the deletion found it.
func TestFullIndexAfterDeletionOnlyStillReconsiders(t *testing.T) {
	r := newLifecycleRepo(t, ambiguousPythonTree())
	r.remove(t, "helpers_b.py")
	if _, err := r.idx.Index(r.ctx, Options{RepoRoot: r.root, ScanKind: "index"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	if got := r.edgeState(t, "main.py", "Foo"); !strings.Contains(got, "helpers_a.py") {
		t.Fatalf("deletion-only full index: want a binding to helpers_a.Foo, got %s", got)
	}
	r.assertFreshParity(t, "deletion-only full index")
}

func TestJavaDotSuffixFreshParityAcrossTargetStates(t *testing.T) {
	const caller = "class Caller { void run() { A.B.C.m(); } }\n"
	const target = "class A { class B { class C { static void m() {} } } }\n"
	const renamed = "class X { class Y { class Z { static void m() {} } } }\n"

	r := newLifecycleRepo(t, tree{"Caller.java": caller, "Target.java": target})
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); !strings.Contains(got, "Target.java") || !strings.Contains(got, "dot_suffix/low") {
		t.Fatalf("unique dot-suffix state: %s", got)
	}
	r.assertFreshParity(t, "unique")

	r.write(t, "Competitor.java", target)
	r.update(t, "Competitor.java")
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); !strings.Contains(got, ":: [/]") {
		t.Fatalf("competitor should make dot-suffix ambiguous: %s", got)
	}
	r.assertFreshParity(t, "competitor added")

	r.remove(t, "Competitor.java")
	r.update(t, "Competitor.java")
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); !strings.Contains(got, "Target.java") || !strings.Contains(got, "dot_suffix/low") {
		t.Fatalf("competitor removal should recover dot-suffix: %s", got)
	}
	r.assertFreshParity(t, "competitor removed")

	r.remove(t, "Target.java")
	r.update(t, "Target.java")
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); strings.Contains(got, "Target.java") {
		t.Fatalf("deleted target survived: %s", got)
	}
	r.assertFreshParity(t, "target deleted")

	r.write(t, "Target.java", target)
	r.update(t, "Target.java")
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); !strings.Contains(got, "Target.java") {
		t.Fatalf("target recreation should restore dot-suffix: %s", got)
	}

	r.write(t, "Target.java", renamed)
	r.update(t, "Target.java")
	if got := r.edgeState(t, "Caller.java", "A.B.C.m"); strings.Contains(got, "Target.java") {
		t.Fatalf("renamed target retained old binding: %s", got)
	}
	r.assertFreshParity(t, "target renamed")
}

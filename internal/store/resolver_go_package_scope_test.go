package store_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
	"github.com/isink17/codegraph/internal/store"
)

// Regression coverage for P22.6: a bare call spelling in Go names something in
// the calling file's own package, or nothing.
//
// Every fixture here drives the real parser and the real resolver
// (reindexFixture, see reindex_stale_edge_targets_test.go), so the package
// clause under test is the one the adapter actually read -- not a hand-written
// qualified_name that happens to look like one.

// goScopeProjection is the semantic answer these tests compare: which call
// spelling in which file reached which symbol, by name. No row ids, so an
// identical graph built in a different order projects identically.
func goScopeProjection(t *testing.T, f *reindexFixture) []string {
	t.Helper()
	var out []string
	for _, e := range f.edges() {
		target := "<unresolved>"
		if e.DstSymbolID != nil {
			target = e.DstQualifiedName
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s", e.FilePath, e.DstName, target, e.ResolutionStrategy))
	}
	sort.Strings(out)
	return out
}

func callerNamesOf(t *testing.T, f *reindexFixture, symbol string) []string {
	t.Helper()
	syms, err := f.store.FindCallers(f.ctx, f.repoID, symbol, 0, 100, 0)
	if err != nil {
		t.Fatalf("FindCallers(%q) error = %v", symbol, err)
	}
	return qualifiedNamesOf(syms)
}

func calleeNamesOf(t *testing.T, f *reindexFixture, symbol string) []string {
	t.Helper()
	syms, err := f.store.FindCallees(f.ctx, f.repoID, symbol, 0, 100, 0)
	if err != nil {
		t.Fatalf("FindCallees(%q) error = %v", symbol, err)
	}
	return qualifiedNamesOf(syms)
}

func qualifiedNamesOf(syms []graph.Symbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.QualifiedName)
	}
	sort.Strings(out)
	return out
}

func contextCallerNamesOf(t *testing.T, f *reindexFixture, symbol string) []string {
	t.Helper()
	id := symbolIDOf(t, f, symbol)
	short := store.LookupSymbolShortName(symbol)
	got, err := f.store.FindContextNeighbors(f.ctx, f.repoID, []store.ContextSeed{{
		SymbolID:           id,
		QualifiedName:      symbol,
		ShortName:          short,
		AllowShortEvidence: true,
	}}, 50)
	if err != nil {
		t.Fatalf("FindContextNeighbors(%q) error = %v", symbol, err)
	}
	var out []string
	for _, sym := range got[0].Callers {
		out = append(out, sym.QualifiedName)
	}
	sort.Strings(out)
	return out
}

func symbolIDOf(t *testing.T, f *reindexFixture, qualified string) int64 {
	t.Helper()
	// FindSymbol, not SearchSymbols: the latter is FTS-backed and a short name
	// that the tokenizer does not index would make the helper fail for a reason
	// that has nothing to do with what the test is asserting.
	syms, err := f.store.FindSymbol(f.ctx, f.repoID, store.LookupSymbolShortName(qualified), 200, 0)
	if err != nil {
		t.Fatalf("FindSymbol(%q) error = %v", qualified, err)
	}
	for _, sym := range syms {
		if sym.QualifiedName == qualified {
			return sym.ID
		}
	}
	t.Fatalf("symbol %q not found in %d candidates", qualified, len(syms))
	return 0
}

func assertNoCaller(t *testing.T, got []string, unwanted string, what string) {
	t.Helper()
	for _, name := range got {
		if name == unwanted {
			t.Fatalf("%s: %q present in %v, want absent", what, unwanted, got)
		}
	}
}

// -- positive controls: what a bare Go call legitimately reaches -------------

// A bare call to a declaration in the same file is the simplest valid case and
// must keep resolving.
func TestGoBareCallResolvesWithinSameFile(t *testing.T) {
	f := newReindexFixture(t)
	f.write("a.go", "package main\n\nfunc Helper() {}\n\nfunc Caller() { Helper() }\n")
	f.index()

	assertResolved(t, f.edge("a.go", "Helper"), "main.Helper")
}

// Cross-file within one package is valid Go and carries real recall: it is what
// most same-package calls in a real repository look like.
func TestGoBareCallResolvesAcrossFilesInSamePackage(t *testing.T) {
	f := newReindexFixture(t)
	f.write("one.go", "package main\n\nfunc Helper() {}\n")
	f.write("two.go", "package main\n\nfunc Caller() { Helper() }\n")
	f.index()

	assertResolved(t, f.edge("two.go", "Helper"), "main.Helper")
	if got := callerNamesOf(t, f, "main.Helper"); !equalStringSlices(got, []string{"main.Caller"}) {
		t.Fatalf("FindCallers(main.Helper) = %v, want [main.Caller]", got)
	}
}

// -- negative controls: what a bare Go call must never reach -----------------

// The class google/pprof exposed: an unexported name in another package.
func TestGoBareCallDoesNotBindUnexportedForeignPackage(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("a")
	f.mkdir("b")
	f.write("a/caller.go", "package a\n\nfunc Caller() { countTags() }\n")
	f.write("b/graph.go", "package b\n\nfunc countTags() int { return 0 }\n")
	f.index()

	assertUnbound(t, f.edge("a/caller.go", "countTags"))
	assertNoCaller(t, callerNamesOf(t, f, "b.countTags"), "a.Caller", "FindCallers")
	assertNoCaller(t, calleeNamesOf(t, f, "a.Caller"), "b.countTags", "FindCallees")
	assertNoCaller(t, contextCallerNamesOf(t, f, "b.countTags"), "a.Caller", "context callers")
}

// Being exported does not make a name visible as a bare identifier: Go spells
// that call `b.NewThing()`. This is what makes the rule cover all cross-package
// bare calls rather than only unexported ones.
func TestGoBareCallDoesNotBindExportedForeignPackage(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("a")
	f.mkdir("b")
	f.write("a/caller.go", "package a\n\nfunc Caller() { NewThing() }\n")
	f.write("b/thing.go", "package b\n\nfunc NewThing() {}\n")
	f.index()

	assertUnbound(t, f.edge("a/caller.go", "NewThing"))
	assertNoCaller(t, callerNamesOf(t, f, "b.NewThing"), "a.Caller", "FindCallers")
}

// Even a name that is unique in the whole repository is not reachable from
// another package. Global uniqueness is not a visibility fact.
func TestGoBareCallGlobalUniquenessIsNotEvidence(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("a")
	f.mkdir("b")
	f.write("a/caller.go", "package a\n\nfunc Caller() { UniqueName() }\n")
	f.write("b/unique.go", "package b\n\nfunc UniqueName() {}\n")
	f.index()

	edge := f.edge("a/caller.go", "UniqueName")
	assertUnbound(t, edge)
	assertNoCaller(t, callerNamesOf(t, f, "b.UniqueName"), "a.Caller", "FindCallers")
}

// The google/pprof shape exactly: the callee is a local closure the parser
// emits no symbol for. An unmodelled local declaration must leave the call
// unattributed, never attributed to an unrelated project symbol.
func TestGoBareCallFromLocalClosureDoesNotBindForeignSymbol(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("profile")
	f.mkdir("graph")
	f.write("profile/filter_test.go", "package profile\n\nimport \"testing\"\n\n"+
		"func TestTagFilter(t *testing.T) {\n"+
		"\tcountTags := func() int { return 0 }\n"+
		"\tif countTags() != 0 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n")
	f.write("graph/graph.go", "package graph\n\nfunc countTags() int { return 0 }\n\n"+
		"func Select() int { return countTags() }\n")
	f.index()

	assertUnbound(t, f.edge("profile/filter_test.go", "countTags"))
	// The genuine same-package call in graph.go is untouched.
	assertResolved(t, f.edge("graph/graph.go", "countTags"), "graph.countTags")
	assertNoCaller(t, callerNamesOf(t, f, "graph.countTags"), "profile.TestTagFilter", "FindCallers")

	// And the false relation no longer reaches related tests.
	tests, err := f.store.RelatedTests(f.ctx, f.repoID, "", "graph/graph.go", 20, 0)
	if err != nil {
		t.Fatalf("RelatedTests() error = %v", err)
	}
	for _, rt := range tests {
		if strings.HasPrefix(rt.File, "profile/") {
			t.Fatalf("related tests for graph/graph.go = %v, want no profile/ test", tests)
		}
	}
}

// A bare spelling is never a method call in Go: methods are spelled through a
// receiver. Package-scope filtering must not re-admit them.
func TestGoBareCallDoesNotBindMethod(t *testing.T) {
	f := newReindexFixture(t)
	// A value receiver, deliberately: the go/ast adapter spells a pointer
	// receiver `main.*App.Close` while the tree-sitter one strips the star, and
	// a qualified name that differs per build would make the caller assertions
	// below pass vacuously on one of them.
	f.write("app.go", "package main\n\ntype App struct{}\n\nfunc (a App) Close() error { return nil }\n")
	f.write("caller.go", "package main\n\nfunc Caller() { Close() }\n")
	f.index()

	assertUnbound(t, f.edge("caller.go", "Close"))
	assertNoCaller(t, callerNamesOf(t, f, "main.App.Close"), "main.Caller", "FindCallers")
	assertNoCaller(t, calleeNamesOf(t, f, "main.Caller"), "main.App.Close", "FindCallees")
}

// A predeclared builtin must not bind a same-named project symbol in another
// package. (Inside the declaring package Go itself shadows the builtin, so a
// same-package bind there is correct and is not what this pins.)
func TestGoBuiltinBareCallDoesNotBindForeignProjectSymbol(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("a")
	f.mkdir("b")
	f.write("a/caller.go", "package a\n\nfunc Caller(xs []int) int { return len(xs) }\n")
	f.write("b/shadow.go", "package b\n\nfunc len(xs []int) int { return 0 }\n")
	f.index()

	assertUnbound(t, f.edge("a/caller.go", "len"))
	assertNoCaller(t, callerNamesOf(t, f, "b.len"), "a.Caller", "FindCallers")
	assertNoCaller(t, calleeNamesOf(t, f, "a.Caller"), "b.len", "FindCallees")
}

// The Go package rule (P22.6) and the language rule (P22.7) are independent,
// and each refuses a different writer of the same bare spelling. The Go writer
// in another package is refused because Go does not resolve a bare name that
// way; the Python writer is refused because a Python call does not name a Go
// declaration at all. What survives is the writer both rules allow -- the same
// package, the same language -- on all three surfaces.
//
// Before P22.7 this test asserted the opposite for the Python writer, because
// the shared name cascade carried no language gate: P22.6 deliberately left
// that class alone and recorded it as its own finding.
func TestGoBareScopeAndLanguageGateRefuseDifferentWriters(t *testing.T) {
	f := newPolyglotFixture(t)
	f.mkdir("a")
	f.mkdir("graph")
	f.mkdir("py")
	f.write("a/caller.go", "package a\n\nfunc Caller() { countTags() }\n")
	f.write("graph/graph.go", "package graph\n\nfunc countTags() int { return 0 }\n\n"+
		"func Select() int { return countTags() }\n")
	f.write("py/handler.py", "def handler():\n    return countTags()\n")
	f.index()

	assertUnbound(t, f.edge("a/caller.go", "countTags"))
	assertUnbound(t, f.edge("py/handler.py", "countTags"))

	// graph.Select survives, but on the BOUND leg: its same-package call is
	// resolved by go_package_scope, so it never reaches the name-evidence leg.
	// The positive for the gated name leg itself lives in
	// TestFindCallersBareNameStaysInTargetLanguage and
	// TestFindContextNeighborsStaysInSeedLanguage, whose Go caller edge is
	// deliberately left unresolved.
	callers := callerNamesOf(t, f, "graph.countTags")
	assertNoCaller(t, callers, "a.Caller", "FindCallers")
	assertNoCaller(t, callers, "handler.handler", "FindCallers")
	if !containsName(callers, "graph.Select") {
		t.Fatalf("FindCallers(graph.countTags) = %v, want the same-package Go caller kept", callers)
	}
	ctxCallers := contextCallerNamesOf(t, f, "graph.countTags")
	assertNoCaller(t, ctxCallers, "a.Caller", "context callers")
	assertNoCaller(t, ctxCallers, "handler.handler", "context callers")
	if !containsName(ctxCallers, "graph.Select") {
		t.Fatalf("context callers of graph.countTags = %v, want the same-package Go caller kept "+
			"(FindCallers returned %v)", ctxCallers, callers)
	}
	callees := calleeNamesOf(t, f, "handler.handler")
	assertNoCaller(t, callees, "graph.countTags", "FindCallees")
}

// A method seed's short-name leg is gated but not deleted: the batched pipeline
// must keep whatever writers both rules allow. No bare GO spelling names a Go
// method (P22.6), and since P22.7 no foreign-language spelling names it either,
// so a Go method seed's leg is legitimately empty -- while a Python method seed
// keeps its Python writer through the same leg.
func TestMethodSeedShortNameLegKeepsSameLanguageWriters(t *testing.T) {
	f := newPolyglotFixture(t)
	f.mkdir("cli")
	f.mkdir("py")
	f.write("cli/app.go", "package cli\n\ntype App struct{}\n\nfunc (a App) Close() error { return nil }\n")
	// Two python classes declare Close, so the resolver refuses to bind the
	// bare call and the short-name evidence leg is what the assertions below
	// actually exercise.
	f.write("py/app.py", "class App:\n    def Close(self):\n        return 0\n\n"+
		"class Other:\n    def Close(self):\n        return 1\n")
	f.write("py/shutdown.py", "def shutdown():\n    return Close()\n")
	f.index()

	assertUnbound(t, f.edge("py/shutdown.py", "Close"))

	goCallers := callerNamesOf(t, f, "cli.App.Close")
	assertNoCaller(t, goCallers, "shutdown.shutdown", "FindCallers")
	goCtx := contextCallerNamesOf(t, f, "cli.App.Close")
	assertNoCaller(t, goCtx, "shutdown.shutdown", "context callers")

	pyCallers := callerNamesOf(t, f, "app.App.Close")
	if !containsName(pyCallers, "shutdown.shutdown") {
		t.Fatalf("FindCallers(app.App.Close) = %v, want the python caller kept", pyCallers)
	}
	pyCtx := contextCallerNamesOf(t, f, "app.App.Close")
	if !containsName(pyCtx, "shutdown.shutdown") {
		t.Fatalf("context callers of app.App.Close = %v, want the python caller kept "+
			"(FindCallers returned %v)", pyCtx, pyCallers)
	}
}

// The positive control for the batched caller path: a same-package caller must
// still arrive through the scoped short-name leg, not only through a resolved
// edge. The unresolved spelling is the one under test, so the fixture gives the
// caller no bindable target of its own.
func TestGoBareScopeKeepsSamePackageContextCaller(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("pkg")
	f.write("pkg/target.go", "package pkg\n\nfunc Target() {}\n")
	f.write("pkg/caller.go", "package pkg\n\nfunc Caller() { Target() }\n")
	f.index()

	if got := contextCallerNamesOf(t, f, "pkg.Target"); !containsName(got, "pkg.Caller") {
		t.Fatalf("context callers of pkg.Target = %v, want pkg.Caller", got)
	}
}

// FindCallers on a bare name that indexes no symbol at all cannot prove the
// target is a package-level Go declaration, so Go writers of that spelling are
// not claimed. This is a deliberate recall change on a public surface; it is
// pinned so it cannot drift back silently.
func TestGoBareScopeUnknownTargetClaimsNoGoCaller(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("a")
	f.write("a/caller.go", "package a\n\nfunc Caller() { neverDeclared() }\n")
	f.index()

	if got := callerNamesOf(t, f, "neverDeclared"); len(got) != 0 {
		t.Fatalf("FindCallers(neverDeclared) = %v, want none", got)
	}
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// One batch, two seeds, one spelling: a bare `helper` written by a Go symbol is
// that symbol's own package's business, while the same spelling written by a
// Python symbol is not governed by this rule at all. Each seed must get its own
// answer -- and in particular the Go seed's gate must not consume the name on
// the Python seed's behalf.
func TestGoBareScopeKeepsPerSeedCalleeAnswers(t *testing.T) {
	f := newPolyglotFixture(t)
	f.mkdir("a")
	f.mkdir("py")
	// A Go seed whose bare callee its own package declares twice -- the shape
	// build-tagged files produce. The resolver refuses to pick one, so the edge
	// stays unresolved and reaches the callee name fallback, where the scoped
	// lookup answers it with both same-package declarations.
	f.write("a/caller.go", "package a\n\nfunc Caller() { helper() }\n")
	f.write("a/helper_linux.go", "//go:build linux\n\npackage a\n\nfunc helper() int { return 1 }\n")
	f.write("a/helper_darwin.go", "//go:build darwin\n\npackage a\n\nfunc helper() int { return 2 }\n")
	// A Python seed writing the same spelling. Two definitions keep that edge
	// unresolved too, so it reaches the same fallback by the other route.
	f.write("py/mod.py", "def driver():\n    return helper()\n")
	f.write("py/one.py", "def helper():\n    return 1\n")
	f.write("py/two.py", "def helper():\n    return 2\n")
	f.index()

	// The premise: neither edge is bound, so both seeds genuinely depend on the
	// callee name fallback.
	assertUnbound(t, f.edge("a/caller.go", "helper"))
	assertUnbound(t, f.edge("py/mod.py", "helper"))

	goSeed := contextSeedFor(t, f, "a.Caller")
	pySeed := contextSeedFor(t, f, "mod.driver")
	got, err := f.store.FindContextNeighbors(f.ctx, f.repoID, []store.ContextSeed{goSeed, pySeed}, 20)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}

	goCallees := calleeNamesIn(got[0])
	if !containsName(goCallees, "a.helper") {
		t.Fatalf("a.Caller callees = %v, want its own package's helper", goCallees)
	}
	for _, name := range goCallees {
		if name == "one.helper" || name == "two.helper" {
			t.Fatalf("a.Caller callees = %v: a bare Go call reached a python symbol", goCallees)
		}
	}
	pyCallees := calleeNamesIn(got[1])
	if !containsName(pyCallees, "one.helper") || !containsName(pyCallees, "two.helper") {
		t.Fatalf("mod.driver callees = %v, want both python helpers -- the Go seed's "+
			"package gate must not consume another seed's name evidence", pyCallees)
	}
	// P22.7: the python seed no longer picks up the Go `a.helper` either. Its
	// spelling reaches the shared cascade tagged with the seed's own persisted
	// language, so stage-2 equality can only answer it with python symbols.
	for _, name := range pyCallees {
		if name == "a.helper" {
			t.Fatalf("mod.driver callees = %v: a python seed reached a Go symbol by name", pyCallees)
		}
	}
}

func contextSeedFor(t *testing.T, f *reindexFixture, qualified string) store.ContextSeed {
	t.Helper()
	return store.ContextSeed{
		SymbolID:           symbolIDOf(t, f, qualified),
		QualifiedName:      qualified,
		ShortName:          store.LookupSymbolShortName(qualified),
		AllowShortEvidence: true,
	}
}

func calleeNamesIn(n store.ContextNeighbors) []string {
	out := make([]string, 0, len(n.Callees))
	for _, sym := range n.Callees {
		out = append(out, sym.QualifiedName)
	}
	sort.Strings(out)
	return out
}

// -- test packages ----------------------------------------------------------

// `package pkg_test` is a different package from `package pkg` even though it
// shares a directory, so it has no bare access to production declarations --
// exported or not. Directory equality alone would fabricate both edges here.
func TestGoExternalTestPackageHasNoBareAccessToProduction(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("pkg")
	f.write("pkg/pkg.go", "package pkg\n\nfunc hidden() {}\n\nfunc Exported() {}\n")
	f.write("pkg/pkg_test.go", "package pkg_test\n\nimport \"testing\"\n\n"+
		"func TestExternal(t *testing.T) {\n\thidden()\n\tExported()\n}\n")
	f.index()

	assertUnbound(t, f.edge("pkg/pkg_test.go", "hidden"))
	assertUnbound(t, f.edge("pkg/pkg_test.go", "Exported"))
	assertNoCaller(t, callerNamesOf(t, f, "pkg.hidden"), "pkg_test.TestExternal", "FindCallers")
	assertNoCaller(t, callerNamesOf(t, f, "pkg.Exported"), "pkg_test.TestExternal", "FindCallers")
}

// The internal-test counterpart: `package pkg` in a _test.go file IS the
// production package, so its bare call is valid and must keep resolving. This
// is the pair that proves the rule reads the package clause and not the path.
func TestGoInternalTestPackageKeepsBareAccessToProduction(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("pkg")
	f.write("pkg/pkg.go", "package pkg\n\nfunc hidden() {}\n")
	f.write("pkg/pkg_test.go", "package pkg\n\nimport \"testing\"\n\n"+
		"func TestInternal(t *testing.T) {\n\thidden()\n}\n")
	f.index()

	assertResolved(t, f.edge("pkg/pkg_test.go", "hidden"), "pkg.hidden")
}

// -- dot import -------------------------------------------------------------

// `file_imports` persists only the import path -- the parsers drop the `.`
// import name -- so nothing in the database distinguishes a dot import from a
// plain one. Binding here would be a guess, so the edge stays unresolved and
// the limitation is pinned rather than papered over.
func TestGoDotImportStaysUnresolvedWithoutPersistedEvidence(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("app")
	f.mkdir("util")
	f.write("go.mod", "module example.com/project\n\ngo 1.22\n")
	f.write("util/util.go", "package util\n\nfunc Helper() {}\n")
	f.write("app/app.go", "package app\n\nimport . \"example.com/project/util\"\n\nfunc Caller() { Helper() }\n")
	f.index()

	assertUnbound(t, f.edge("app/app.go", "Helper"))
}

// -- strong-evidence veto ---------------------------------------------------

// Package scope vetoes the weaker repo-global fallbacks: the caller's package
// holding no eligible declaration is an answer, not a reason to look elsewhere.
func TestGoBarePackageScopeVetoesGlobalFallback(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("a")
	f.mkdir("b")
	// Package a has several declarations, none named Foo.
	f.write("a/caller.go", "package a\n\nfunc Bar() {}\n\nfunc Caller() { Foo() }\n")
	// Package b holds the repository's only Foo, at package level, exported.
	f.write("b/foo.go", "package b\n\nfunc Foo() {}\n")
	f.index()

	edge := f.edge("a/caller.go", "Foo")
	assertUnbound(t, edge)
	if edge.ResolutionStrategy != "" || edge.ResolutionConfidence != "" {
		t.Fatalf("vetoed edge kept metadata (%q, %q), want both empty",
			edge.ResolutionStrategy, edge.ResolutionConfidence)
	}
}

// -- entrypoint agreement ---------------------------------------------------

// The repo-wide resolver, the path-scoped resolver and the name-targeted
// resolver must refuse and accept the same edges. A divergence here is the
// full/incremental parity defect in a new place.
func TestGoBareScopeAgreesAcrossResolverEntrypoints(t *testing.T) {
	build := func(f *reindexFixture) {
		f.mkdir("a")
		f.mkdir("b")
		f.write("a/one.go", "package a\n\nfunc Helper() {}\n")
		f.write("a/two.go", "package a\n\nfunc Caller() { Helper() }\n")
		f.write("b/other.go", "package b\n\nfunc Foreign() { Helper() }\n")
	}

	resolvers := []struct {
		name string
		run  func(t *testing.T, f *reindexFixture)
	}{
		{"repo", func(t *testing.T, f *reindexFixture) {
			if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
				t.Fatalf("ResolveEdges() error = %v", err)
			}
		}},
		{"paths", func(t *testing.T, f *reindexFixture) {
			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"a/two.go", "b/other.go"}); err != nil {
				t.Fatalf("ResolveEdgesForPaths() error = %v", err)
			}
		}},
		{"names", func(t *testing.T, f *reindexFixture) {
			if _, err := f.store.ResolveEdgesForNames(f.ctx, f.repoID, []string{"Helper"}); err != nil {
				t.Fatalf("ResolveEdgesForNames() error = %v", err)
			}
		}},
	}

	for _, resolver := range resolvers {
		t.Run(resolver.name, func(t *testing.T) {
			f := newReindexFixture(t)
			build(f)
			f.index()
			f.clearResolutions(t)
			resolver.run(t, f)

			assertResolved(t, f.edge("a/two.go", "Helper"), "a.Helper")
			assertUnbound(t, f.edge("b/other.go", "Helper"))
		})
	}
}

// -- lifecycle --------------------------------------------------------------

// A same-package target that appears resolves, and when it disappears the edge
// unresolves instead of falling through to the foreign-package declaration that
// has been there the whole time.
func TestGoBareScopeTargetLifecycle(t *testing.T) {
	f := newReindexFixture(t)
	f.mkdir("a")
	f.mkdir("b")
	f.write("b/foo.go", "package b\n\nfunc Foo() {}\n")
	f.write("a/caller.go", "package a\n\nfunc Caller() { Foo() }\n")
	f.index()

	// Only the foreign candidate exists: unresolved.
	assertUnbound(t, f.edge("a/caller.go", "Foo"))

	// The caller's own package gains a Foo: resolves, to its own package's.
	f.write("a/foo.go", "package a\n\nfunc Foo() {}\n")
	f.update()
	assertResolved(t, f.edge("a/caller.go", "Foo"), "a.Foo")
	f.assertNoDangling()

	// It goes away again: back to unresolved, never to b.Foo.
	f.remove("a/foo.go")
	f.update()
	f.assertNoDangling()
	assertUnbound(t, f.edge("a/caller.go", "Foo"))

	// And it comes back.
	f.write("a/foo.go", "package a\n\nfunc Foo() {}\n")
	f.update()
	assertResolved(t, f.edge("a/caller.go", "Foo"), "a.Foo")
	f.assertNoDangling()
}

// -- order independence -----------------------------------------------------

// The same final tree must project identically whichever package was indexed
// first, and whether it was reached by a full index or by incremental updates.
func TestGoBareScopeIsIndependentOfBuildOrder(t *testing.T) {
	files := map[string]string{
		"a/one.go":   "package a\n\nfunc Helper() {}\n",
		"a/two.go":   "package a\n\nfunc Caller() { Helper() }\n",
		"b/other.go": "package b\n\nfunc Foreign() { Helper() }\n",
		"b/own.go":   "package b\n\nfunc Helper() {}\n",
	}
	orders := map[string][][]string{
		"full":            {{"a/one.go", "a/two.go", "b/other.go", "b/own.go"}},
		"package_a_first": {{"a/one.go", "a/two.go"}, {"b/other.go", "b/own.go"}},
		"package_b_first": {{"b/other.go", "b/own.go"}, {"a/one.go", "a/two.go"}},
		"interleaved":     {{"a/one.go", "b/other.go"}, {"b/own.go"}, {"a/two.go"}},
	}

	var want []string
	names := make([]string, 0, len(orders))
	for name := range orders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		steps := orders[name]
		f := newReindexFixture(t)
		f.mkdir("a")
		f.mkdir("b")
		for i, step := range steps {
			for _, rel := range step {
				f.write(rel, files[rel])
			}
			if i == 0 {
				f.index()
			} else {
				f.update()
			}
		}
		got := goScopeProjection(t, f)
		if want == nil {
			want = got
			continue
		}
		if !equalStringSlices(got, want) {
			t.Fatalf("build order %q projected\n%v\nwant\n%v", name, got, want)
		}
	}
	// Sanity: the projection actually contains the two decisions under test.
	joined := strings.Join(want, "\n")
	if !strings.Contains(joined, "a/two.go|Helper|a.Helper|") {
		t.Fatalf("projection missing the same-package bind:\n%s", joined)
	}
	if !strings.Contains(joined, "b/other.go|Helper|b.Helper|") {
		t.Fatalf("projection missing package b's own bind:\n%s", joined)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mkdir creates a directory inside the fixture repository, so a fixture can
// place files in real Go package directories.
func (f *reindexFixture) mkdir(rel string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.repoRoot, rel), 0o755); err != nil {
		f.t.Fatalf("MkdirAll(%q) error = %v", rel, err)
	}
}

// clearResolutions unbinds every edge, so one indexed tree can be handed to
// each resolver entrypoint from the same starting state.
func (f *reindexFixture) clearResolutions(t *testing.T) {
	t.Helper()
	if err := f.store.ClearEdgeResolutionsForTest(f.ctx, f.repoID); err != nil {
		t.Fatalf("clear resolutions: %v", err)
	}
}

// newPolyglotFixture is newReindexFixture with the Python adapter registered as
// well, so a fixture can show that the Go rule leaves other languages alone.
// The Go-only fixture cannot: it never parses the .py file, so "the Python
// caller is missing" would pass for the wrong reason.
func newPolyglotFixture(t *testing.T) *reindexFixture {
	t.Helper()
	f := newReindexFixture(t)
	f.idx = indexer.New(f.store, parser.NewRegistry(goparser.New(), pyparser.New()), nil)
	return f
}

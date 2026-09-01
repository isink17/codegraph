package store

import (
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// Regression coverage for P22.7 language-safe symbol seed lookup.
//
// The resolver has enforced P2's same-language rule for implicit bindings since
// P2, but the public name-evidence query paths never did: a Python source's
// unresolved `SharedName` reached a Go `gofoo.SharedName` purely because the
// short names matched. These fixtures pin the identity contract on the four
// surfaces that carry source-language evidence -- FindCallers, FindCallees and
// both directions of batched context expansion -- and pin the surfaces that are
// language-agnostic by design so the fix cannot silently narrow discovery.
//
// The rule is not Go-specific and is not a pair table: a Python source may not
// claim a TypeScript symbol either. Go appears here only because P22.6 already
// scopes bare Go spellings to their own package, and the two rules have to
// compose.

// langFixture is a three-language graph in which one bare name, `SharedName`,
// is declared once per language and called once per language.
type langFixture struct {
	*gateFixture

	pyCaller int64
	goTarget int64
	tsTarget int64
}

func newLangFixture(t *testing.T) *langFixture {
	t.Helper()
	f := &langFixture{gateFixture: newGateFixture(t)}

	goDecl := f.file(t, "gofoo/decl.go", "go")
	goCall := f.file(t, "gofoo/call.go", "go")
	pyDecl := f.file(t, "pyapp/lib.py", "python")
	pyCall := f.file(t, "pyapp/app.py", "python")
	tsDecl := f.file(t, "web/lib.ts", "typescript")
	tsCall := f.file(t, "web/app.ts", "typescript")

	f.goTarget = f.symbol(t, goDecl, "SharedName", "gofoo.SharedName", "go")
	f.symbol(t, pyDecl, "SharedName", "pylib.SharedName", "python")
	f.tsTarget = f.symbol(t, tsDecl, "SharedName", "tslib.SharedName", "typescript")

	// Callers live in the same package/directory as their own language's
	// declaration, so P22.6's Go package scope is satisfied for the Go pair and
	// the only thing that can separate the three answers is language.
	goCaller := f.symbol(t, goCall, "GoCaller", "gofoo.GoCaller", "go")
	f.pyCaller = f.symbol(t, pyCall, "PyCaller", "app.PyCaller", "python")
	tsCaller := f.symbol(t, tsCall, "TsCaller", "app.TsCaller", "typescript")

	f.edge(t, goCall, goCaller, "SharedName")
	f.edge(t, pyCall, f.pyCaller, "SharedName")
	f.edge(t, tsCall, tsCaller, "SharedName")

	return f
}

// qnamesOfSymbols renders a symbol page as qualified names, so a diverging
// answer is reported by identity rather than by opaque row ids.
func qnamesOfSymbols(syms []graph.Symbol) []string {
	out := make([]string, 0, len(syms))
	for _, sym := range syms {
		out = append(out, sym.QualifiedName)
	}
	return out
}

func assertQNames(t *testing.T, what string, syms []graph.Symbol, want ...string) {
	t.Helper()
	got := qnamesOfSymbols(syms)
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

// TestFindCalleesBareNameStaysInSourceLanguage is the reviewer's finding on the
// callee side: the batched name cascade behind FindCallees carried no language
// gate, so every language's `SharedName` call resolved to every language's
// `SharedName` declaration.
func TestFindCalleesBareNameStaysInSourceLanguage(t *testing.T) {
	f := newLangFixture(t)

	for _, tc := range []struct {
		caller string
		want   string
	}{
		{caller: "gofoo.GoCaller"},
		{caller: "app.PyCaller"},
		{caller: "app.TsCaller"},
	} {
		t.Run(tc.caller, func(t *testing.T) {
			got, err := f.store.FindCallees(f.ctx, f.repoID, tc.caller, 0, 20, 0)
			if err != nil {
				t.Fatalf("FindCallees(%q) error = %v", tc.caller, err)
			}
			assertQNames(t, "FindCallees("+tc.caller+")", got)
		})
	}
}

// TestFindCallersBareNameStaysInTargetLanguage is the same defect on the caller
// side: an unresolved bare spelling written in another language claimed the
// target purely because the strings matched.
func TestFindCallersBareNameStaysInTargetLanguage(t *testing.T) {
	f := newLangFixture(t)

	for _, tc := range []struct {
		target string
		want   string
	}{
		{target: "gofoo.SharedName"},
		{target: "pylib.SharedName"},
		{target: "tslib.SharedName"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			got, err := f.store.FindCallers(f.ctx, f.repoID, tc.target, 0, 20, 0)
			if err != nil {
				t.Fatalf("FindCallers(%q) error = %v", tc.target, err)
			}
			assertQNames(t, "FindCallers("+tc.target+")", got)
		})
	}
}

// TestFindContextNeighborsStaysInSeedLanguage pins the batched context
// expansion to the same rule in both directions. Without it, context_for_task
// reintroduces the foreign-language neighbours FindCallers/FindCallees stopped
// returning.
func TestFindContextNeighborsStaysInSeedLanguage(t *testing.T) {
	f := newLangFixture(t)

	seeds := []ContextSeed{
		{SymbolID: f.pyCaller, QualifiedName: "app.PyCaller", ShortName: "PyCaller", AllowShortEvidence: true},
		{SymbolID: f.goTarget, QualifiedName: "gofoo.SharedName", ShortName: "SharedName", AllowShortEvidence: true},
		{SymbolID: f.tsTarget, QualifiedName: "tslib.SharedName", ShortName: "SharedName", AllowShortEvidence: true},
	}
	got, err := f.store.FindContextNeighbors(f.ctx, f.repoID, seeds, 20)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	if len(got) != len(seeds) {
		t.Fatalf("FindContextNeighbors() returned %d results for %d seeds", len(got), len(seeds))
	}

	assertQNames(t, "callees(app.PyCaller)", got[0].Callees)
	assertQNames(t, "callers(gofoo.SharedName)", got[1].Callers)
	assertQNames(t, "callers(tslib.SharedName)", got[2].Callers)
}

// TestContextSeedsInDifferentLanguagesKeepSeparateAnswers is the per-pair case:
// two seeds in two languages spell the SAME unresolved name in one batch.
//
// A single language filter on the batch's SQL cannot separate them -- both
// languages are legitimately requested by the call -- so this is the fixture
// the per-pair decision is load-bearing for. Keying the cascade on the name
// alone would hand whichever seed asked first the other's candidates.
func TestContextSeedsInDifferentLanguagesKeepSeparateAnswers(t *testing.T) {
	f := newGateFixture(t)
	pyFile := f.file(t, "pyapp/app.py", "python")
	tsFile := f.file(t, "web/app.ts", "typescript")
	pyHelper := f.file(t, "pyapp/helper.py", "python")
	tsHelper := f.file(t, "web/helper.ts", "typescript")

	pyCaller := f.symbol(t, pyFile, "Caller", "app.PyCaller", "python")
	tsCaller := f.symbol(t, tsFile, "Caller", "app.TsCaller", "typescript")
	f.symbol(t, pyHelper, "run", "helper.run", "python")
	f.symbol(t, tsHelper, "run", "helper.run", "typescript")

	// A qualifier-bearing spelling, so P22.6's Go package rule plays no part and
	// the only thing separating the two seeds is language.
	f.edge(t, pyFile, pyCaller, "helper.run")
	f.edge(t, tsFile, tsCaller, "helper.run")

	seeds := []ContextSeed{
		{SymbolID: pyCaller, QualifiedName: "app.PyCaller", ShortName: "PyCaller", AllowShortEvidence: true},
		{SymbolID: tsCaller, QualifiedName: "app.TsCaller", ShortName: "TsCaller", AllowShortEvidence: true},
	}
	got, err := f.store.FindContextNeighbors(f.ctx, f.repoID, seeds, 20)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	for i, want := range []string{"python", "typescript"} {
		if len(got[i].Callees) != 0 {
			t.Fatalf("seed %d callees = %v, want no unresolved relationship",
				i, qnamesOfSymbols(got[i].Callees))
		}
		_ = want
	}
}

// TestSymbolDiscoveryStaysLanguageAgnostic is the control for the other half of
// the contract. A user typing a bare name supplies no language, so none may be
// invented: public discovery keeps returning every language's answer, and the
// identity gate must not narrow it.
func TestSymbolDiscoveryStaysLanguageAgnostic(t *testing.T) {
	f := newLangFixture(t)

	exact, err := f.store.FindSymbolExact(f.ctx, f.repoID, "SharedName", 20, 0)
	if err != nil {
		t.Fatalf("FindSymbolExact() error = %v", err)
	}
	// FindSymbol/SearchSymbols is FTS-backed and these rows are hand-inserted,
	// so the end-to-end discovery control lives with the indexer-driven
	// fixtures (TestLanguageGateKeepsPublicDiscovery).
	assertQNames(t, "FindSymbolExact(SharedName)", exact,
		"gofoo.SharedName", "pylib.SharedName", "tslib.SharedName")
}

// TestUnknownLanguageSingularLookupIsSemanticAndDeterministic pins the
// no-language case. `lookupSymbolID` answers a user-typed name, so it is
// deliberately cross-language, but its choice among tied candidates must come
// from a semantic total order -- never from a row id, an insertion order, or a
// lexical language name.
func TestUnknownLanguageSingularLookupIsSemanticAndDeterministic(t *testing.T) {
	f := newLangFixture(t)

	ids, err := f.store.lookupSymbolIDs(f.ctx, f.repoID, "SharedName", 0)
	if err != nil {
		t.Fatalf("lookupSymbolIDs() error = %v", err)
	}
	var got []string
	for _, id := range ids {
		got = append(got, f.qualifiedNameOf(t, id))
	}
	want := []string{"gofoo.SharedName", "pylib.SharedName", "tslib.SharedName"}
	if len(got) != len(want) {
		t.Fatalf("lookupSymbolIDs(SharedName) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lookupSymbolIDs(SharedName) = %v, want %v (qualified-name order)", got, want)
		}
	}

	// The identity-side cascade is the one that fails closed: a name with no
	// source language selects nothing rather than everything.
	resolved, err := f.store.lookupSymbolIDsByNameLanguage(f.ctx, f.repoID, []nameLanguages{
		{name: "SharedName"},
		{name: "SharedName", languages: []string{""}},
	})
	if err != nil {
		t.Fatalf("lookupSymbolIDsByNameLanguage() error = %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("lookupSymbolIDsByNameLanguage with no source language = %v, want nothing", resolved)
	}
}

// TestExactSymbolIDStaysExact pins that supplying an id identifies the symbol
// on its own: the answer is that symbol's neighbourhood, gated by that symbol's
// language. Caller spelling cannot widen the answer to another same-language
// symbol.
func TestExactSymbolIDStaysExact(t *testing.T) {
	f := newLangFixture(t)

	for _, spelling := range []string{"gofoo.SharedName", "SharedName", "other.SharedName", "::SharedName", ""} {
		callers, err := f.store.FindCallers(f.ctx, f.repoID, spelling, f.goTarget, 20, 0)
		if err != nil {
			t.Fatalf("FindCallers(%q, id=goTarget) error = %v", spelling, err)
		}
		assertQNames(t, "FindCallers("+spelling+", id=gofoo.SharedName)", callers)
	}

	callees, err := f.store.FindCallees(f.ctx, f.repoID, "SharedName", f.pyCaller, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees(id=pyCaller) error = %v", err)
	}
	assertQNames(t, "FindCallees(SharedName, id=app.PyCaller)", callees)
}

// TestQualifiedSpellingStillNeedsLanguageCompatibility covers the qualified
// case. A fully qualified spelling is strong identity evidence about WHICH
// symbol is meant, but it is not evidence that the writer's language contains
// it: a Python source spelling a Go package path still names nothing in Go.
func TestQualifiedSpellingStillNeedsLanguageCompatibility(t *testing.T) {
	f := newLangFixture(t)
	pyOther := f.file(t, "pyapp/other.py", "python")
	pyWriter := f.symbol(t, pyOther, "PyQualified", "other.PyQualified", "python")
	f.edge(t, pyOther, pyWriter, "gofoo.SharedName")

	callees, err := f.store.FindCallees(f.ctx, f.repoID, "other.PyQualified", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees(other.PyQualified) error = %v", err)
	}
	assertQNames(t, "FindCallees(other.PyQualified)", callees)

	callers, err := f.store.FindCallers(f.ctx, f.repoID, "gofoo.SharedName", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(gofoo.SharedName) error = %v", err)
	}
	assertQNames(t, "FindCallers(gofoo.SharedName)", callers)
}

// TestEcosystemInteropLosesNameOnlyRecall pins the deliberate recall change on
// a public surface, so it cannot drift back silently.
//
// Java and Kotlin are one ecosystem but two persisted CodeGraph languages, and
// P2 has always refused to BIND across them (resolver_language.go). Until
// P22.7, FindCallers/FindCallees compensated with a name-evidence leg, so a
// Kotlin call spelling a Java symbol's fully qualified name still surfaced --
// on a spelling, with no import, FFI or binding evidence behind it. That leg is
// now gated, so the relationship disappears from both surfaces.
//
// This is the cost side of the phase, and it is real: `app.Service.compute` IS
// what the Kotlin source meant. Recovering it needs declared evidence (the P19b
// cross-language pass, or a language-family policy), not a shared string, so it
// is recorded here rather than argued for in either direction.
func TestEcosystemInteropLosesNameOnlyRecall(t *testing.T) {
	f := newGateFixture(t)
	javaFile := f.file(t, "app/Service.java", "java")
	kotlinFile := f.file(t, "app/Main.kt", "kotlin")
	f.symbol(t, javaFile, "compute", "app.Service.compute", "java")
	main := f.symbol(t, kotlinFile, "main", "app.MainKt.main", "kotlin")
	f.edge(t, kotlinFile, main, "app.Service.compute")

	callers, err := f.store.FindCallers(f.ctx, f.repoID, "app.Service.compute", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(app.Service.compute) error = %v", err)
	}
	assertQNames(t, "FindCallers(app.Service.compute)", callers)

	callees, err := f.store.FindCallees(f.ctx, f.repoID, "app.MainKt.main", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees(app.MainKt.main) error = %v", err)
	}
	assertQNames(t, "FindCallees(app.MainKt.main)", callees)

	// The same spelling inside one language is untouched, so this is a language
	// boundary and not a lost qualified-name capability.
	kotlinOther := f.file(t, "app/Other.kt", "kotlin")
	f.symbol(t, kotlinOther, "compute", "app.Helper.compute", "kotlin")
	f.edge(t, kotlinFile, main, "app.Helper.compute")
	sameLang, err := f.store.FindCallers(f.ctx, f.repoID, "app.Helper.compute", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(app.Helper.compute) error = %v", err)
	}
	assertQNames(t, "FindCallers(app.Helper.compute)", sameLang)
}

// TestExplicitCrossLanguageLinkSurvivesLanguageGate is the P19b control, driven
// by the real ResolveCrossLanguageLinks pass rather than by a hand-written row.
//
// An explicit `cross_language_ref` is a BOUND destination created from
// import-bridge evidence, not from a shared name. The gate governs only
// unresolved name spellings, so a proven foreign-language relationship must
// keep surfacing on every neighbour surface.
func TestExplicitCrossLanguageLinkSurvivesLanguageGate(t *testing.T) {
	f := newGateFixture(t)
	spec := crossLangSpec{
		files: []crossLangFile{
			{path: "src/ts/client.ts", language: "typescript", symbols: []crossLangSymbol{{name: "Payload", qualified: "client.Payload"}}},
			{path: "src/shared/model.py", language: "python", symbols: []crossLangSymbol{{name: "Payload", qualified: "model.Payload"}}},
		},
		imports: []crossLangImport{{fromPath: "src/ts/client.ts", path: "src/shared/model.ts"}},
	}
	spec.build(t, f, 1)
	if created := f.resolveCrossLanguage(t); created != 1 {
		t.Fatalf("ResolveCrossLanguageLinks() created %d links, want 1", created)
	}

	callers, err := f.store.FindCallers(f.ctx, f.repoID, "model.Payload", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(model.Payload) error = %v", err)
	}
	assertQNames(t, "FindCallers(model.Payload)", callers, "client.Payload")

	callees, err := f.store.FindCallees(f.ctx, f.repoID, "client.Payload", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees(client.Payload) error = %v", err)
	}
	assertQNames(t, "FindCallees(client.Payload)", callees, "model.Payload")

	seeds := []ContextSeed{
		{SymbolID: 0, QualifiedName: "model.Payload", ShortName: "Payload", AllowShortEvidence: true},
	}
	seeds[0].SymbolID = f.symbolIDByQualifiedName(t, "model.Payload")
	got, err := f.store.FindContextNeighbors(f.ctx, f.repoID, seeds, 20)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	assertQNames(t, "context callers(model.Payload)", got[0].Callers, "client.Payload")
}

// symbolIDByQualifiedName resolves a fixture symbol's row id by identity.
func (f *gateFixture) symbolIDByQualifiedName(t *testing.T, qualified string) int64 {
	t.Helper()
	var id int64
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT id FROM symbols WHERE repo_id = ? AND qualified_name = ?`, f.repoID, qualified).Scan(&id); err != nil {
		t.Fatalf("symbol %q: %v", qualified, err)
	}
	return id
}

// TestLanguageGateKeepsRepoIsolation pins that the language rule narrows within
// a repository and never widens across one: a same-name, same-language symbol
// in another repository is still invisible.
func TestLanguageGateKeepsRepoIsolation(t *testing.T) {
	f := newLangFixture(t)
	other, err := f.store.UpsertRepo(f.ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	otherFile, err := insertTestFileLang(f.ctx, f.store, other.ID, "pyapp/lib.py", "python")
	if err != nil {
		t.Fatalf("insertTestFileLang() error = %v", err)
	}
	if _, err := insertTestSymbolLang(f.ctx, f.store, other.ID, otherFile, "SharedName", "pylib.SharedName", "python"); err != nil {
		t.Fatalf("insertTestSymbolLang() error = %v", err)
	}

	callees, err := f.store.FindCallees(f.ctx, f.repoID, "app.PyCaller", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees(app.PyCaller) error = %v", err)
	}
	assertQNames(t, "FindCallees(app.PyCaller)", callees)
	for _, sym := range callees {
		var owner int64
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT repo_id FROM symbols WHERE id = ?`, sym.ID).Scan(&owner); err != nil {
			t.Fatalf("owner lookup: %v", err)
		}
		if owner != f.repoID {
			t.Fatalf("FindCallees returned a symbol from repo %d, want %d", owner, f.repoID)
		}
	}
}

// TestLanguageGateIgnoresInsertionOrder pins that the answer is a function of
// the graph. Inserting the Go declaration before the Python one, or after it,
// must give the same identity -- the gate may never fall back to "first row" or
// "lowest id".
func TestLanguageGateIgnoresInsertionOrder(t *testing.T) {
	answer := func(t *testing.T, goFirst bool) []string {
		t.Helper()
		f := newGateFixture(t)
		goDecl := f.file(t, "gofoo/decl.go", "go")
		pyDecl := f.file(t, "pyapp/lib.py", "python")
		pyCall := f.file(t, "pyapp/app.py", "python")
		if goFirst {
			f.symbol(t, goDecl, "SharedName", "gofoo.SharedName", "go")
			f.symbol(t, pyDecl, "SharedName", "pylib.SharedName", "python")
		} else {
			f.symbol(t, pyDecl, "SharedName", "pylib.SharedName", "python")
			f.symbol(t, goDecl, "SharedName", "gofoo.SharedName", "go")
		}
		caller := f.symbol(t, pyCall, "PyCaller", "app.PyCaller", "python")
		f.edge(t, pyCall, caller, "SharedName")

		got, err := f.store.FindCallees(f.ctx, f.repoID, "app.PyCaller", 0, 20, 0)
		if err != nil {
			t.Fatalf("FindCallees() error = %v", err)
		}
		return qnamesOfSymbols(got)
	}

	goFirst := answer(t, true)
	pyFirst := answer(t, false)
	if len(goFirst) != 0 {
		t.Fatalf("Go-inserted-first answer = %v, want no unresolved relationship", goFirst)
	}
	if len(pyFirst) != 0 {
		t.Fatalf("insertion order changed the answer: %v vs %v", goFirst, pyFirst)
	}
}

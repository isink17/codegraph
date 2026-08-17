package store

import (
	"sort"
	"testing"
)

// Regression coverage for P22.14: an empty target-scope set means "no target
// identity to derive a scope from", not "every scope-gated language is
// excluded".
//
// FindCallers' bare name leg carries the P22.6 Go package rule and the P22.13
// C/C++ file rule as a predicate over the WRITER's scope, bound to the scope
// keys of the symbols the input matched. When the input matches no symbol at
// all there are no keys, and the predicate degenerated into "the writer is
// neither Go nor C/C++" -- which silently deleted exactly the writers the
// unresolved-name question exists to surface.
//
// The distinction these fixtures pin:
//
//	no matched target        no language, package or file evidence exists, so
//	                         no target-derived predicate may be applied
//	matched target, no scope the target IS known and its scope rules say no
//	                         bare spelling reaches it, which is a refusal
//
// The second half is P22.6/P22.7/P22.13 and must not move.

// missingNameFixture writes one unresolved `MissingThing` call per language and
// declares no symbol of that name anywhere.
func newMissingNameFixture(t *testing.T) *gateFixture {
	t.Helper()
	f := newGateFixture(t)

	goFile := f.file(t, "gopkg/call.go", "go")
	cppFile := f.file(t, "src/call.cpp", "cpp")
	pyFile := f.file(t, "app/call.py", "python")

	goCaller := f.symbol(t, goFile, "caller", "gopkg.caller", "go")
	cppCaller := f.symbol(t, cppFile, "caller", "caller", "cpp")
	pyCaller := f.symbol(t, pyFile, "caller", "app.caller", "python")

	f.edge(t, goFile, goCaller, "MissingThing")
	f.edge(t, cppFile, cppCaller, "MissingThing")
	f.edge(t, pyFile, pyCaller, "MissingThing")

	return f
}

func callerQNames(t *testing.T, f *gateFixture, symbol string, symbolID int64) []string {
	t.Helper()
	got, err := f.store.FindCallers(f.ctx, f.repoID, symbol, symbolID, 50, 0)
	if err != nil {
		t.Fatalf("FindCallers(%q, %d) error = %v", symbol, symbolID, err)
	}
	names := qnamesOfSymbols(got)
	sort.Strings(names)
	return names
}

func assertSet(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

// TestFindCallersUnknownGoNameKeepsGoWriter is the Go half of the defect: with
// no `MissingThing` symbol in the index there is no package to scope against,
// and the Go writer is the only evidence there is.
func TestFindCallersUnknownGoNameKeepsGoWriter(t *testing.T) {
	f := newGateFixture(t)
	goFile := f.file(t, "gopkg/call.go", "go")
	goCaller := f.symbol(t, goFile, "caller", "gopkg.caller", "go")
	f.edge(t, goFile, goCaller, "MissingThing")

	assertSet(t, `FindCallers("MissingThing")`, callerQNames(t, f, "MissingThing", 0), "gopkg.caller")
}

// TestFindCallersUnknownCppNameKeepsCppWriter is the C/C++ half: P22.13's
// file/include scope has nothing to gate against when no target exists.
func TestFindCallersUnknownCppNameKeepsCppWriter(t *testing.T) {
	f := newGateFixture(t)
	cppFile := f.file(t, "src/call.cpp", "cpp")
	cppCaller := f.symbol(t, cppFile, "caller", "caller", "cpp")
	f.edge(t, cppFile, cppCaller, "MissingThing")

	assertSet(t, `FindCallers("MissingThing")`, callerQNames(t, f, "MissingThing", 0), "caller")
}

// TestFindCallersUnknownNameKeepsEveryLanguage: an unindexed name is discovery
// evidence. No target language exists, so no language may be preferred -- and
// no cross-language identity is invented either, because nothing is bound.
func TestFindCallersUnknownNameKeepsEveryLanguage(t *testing.T) {
	f := newMissingNameFixture(t)

	assertSet(t, `FindCallers("MissingThing")`, callerQNames(t, f, "MissingThing", 0),
		"gopkg.caller", "caller", "app.caller")

	// Neither does the spelling the caller happened to type: a qualified input
	// whose short name is the unindexed one matches no symbol either, so it
	// asks the same question and must get the same answer.
	assertSet(t, `FindCallers("nowhere.MissingThing")`, callerQNames(t, f, "nowhere.MissingThing", 0),
		"gopkg.caller", "caller", "app.caller")

	// The answer is a hint, not a binding: a query may not write the identity
	// the resolver refused to prove, and it may not invent one across
	// languages either.
	var bound int
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT COUNT(*) FROM edges WHERE dst_name = 'MissingThing' AND dst_symbol_id IS NOT NULL`,
	).Scan(&bound); err != nil {
		t.Fatalf("count bound edges error = %v", err)
	}
	if bound != 0 {
		t.Fatalf("edges bound by a query = %d, want 0", bound)
	}
}

// TestFindCallersKnownGoTargetKeepsPackageScope is the load-bearing opposite:
// a real target in another package must not collect a writer merely for
// spelling its short name (P22.6).
func TestFindCallersKnownGoTargetKeepsPackageScope(t *testing.T) {
	f := newGateFixture(t)
	callFile := f.file(t, "a/call.go", "go")
	declFile := f.file(t, "b/decl.go", "go")
	localFile := f.file(t, "b/local.go", "go")

	f.symbol(t, declFile, "Foo", "b.Foo", "go")
	outsider := f.symbol(t, callFile, "outsider", "a.outsider", "go")
	insider := f.symbol(t, localFile, "insider", "b.insider", "go")
	f.edge(t, callFile, outsider, "Foo")
	f.edge(t, localFile, insider, "Foo")

	assertSet(t, `FindCallers("b.Foo")`, callerQNames(t, f, "b.Foo", 0), "b.insider")
}

// TestFindCallersKnownCppTargetKeepsFileScope pins P22.13: a known C++ target
// in an unrelated translation unit stays refused.
func TestFindCallersKnownCppTargetKeepsFileScope(t *testing.T) {
	f := newGateFixture(t)
	otherFile := f.file(t, "src/other.cpp", "cpp")
	callFile := f.file(t, "src/call.cpp", "cpp")

	target := f.symbol(t, otherFile, "Foo", "Foo", "cpp")
	caller := f.symbol(t, callFile, "caller", "caller", "cpp")
	f.edge(t, callFile, caller, "Foo")

	assertSet(t, `FindCallers(cpp Foo)`, callerQNames(t, f, "Foo", target), /* none */)
}

// TestFindCallersKnownTargetKeepsLanguageGate: a known target in one language
// does not collect writers in another (P22.7). Only the unknown-name case is
// language-agnostic, because only there is there no language to read.
func TestFindCallersKnownTargetKeepsLanguageGate(t *testing.T) {
	f := newMissingNameFixture(t)
	pyDecl := f.file(t, "app/lib.py", "python")
	target := f.symbol(t, pyDecl, "MissingThing", "lib.MissingThing", "python")

	assertSet(t, `FindCallers(python MissingThing)`, callerQNames(t, f, "lib.MissingThing", target), "app.caller")
}

// TestFindCallersZeroScopeDoesNotWidenExactID is the P22.18 control: the fix
// must not make an exact symbol id answer any wider than it already is.
func TestFindCallersZeroScopeDoesNotWidenExactID(t *testing.T) {
	f := newGateFixture(t)
	callFile := f.file(t, "a/call.go", "go")
	declFile := f.file(t, "b/decl.go", "go")

	target := f.symbol(t, declFile, "Foo", "b.Foo", "go")
	outsider := f.symbol(t, callFile, "outsider", "a.outsider", "go")
	f.edge(t, callFile, outsider, "Foo")

	assertSet(t, `FindCallers(id b.Foo)`, callerQNames(t, f, "b.Foo", target), /* none */)
}

// TestFindCallersStaleSymbolIDStaysRefused: an explicit symbol id is an
// assertion of identity, so a stale one -- an id whose row a reindex removed --
// must not fail open into the unknown-name contract. Nothing was matched, but
// nothing was asked about a name either.
func TestFindCallersStaleSymbolIDStaysRefused(t *testing.T) {
	f := newGateFixture(t)
	callFile := f.file(t, "a/call.go", "go")
	declFile := f.file(t, "b/decl.go", "go")

	target := f.symbol(t, declFile, "Foo", "b.Foo", "go")
	outsider := f.symbol(t, callFile, "outsider", "a.outsider", "go")
	f.edge(t, callFile, outsider, "Foo")
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE id = ?`, target); err != nil {
		t.Fatalf("DELETE symbol error = %v", err)
	}

	assertSet(t, `FindCallers(stale id)`, callerQNames(t, f, "Foo", target) /* none */)
}

// TestFindCallersGoMethodTargetKeepsRefusal is the gated-language half of the
// "matched target, zero scope keys" state: no bare Go spelling names a method,
// so even a writer in the method's own package is refused. This is the state
// the fix must NOT reclassify as an absence of evidence.
func TestFindCallersGoMethodTargetKeepsRefusal(t *testing.T) {
	f := newGateFixture(t)
	declFile := f.file(t, "b/decl.go", "go")
	callFile := f.file(t, "b/call.go", "go")

	target := f.method(t, declFile, "Foo", "b.T.Foo", "T", "go")
	insider := f.symbol(t, callFile, "insider", "b.insider", "go")
	f.edge(t, callFile, insider, "Foo")

	assertSet(t, `FindCallers("b.T.Foo")`, callerQNames(t, f, "b.T.Foo", target) /* none */)
	assertSet(t, `FindCallers(name b.T.Foo)`, callerQNames(t, f, "b.T.Foo", 0) /* none */)
}

// TestFindCallersTargetLifecycleSwitchesContract walks the lifecycle the
// contract is defined over: with no target the writer is name evidence, and as
// soon as a target identity exists its scope rules decide. Deleting the target
// returns the answer to the unknown-name contract, so the same final graph
// state yields the same answer whichever way it was reached.
func TestFindCallersTargetLifecycleSwitchesContract(t *testing.T) {
	f := newGateFixture(t)
	callFile := f.file(t, "a/call.go", "go")
	declFile := f.file(t, "b/decl.go", "go")
	outsider := f.symbol(t, callFile, "outsider", "a.outsider", "go")
	f.edge(t, callFile, outsider, "Foo")

	assertSet(t, "before target", callerQNames(t, f, "Foo", 0), "a.outsider")

	target := f.symbol(t, declFile, "Foo", "b.Foo", "go")
	assertSet(t, "with target", callerQNames(t, f, "Foo", 0) /* none: b.Foo is out of a's package */)

	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE id = ?`, target); err != nil {
		t.Fatalf("DELETE symbol error = %v", err)
	}
	assertSet(t, "after delete", callerQNames(t, f, "Foo", 0), "a.outsider")

	f.symbol(t, declFile, "Foo", "b.Foo", "go")
	assertSet(t, "after recreate", callerQNames(t, f, "Foo", 0) /* none */)
}

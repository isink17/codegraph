//go:build cgo

package indexer

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// TestCppBareCallDeclarationDeletionReconsidersUntouchedCaller pins H across
// the declaration/definition bridge: removing the declaration must not leave
// the caller bound to the old target.
func TestCppBareCallDeclarationDeletionReconsidersUntouchedCaller(t *testing.T) {
	r := newCppRepo(t)
	r.write("foo.h", "void foo();\n")
	r.write("foo.cpp", "#include \"foo.h\"\nvoid foo() {}\n")
	caller := "#include \"foo.h\"\nvoid caller() { foo(); }\n"
	r.write("caller.cpp", caller)
	r.run("index")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"foo"}) {
		t.Fatalf("initial targets = %v, want [foo]", got)
	}

	r.write("foo.h", "void other();\n")
	r.run("update")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("after declaration deletion targets = %v, want unresolved", got)
	}
	assertCppFreshParity(t, "declaration deletion", r, map[string]string{
		"foo.h": "void other();\n", "foo.cpp": "#include \"foo.h\"\nvoid foo() {}\n", "caller.cpp": caller,
	})
}

// TestCppRefusedClassTargetIsNotReconstructedByContext adds the context
// surface to the caller/callee refusal checks already covering this fixture.
func TestCppRefusedClassTargetIsNotReconstructedByContext(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "#include \"b.h\"\nstruct A { void caller() { foo(); } };\n")
	r.write("b.h", "struct B { void foo() {} };\n")
	r.run("index")

	symbols, err := r.store.FindSymbolExact(context.Background(), r.repo.ID, "B::foo", 20, 0)
	if err != nil || len(symbols) != 1 {
		t.Fatalf("FindSymbolExact(B::foo) = %v, %v", symbols, err)
	}
	neighbors, err := r.store.FindContextNeighbors(context.Background(), r.repo.ID, []store.ContextSeed{{
		SymbolID: symbols[0].ID, QualifiedName: "B::foo", ShortName: "foo", AllowShortEvidence: true,
	}}, 20)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	if len(neighbors) != 1 || len(neighbors[0].Callers) != 0 {
		t.Fatalf("context rebuilt refused target: %+v", neighbors)
	}
}

// TestCppBareCallDefinitionDeletionHasNoDanglingTarget pins K without
// assuming whether the remaining declaration is itself a public destination:
// a surviving edge may be declaration-bound or unresolved, never definition-bound.
func TestCppBareCallDefinitionDeletionHasNoDanglingTarget(t *testing.T) {
	r := newCppRepo(t)
	r.write("foo.h", "void foo();\n")
	r.write("foo.cpp", "#include \"foo.h\"\nvoid foo() {}\n")
	r.write("caller.cpp", "#include \"foo.h\"\nvoid caller() { foo(); }\n")
	r.run("index")
	r.remove("foo.cpp")
	r.run("update")

	ctx := context.Background()
	symbols, err := r.store.FindSymbolExact(ctx, r.repo.ID, "foo", 20, 0)
	if err != nil {
		t.Fatalf("FindSymbolExact() error = %v", err)
	}
	allowed := make(map[int64]bool, len(symbols))
	for _, symbol := range symbols {
		if symbol.FilePath == "foo.h" {
			allowed[symbol.ID] = true
		}
	}
	edges, err := r.store.ExportEdgesPage(ctx, r.repo.ID, 100, 0)
	if err != nil {
		t.Fatalf("ExportEdgesPage() error = %v", err)
	}
	for _, edge := range edges {
		if edge.FilePath == "caller.cpp" && edge.DstName == "foo" && edge.DstSymbolID != nil && !allowed[*edge.DstSymbolID] {
			t.Fatalf("definition deletion left target %d outside surviving declaration: %+v", *edge.DstSymbolID, edge)
		}
	}
}

// TestCppQualifiedDefinitionRenameReconsidersUntouchedCaller pins I: OLD and
// NEW qualified identities must both invalidate an untouched caller.
func TestCppQualifiedDefinitionRenameReconsidersUntouchedCaller(t *testing.T) {
	r := newCppRepo(t)
	r.write("target.h", "namespace b { void foo(); }\n")
	r.write("target.cpp", "namespace a { void foo() {} }\n")
	caller := "#include \"target.h\"\nvoid caller() { b::foo(); }\n"
	r.write("caller.cpp", caller)
	r.run("index")
	if got := cppTargets(r, "b::foo"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("initial targets = %v, want unresolved", got)
	}

	r.write("target.cpp", "namespace b { void foo() {} }\n")
	r.run("update")
	if got := cppTargets(r, "b::foo"); !equalStrings(got, []string{"b::foo"}) {
		t.Fatalf("after qualified rename targets = %v, want [b::foo]", got)
	}
	assertCppFreshParity(t, "qualified definition rename", r, map[string]string{
		"target.h": "namespace b { void foo(); }\n", "target.cpp": "namespace b { void foo() {} }\n", "caller.cpp": caller,
	})
}

func TestCppBareCallSameNamespaceBindsDespiteForeignNamespaceSibling(t *testing.T) {
	r := newCppRepo(t)
	r.write("fixture.cpp", "namespace a { void foo() {} }\nnamespace b {\nvoid foo() {}\nvoid caller() { foo(); }\n}\n")
	r.run("index")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"b::foo"}) {
		t.Fatalf("targets = %v, want [b::foo]", got)
	}
}

func TestCppOutOfLineClassHeaderRenameReconsidersUnchangedImplementation(t *testing.T) {
	implementation := "#include \"a.h\"\nvoid A::foo() {}\nvoid A::caller() { foo(); }\n"
	r := newCppRepo(t)
	r.write("a.h", "class A { public: void foo(); void caller(); };\n")
	r.write("a.cpp", implementation)
	r.run("index")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"A::foo"}) {
		t.Fatalf("initial targets = %v, want [A::foo]", got)
	}
	r.write("a.h", "class C { public: void foo(); void caller(); };\n")
	r.run("update")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("after header class rename targets = %v, want unresolved", got)
	}
	assertCppFreshParity(t, "header class rename", r, map[string]string{
		"a.h": "class C { public: void foo(); void caller(); };\n", "a.cpp": implementation,
	})
}

func TestCppHiddenFriendDoesNotEnterOrdinaryBareLookup(t *testing.T) {
	r := newCppRepo(t)
	r.write("fixture.cpp", `namespace n {
struct Box { friend int compare(const Box&, const Box&) { return 0; } };
int caller(Box a, Box b) { return compare(a, b); }
}
namespace other { int compare(int, int) { return 0; } }
`)
	r.run("index")
	if got := cppTargets(r, "compare"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("hidden friend bare lookup targets = %v, want unresolved", got)
	}
}

func TestCppMacroWrappedCallStaysUnresolvedWithoutExpansion(t *testing.T) {
	r := newCppRepo(t)
	r.write("fixture.cpp", `#define WRAP(x) x
void dup(int) {}
void caller() { WRAP(dup(1)); }
`)
	r.run("index")
	if got := cppTargets(r, "dup"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("macro-wrapped call targets = %v, want unresolved", got)
	}
}

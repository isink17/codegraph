//go:build cgo

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// legacy80Source is the fixture from #80. It is kept verbatim, because the
// value of the report was the shape of the code, not the expectation that was
// derived from it.
//
// What changed in P22.11 is the contract, not the fixture. #80 pinned
// `pcsApmMap->LoadWorldMap()` and `map.LoadWorldMap()` as callers of
// `ApmMap::LoadWorldMap`. Both are correct *at the source level* -- and neither
// is provable from anything CodeGraph models. The adapter extracts no field or
// parameter declarations, so nothing in the graph connects the variable
// `pcsApmMap` (or `map`) to the type `ApmMap`; the relation held only because
// the receiver was discarded and `LoadWorldMap` happened to name one thing in
// this repository. The same discard bound `v.size()` on a `std::vector` to a
// project `size` method in fmt and googletest, which is the identical inference
// with a wrong answer. A test may pin history; it may not pin an unsound rule,
// so the expectation is now: the receiver survives, and a receiver CodeGraph
// cannot type stays unresolved. TestCppCallGraphQualifiedCallResolves covers
// the C++ calls that do carry evidence.
const legacy80Source = `class ApmMap {
public:
    bool LoadWorldMap(char* pFileName, bool bDecryption = false);
};

class AgcmMinimap {
    ApmMap* pcsApmMap;
public:
    bool F() {
        if (pcsApmMap->LoadWorldMap("x", false)) {
            return true;
        }
        return false;
    }
};

bool ApmMap::LoadWorldMap(char* pFileName, bool bDecryption) {
    return true;
}

bool Direct(ApmMap& map) {
    return map.LoadWorldMap("x", false);
}

bool Internal(char* pFileName, bool bDecryption) {
    return LoadWorldMap(pFileName, bDecryption);
}
`

// indexCppFixture writes files into a fresh repo, indexes them with only the
// C/C++ adapter registered, and returns the store and repo.
func indexCppFixture(t *testing.T, files map[string]string) (*store.Store, graph.Repo) {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()
	for name, src := range files {
		path := filepath.Join(repoRoot, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", name, err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })

	idx := New(s, parser.NewRegistry(tsparser.NewCpp()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	return s, repo
}

// TestCppCallGraphSymbolIdentities keeps the half of #80 that was always sound:
// an out-of-line `bool ApmMap::LoadWorldMap(...)` definition is one symbol
// whose qualified name is `ApmMap::LoadWorldMap`, not `ApmMap.ApmMap::…`.
func TestCppCallGraphSymbolIdentities(t *testing.T) {
	ctx := context.Background()
	s, repo := indexCppFixture(t, map[string]string{"fixture.cpp": legacy80Source})

	symbols, err := s.SearchSymbols(ctx, repo.ID, "LoadWorldMap", 20, 0)
	if err != nil {
		t.Fatalf("SearchSymbols() error = %v", err)
	}
	if !hasSymbolQName(symbols, "ApmMap::LoadWorldMap") {
		t.Fatalf("search_symbols missing canonical ApmMap::LoadWorldMap, got %+v", symbols)
	}
	if hasSymbolQName(symbols, "ApmMap.ApmMap::LoadWorldMap") {
		t.Fatalf("search_symbols still returned duplicated qualified_name")
	}

	exact, err := s.FindSymbolExact(ctx, repo.ID, "ApmMap::LoadWorldMap", 20, 0)
	if err != nil {
		t.Fatalf("FindSymbolExact() error = %v", err)
	}
	if len(exact) == 0 {
		t.Fatalf("FindSymbolExact() returned no symbols")
	}
	if exact[0].QualifiedName != "ApmMap::LoadWorldMap" {
		t.Fatalf("qualified_name = %q, want ApmMap::LoadWorldMap", exact[0].QualifiedName)
	}
}

// TestCppCallGraphUnprovenReceiverStaysUnresolved is the replacement contract
// for #80: the receiver reaches the graph, and no surface turns it back into a
// bare-name claim on `ApmMap::LoadWorldMap`.
func TestCppCallGraphUnprovenReceiverStaysUnresolved(t *testing.T) {
	ctx := context.Background()
	s, repo := indexCppFixture(t, map[string]string{"fixture.cpp": legacy80Source})

	// The parser's receiver survives into the persisted destination identity.
	for _, dst := range []string{"pcsApmMap.LoadWorldMap", "map.LoadWorldMap"} {
		n, err := s.CountUnresolvedEdgesByDstName(ctx, repo.ID, dst)
		if err != nil {
			t.Fatalf("CountUnresolvedEdgesByDstName(%s) error = %v", dst, err)
		}
		if n != 1 {
			t.Fatalf("unresolved edges for %q = %d, want 1 (receiver must be kept and must not bind)", dst, n)
		}
	}

	// FindCallers/FindCallees must not rebuild the refused relation from the
	// name evidence either -- a receiver-bearing spelling shares no qualifier
	// with `ApmMap::LoadWorldMap` (P22.1).
	callers, err := s.FindCallers(ctx, repo.ID, "ApmMap::LoadWorldMap", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers() error = %v", err)
	}
	if hasSymbolQName(callers, "AgcmMinimap::F") {
		t.Fatalf("FindCallers rebuilt the unproven `pcsApmMap->` relation: %+v", callers)
	}
	if hasSymbolQName(callers, "fixture::Direct") {
		t.Fatalf("FindCallers rebuilt the unproven `map.` relation: %+v", callers)
	}
	callees, err := s.FindCallees(ctx, repo.ID, "AgcmMinimap::F", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees() error = %v", err)
	}
	if hasSymbolQName(callees, "ApmMap::LoadWorldMap") {
		t.Fatalf("FindCallees rebuilt the unproven `pcsApmMap->` relation: %+v", callees)
	}

	// #80 also queried the bare and leading-`::` spellings, and those are the
	// shapes a user actually types. A bare identity shares no qualifier with
	// `pcsApmMap.LoadWorldMap`, so the suffix leg must not extend it either.
	//
	// The genuinely unqualified call site in `Internal` may not answer either,
	// and that is the P22.15 closeout (cpp_class_scope.go): `Internal` is a FREE
	// function, `ApmMap::LoadWorldMap` is a class member, and an unqualified call
	// in a free function does not reach a member in C++ -- the graph held that
	// relation only because `LoadWorldMap` happened to name one thing in this
	// repository, which is the same non-evidence the receiver half of this test
	// already refuses. The resolver never bound this edge (`exact_name` matches
	// `symbols.name`, which is `ApmMap::LoadWorldMap` here); the claim existed
	// only on the query surface's short-name leg, so this is where it dies.
	for _, spelling := range []string{"LoadWorldMap", "::LoadWorldMap"} {
		bare, err := s.FindCallers(ctx, repo.ID, spelling, 0, 20, 0)
		if err != nil {
			t.Fatalf("FindCallers(%s) error = %v", spelling, err)
		}
		for _, unproven := range []string{"AgcmMinimap::F", "fixture::Direct", "fixture::Internal"} {
			if hasSymbolQName(bare, unproven) {
				t.Fatalf("FindCallers(%s) rebuilt the unproven relation via %s: %+v", spelling, unproven, bare)
			}
		}
	}
}

// TestCppCallGraphQualifiedCallResolves is the positive control: C++ calls
// whose target CodeGraph can actually prove keep their recall. A `::`-qualified
// call names a type and a member, and that spelling matches the destination's
// own identity, so it binds with no receiver-type inference anywhere.
func TestCppCallGraphQualifiedCallResolves(t *testing.T) {
	ctx := context.Background()
	s, repo := indexCppFixture(t, map[string]string{"fixture.cpp": `class ApmMap {
public:
    bool LoadWorldMap(char* pFileName, bool bDecryption = false);
};

bool ApmMap::LoadWorldMap(char* pFileName, bool bDecryption) {
    return true;
}

bool Qualified() {
    return ApmMap::LoadWorldMap(nullptr, false);
}

bool AlsoQualified() {
    return ApmMap::LoadWorldMap(nullptr, true);
}
`})

	n, err := s.CountUnresolvedEdgesByDstName(ctx, repo.ID, "ApmMap::LoadWorldMap")
	if err != nil {
		t.Fatalf("CountUnresolvedEdgesByDstName() error = %v", err)
	}
	if n != 0 {
		t.Fatalf("%d qualified ApmMap::LoadWorldMap calls left unresolved, want 0", n)
	}

	callers, err := s.FindCallers(ctx, repo.ID, "ApmMap::LoadWorldMap", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers() error = %v", err)
	}
	for _, want := range []string{"Qualified", "AlsoQualified"} {
		if !hasSymbolQName(callers, want) {
			t.Fatalf("FindCallers(ApmMap::LoadWorldMap) missing %s, got %+v", want, callers)
		}
	}
	callees, err := s.FindCallees(ctx, repo.ID, "Qualified", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees() error = %v", err)
	}
	if !hasSymbolQName(callees, "ApmMap::LoadWorldMap") {
		t.Fatalf("FindCallees(Qualified) missing ApmMap::LoadWorldMap, got %+v", callees)
	}

	// Pagination over the two evidence-backed callers.
	p0, err := s.FindCallers(ctx, repo.ID, "ApmMap::LoadWorldMap", 0, 1, 0)
	if err != nil {
		t.Fatalf("FindCallers page0: %v", err)
	}
	p1, err := s.FindCallers(ctx, repo.ID, "ApmMap::LoadWorldMap", 0, 1, 1)
	if err != nil {
		t.Fatalf("FindCallers page1: %v", err)
	}
	if len(p0) != 1 || len(p1) != 1 {
		t.Fatalf("FindCallers pagination: got %d and %d, want 1 and 1", len(p0), len(p1))
	}
	if p0[0].ID == p1[0].ID {
		t.Fatalf("FindCallers pagination returned duplicate: %q on both pages", p0[0].QualifiedName)
	}
}

func TestCppNamespaceAndGlobalQualifiedCalls(t *testing.T) {
	ctx := context.Background()
	s, repo := indexCppFixture(t, map[string]string{"fixture.cpp": `
namespace a { void foo() {} }
namespace b { void bare() { foo(); } }
namespace n { void Foo() {} }
void Foo() {}
void global() {}
void caller() { a::foo(); ::Foo(); ::global(); }
`})
	if n, err := s.CountUnresolvedEdgesByDstName(ctx, repo.ID, "a::foo"); err != nil || n != 0 {
		t.Fatalf("a::foo unresolved = %d, err = %v", n, err)
	}
	if n, err := s.CountUnresolvedEdgesByDstName(ctx, repo.ID, "::global"); err != nil || n != 0 {
		t.Fatalf("::global unresolved = %d, err = %v", n, err)
	}
	callees, err := s.FindCallees(ctx, repo.ID, "b::bare", 0, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasSymbolQName(callees, "a::foo") {
		t.Fatalf("bare b::bare call crossed into a::foo: %+v", callees)
	}
	callers, err := s.FindCallers(ctx, repo.ID, "a::foo", 0, 20, 0)
	if err != nil || !hasSymbolQName(callers, "caller") {
		t.Fatalf("FindCallers(a::foo) = %+v, err = %v", callers, err)
	}
	global, err := s.FindCallers(ctx, repo.ID, "::global", 0, 20, 0)
	if err != nil || !hasSymbolQName(global, "caller") {
		t.Fatalf("FindCallers(::global) = %+v, err = %v", global, err)
	}
	callees, err = s.FindCallees(ctx, repo.ID, "caller", 0, 20, 0)
	if err != nil || !hasSymbolQName(callees, "Foo") || hasSymbolQName(callees, "n::Foo") {
		t.Fatalf("FindCallees(caller) global Foo = %+v, err = %v", callees, err)
	}
}

func TestCppQualifiedNameRenameReconsidersUntouchedCaller(t *testing.T) {
	r := newCppRepo(t)
	r.write("target.h", "namespace b { void foo(); }\n")
	r.write("target.cpp", "namespace a { void foo() {} }\n")
	r.write("caller.cpp", "#include \"target.h\"\nvoid caller() { b::foo(); }\n")
	r.run("index")
	if got := r.unresolved("b::foo"); got != 1 {
		t.Fatalf("initial b::foo unresolved = %d, want 1", got)
	}
	r.write("target.cpp", "namespace b { void foo() {} }\n")
	r.run("update")
	if got := r.boundTargets("b::foo"); len(got) != 1 || got[0] != "b::foo" {
		t.Fatalf("after namespace rename b::foo = %v, want [b::foo]", got)
	}
}

func TestCppBareCallNamespaceEligibility(t *testing.T) {
	tests := []struct {
		name           string
		files          map[string]string
		caller, target string
		want           bool
	}{
		{
			name: "same namespace",
			files: map[string]string{"fixture.cpp": `namespace a {
void foo() {}
void caller() { foo(); }
}`},
			caller: "a::caller", target: "a::foo", want: true,
		},
		{
			name: "different namespace",
			files: map[string]string{"fixture.cpp": `namespace a { void foo() {} }
namespace b { void caller() { foo(); } }`},
			caller: "b::caller", target: "a::foo",
		},
		{
			name: "same file does not cross namespace",
			files: map[string]string{"fixture.cpp": `namespace a { void foo() {} }
namespace b { void caller() { foo(); } }`},
			caller: "b::caller", target: "a::foo",
		},
		{
			name: "include does not cross namespace",
			files: map[string]string{
				"a.h": `namespace a { inline void foo() {} }`,
				"caller.cpp": `#include "a.h"
namespace b { void caller() { foo(); } }`,
			},
			caller: "b::caller", target: "a::foo",
		},
		{
			name: "global caller does not claim namespace",
			files: map[string]string{"fixture.cpp": `namespace a { void foo() {} }
void caller() { foo(); }`},
			caller: "caller", target: "a::foo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, repo := indexCppFixture(t, tt.files)
			callees, err := s.FindCallees(ctx, repo.ID, tt.caller, 0, 20, 0)
			if err != nil {
				t.Fatal(err)
			}
			got := hasSymbolQName(callees, tt.target)
			if got != tt.want {
				t.Fatalf("FindCallees(%q) target %q = %v, want %v; got %+v", tt.caller, tt.target, got, tt.want, callees)
			}
		})
	}
}

// TestCppCallGraphCalleePagination pins callee pagination on evidence-backed
// destinations.
func TestCppCallGraphCalleePagination(t *testing.T) {
	ctx := context.Background()
	s, repo := indexCppFixture(t, map[string]string{"fixture.cpp": `class ApmMap {
public:
    bool LoadWorldMap(char* pFileName);
    bool SaveWorldMap(char* pFileName);
};

bool ApmMap::LoadWorldMap(char* pFileName) { return true; }
bool ApmMap::SaveWorldMap(char* pFileName) { return true; }

bool MultiCallee() {
    ApmMap::LoadWorldMap("x");
    ApmMap::SaveWorldMap("x");
    return true;
}
`})

	cp0, err := s.FindCallees(ctx, repo.ID, "MultiCallee", 0, 1, 0)
	if err != nil {
		t.Fatalf("FindCallees page0: %v", err)
	}
	cp1, err := s.FindCallees(ctx, repo.ID, "MultiCallee", 0, 1, 1)
	if err != nil {
		t.Fatalf("FindCallees page1: %v", err)
	}
	if len(cp0) != 1 || len(cp1) != 1 {
		t.Fatalf("FindCallees pagination: got %d and %d, want 1 and 1", len(cp0), len(cp1))
	}
	if cp0[0].ID == cp1[0].ID {
		t.Fatalf("FindCallees pagination returned duplicate: %q on both pages", cp0[0].QualifiedName)
	}
}

func hasSymbolQName(symbols []graph.Symbol, qname string) bool {
	for _, sym := range symbols {
		if strings.EqualFold(sym.QualifiedName, qname) {
			return true
		}
	}
	return false
}

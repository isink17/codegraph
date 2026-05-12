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

func TestCppCallGraphMemberAndQualifiedNames(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	source := `class ApmMap {
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
	if err := os.WriteFile(filepath.Join(repoRoot, "fixture.cpp"), []byte(source), 0o644); err != nil {
		t.Fatalf("WriteFile(fixture.cpp) error = %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(tsparser.NewCpp()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}

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

	callers, err := s.FindCallers(ctx, repo.ID, "ApmMap::LoadWorldMap", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(ApmMap::LoadWorldMap) error = %v", err)
	}
	unresolved, err := s.CountUnresolvedEdgesByDstName(ctx, repo.ID, "LoadWorldMap")
	if err != nil {
		t.Fatalf("CountUnresolvedEdgesByDstName(LoadWorldMap) error = %v", err)
	}
	if unresolved == 0 {
		t.Fatalf("expected unresolved LoadWorldMap edges for fallback resolution")
	}
	if !hasSymbolQName(callers, "AgcmMinimap::F") {
		t.Fatalf("FindCallers(ApmMap::LoadWorldMap) missing AgcmMinimap::F, got %+v", callers)
	}

	callees, err := s.FindCallees(ctx, repo.ID, "AgcmMinimap::F", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees(AgcmMinimap::F) error = %v", err)
	}
	if !hasSymbolQName(callees, "ApmMap::LoadWorldMap") {
		t.Fatalf("FindCallees(AgcmMinimap::F) missing ApmMap::LoadWorldMap, got %+v", callees)
	}

	shortCallers, err := s.FindCallers(ctx, repo.ID, "LoadWorldMap", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(LoadWorldMap) error = %v", err)
	}
	if len(shortCallers) == 0 {
		t.Fatalf("FindCallers(LoadWorldMap) returned no callers")
	}

	directCallees, err := s.FindCallees(ctx, repo.ID, "Direct", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees(Direct) error = %v", err)
	}
	if !hasSymbolQName(directCallees, "ApmMap::LoadWorldMap") {
		t.Fatalf("FindCallees(Direct) missing ApmMap::LoadWorldMap, got %+v", directCallees)
	}

	leadingCallers, err := s.FindCallers(ctx, repo.ID, "::LoadWorldMap", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(::LoadWorldMap) error = %v", err)
	}
	if len(leadingCallers) == 0 {
		t.Fatalf("FindCallers(::LoadWorldMap) returned no callers")
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

func TestCppCallGraphPagination(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	source := `class ApmMap {
public:
    bool LoadWorldMap(char* pFileName, bool bDecryption = false);
    bool SaveWorldMap(char* pFileName);
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

bool ApmMap::SaveWorldMap(char* pFileName) {
    return true;
}

bool Direct(ApmMap& map) {
    return map.LoadWorldMap("x", false);
}

bool Internal(char* pFileName, bool bDecryption) {
    return LoadWorldMap(pFileName, bDecryption);
}

bool MultiCallee(ApmMap& map) {
    map.LoadWorldMap("x", false);
    map.SaveWorldMap("x");
    return true;
}
`
	if err := os.WriteFile(filepath.Join(repoRoot, "fixture.cpp"), []byte(source), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(tsparser.NewCpp()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}

	// FindCallers pagination: LoadWorldMap has 3 callers (F, Direct, Internal).
	p0, err := s.FindCallers(ctx, repo.ID, "ApmMap::LoadWorldMap", 0, 1, 0)
	if err != nil {
		t.Fatalf("FindCallers page0: %v", err)
	}
	if len(p0) != 1 {
		t.Fatalf("FindCallers(limit=1,offset=0): want 1, got %d: %+v", len(p0), p0)
	}

	p1, err := s.FindCallers(ctx, repo.ID, "ApmMap::LoadWorldMap", 0, 1, 1)
	if err != nil {
		t.Fatalf("FindCallers page1: %v", err)
	}
	if len(p1) != 1 {
		t.Fatalf("FindCallers(limit=1,offset=1): want 1, got %d: %+v", len(p1), p1)
	}
	if p0[0].ID == p1[0].ID {
		t.Fatalf("FindCallers pagination returned duplicate: %q on both pages", p0[0].QualifiedName)
	}

	// FindCallees pagination: MultiCallee calls LoadWorldMap and SaveWorldMap (2 callees).
	cp0, err := s.FindCallees(ctx, repo.ID, "MultiCallee", 0, 1, 0)
	if err != nil {
		t.Fatalf("FindCallees page0: %v", err)
	}
	if len(cp0) != 1 {
		t.Fatalf("FindCallees(limit=1,offset=0): want 1, got %d: %+v", len(cp0), cp0)
	}

	cp1, err := s.FindCallees(ctx, repo.ID, "MultiCallee", 0, 1, 1)
	if err != nil {
		t.Fatalf("FindCallees page1: %v", err)
	}
	if len(cp1) != 1 {
		t.Fatalf("FindCallees(limit=1,offset=1): want 1, got %d: %+v", len(cp1), cp1)
	}
	if cp0[0].ID == cp1[0].ID {
		t.Fatalf("FindCallees pagination returned duplicate: %q on both pages", cp0[0].QualifiedName)
	}
}

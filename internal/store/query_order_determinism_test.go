package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestQueryPagesIgnoreSymbolInsertionOrder(t *testing.T) {
	type result struct {
		search, search2, exact, callers, callers2, impact, impact2, impact3 []string
		impactAmbiguous                                                     bool
	}
	build := func(reverse bool) result {
		t.Helper()
		f := newGateFixture(t)
		paths := []string{"a/caller.go", "z/caller.go"}
		if reverse {
			paths[0], paths[1] = paths[1], paths[0]
		}
		files := make(map[string]int64, len(paths)+1)
		for _, path := range append(paths, "target.go") {
			files[path] = f.file(t, path, "go")
		}
		target := f.symbol(t, files["target.go"], "Target", "pkg.Target", "go")
		callers := make(map[string]int64)
		for _, path := range paths {
			callers[path] = f.symbol(t, files[path], "Caller", "pkg.Caller", "go")
			if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO symbol_fts(repo_id, symbol_id, name, qualified_name) VALUES(?, ?, 'Caller', 'pkg.Caller')`, f.repoID, callers[path]); err != nil {
				t.Fatal(err)
			}
		}
		for _, path := range paths {
			edge, err := insertTestEdge(f.ctx, f.store, f.repoID, files[path], callers[path], "pkg.Target")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, target, edge); err != nil {
				t.Fatal(err)
			}
		}

		search, err := f.store.FindSymbol(f.ctx, f.repoID, "Caller", 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		callerPage, err := f.store.FindCallers(f.ctx, f.repoID, "pkg.Target", target, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		searchPage2, err := f.store.FindSymbol(f.ctx, f.repoID, "Caller", 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		exact, err := f.store.FindSymbolExact(f.ctx, f.repoID, "pkg.Caller", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		callerPage2, err := f.store.FindCallers(f.ctx, f.repoID, "pkg.Target", target, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		impact, err := f.store.ImpactRadius(f.ctx, f.repoID, []string{"pkg.Target"}, nil, 1, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		impactNames := symbolPageIdentity(impact["symbols"].([]graph.Symbol))
		impactPage2, err := f.store.ImpactRadius(f.ctx, f.repoID, []string{"pkg.Target"}, nil, 1, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		impactNames2 := symbolPageIdentity(impactPage2["symbols"].([]graph.Symbol))
		impactPage3, err := f.store.ImpactRadius(f.ctx, f.repoID, []string{"pkg.Target"}, nil, 1, 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		impactNames3 := symbolPageIdentity(impactPage3["symbols"].([]graph.Symbol))
		_, impactErr := f.store.ImpactRadius(f.ctx, f.repoID, []string{"pkg.Caller"}, nil, 1, 1, 0)
		return result{
			search:          symbolPageIdentity(search),
			search2:         symbolPageIdentity(searchPage2),
			exact:           symbolPageIdentity(exact),
			callers:         symbolPageIdentity(callerPage),
			callers2:        symbolPageIdentity(callerPage2),
			impact:          impactNames,
			impact2:         impactNames2,
			impact3:         impactNames3,
			impactAmbiguous: errors.Is(impactErr, ErrSymbolAmbiguous),
		}
	}

	a, b := build(false), build(true)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("different insertion orders returned different pages:\nforward=%+v\nreverse=%+v", a, b)
	}
	want := []string{"a/caller.go:pkg.Caller"}
	if !reflect.DeepEqual(a.search, want) || !reflect.DeepEqual(a.callers, want) {
		t.Fatalf("first-page identity = %+v; want %v", a, want)
	}
	wantExact := []string{"a/caller.go:pkg.Caller", "z/caller.go:pkg.Caller"}
	if !reflect.DeepEqual(a.exact, wantExact) {
		t.Fatalf("exact symbol identities = %v; want %v", a.exact, wantExact)
	}
	if !a.impactAmbiguous {
		t.Fatal("duplicate impact seed stopped failing closed as ambiguous")
	}
	want = []string{"z/caller.go:pkg.Caller"}
	if !reflect.DeepEqual(a.search2, want) || !reflect.DeepEqual(a.callers2, want) {
		t.Fatalf("second-page identity = %+v; want %v", a, want)
	}
	if !reflect.DeepEqual(a.impact, []string{"a/caller.go:pkg.Caller"}) ||
		!reflect.DeepEqual(a.impact2, []string{"target.go:pkg.Target"}) || !reflect.DeepEqual(a.impact3, want) {
		t.Fatalf("impact page identities = %v, %v; want semantic order", a.impact, a.impact2)
	}
}

func symbolPageIdentity(symbols []graph.Symbol) []string {
	got := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		got = append(got, symbol.FilePath+":"+symbol.QualifiedName)
	}
	return got
}

func TestContextNeighborFanoutUsesSemanticIdentity(t *testing.T) {
	inputs := [][]graph.Symbol{
		{{ID: 90, FilePath: "z/caller.go", QualifiedName: "pkg.Caller", Kind: "function", StableKey: "pkg.Caller"},
			{ID: 2, FilePath: "a/caller.go", QualifiedName: "pkg.Caller", Kind: "function", StableKey: "pkg.Caller"}},
		{{ID: 2, FilePath: "a/caller.go", QualifiedName: "pkg.Caller", Kind: "function", StableKey: "pkg.Caller"},
			{ID: 90, FilePath: "z/caller.go", QualifiedName: "pkg.Caller", Kind: "function", StableKey: "pkg.Caller"}},
	}
	for _, input := range inputs {
		page := mergeNeighborPage(input, 1)
		if got := symbolPageIdentity(page); !reflect.DeepEqual(got, []string{"a/caller.go:pkg.Caller"}) {
			t.Fatalf("context fanout page = %v; want semantic first identity", got)
		}
	}
}

func TestContextNeighborPageIgnoresSymbolInsertionOrder(t *testing.T) {
	build := func(reverse bool) []string {
		t.Helper()
		f := newGateFixture(t)
		paths := []string{"a/caller.go", "z/caller.go"}
		if reverse {
			paths[0], paths[1] = paths[1], paths[0]
		}
		files := make(map[string]int64, len(paths)+1)
		for _, path := range append(paths, "target.go") {
			files[path] = f.file(t, path, "go")
		}
		target := f.symbol(t, files["target.go"], "Target", "pkg.Target", "go")
		for _, path := range paths {
			caller := f.symbol(t, files[path], "Caller", "pkg.Caller", "go")
			edge, err := insertTestEdge(f.ctx, f.store, f.repoID, files[path], caller, "pkg.Target")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, target, edge); err != nil {
				t.Fatal(err)
			}
		}
		neighbors, err := f.store.FindContextNeighbors(f.ctx, f.repoID, []ContextSeed{{
			SymbolID: target, QualifiedName: "pkg.Target", ShortName: "Target",
		}}, 1)
		if err != nil {
			t.Fatal(err)
		}
		return symbolPageIdentity(neighbors[0].Callers)
	}

	want := []string{"a/caller.go:pkg.Caller"}
	forward, reverse := build(false), build(true)
	if !reflect.DeepEqual(forward, want) || !reflect.DeepEqual(reverse, want) {
		t.Fatalf("context neighbor page = %v / %v; want %v for both insertion orders", forward, reverse, want)
	}
}

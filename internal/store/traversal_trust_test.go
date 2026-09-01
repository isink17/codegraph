package store

import (
	"context"
	"errors"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func hasSymbolID(symbols []graph.Symbol, id int64) bool {
	for _, symbol := range symbols {
		if symbol.ID == id {
			return true
		}
	}
	return false
}

func TestImpactTrustClosureAndEvidence(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := context.Background()
	fileID, err := insertTestFile(ctx, s, repoID, "trust.go")
	if err != nil {
		t.Fatal(err)
	}
	a, err := insertTestSymbol(ctx, s, repoID, fileID, "A", "pkg.A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := insertTestSymbol(ctx, s, repoID, fileID, "B", "pkg.B")
	if err != nil {
		t.Fatal(err)
	}
	foo, err := insertTestSymbol(ctx, s, repoID, fileID, "Foo", "pkg.Foo")
	if err != nil {
		t.Fatal(err)
	}
	x, err := insertTestSymbol(ctx, s, repoID, fileID, "X", "pkg.X")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = insertTestEdge(ctx, s, repoID, fileID, a, "Foo"); err != nil {
		t.Fatal(err)
	}
	if _, err = insertTestEdge(ctx, s, repoID, fileID, a, "Foo"); err != nil {
		t.Fatal(err)
	}
	if _, err = insertTestEdge(ctx, s, repoID, fileID, a, "Bar"); err != nil {
		t.Fatal(err)
	}
	resolved, err := insertTestEdge(ctx, s, repoID, fileID, a, "pkg.B")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, b, resolved); err != nil {
		t.Fatal(err)
	}
	if _, err = insertTestEdge(ctx, s, repoID, fileID, x, "pkg.B"); err != nil {
		t.Fatal(err)
	}

	for _, page := range []map[string]any{
		must(s.ImpactRadius(ctx, repoID, []string{"pkg.A"}, nil, 1, 1, 0)),
		must(s.ImpactRadius(ctx, repoID, []string{"pkg.A"}, nil, 1, 1, 99)),
	} {
		summary := page["summary"].(map[string]any)
		if summary["unresolved_edges"] != 3 || summary["unresolved_names"] != 2 {
			t.Fatalf("evidence summary = %#v", summary)
		}
	}
	page := must(s.ImpactRadius(ctx, repoID, []string{"pkg.A"}, nil, 0, 10, 0))
	if !hasSymbolID(page["symbols"].([]graph.Symbol), a) || !hasSymbolID(page["symbols"].([]graph.Symbol), b) {
		t.Fatalf("resolved closure omitted seed or target: %#v", page["symbols"])
	}
	if hasSymbolID(page["symbols"].([]graph.Symbol), foo) || hasSymbolID(page["symbols"].([]graph.Symbol), x) {
		t.Fatalf("unresolved spelling expanded closure: %#v", page["symbols"])
	}
}

func TestImpactRadiusIncludesIsolatedFoundSeed(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := context.Background()
	fileID, err := insertTestFile(ctx, s, repoID, "isolated.go")
	if err != nil {
		t.Fatal(err)
	}
	id, err := insertTestSymbol(ctx, s, repoID, fileID, "Known", "pkg.Known")
	if err != nil {
		t.Fatal(err)
	}
	result := must(s.ImpactRadius(ctx, repoID, []string{"Known"}, nil, 1, 10, 0))
	if !hasSymbolID(result["symbols"].([]graph.Symbol), id) {
		t.Fatalf("isolated seed omitted: %#v", result["symbols"])
	}
}

func TestTraceTrustExactAmbiguousAndRepositoryScoped(t *testing.T) {
	s, repoID := newQueryTestStore(t)
	ctx := context.Background()
	fileID, err := insertTestFile(ctx, s, repoID, "trace.go")
	if err != nil {
		t.Fatal(err)
	}
	foo, err := insertTestSymbol(ctx, s, repoID, fileID, "Foo", "pkg.Foo")
	if err != nil {
		t.Fatal(err)
	}
	fooBar, err := insertTestSymbol(ctx, s, repoID, fileID, "FooBar", "pkg.FooBar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = insertTestEdge(ctx, s, repoID, fileID, fooBar, "pkg.Foo"); err != nil {
		t.Fatal(err)
	}
	trace := must(s.TraceDependenciesResult(ctx, repoID, "Foo", "downstream", 1, 10, 0))
	if !trace.TargetFound || trace.Total != 1 || trace.Dependencies[0]["symbol"] != "pkg.Foo" {
		t.Fatalf("exact Foo trace = %+v", trace)
	}
	if _, err = insertTestSymbol(ctx, s, repoID, fileID, "Foo", "a.Foo"); err != nil {
		t.Fatal(err)
	}
	if _, err = insertTestSymbol(ctx, s, repoID, fileID, "Foo", "b.Foo"); err != nil {
		t.Fatal(err)
	}
	_, err = s.TraceDependenciesResult(ctx, repoID, "Foo", "downstream", 1, 10, 0)
	if !errors.Is(err, ErrSymbolAmbiguous) {
		t.Fatalf("ambiguous trace error = %v", err)
	}
	missing := must(s.TraceDependenciesResult(ctx, repoID, "NoSuchSymbol", "downstream", 1, 10, 0))
	if missing.TargetFound || missing.Total != 0 || len(missing.Dependencies) != 0 {
		t.Fatalf("missing trace = %+v", missing)
	}
	foreignRepo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foreignFile, err := insertTestFile(ctx, s, foreignRepo.ID, "foreign.go")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := insertTestSymbol(ctx, s, foreignRepo.ID, foreignFile, "Foreign", "foreign.Foreign")
	if err != nil {
		t.Fatal(err)
	}
	localEdge, err := insertTestEdge(ctx, s, repoID, fileID, foo, "foreign.Foreign")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, foreign, localEdge); err != nil {
		t.Fatal(err)
	}
	if got := must(s.TraceDependenciesResult(ctx, repoID, "pkg.Foo", "downstream", 1, 10, 0)); got.Total != 1 {
		t.Fatalf("cross-repo downstream leaked: %+v", got)
	}
	foreignSource, err := insertTestSymbol(ctx, s, foreignRepo.ID, foreignFile, "Caller", "foreign.Caller")
	if err != nil {
		t.Fatal(err)
	}
	foreignEdge, err := insertTestEdge(ctx, s, foreignRepo.ID, foreignFile, foreignSource, "pkg.Foo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, foo, foreignEdge); err != nil {
		t.Fatal(err)
	}
	if got := must(s.TraceDependenciesResult(ctx, repoID, "pkg.Foo", "upstream", 1, 10, 0)); got.Total != 1 {
		t.Fatalf("cross-repo upstream leaked: %+v", got)
	}
}

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

func TestSingularQueryAmbiguityParity(t *testing.T) {
	for _, tc := range []struct {
		name, query, firstName, firstQualified, secondName, secondQualified string
	}{
		{"bare name", "Shared", "Shared", "a.Shared", "Shared", "b.Shared"},
		{"qualified name", "pkg.Shared", "Shared", "pkg.Shared", "Shared", "pkg.Shared"},
		{"qualified suffix", "ns.Shared", "First", "a.Shared", "Second", "b.Shared"},
		{"short fallback", "ns.Shared", "Shared", "First", "Shared", "Second"},
	} {
		for _, reverse := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/forward", true: "/reverse"}[reverse], func(t *testing.T) {
				s, repoID := newQueryTestStore(t)
				ctx := context.Background()
				pairs := [][2]string{{tc.firstName, tc.firstQualified}, {tc.secondName, tc.secondQualified}}
				if reverse {
					pairs[0], pairs[1] = pairs[1], pairs[0]
				}
				for i, pair := range pairs {
					fileID, err := insertTestFile(ctx, s, repoID, []string{"a.go", "b.go"}[i])
					if err != nil {
						t.Fatal(err)
					}
					if _, err := insertTestSymbol(ctx, s, repoID, fileID, pair[0], pair[1]); err != nil {
						t.Fatal(err)
					}
				}
				if ids, err := s.lookupSymbolIDs(ctx, repoID, tc.query, 0); err != nil || len(ids) != 2 {
					t.Fatalf("plural lookup = %v, %v; want both candidates", ids, err)
				}
				checks := map[string]func() error{
					"callers":         func() error { _, err := s.FindCallers(ctx, repoID, tc.query, 0, 10, 0); return err },
					"callees":         func() error { _, err := s.FindCallees(ctx, repoID, tc.query, 0, 10, 0); return err },
					"caller presence": func() error { _, err := s.FindCallersResult(ctx, repoID, tc.query, 0, 10, 0); return err },
					"callee presence": func() error { _, err := s.FindCalleesResult(ctx, repoID, tc.query, 0, 10, 0); return err },
					"lookup":          func() error { _, err := s.lookupSymbolID(ctx, repoID, tc.query, 0); return err },
					"impact": func() error {
						_, err := s.ImpactRadius(ctx, repoID, []string{"Absent", tc.query}, nil, 1, 10, 0)
						return err
					},
					"related tests":    func() error { _, err := s.RelatedTests(ctx, repoID, tc.query, "", 10, 0); return err },
					"related presence": func() error { _, err := s.RelatedTestsResult(ctx, repoID, tc.query, "", 10, 0); return err },
					"trace": func() error {
						_, _, err := s.TraceDependencies(ctx, repoID, tc.query, "downstream", 1, 10, 0)
						return err
					},
					"trace presence": func() error {
						_, err := s.TraceDependenciesResult(ctx, repoID, tc.query, "downstream", 1, 10, 0)
						return err
					},
				}
				for name, check := range checks {
					if err := check(); !errors.Is(err, ErrSymbolAmbiguous) || errors.Is(err, ErrSymbolNotFound) {
						t.Errorf("%s(%q) error = %v; want ambiguity", name, tc.query, err)
					}
				}
			})
		}
	}
}

func TestSingularQueryPrecedenceParity(t *testing.T) {
	for _, tc := range []struct {
		name, query, targetName, targetQualified, otherName, otherQualified string
	}{
		{"qualified beats bare", "Shared", "Shared", "Shared", "Shared", "other.Shared"},
		{"qualified beats conflicting name", "pkg.Shared", "Shared", "pkg.Shared", "pkg.Shared", "other.Name"},
		{"name beats suffix", "Shared", "Shared", "Target", "Other", "other.Shared"},
		{"unique suffix", "ns.Shared", "Other", "pkg.Shared", "Unrelated", "other.Unrelated"},
		{"unique short fallback", "ns.Shared", "Shared", "Target", "Unrelated", "Other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repoID := newQueryTestStore(t)
			ctx := context.Background()
			fileID, err := insertTestFile(ctx, s, repoID, "precedence.go")
			if err != nil {
				t.Fatal(err)
			}
			// Insert distractor first so ID/insertion order cannot choose target.
			if _, err := insertTestSymbol(ctx, s, repoID, fileID, tc.otherName, tc.otherQualified); err != nil {
				t.Fatal(err)
			}
			targetID, err := insertTestSymbol(ctx, s, repoID, fileID, tc.targetName, tc.targetQualified)
			if err != nil {
				t.Fatal(err)
			}
			if id, err := s.lookupSymbolID(ctx, repoID, tc.query, 0); err != nil || id != targetID {
				t.Fatalf("lookup = %d, %v; want %d", id, err, targetID)
			}
			if id, err := s.lookupSymbolID(ctx, repoID, "ignored", targetID); err != nil || id != targetID {
				t.Fatalf("exact ID = %d, %v; want %d", id, err, targetID)
			}
			impact := must(s.ImpactRadius(ctx, repoID, []string{tc.query, "Absent", tc.query}, nil, 1, 10, 0))
			presence := impact["seed_presence"].(ImpactSeedPresence)
			syms := impact["symbols"].([]graph.Symbol)
			if presence.Requested != 3 || presence.Found != 2 || len(presence.Missing) != 1 || len(syms) != 1 || syms[0].ID != targetID {
				t.Fatalf("impact = %#v; want unique target with duplicate request presence", impact)
			}
			trace := must(s.TraceDependenciesResult(ctx, repoID, tc.query, "downstream", 1, 10, 0))
			if !trace.TargetFound || trace.Total != 1 || trace.Dependencies[0]["symbol"] != tc.targetQualified {
				t.Fatalf("trace = %+v; want only %q", trace, tc.targetQualified)
			}
			if result := must(s.RelatedTestsResult(ctx, repoID, tc.query, "", 10, 0)); !result.TargetFound || len(result.Tests) != 0 {
				t.Fatalf("related = %+v; want found empty result", result)
			}
		})
	}
}

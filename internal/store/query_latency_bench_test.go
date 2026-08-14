package store

import (
	"context"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/latency"
	"github.com/isink17/codegraph/internal/limits"
)

// P12 latency suite for local graph queries against the ~100k-symbol fixture.
//
// Every scenario runs the *production* store call with its default or normal
// bounded arguments -- limit 20, depth 2 -- because that is the contract the
// 50ms budget is claimed against. Scenarios that are unbounded by contract
// (impact radius over a hot hub, trace over a chain) are measured and reported
// as they are rather than quietly narrowed to fit the budget.
//
// Percentiles are reported as custom metrics instead of relying on ns/op:
// ns/op is a mean, and a mean hides exactly the tail this phase is about.

// queryBudget is the P12 target for a warm bounded local graph query.
const queryBudget = 50 * time.Millisecond

type queryScenario struct {
	name string
	// bounded reports whether the scenario's public contract bounds the work,
	// not merely the output. Unbounded scenarios are still measured; they are
	// just not held to the 50ms budget without qualification.
	bounded bool
	run     func(ctx context.Context, f *bigGraphFixture) (int, error)
}

func bigGraphScenarios(f *bigGraphFixture) []queryScenario {
	const limit = 20
	return []queryScenario{
		{"stats", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			st, err := f.store.Stats(ctx, f.repoID)
			return len(st.Languages), err
		}},
		{"symbol_lookup_exact", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.FindSymbolExact(ctx, f.repoID, f.HubTarget, limit, 0)
			return len(out), err
		}},
		{"symbol_lookup_missing", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.FindSymbolExact(ctx, f.repoID, f.MissingName, limit, 0)
			return len(out), err
		}},
		{"symbol_search_fts", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.SearchSymbols(ctx, f.repoID, f.FTSQuery, limit, 0)
			return len(out), err
		}},
		{"symbol_search_ambiguous", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.SearchSymbols(ctx, f.repoID, f.CommonName, limit, 0)
			return len(out), err
		}},
		{"callers_low_degree", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.FindCallers(ctx, f.repoID, f.LowTarget, 0, limit, 0)
			return len(out), err
		}},
		{"callers_medium_degree", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.FindCallers(ctx, f.repoID, f.MediumTarget, 0, limit, 0)
			return len(out), err
		}},
		{"callers_hub", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.FindCallers(ctx, f.repoID, f.HubTarget, 0, limit, 0)
			return len(out), err
		}},
		{"callees_low_degree", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.FindCallees(ctx, f.repoID, f.PlainSymbol, 0, limit, 0)
			return len(out), err
		}},
		{"callees_hub", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.FindCallees(ctx, f.repoID, f.HubSource, 0, limit, 0)
			return len(out), err
		}},
		{"callees_unresolved_fanout", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.FindCallees(ctx, f.repoID, f.UnresolvedSource, 0, limit, 0)
			return len(out), err
		}},
		{"related_tests_symbol", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.RelatedTests(ctx, f.repoID, f.HotSymbol, "", limit, 0)
			return len(out), err
		}},
		{"related_tests_file_fanout", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.RelatedTests(ctx, f.repoID, "", f.HotFile, limit, 0)
			return len(out), err
		}},
		{"list_files", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.ListFiles(ctx, f.repoID, "internal/pkg001", limit, 0)
			return len(out), err
		}},
		{"dead_code", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.FindDeadCode(ctx, f.repoID, limit, 0)
			return len(out), err
		}},
		{"semantic_search", true, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.SemanticSearch(ctx, f.repoID, "scheduler backoff", limit, 0)
			return len(out), err
		}},
		{"impact_radius_low_degree", false, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.ImpactRadius(ctx, f.repoID, []string{f.LowTarget}, nil, 2, limits.MaxPage, 0)
			return impactSize(out), err
		}},
		{"impact_radius_file", false, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.ImpactRadius(ctx, f.repoID, nil, []string{f.PlainFile}, 2, limits.MaxPage, 0)
			return impactSize(out), err
		}},
		{"impact_radius_hub", false, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.ImpactRadius(ctx, f.repoID, []string{f.HubTarget}, nil, 2, limits.MaxPage, 0)
			return impactSize(out), err
		}},
		{"trace_dependencies_chain", false, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, _, err := f.store.TraceDependencies(ctx, f.repoID, f.ChainHead, "downstream", 3, limits.MaxPage, 0)
			return len(out), err
		}},
		{"architecture_overview", false, func(ctx context.Context, f *bigGraphFixture) (int, error) {
			out, err := f.store.ArchitectureOverview(ctx, f.repoID)
			return len(out), err
		}},
	}
}

func impactSize(out map[string]any) int {
	if out == nil {
		return 0
	}
	if syms, ok := out["symbols"].([]any); ok {
		return len(syms)
	}
	if summary, ok := out["summary"].(map[string]any); ok {
		if n, ok := summary["affected_symbols"].(int); ok {
			return n
		}
	}
	return 0
}

func BenchmarkQueryLatency100k(b *testing.B) {
	ctx := context.Background()
	f := bigGraph(b)
	b.Logf("sqlite_driver=%s", SQLiteDriverName())
	b.Logf("fixture: %s", f.Describe())

	for _, sc := range bigGraphScenarios(f) {
		b.Run(sc.name, func(b *testing.B) {
			// One warm call before the timer: the budget is stated for a warm
			// database, and the first touch of a cold btree page is not that.
			count, err := sc.run(ctx, f)
			if err != nil {
				b.Fatalf("%s: %v", sc.name, err)
			}

			samples := make(latency.Samples, 0, b.N)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				start := time.Now()
				if _, err := sc.run(ctx, f); err != nil {
					b.Fatalf("%s: %v", sc.name, err)
				}
				samples = append(samples, time.Since(start))
			}
			b.StopTimer()

			sorted := samples.Sorted()
			b.ReportMetric(latency.Millis(sorted.SortedPercentile(50)), "p50_ms")
			b.ReportMetric(latency.Millis(sorted.SortedPercentile(95)), "p95_ms")
			b.ReportMetric(latency.Millis(sorted.Max()), "max_ms")
			b.ReportMetric(float64(count), "results")
			if !sc.bounded {
				b.Logf("scenario %s is unbounded by contract; budget applies with that caveat", sc.name)
			}
		})
	}
}

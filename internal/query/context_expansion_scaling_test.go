package query

import (
	"context"
	"fmt"
	"testing"
)

// The regression guard for P19's central claim, stated at the layer an agent
// actually calls: neighbour expansion is one batched round trip whatever the
// seed count.
//
// The pre-P19 shape made two store calls per expanded seed. If this test starts
// reporting a count that tracks the seed count, expansion has grown a per-seed
// loop again -- which is the failure the eight-seed ceiling existed to contain,
// and the ceiling is gone.
func TestNeighbourExpansionDoesNotScaleWithSeedCount(t *testing.T) {
	for _, seeds := range []int{1, 8, 12, 30} {
		t.Run(fmt.Sprintf("seeds%d", seeds), func(t *testing.T) {
			fx := newWideFixture(t, max(seeds, callerOfSeed))
			counter := newCountingStore(fx.svc.ctxStore)
			fx.svc.ctxStore = counter
			opts := wideOpts(seeds)
			opts.MaxSymbols = seeds
			mustContext(t, fx, wideTask, opts)

			if got := counter.calls["FindContextNeighbors"]; got != 1 {
				t.Fatalf("%d seeds made %d neighbour calls, want 1", seeds, got)
			}
			if got := counter.calls["SymbolNameCounts"]; got != 1 {
				t.Fatalf("%d seeds made %d SymbolNameCounts calls, want 1", seeds, got)
			}
			// Every seed is in the batch, not a prefix of them.
			if len(counter.seeds) != 1 || len(counter.seeds[0]) != min(seeds, contextExpansionSeeds) {
				t.Fatalf("%d seeds produced batches %v, want one batch of %d",
					seeds, batchSizes(counter), min(seeds, contextExpansionSeeds))
			}
		})
	}
}

// Graph expansion is bounded by contextExpansionSeeds even when MaxSymbols is
// far above the default. The extra seeds still reach the answer as direct
// matches; what stays bounded is the graph work.
func TestLargeMaxSymbolsDoesNotWidenExpansion(t *testing.T) {
	const seeds = defaultContextMaxSymbols + 20
	fx := newWideFixture(t, seeds)
	counter := newCountingStore(fx.svc.ctxStore)
	fx.svc.ctxStore = counter

	opts := wideOpts(seeds)
	res := mustContext(t, fx, wideTask, opts)

	if len(counter.seeds) != 1 || len(counter.seeds[0]) != contextExpansionSeeds {
		t.Fatalf("MaxSymbols=%d expanded %v seeds, want %d", seeds, batchSizes(counter), contextExpansionSeeds)
	}
	direct := 0
	for _, sym := range allSymbols(res) {
		if sym.Relevance == relevanceDirectMatch {
			direct++
		}
	}
	if direct <= contextExpansionSeeds {
		t.Fatalf("only %d direct matches survived; the seed set should still be MaxSymbols-wide", direct)
	}
}

func batchSizes(c *countingContextStore) []int {
	out := make([]int, 0, len(c.seeds))
	for _, b := range c.seeds {
		out = append(out, len(b))
	}
	return out
}

// BenchmarkContextExpansionBySeedCount is the measurement behind P19's latency
// claim. Before batching the expansion stage was linear in the seed count; it
// is now dominated by the fixed pipeline.
func BenchmarkContextExpansionBySeedCount(b *testing.B) {
	for _, seeds := range []int{1, 8, 30} {
		b.Run(fmt.Sprintf("seeds%d", seeds), func(b *testing.B) {
			fx := newWideFixture(b, max(seeds, callerOfSeed))
			opts := wideOpts(seeds)
			opts.MaxSymbols = seeds
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := fx.svc.rankContextCandidates(ctx, fx.repoID, wideTask, opts); err != nil {
					b.Fatalf("rankContextCandidates() error = %v", err)
				}
			}
		})
	}
}

// BenchmarkContextRelatedTestsShare isolates the related-test leg, which is
// still one query per returned file. P19 deliberately left it alone; this is
// the measurement that says whether that is still defensible.
func BenchmarkContextRelatedTestsShare(b *testing.B) {
	for _, tests := range []bool{false, true} {
		b.Run(fmt.Sprintf("tests%v", tests), func(b *testing.B) {
			fx := newContextFixtureForBench(b)
			opts := ContextForTaskOptions{IncludeCallers: true, IncludeTests: tests}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := fx.svc.rankContextCandidates(ctx, fx.repoID, taskPayment, opts); err != nil {
					b.Fatalf("rankContextCandidates() error = %v", err)
				}
			}
		})
	}
}

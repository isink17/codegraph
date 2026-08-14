package query

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

// countingContextStore counts the round trips a context request makes.
type countingContextStore struct {
	inner contextStoreOps
	calls map[string]int
}

func newCountingStore(inner contextStoreOps) *countingContextStore {
	return &countingContextStore{inner: inner, calls: map[string]int{}}
}

func (c *countingContextStore) LastScanID(ctx context.Context, repoID int64) (int64, error) {
	c.calls["LastScanID"]++
	return c.inner.LastScanID(ctx, repoID)
}

func (c *countingContextStore) SymbolsForRefs(ctx context.Context, repoID int64, refs []store.SymbolRef) (map[store.SymbolRef]graph.Symbol, error) {
	c.calls["SymbolsForRefs"]++
	return c.inner.SymbolsForRefs(ctx, repoID, refs)
}

func (c *countingContextStore) SymbolNameCounts(ctx context.Context, repoID int64, names []string) (map[string]int, error) {
	c.calls["SymbolNameCounts"]++
	return c.inner.SymbolNameCounts(ctx, repoID, names)
}

func (c *countingContextStore) FindCallers(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error) {
	c.calls["FindCallers"]++
	return c.inner.FindCallers(ctx, repoID, symbol, symbolID, limit, offset)
}

func (c *countingContextStore) FindCallees(ctx context.Context, repoID int64, symbol string, symbolID int64, limit, offset int) ([]graph.Symbol, error) {
	c.calls["FindCallees"]++
	return c.inner.FindCallees(ctx, repoID, symbol, symbolID, limit, offset)
}

func (c *countingContextStore) RelatedTests(ctx context.Context, repoID int64, symbol, file string, limit, offset int) ([]store.RelatedTest, error) {
	c.calls["RelatedTests"]++
	return c.inner.RelatedTests(ctx, repoID, symbol, file, limit, offset)
}

// Seed resolution and test-symbol resolution are batched, and expansion is
// bounded by the seed and file counts -- not by the size of the repository.
// This is the guard against the N+1 storm that fixing seed parsing could
// otherwise have introduced.
func TestContextForTaskQueryCountIsBounded(t *testing.T) {
	fx := newContextFixture(t)
	counter := newCountingStore(fx.svc.ctxStore)
	fx.svc.ctxStore = counter

	opts := fullOpts()
	opts.MaxTokens = MaxContextMaxTokens
	res := mustContext(t, fx, taskPayment, opts)

	seeds := 0
	for _, sym := range allSymbols(res) {
		if sym.Relevance == relevanceDirectMatch {
			seeds++
		}
	}
	files := len(res.Files)

	// Two: the generation is read before and after ranking, so a page cannot mix
	// generations.
	if got := counter.calls["LastScanID"]; got != 2 {
		t.Fatalf("LastScanID calls = %d, want 2", got)
	}
	// One batch for the seeds, one for the test symbols.
	if got := counter.calls["SymbolsForRefs"]; got > 2 {
		t.Fatalf("SymbolsForRefs calls = %d, want at most 2 (one per batch)", got)
	}
	if got := counter.calls["SymbolNameCounts"]; got != 1 {
		t.Fatalf("SymbolNameCounts calls = %d, want 1", got)
	}
	// Expansion is one call per expanded seed per direction, and expansion covers
	// at most contextExpansionSeeds seeds however many the search returned. This
	// is the bound that keeps a context request off the N+1 path.
	if got := counter.calls["FindCallers"]; got > contextExpansionSeeds {
		t.Fatalf("FindCallers calls = %d, want at most contextExpansionSeeds (%d)", got, contextExpansionSeeds)
	}
	if counter.calls["FindCallers"] != counter.calls["FindCallees"] {
		t.Fatalf("callers/callees call counts differ: %d vs %d", counter.calls["FindCallers"], counter.calls["FindCallees"])
	}
	if want := min(seeds, contextExpansionSeeds); counter.calls["FindCallers"] < want {
		t.Fatalf("FindCallers calls = %d for %d seeds, want at least %d", counter.calls["FindCallers"], seeds, want)
	}
	// Related tests are fetched per returned file, not per candidate.
	if got := counter.calls["RelatedTests"]; got > files+len(res.TestFiles) {
		t.Fatalf("RelatedTests calls = %d for %d returned files", got, files)
	}
}

// A continuation must not re-expand more than the first page did: the ranking is
// recomputed, and that cost is bounded by the same numbers.
func TestContinuationQueryCountMatchesFirstPage(t *testing.T) {
	fx := newContextFixture(t)
	first := newCountingStore(fx.svc.ctxStore)
	fx.svc.ctxStore = first

	small := fullOpts()
	small.MaxTokens = 200
	page1, err := fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, small)
	if err != nil {
		t.Fatalf("page 1 error = %v", err)
	}
	if !page1.HasMore {
		t.Fatal("page 1 exhausted the stream; nothing to continue")
	}
	firstCounts := map[string]int{}
	for k, v := range first.calls {
		firstCounts[k] = v
	}

	second := newCountingStore(first.inner)
	fx.svc.ctxStore = second
	call := small
	call.Cursor = page1.NextCursor
	if _, err := fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, call); err != nil {
		t.Fatalf("page 2 error = %v", err)
	}
	for name, got := range second.calls {
		if got > firstCounts[name] {
			t.Fatalf("continuation made %d %s calls, first page made %d", got, name, firstCounts[name])
		}
	}
}

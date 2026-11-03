package query

import (
	"context"
	"errors"
	"testing"
)

// A cursor this package hands out must always advance. One candidate wider
// than max_tokens used to end the walk with a permanent error, which the
// ranking change made reachable: the page now steps over it instead, and
// every page still fits the budget.
func TestContinuationAlwaysAdvancesPastAnOversizedCandidate(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()
	stranded := 0
	walked := 0
	for budget := 120; budget <= 1200; budget++ {
		opts := fullOpts()
		opts.MaxTokens = budget
		page, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, opts)
		if err != nil {
			if errors.Is(err, ErrContextBudgetTooSmall) {
				continue
			}
			t.Fatalf("first page at max_tokens=%d: %v", budget, err)
		}
		walked++
		seen := 0
		for page.NextCursor != "" {
			if page.EstimatedTokens > budget {
				t.Fatalf("page at max_tokens=%d reported %d tokens", budget, page.EstimatedTokens)
			}
			next := opts
			next.Cursor = page.NextCursor
			page, err = fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, next)
			if err != nil {
				stranded++
				t.Errorf("continuation at max_tokens=%d: %v", budget, err)
				break
			}
			seen++
			if seen > 10_000 {
				t.Fatalf("walk at max_tokens=%d did not terminate", budget)
			}
		}
	}
	if walked < 100 {
		t.Fatalf("only %d budgets were walkable; the sweep proved little", walked)
	}
	if stranded != 0 {
		t.Fatalf("%d budgets stranded a cursor", stranded)
	}
}

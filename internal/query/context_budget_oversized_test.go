package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/tokenest"
)

// oversizedStream builds a ranked stream whose candidate at index wide is far
// wider than the others, so a budget exists that serves its neighbours and
// cannot serve it.
func oversizedStream(t *testing.T, n, wide int) []contextCandidate {
	t.Helper()
	candidates := make([]contextCandidate, 0, n)
	for i := range n {
		doc := "short"
		if i == wide {
			doc = strings.Repeat("w", maxContextDocChars)
		}
		candidates = append(candidates, contextCandidate{
			sym: graph.Symbol{
				ID:            int64(i + 1),
				Name:          fmt.Sprintf("Sym%02d", i),
				QualifiedName: fmt.Sprintf("pkg.Sym%02d", i),
				FilePath:      fmt.Sprintf("pkg/f%02d.go", i),
				Language:      "go",
				StableKey:     fmt.Sprintf("k%02d", i),
				DocSummary:    doc,
			},
			relevance: relevanceDirectMatch,
		})
	}
	return candidates
}

func budgetInputAt(candidates []contextCandidate, offset, maxTokens int) budgetInput {
	return budgetInput{
		task:       taskPayment,
		candidates: candidates,
		offset:     offset,
		maxTokens:  maxTokens,
		cursor:     contextCursorState{V: contextCursorVersion, Repo: 1, Scan: 1, Task: "t", Opts: "o", Rank: "r"},
	}
}

// candidateTokens is what one candidate costs on a page of its own.
func candidateTokens(t *testing.T, cand contextCandidate) int {
	t.Helper()
	cost, err := candidateCostBytes(cand, map[string]bool{})
	if err != nil {
		t.Fatalf("candidateCostBytes() error = %v", err)
	}
	return tokenest.FromBytes(cost)
}

// A budget that cannot carry the first candidate of the stream is reported, not
// worked around: the caller has not spent a cursor yet and can simply ask for
// more.
func TestFirstPageRefusesAnOversizedCandidate(t *testing.T) {
	stream := oversizedStream(t, 6, 0)
	narrow := candidateTokens(t, stream[0])
	_, err := buildBudgetedContext(budgetInputAt(stream, 0, narrow))
	if !errors.Is(err, ErrContextBudgetTooSmall) {
		t.Fatalf("error = %v, want ErrContextBudgetTooSmall", err)
	}
}

// The same refusal on a continuation must not consume the candidate: the offset
// the request carried is still the offset a later, larger request resumes from,
// and that request returns the candidate the narrow one could not.
func TestContinuationPreservesAnOversizedCandidate(t *testing.T) {
	const wide = 2
	stream := oversizedStream(t, 6, wide)
	narrow := candidateTokens(t, stream[wide]) - 1

	if _, err := buildBudgetedContext(budgetInputAt(stream, wide, narrow)); !errors.Is(err, ErrContextBudgetTooSmall) {
		t.Fatalf("narrow continuation error = %v, want ErrContextBudgetTooSmall", err)
	}

	page, err := buildBudgetedContext(budgetInputAt(stream, wide, MaxContextMaxTokens))
	if err != nil {
		t.Fatalf("wide continuation error = %v", err)
	}
	if got := symbolKeys(page); len(got) == 0 || !strings.Contains(got[0], stream[wide].sym.QualifiedName) {
		t.Fatalf("resumed page = %v, want it to start at %s", got, stream[wide].sym.QualifiedName)
	}
}

// A page that is served stays inside the budget it was given, on the first page
// and on every continuation.
func TestServedPagesStayWithinBudget(t *testing.T) {
	stream := oversizedStream(t, 8, 5)
	for budget := 200; budget <= 2000; budget += 7 {
		offset := 0
		for offset < len(stream) {
			page, err := buildBudgetedContext(budgetInputAt(stream, offset, budget))
			if err != nil {
				if errors.Is(err, ErrContextBudgetTooSmall) {
					break
				}
				t.Fatalf("budget=%d offset=%d error = %v", budget, offset, err)
			}
			if page.EstimatedTokens > budget {
				t.Fatalf("budget=%d offset=%d page reported %d tokens", budget, offset, page.EstimatedTokens)
			}
			if page.ReturnedSymbols == 0 {
				break
			}
			offset += page.ReturnedSymbols
		}
	}
}

// Walking the stream to exhaustion, raising the budget whenever a candidate is
// too wide for the current one, must reproduce the ranked stream exactly: every
// identity once, in order, none dropped.
func TestExhaustiveWalkLosesNoCandidate(t *testing.T) {
	stream := oversizedStream(t, 8, 3)
	want := make([]string, 0, len(stream))
	for _, cand := range stream {
		want = append(want, cand.sym.QualifiedName)
	}

	for budget := 200; budget <= 1200; budget += 11 {
		var got []string
		offset, raises := 0, 0
		for offset < len(stream) {
			at := budget
			if raises > 0 {
				at = MaxContextMaxTokens
			}
			page, err := buildBudgetedContext(budgetInputAt(stream, offset, at))
			if err != nil {
				if errors.Is(err, ErrContextBudgetTooSmall) && raises == 0 {
					// The contract the refusal promises: same offset, bigger budget.
					raises++
					continue
				}
				t.Fatalf("budget=%d offset=%d error = %v", budget, offset, err)
			}
			raises = 0
			for _, key := range symbolKeys(page) {
				got = append(got, strings.TrimSuffix(key, "#"+page.Task))
			}
			if page.ReturnedSymbols == 0 {
				break
			}
			offset += page.ReturnedSymbols
		}
		if len(got) != len(want) {
			t.Fatalf("budget=%d walked %d symbols, want %d", budget, len(got), len(want))
		}
		for i := range want {
			if !strings.Contains(got[i], want[i]) {
				t.Fatalf("budget=%d position %d = %q, want %q", budget, i, got[i], want[i])
			}
		}
	}
}

// End to end through the service: a budget that strands a real cursor refuses
// it recoverably, and replaying that exact cursor with a larger max_tokens
// serves the page. max_tokens is deliberately outside the cursor's option
// fingerprint so the replay is not a staleness mismatch.
func TestStrandedCursorIsRecoverableWithALargerBudget(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()
	recovered := 0
	for budget := 120; budget <= 1200; budget++ {
		opts := fullOpts()
		opts.MaxTokens = budget
		page, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, opts)
		if err != nil {
			continue
		}
		for page.NextCursor != "" {
			next := opts
			next.Cursor = page.NextCursor
			stranded := page.NextCursor
			page, err = fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, next)
			if err == nil {
				continue
			}
			if !errors.Is(err, ErrContextBudgetTooSmall) {
				t.Fatalf("continuation at max_tokens=%d error = %v", budget, err)
			}
			retry := opts
			retry.Cursor = stranded
			retry.MaxTokens = MaxContextMaxTokens
			resumed, retryErr := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, retry)
			if retryErr != nil {
				t.Fatalf("retry of the stranded cursor at max_tokens=%d: %v", budget, retryErr)
			}
			if resumed.ReturnedSymbols == 0 {
				t.Fatalf("retry of the stranded cursor at max_tokens=%d returned nothing", budget)
			}
			recovered++
			break
		}
	}
	if recovered == 0 {
		t.Fatal("no budget stranded a cursor; the recovery path was never exercised")
	}
}

// Staleness is decided before the budget is: a cursor stranded by a small
// budget and then replayed against a re-ranked stream is refused as stale, not
// served and not reported as a budget problem.
func TestStrandedCursorStillFailsClosedOnRankingDrift(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()
	opts := fullOpts()
	opts.MaxTokens = 162
	page, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, opts)
	if err != nil {
		t.Fatalf("first page error = %v", err)
	}
	if page.NextCursor == "" {
		t.Fatal("first page carries no cursor")
	}
	drifted := opts
	drifted.Cursor = page.NextCursor
	drifted.MaxTokens = MaxContextMaxTokens
	drifted.IncludeCallers = !opts.IncludeCallers
	if _, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, drifted); !errors.Is(err, ErrContextCursorStale) {
		t.Fatalf("error = %v, want ErrContextCursorStale", err)
	}
}

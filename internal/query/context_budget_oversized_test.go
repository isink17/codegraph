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

// oversizedStream builds a ranked stream whose candidate at index wide costs
// noticeably more than the others -- 92 tokens against 53, the doc summary
// being capped at maxContextDocChars -- so a band of budgets exists that
// serves its neighbours and cannot serve it. The band is narrow, which is why
// the sweeps below start low enough to enter it and assert that they did.
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
	refusals := 0
	for budget := 105; budget <= 2000; budget += 7 {
		offset := 0
		for offset < len(stream) {
			page, err := buildBudgetedContext(budgetInputAt(stream, offset, budget))
			if err != nil {
				if errors.Is(err, ErrContextBudgetTooSmall) {
					refusals++
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
	if refusals == 0 {
		t.Fatal("no budget refused a page; the sweep never reached the oversized candidate")
	}
}

// Walking the stream to exhaustion, raising the budget whenever a candidate is
// too wide for the current one, must reproduce the ranked stream exactly: every
// identity once, in order, none dropped.
func TestExhaustiveWalkLosesNoCandidate(t *testing.T) {
	stream := oversizedStream(t, 8, 3)
	want := make([]string, 0, len(stream))
	for _, cand := range stream {
		want = append(want, cand.sym.StableKey+"|"+cand.sym.QualifiedName)
	}

	// The refusal at offset 3 fires for budgets 57-140, and a budget below
	// ~101 cannot serve even a narrow candidate, so the window that walks the
	// whole stream with exactly one raise is 105-140. The sweep covers it and
	// the budgets above it, and totalRaises below fails if it ever stops
	// landing in it.
	totalRaises := 0
	for budget := 105; budget <= 1200; budget++ {
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
					totalRaises++
					continue
				}
				t.Fatalf("budget=%d offset=%d error = %v", budget, offset, err)
			}
			raises = 0
			got = append(got, symbolKeys(page)...)
			if page.ReturnedSymbols == 0 {
				break
			}
			offset += page.ReturnedSymbols
		}
		if len(got) != len(want) {
			t.Fatalf("budget=%d walked %d symbols, want %d", budget, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("budget=%d position %d = %q, want %q", budget, i, got[i], want[i])
			}
		}
	}
	if totalRaises == 0 {
		t.Fatal("no budget exercised the refusal; the walk proved nothing about the oversized candidate")
	}
}

// strandingBudget searches for a budget at which this fixture serves a first
// page and then refuses the continuation because the next candidate is wider
// than the budget, and returns that budget with the cursor it stranded.
//
// The band is found rather than hardcoded: the costs that place it are the
// serialized sizes of a fixture the fixture owns, and they do not have to be
// the same on every platform or after any change to the response record.
func strandingBudget(t *testing.T, fx *contextFixture) (int, string) {
	t.Helper()
	ctx := context.Background()
	for budget := 120; budget <= 1200; budget++ {
		opts := fullOpts()
		opts.MaxTokens = budget
		page, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, opts)
		if err != nil {
			continue
		}
		for page.NextCursor != "" {
			stranded := page.NextCursor
			next := opts
			next.Cursor = stranded
			page, err = fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, next)
			if err == nil {
				continue
			}
			if !errors.Is(err, ErrContextBudgetTooSmall) {
				t.Fatalf("continuation at max_tokens=%d error = %v", budget, err)
			}
			return budget, stranded
		}
	}
	t.Fatal("no budget stranded a cursor; the oversized-candidate path was never reached")
	return 0, ""
}

// End to end through the service: a budget that strands a real cursor refuses
// it recoverably, and replaying that exact cursor with a larger max_tokens
// serves the page. max_tokens is deliberately outside the cursor's option
// fingerprint so the replay is not a staleness mismatch.
func TestStrandedCursorIsRecoverableWithALargerBudget(t *testing.T) {
	fx := newContextFixture(t)
	budget, stranded := strandingBudget(t, fx)

	retry := fullOpts()
	retry.Cursor = stranded
	retry.MaxTokens = MaxContextMaxTokens
	resumed, err := fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, retry)
	if err != nil {
		t.Fatalf("retry of the cursor stranded at max_tokens=%d: %v", budget, err)
	}
	if resumed.ReturnedSymbols == 0 {
		t.Fatalf("retry of the cursor stranded at max_tokens=%d returned nothing", budget)
	}
}

// Staleness is decided before the budget is: a cursor stranded by a small
// budget and then replayed against a re-ranked stream is refused as stale, not
// served and not reported as a budget problem. The budget stays at the value
// that stranded it, so both failure modes are live and the one that wins is
// the one under test.
func TestStrandedCursorStillFailsClosedOnRankingDrift(t *testing.T) {
	fx := newContextFixture(t)
	budget, stranded := strandingBudget(t, fx)

	drifted := fullOpts()
	drifted.MaxTokens = budget
	drifted.Cursor = stranded
	drifted.IncludeCallers = !drifted.IncludeCallers
	_, err := fx.svc.ContextForTask(context.Background(), fx.repoID, taskPayment, drifted)
	if !errors.Is(err, ErrContextCursorStale) {
		t.Fatalf("error = %v, want ErrContextCursorStale", err)
	}
	if errors.Is(err, ErrContextBudgetTooSmall) {
		t.Fatalf("budget refusal preempted the staleness check: %v", err)
	}
}

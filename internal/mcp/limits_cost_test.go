package mcp

import (
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/limits"
	"github.com/isink17/codegraph/internal/tokenest"
)

// TestOversizedRequestIsRejectedBeforeAnyQuery proves the ordering P18 depends
// on: an out-of-range argument must cost a string comparison, not a hundred
// million rows followed by an apology.
//
// The proof is negative and exact. The store is closed first, so every code
// path that reaches the database fails loudly; a clean validation message can
// therefore only mean the request never got that far.
func TestOversizedRequestIsRejectedBeforeAnyQuery(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	if err := server.store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// A valid request must now fail at the database, which is what makes the
	// rejected ones below meaningful.
	if _, text := callResult(t, server, "search_symbols", map[string]any{"query": "Helper", "limit": 10}); !strings.Contains(text, "closed") {
		t.Fatalf("a valid request against a closed store should fail at the database, got: %s", text)
	}

	for _, args := range []map[string]any{
		{"query": "Helper", "limit": 100000000},
		{"query": "Helper", "limit": -1},
		{"query": "Helper", "offset": -1},
		{"query": "Helper", "limit": 51, "detail": "full"},
	} {
		msg := callError(t, server, "search_symbols", args)
		if strings.Contains(msg, "closed") {
			t.Fatalf("args %v reached the database before validation: %s", args, msg)
		}
		if !strings.Contains(msg, "must be") {
			t.Fatalf("args %v produced %q, want a validation message", args, msg)
		}
	}

	// Depth is validated on the same pass, before any traversal begins.
	if msg := callError(t, server, "get_impact_radius", map[string]any{"symbols": []any{"Helper"}, "depth": 1000000}); strings.Contains(msg, "closed") {
		t.Fatalf("an out-of-range depth reached the database: %s", msg)
	}
}

// TestDatabaseFailureIsNotReportedAsNotFound guards the other direction of the
// not-found work: only genuine absence becomes ErrSymbolNotFound. A broken
// database that reported "symbol not found" would send a caller hunting for a
// typo that is not there.
func TestDatabaseFailureIsNotReportedAsNotFound(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	if err := server.store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	msg := callError(t, server, "find_related_tests", map[string]any{"symbol": "Helper"})
	if strings.Contains(msg, "not found") {
		t.Fatalf("a database failure was reported as a missing symbol: %q", msg)
	}
	if !strings.Contains(msg, "closed") {
		t.Fatalf("error = %q, want the database failure to survive", msg)
	}
}

// TestLegalMaximumResponseStaysWithinItsBudget is the output half of the
// policy. The ceilings were chosen from measured bytes per row, so the test
// asserts the budget they were chosen for rather than an exact size, which
// would move with the fixture.
func TestLegalMaximumResponseStaysWithinItsBudget(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)

	// The budget each detail level's ceiling was sized against, with headroom
	// for a fixture whose rows are wider than the ones that were measured.
	const budgetTokens = 60_000

	for _, tc := range []struct {
		level string
		limit int
	}{
		{"card", limits.MaxPage},
		{"skeleton", limits.MaxPageSkeleton},
		{"excerpt", limits.MaxPageSource},
		{"full", limits.MaxPageSource},
	} {
		isErr, text := callResult(t, server, "search_symbols", map[string]any{
			"query": "Helper", "limit": tc.limit, "detail": tc.level,
		})
		if isErr {
			t.Fatalf("the legal maximum at detail=%s was rejected: %s", tc.level, text)
		}
		if got := tokenest.OfString(text); got > budgetTokens {
			t.Fatalf("detail=%s at limit=%d cost %d estimated tokens, over the %d budget", tc.level, tc.limit, got, budgetTokens)
		}
	}
}

// TestRejectedRequestPayloadStaysSmall checks that refusing an absurd request
// is itself cheap: the error names the bound, and does not echo the request or
// carry a partial result.
func TestRejectedRequestPayloadStaysSmall(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	_, text := callResult(t, server, "search_symbols", map[string]any{"query": "Helper", "limit": 100000000})
	if got := tokenest.OfString(text); got > 100 {
		t.Fatalf("a rejection cost %d estimated tokens, want a short message: %s", got, text)
	}
}

// TestResultsBelowTheCeilingAreUnchanged is the compatibility half: P18 bounds
// the extremes and must not alter an ordinary page.
func TestResultsBelowTheCeilingAreUnchanged(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	for _, limit := range []int{1, 5, limits.DefaultPage, 100} {
		got := searchNamesPage(t, server, limit, 0)
		want := searchNamesPage(t, server, limits.MaxPage, 0)
		if len(want) > limit {
			want = want[:limit]
		}
		if len(got) != len(want) {
			t.Fatalf("limit=%d returned %d rows, want %d", limit, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("limit=%d row %d = %q, want %q", limit, i, got[i], want[i])
			}
		}
	}
}

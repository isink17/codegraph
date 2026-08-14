package query

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// seedPolicy returns the evidence policy the ranking layer decided on for each
// expanded seed of one request.
func seedPolicy(t *testing.T, fx *contextFixture, task string, opts ContextForTaskOptions) map[string]store.ContextSeed {
	t.Helper()
	counter := newCountingStore(fx.svc.ctxStore)
	fx.svc.ctxStore = counter
	mustContext(t, fx, task, opts)
	out := map[string]store.ContextSeed{}
	for _, batch := range counter.seeds {
		for _, sd := range batch {
			out[sd.QualifiedName] = sd
		}
	}
	return out
}

// The ambiguity gate is asked about the short name the evidence legs actually
// match on, and it is a gate on those legs only: the seed keeps its id and its
// qualified name whatever the answer.
func TestSeedEvidencePolicyFollowsShortNameAmbiguity(t *testing.T) {
	fx := newContextFixture(t)
	opts := fullOpts()
	opts.MaxTokens = MaxContextMaxTokens
	got := seedPolicy(t, fx, "renew billing subscription payment", opts)

	for _, qname := range []string{"billing.Renew", "subscription.Renew"} {
		sd, ok := got[qname]
		if !ok {
			t.Fatalf("%s was not expanded; seeds = %v", qname, got)
		}
		if sd.AllowShortEvidence {
			t.Fatalf("%s allows short evidence, but `Renew` names two symbols", qname)
		}
		// The recall half of P19: the gate must not also take the exact
		// qualified name away.
		if sd.QualifiedName != qname || sd.SymbolID == 0 {
			t.Fatalf("%s lost its exact identity: %+v", qname, sd)
		}
	}
}

// A short name only one symbol carries keeps the pre-P19 recall.
func TestSeedEvidencePolicyKeepsUniqueShortNameRecall(t *testing.T) {
	fx := newContextFixture(t)
	opts := fullOpts()
	opts.MaxTokens = MaxContextMaxTokens
	got := seedPolicy(t, fx, taskPayment, opts)

	sd, ok := got["paymentsvc.ProcessPayment"]
	if !ok {
		t.Fatalf("ProcessPayment was not expanded; seeds = %v", got)
	}
	if !sd.AllowShortEvidence {
		t.Fatalf("ProcessPayment is repo-unique but short evidence is blocked: %+v", sd)
	}
	if sd.ShortName != "ProcessPayment" {
		t.Fatalf("ShortName = %q, want the name the suffix patterns are built from", sd.ShortName)
	}
}

// A same-named symbol in another package must not appear as this seed's caller.
func TestAmbiguousSeedDoesNotInheritTheOtherPackagesCallers(t *testing.T) {
	fx := newContextFixture(t)
	opts := fullOpts()
	opts.MaxTokens = MaxContextMaxTokens
	opts.MaxFiles = 20
	res := mustContext(t, fx, "renew billing subscription payment", opts)

	// SubscriptionDriver calls subscription.Renew and nothing in billing. If it
	// arrives as caller context it can only have come through a bare-name match.
	for _, sym := range allSymbols(res) {
		if sym.QualifiedName == "subscription.SubscriptionDriver" && sym.Relevance == relevanceCaller {
			t.Fatalf("subscription.SubscriptionDriver appears as caller context; ambiguous evidence leaked across packages")
		}
	}
}

// The candidate universe is a function of the ranking options and the graph --
// never of the token budget. P14's cursor contract lets max_tokens change
// between pages of one sequence, so a universe that grew with the budget would
// make page 2 point into a different stream than page 1 was cut from.
func TestCandidateUniverseIsIndependentOfMaxTokens(t *testing.T) {
	fx := newWideFixture(t, 12)
	ctx := context.Background()
	opts := wideOpts(12)

	var want string
	for _, budget := range []int{600, 2000, 8000, MaxContextMaxTokens} {
		call := opts
		call.MaxTokens = budget
		candidates, err := fx.svc.rankContextCandidates(ctx, fx.repoID, wideTask, call)
		if err != nil {
			t.Fatalf("rankContextCandidates(max_tokens=%d) error = %v", budget, err)
		}
		got := rankingFingerprint(candidates)
		if want == "" {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("max_tokens=%d changed the candidate stream: fingerprint %s, want %s", budget, got, want)
		}
	}
}

// Pagination over the widened universe: no gaps, no duplicates, and the
// concatenation is the whole ranked stream. The budget may change between
// pages; the stream may not.
func TestWideContextPaginationIsComplete(t *testing.T) {
	fx := newWideFixture(t, 12)
	ctx := context.Background()
	whole := wideOpts(12)
	reference := symbolKeys(mustContext(t, fx, wideTask, whole))

	call := wideOpts(12)
	call.MaxTokens = 700
	var paged []string
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page > 200 {
			t.Fatal("pagination did not terminate")
		}
		res, err := fx.svc.ContextForTask(ctx, fx.repoID, wideTask, call)
		if err != nil {
			t.Fatalf("page %d error = %v", page, err)
		}
		for _, key := range symbolKeys(res) {
			if seen[key] {
				t.Fatalf("page %d repeated %s", page, key)
			}
			seen[key] = true
			paged = append(paged, key)
		}
		if !res.HasMore {
			break
		}
		call.Cursor = res.NextCursor
		// Vary the budget between pages: P14 allows it, and the candidate
		// universe must not notice.
		call.MaxTokens = 700 + 300*(page%3)
	}

	if len(paged) != len(reference) {
		t.Fatalf("paged %d symbols, whole answer has %d", len(paged), len(reference))
	}
	for i := range reference {
		if paged[i] != reference[i] {
			t.Fatalf("paged[%d] = %s, whole answer has %s", i, paged[i], reference[i])
		}
	}
}

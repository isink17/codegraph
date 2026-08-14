package query

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func cand(relevance, path, name string, signal float64) contextCandidate {
	return contextCandidate{
		sym: graph.Symbol{
			ID:            1,
			Name:          strings.TrimPrefix(name, path+"."),
			QualifiedName: name,
			FilePath:      path,
			StableKey:     "func:" + name,
			Kind:          "function",
		},
		relevance:  relevance,
		taskSignal: signal,
		isTest:     relevance == relevanceTest,
	}
}

// The scoring bands are the ranking contract; assert the band edges rather than
// individual magic numbers.
func TestScoringModelBands(t *testing.T) {
	maxBonus := float64(maxFileSupport) * fileSupportUnit

	strongDirect := cand(relevanceDirectMatch, "a.go", "a.Strong", 1).baseScore()
	weakDirect := cand(relevanceDirectMatch, "a.go", "a.Weak", 0).baseScore()
	strongCaller := cand(relevanceCaller, "b.go", "b.Caller", 1).baseScore()
	strongCallee := cand(relevanceCallee, "b.go", "b.Callee", 1).baseScore()
	strongTest := cand(relevanceTest, "c_test.go", "c.Test", 1).baseScore()
	weakCallee := cand(relevanceCallee, "b.go", "b.Cold", 0).baseScore()

	if strongDirect <= strongCaller+maxBonus {
		t.Fatalf("a strong direct match (%v) must outrank any caller (%v + %v bonus)", strongDirect, strongCaller, maxBonus)
	}
	if strongDirect <= strongTest+maxBonus {
		t.Fatalf("a strong direct match (%v) must outrank any test (%v)", strongDirect, strongTest)
	}
	if strongCaller <= weakDirect {
		t.Fatalf("a caller of the best seed (%v) should outrank a near-irrelevant hit (%v)", strongCaller, weakDirect)
	}
	if strongCaller <= strongCallee {
		t.Fatalf("callers (%v) rank ahead of callees (%v)", strongCaller, strongCallee)
	}
	if strongTest > weakCallee {
		t.Fatalf("the best test (%v) must not outrank implementation context (%v)", strongTest, weakCallee)
	}
}

// Hub fan-in is not a scoring input; the most a shared file can add is the
// capped support bonus, which cannot lift a callee past a direct match.
func TestHubFanInCannotOutrankDirectMatch(t *testing.T) {
	set := newCandidateSet()
	set.add(cand(relevanceDirectMatch, "svc.go", "svc.Target", 1))
	for i := 0; i < 50; i++ {
		c := cand(relevanceCallee, "hub.go", "hub.Helper"+string(rune('A'+i%26))+string(rune('a'+i/26)), 1)
		set.add(c)
	}
	ranked := set.finalize()
	if ranked[0].sym.QualifiedName != "svc.Target" {
		t.Fatalf("first candidate = %q, want svc.Target", ranked[0].sym.QualifiedName)
	}
}

func TestDeduplicationKeepsStrongestRelevance(t *testing.T) {
	set := newCandidateSet()
	set.add(cand(relevanceCallee, "a.go", "a.Sym", 1))
	set.add(cand(relevanceDirectMatch, "a.go", "a.Sym", 0.5))
	set.add(cand(relevanceCaller, "a.go", "a.Sym", 1))
	set.add(cand(relevanceTest, "a.go", "a.Sym", 1))
	ranked := set.finalize()
	if len(ranked) != 1 {
		t.Fatalf("candidates = %d, want 1 after dedup: %+v", len(ranked), ranked)
	}
	if ranked[0].relevance != relevanceDirectMatch {
		t.Fatalf("relevance = %q, want direct_match", ranked[0].relevance)
	}
}

// Equal scores must break by class, then file path, then stable identity.
func TestTiesBreakLexically(t *testing.T) {
	set := newCandidateSet()
	// One candidate per file, so the file-support bonus is equal and the scores
	// tie exactly; only the declared tie-breaks can order these.
	set.add(cand(relevanceCallee, "z.go", "z.Two", 0.5))
	set.add(cand(relevanceCallee, "a.go", "a.Two", 0.5))
	set.add(cand(relevanceCaller, "m.go", "m.One", 0.5))
	ranked := set.finalize()
	got := []string{ranked[0].sym.QualifiedName, ranked[1].sym.QualifiedName, ranked[2].sym.QualifiedName}
	want := []string{"m.One", "a.Two", "z.Two"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// Insertion order -- which is map iteration order in the caller -- must not
// reach the output.
func TestRandomizedInsertionOrderIsByteIdentical(t *testing.T) {
	base := []contextCandidate{
		cand(relevanceDirectMatch, "svc.go", "svc.Alpha", 1),
		cand(relevanceDirectMatch, "svc.go", "svc.Beta", 1),
		cand(relevanceDirectMatch, "other.go", "other.Gamma", 0.5),
		cand(relevanceCaller, "api.go", "api.Handler", 1),
		cand(relevanceCaller, "api.go", "api.Other", 0.25),
		cand(relevanceCallee, "hub.go", "hub.Log", 1),
		cand(relevanceCallee, "hub.go", "hub.Trace", 1),
		cand(relevanceTest, "svc_test.go", "svc.TestAlpha", 1),
		cand(relevanceTest, "svc_test.go", "svc.TestBeta", 0.5),
	}

	reference := ""
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 20; round++ {
		shuffled := make([]contextCandidate, len(base))
		copy(shuffled, base)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		set := newCandidateSet()
		for _, c := range shuffled {
			set.add(c)
		}
		ranked := set.finalize()
		files, testFiles := projectCandidates(ranked, fileRanks(ranked))
		payload, err := json.Marshal(graph.TaskContext{Task: "t", Files: files, TestFiles: testFiles})
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		fingerprint := rankingFingerprint(ranked) + "|" + string(payload)
		if round == 0 {
			reference = fingerprint
			continue
		}
		if fingerprint != reference {
			t.Fatalf("round %d differs:\n%s\n%s", round, reference, fingerprint)
		}
	}
}

func TestRankingFingerprintChangesWithOrder(t *testing.T) {
	a := []contextCandidate{cand(relevanceDirectMatch, "a.go", "a.One", 1), cand(relevanceCaller, "b.go", "b.Two", 1)}
	b := []contextCandidate{cand(relevanceDirectMatch, "a.go", "a.One", 1), cand(relevanceCaller, "b.go", "b.Three", 1)}
	if rankingFingerprint(a) == rankingFingerprint(b) {
		t.Fatal("different candidate streams share a fingerprint")
	}
	if rankingFingerprint(a) != rankingFingerprint(a) {
		t.Fatal("fingerprint is not stable for one stream")
	}
}

func TestNormalizeTaskSignalUsesScoresThenRank(t *testing.T) {
	hits := []searchHit{
		{File: "a.go", QualifiedName: "a.A", Score: 4, Rank: 0},
		{File: "b.go", QualifiedName: "b.B", Score: 1, Rank: 1},
	}
	signals := normalizeTaskSignal(hits)
	if signals[0] != 1 || signals[1] != 0.25 {
		t.Fatalf("signals = %v, want [1 0.25]", signals)
	}

	unscored := []searchHit{
		{File: "a.go", QualifiedName: "a.A", Rank: 0},
		{File: "b.go", QualifiedName: "b.B", Rank: 1},
	}
	fallback := normalizeTaskSignal(unscored)
	if !(fallback[0] > fallback[1]) {
		t.Fatalf("without scores, rank must order the signals: %v", fallback)
	}
}

// Integration: on the real fixture the task's own symbol leads, its caller
// follows, and the unrelated hub trails -- without asserting exact scores.
func TestRankingOrderOnFixture(t *testing.T) {
	fx := newContextFixture(t)
	opts := fullOpts()
	opts.MaxTokens = MaxContextMaxTokens
	res := mustContext(t, fx, taskPayment, opts)

	order := map[string]int{}
	for i, sym := range allSymbols(res) {
		order[sym.QualifiedName] = i
	}
	must := func(name string) int {
		pos, ok := order[name]
		if !ok {
			t.Fatalf("%s missing from context: %v", name, order)
		}
		return pos
	}
	if must("paymentsvc.ProcessPayment") >= must("handler.HandleCheckout") {
		t.Fatalf("direct match must precede its caller: %v", order)
	}
	if must("handler.HandleCheckout") >= must("util.Log") {
		t.Fatalf("caller of the seed must precede the unrelated hub: %v", order)
	}
}

// Regression for the same-name expansion hazard: semantic search picks one
// Renew, and expansion must follow that symbol's graph, not the other package's.
func TestSameNameSeedExpandsOwnGraph(t *testing.T) {
	fx := newContextFixture(t)
	opts := fullOpts()
	opts.MaxTokens = MaxContextMaxTokens
	res := mustContext(t, fx, "renew billing payment", opts)

	names := map[string]string{}
	for _, sym := range allSymbols(res) {
		names[sym.QualifiedName] = sym.Relevance
	}
	if _, ok := names["billing.Renew"]; !ok {
		t.Fatalf("expected billing.Renew as the seed: %v", names)
	}
	if rel, ok := names["subscription.SubscriptionDriver"]; ok {
		t.Fatalf("subscription.SubscriptionDriver leaked in as %q: the other Renew's caller was followed", rel)
	}
	if rel, ok := names["subscription.Renew"]; ok && rel != relevanceDirectMatch {
		t.Fatalf("subscription.Renew appeared as %q rather than as its own search hit", rel)
	}
}

// Regression: a stable key is scoped to a package or a file's base name, not to a
// path, so two files can carry the same key. Dedup must keep both.
func TestIdentityKeepsSameStableKeyInDifferentFiles(t *testing.T) {
	cgo := contextCandidate{
		sym:        graph.Symbol{ID: 1, Name: "isSQLiteBusy", QualifiedName: "store.isSQLiteBusy", FilePath: "internal/store/sqlite_busy_cgo.go", StableKey: "func:store::isSQLiteBusy"},
		relevance:  relevanceDirectMatch,
		taskSignal: 1,
	}
	modernc := contextCandidate{
		sym:        graph.Symbol{ID: 2, Name: "isSQLiteBusy", QualifiedName: "store.isSQLiteBusy", FilePath: "internal/store/sqlite_busy_modernc.go", StableKey: "func:store::isSQLiteBusy"},
		relevance:  relevanceDirectMatch,
		taskSignal: 0.5,
	}
	if cgo.identity() == modernc.identity() {
		t.Fatalf("identities collide across files: %q", cgo.identity())
	}
	set := newCandidateSet()
	set.add(cgo)
	set.add(modernc)
	ranked := set.finalize()
	if len(ranked) != 2 {
		t.Fatalf("candidates = %d, want 2: %+v", len(ranked), ranked)
	}
}

// The bands overlap only at their edges, and the overlap is the documented one:
// a strong caller may pass a hit the search rated near zero, and nothing else.
func TestBandOverlapIsBoundedToWeakDirectMatches(t *testing.T) {
	maxBonus := float64(maxFileSupport) * fileSupportUnit
	strongCallerWithBonus := cand(relevanceCaller, "b.go", "b.Caller", 1).baseScore() + maxBonus
	directWithSignal := cand(relevanceDirectMatch, "a.go", "a.Hit", 0.3).baseScore()
	weakDirect := cand(relevanceDirectMatch, "a.go", "a.Noise", 0).baseScore() + maxBonus

	if directWithSignal <= strongCallerWithBonus {
		t.Fatalf("a hit with a 0.3 signal (%v) must stay ahead of the best caller (%v)", directWithSignal, strongCallerWithBonus)
	}
	if strongCallerWithBonus <= weakDirect {
		t.Fatalf("the best caller (%v) should pass a zero-signal hit (%v)", strongCallerWithBonus, weakDirect)
	}
	bestTest := cand(relevanceTest, "a_test.go", "a.TestNoise", 1).baseScore() + maxBonus
	worstCallee := cand(relevanceCallee, "b.go", "b.Cold", 0).baseScore()
	if bestTest > worstCallee {
		t.Fatalf("the best test (%v) must not outrank implementation context (%v)", bestTest, worstCallee)
	}
}

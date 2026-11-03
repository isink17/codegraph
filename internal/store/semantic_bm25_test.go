package store

import (
	"context"
	"fmt"
	"testing"
)

// token is one (text, term frequency) pair of a synthetic symbol.
type scoredToken struct {
	text   string
	weight float64
}

func seedScoredSymbol(t *testing.T, ctx context.Context, s *Store, repoID, fileID int64, name string, tokens ...scoredToken) int64 {
	t.Helper()
	symID, err := insertTestSymbol(ctx, s, repoID, fileID, name, "pkg."+name)
	if err != nil {
		t.Fatalf("insertTestSymbol(%s): %v", name, err)
	}
	for _, tok := range tokens {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO symbol_tokens(symbol_id, token, weight) VALUES(?, ?, ?)`,
			symID, tok.text, tok.weight); err != nil {
			t.Fatalf("insert token %q: %v", tok.text, err)
		}
	}
	return symID
}

func searchOrder(t *testing.T, ctx context.Context, s *Store, repoID int64, query string) []string {
	t.Helper()
	rows, err := s.SemanticSearch(ctx, repoID, query, 50, 0)
	if err != nil {
		t.Fatalf("SemanticSearch(%q): %v", query, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprint(r["symbol"]))
	}
	return out
}

// A word carried by most of the corpus must not outweigh one carried by two
// symbols. This is the measured OkHttp failure in miniature: a long symbol
// repeating `the` outscored the small implementation the task named.
func TestSemanticSearchRanksByInverseDocumentFrequency(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	// `the` is everywhere; `quiesce` is in one symbol.
	for i := 0; i < 40; i++ {
		seedScoredSymbol(t, ctx, s, repoID, fileID, fmt.Sprintf("Filler%02d", i), scoredToken{"the", 3})
	}
	seedScoredSymbol(t, ctx, s, repoID, fileID, "Decoy", scoredToken{"the", 30})
	seedScoredSymbol(t, ctx, s, repoID, fileID, "Target", scoredToken{"quiesce", 1}, scoredToken{"the", 1})

	got := searchOrder(t, ctx, s, repoID, "the quiesce")
	if len(got) == 0 || got[0] != "pkg.Target" {
		t.Fatalf("ranking = %v, want pkg.Target first: one rare term must beat thirty common ones", got)
	}
}

// Two symbols matching the same term, one of them ten times longer: the short
// one is the better answer and must rank first. Without length normalization
// the long one wins on volume, which is the Kotlin signature-body bug.
func TestSemanticSearchNormalizesForSymbolLength(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	long := []scoredToken{{"renew", 2}}
	for i := 0; i < 40; i++ {
		long = append(long, scoredToken{fmt.Sprintf("filler%02d", i), 3})
	}
	seedScoredSymbol(t, ctx, s, repoID, fileID, "LongBody", long...)
	seedScoredSymbol(t, ctx, s, repoID, fileID, "Short", scoredToken{"renew", 1})
	// Background corpus, so `renew` is not in every symbol.
	for i := 0; i < 20; i++ {
		seedScoredSymbol(t, ctx, s, repoID, fileID, fmt.Sprintf("Other%02d", i), scoredToken{"unrelated", 1})
	}

	got := searchOrder(t, ctx, s, repoID, "renew")
	if len(got) < 2 || got[0] != "pkg.Short" {
		t.Fatalf("ranking = %v, want pkg.Short first: a long symbol must not win on volume", got)
	}
}

// Repeating one word must saturate rather than accumulate linearly.
func TestSemanticSearchSaturatesRepeatedTerms(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	seedScoredSymbol(t, ctx, s, repoID, fileID, "Once", scoredToken{"renew", 1})
	seedScoredSymbol(t, ctx, s, repoID, fileID, "Hundred", scoredToken{"renew", 100})
	for i := 0; i < 20; i++ {
		seedScoredSymbol(t, ctx, s, repoID, fileID, fmt.Sprintf("Other%02d", i), scoredToken{"unrelated", 1})
	}
	rows, err := s.SemanticSearch(ctx, repoID, "renew", 10, 0)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	score := map[string]float64{}
	for _, r := range rows {
		score[fmt.Sprint(r["symbol"])] = r["score"].(float64)
	}
	once, hundred := score["pkg.Once"], score["pkg.Hundred"]
	if once <= 0 || hundred <= 0 {
		t.Fatalf("both symbols must score: once=%v hundred=%v", once, hundred)
	}
	if hundred/once > 3 {
		t.Fatalf("a hundredfold term frequency scored %.2fx a single one; k1 is not saturating", hundred/once)
	}
}

// A symbol whose file row is deleted must not score, and must not contribute
// to N, to the average length, or to any token's document frequency either --
// a ghost inflating the corpus statistics would skew every live symbol.
func TestSemanticSearchIgnoresGhostSymbolsInCorpusStatistics(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	liveFile, err := insertTestFile(ctx, s, repoID, "live.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	deadFile, err := insertTestFile(ctx, s, repoID, "dead.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	seedScoredSymbol(t, ctx, s, repoID, liveFile, "Live", scoredToken{"renew", 1})
	for i := 0; i < 30; i++ {
		seedScoredSymbol(t, ctx, s, repoID, deadFile, fmt.Sprintf("Ghost%02d", i), scoredToken{"renew", 50})
	}

	withGhosts, err := s.corpusTokenStatsFor(ctx, repoID)
	if err != nil {
		t.Fatalf("corpusTokenStatsFor: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET is_deleted = 1 WHERE id = ?`, deadFile); err != nil {
		t.Fatalf("delete file: %v", err)
	}
	// The cache is keyed by scan generation, so force a recompute the way a
	// re-index would.
	s.tokenStats.put(repoID, corpusTokenStats{Scan: -1})
	afterDelete, err := s.corpusTokenStatsFor(ctx, repoID)
	if err != nil {
		t.Fatalf("corpusTokenStatsFor: %v", err)
	}
	if afterDelete.Symbols != 1 {
		t.Fatalf("live symbol count = %d, want 1 (ghosts excluded)", afterDelete.Symbols)
	}
	if afterDelete.AvgLen >= withGhosts.AvgLen {
		t.Fatalf("average length %v did not drop after the ghosts went away (was %v)", afterDelete.AvgLen, withGhosts.AvgLen)
	}
	got := searchOrder(t, ctx, s, repoID, "renew")
	if len(got) != 1 || got[0] != "pkg.Live" {
		t.Fatalf("ranking = %v, want only pkg.Live", got)
	}
}

// The corpus statistics are cached, so the cache must follow the graph
// generation and never serve a stale corpus.
func TestCorpusTokenStatsCacheFollowsTheScanGeneration(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	seedScoredSymbol(t, ctx, s, repoID, fileID, "One", scoredToken{"renew", 1})

	first, err := s.corpusTokenStatsFor(ctx, repoID)
	if err != nil {
		t.Fatalf("corpusTokenStatsFor: %v", err)
	}
	seedScoredSymbol(t, ctx, s, repoID, fileID, "Two", scoredToken{"renew", 1}, scoredToken{"extra", 5})

	cached, err := s.corpusTokenStatsFor(ctx, repoID)
	if err != nil {
		t.Fatalf("corpusTokenStatsFor: %v", err)
	}
	if cached != first {
		t.Fatalf("statistics recomputed without a new scan: %+v then %+v", first, cached)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scans(repo_id, scan_kind, started_at, status) VALUES(?, 'full', '2026-01-01T00:00:00Z', 'finished')`,
		repoID); err != nil {
		t.Fatalf("record scan: %v", err)
	}
	fresh, err := s.corpusTokenStatsFor(ctx, repoID)
	if err != nil {
		t.Fatalf("corpusTokenStatsFor: %v", err)
	}
	if fresh.Symbols != 2 {
		t.Fatalf("after a new scan the statistics must be recomputed, got %+v", fresh)
	}
}

func TestBM25IDFIsMonotonicAndNonNegative(t *testing.T) {
	const n = 1000
	prev := bm25IDF(n, 1)
	if prev <= 0 {
		t.Fatalf("idf(1 of %d) = %v, want > 0", n, prev)
	}
	for df := int64(2); df <= n; df *= 2 {
		got := bm25IDF(n, df)
		if got < 0 {
			t.Fatalf("idf(%d of %d) = %v, must never be negative", df, n, got)
		}
		if got >= prev {
			t.Fatalf("idf(%d) = %v did not fall below idf of the rarer term (%v)", df, got, prev)
		}
		prev = got
	}
	if bm25IDF(n, 0) != 0 {
		t.Fatalf("a token no symbol carries must contribute nothing")
	}
}

package store

import (
	"context"
	"math"
	"sync"
)

// BM25 parameters. k1 saturates repeated terms so a word written fifteen
// times in one body stops behaving like fifteen distinct matches; b is the
// usual 0.75 length normalization. Both are the standard defaults, pinned
// here rather than made configurable: a tuning knob on a public ranking is a
// compatibility surface, and nothing in the retrieval benchmark justified
// moving them.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
	// bm25ScorePrecision rounds the summed score before it is ordered and
	// returned. A float sum over a GROUP BY depends on the order SQLite
	// aggregates rows in, which the planner may change; rounding makes a
	// disagreement between two runs unlikely rather than impossible, since
	// two orders can still straddle a rounding boundary. What actually makes
	// a page boundary deterministic is the tie-break column chain in the
	// ORDER BY, not this.
	bm25ScorePrecision = 6
)

// corpusTokenStats is the per-repository statistics BM25 needs beyond the
// per-symbol term frequencies already in symbol_tokens.
//
// Symbols is N, the number of live symbols in the repository, and AvgLen is
// the mean token mass of one symbol. Both describe the whole corpus, so
// neither can be derived from the rows a single query matches.
type corpusTokenStats struct {
	Scan    int64
	Symbols int64
	AvgLen  float64
}

// tokenStatsCache holds the statistics per repository, keyed by the
// generation of the newest scan that has finished running.
//
// Computing them costs a full aggregate over symbol_tokens -- tens of
// milliseconds on a repository of a few thousand symbols, which is more than
// the query it would serve. They change only when the repository is
// re-indexed, so caching them is what keeps BM25 cheaper than the scoring it
// replaces, and is why a persisted column behind a migration was not needed.
//
// The key deliberately excludes a scan that is still running. A scan row is
// inserted with status 'running' before the first symbol of that scan is
// written, so keying on MAX(id) alone would let a query that lands in the
// middle of an index compute pre-scan statistics and store them under the
// post-scan generation -- where they would be served until the scan after
// next. That is not merely stale: df is recomputed live on every query, so a
// cached N from before the scan can be smaller than a token's current df,
// and ln(1 + (N-df+0.5)/(df+0.5)) then goes negative and inverts the order.
// See also the max(symbols, df) floor in SemanticSearch, which makes that
// arithmetic unreachable however the cache behaves.
type tokenStatsCache struct {
	mu sync.RWMutex
	by map[int64]corpusTokenStats
}

func (c *tokenStatsCache) get(repoID, scan int64) (corpusTokenStats, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	got, ok := c.by[repoID]
	return got, ok && got.Scan == scan
}

// forget drops a repository's entry. A token refresh rewrites document
// lengths without moving the scan generation the cache is keyed by, so the
// key alone would keep serving an average computed over the old tokenization.
//
// What makes this sufficient, rather than a race, is that a refresh only ever
// runs on a repository whose stored version is stale, and every reader of
// these statistics passes requireCurrentSemanticTokens first and is refused
// while that is true. So no reader can be holding a pre-refresh aggregate to
// put back after the forget. If that ever stops holding -- a refresh on a
// current repository, or a reader that reaches corpusTokenStatsFor without
// the guard -- this needs a generation counter checked by put, not a delete.
// The consequence would be a stale average rather than an inverted ranking;
// the max(symbols, df) floor in SemanticSearch keeps idf non-negative however
// this cache behaves.
func (c *tokenStatsCache) forget(repoID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.by, repoID)
}

func (c *tokenStatsCache) put(repoID int64, stats corpusTokenStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = map[int64]corpusTokenStats{}
	}
	c.by[repoID] = stats
}

// corpusTokenStatsFor returns the repository's BM25 corpus statistics,
// recomputing them only when the graph generation has moved.
func (s *Store) corpusTokenStatsFor(ctx context.Context, repoID int64) (corpusTokenStats, error) {
	// Read the generation through the same queryer as the aggregate below. A
	// read that has pinned a snapshot must label the statistics with the
	// generation of that snapshot, not with one a commit landed afterwards.
	var scan int64
	err := s.parserSemanticQueryer(ctx).QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM scans WHERE repo_id = ? AND status <> 'running'`,
		repoID).Scan(&scan)
	if err != nil {
		return corpusTokenStats{}, err
	}
	if got, ok := s.tokenStats.get(repoID, scan); ok {
		return got, nil
	}
	stats := corpusTokenStats{Scan: scan}
	// A ghost symbol -- one whose file row is deleted -- must not inflate N or
	// the average length, for the same reason it must not score: it is not
	// part of the corpus any more.
	err = s.parserSemanticQueryer(ctx).QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(AVG(len), 0) FROM (
			SELECT SUM(st.weight) AS len
			FROM symbol_tokens st
			JOIN symbols s ON s.id = st.symbol_id
			JOIN files f ON f.id = s.file_id AND f.is_deleted = 0
			WHERE s.repo_id = ?
			GROUP BY st.symbol_id
		)`, repoID).Scan(&stats.Symbols, &stats.AvgLen)
	if err != nil {
		return corpusTokenStats{}, err
	}
	s.tokenStats.put(repoID, stats)
	return stats, nil
}

// bm25IDF is the standard BM25 inverse document frequency, in the
// non-negative form: ln(1 + (N - df + 0.5)/(df + 0.5)).
//
// Non-negative only while df <= symbols. Callers pass a symbols count that
// cannot be below df; see SemanticSearch.
//
// It is what stops a stopword from deciding the ranking. On the pinned OkHttp
// index `the` occurs in 413 of 6 081 symbols and `store` in 2; under a raw
// term-frequency sum each occurrence was worth the same.
func bm25IDF(symbols, df int64) float64 {
	if df <= 0 {
		return 0
	}
	n := float64(symbols)
	d := float64(df)
	return math.Log(1 + (n-d+0.5)/(d+0.5))
}

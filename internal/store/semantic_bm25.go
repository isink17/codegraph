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
	// aggregates rows in, which the planner may change; rounding makes two
	// runs of the same query agree exactly, so the documented tie-break
	// columns decide genuinely equal scores rather than float noise.
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

// tokenStatsCache holds the statistics per repository, keyed by the scan
// generation they were computed from.
//
// Computing them costs a full aggregate over symbol_tokens -- measured at
// about 54 ms on a 6 081-symbol repository, which is more than the entire
// query it would serve. They change only when the repository is re-indexed,
// and the scan id is exactly that generation, so a cache entry is either
// current or replaced; a stale one can never be served. This is why the
// alternative, a persisted column behind a migration, was not needed.
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
	scan, err := s.LastScanID(ctx, repoID)
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

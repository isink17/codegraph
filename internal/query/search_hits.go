package query

import "github.com/isink17/codegraph/internal/store"

// searchHit is the typed view of one semantic-search result row.
//
// Both search paths -- Store.SemanticSearch (token overlap) and
// Store.HybridSearch (FTS + vector, RRF-fused) -- return []map[string]any with
// the keys `file`, `symbol`, `score`, `why`, and (hybrid only) `kind`. That
// stringly contract is what let context_for_task read a key named `name` that no
// producer has ever emitted, silently dropping every seed. Every consumer inside
// this package now goes through parseSearchHits, and searchHitContractKeys
// documents the exact keys the adapter honours so a producer change breaks a
// test instead of a feature.
type searchHit struct {
	File          string
	QualifiedName string
	Kind          string
	Score         float64
	// Rank is the hit's zero-based position in the producer's ordered output.
	Rank int
	Why  []string
	// SymbolID is set only when a producer already resolved identity. Neither
	// current producer does; seeds are resolved via store.SymbolsForRefs.
	SymbolID int64
}

// searchHitContractKeys names the keys parseSearchHits reads. `symbol` is the
// current qualified-name key; `name` is accepted only as a legacy fallback and
// loses to `symbol` when both are present. Any other key is ignored rather than
// guessed at.
var searchHitContractKeys = []string{"file", "symbol", "name", "kind", "score", "why", "symbol_id"}

func parseSearchHits(results []map[string]any) []searchHit {
	hits := make([]searchHit, 0, len(results))
	for i, r := range results {
		hit := searchHit{Rank: i}
		hit.File, _ = r["file"].(string)
		// Precedence: the current `symbol` key wins; `name` is the legacy shape.
		if v, ok := r["symbol"].(string); ok && v != "" {
			hit.QualifiedName = v
		} else if v, ok := r["name"].(string); ok {
			hit.QualifiedName = v
		}
		hit.Kind, _ = r["kind"].(string)
		hit.Score = asFloat(r["score"])
		hit.SymbolID = asInt64(r["symbol_id"])
		switch why := r["why"].(type) {
		case []string:
			hit.Why = why
		case []any:
			for _, w := range why {
				if s, ok := w.(string); ok {
					hit.Why = append(hit.Why, s)
				}
			}
		}
		if hit.File == "" || hit.QualifiedName == "" {
			// Neither field alone identifies a symbol row, so such a hit cannot
			// become context. Dropping it here keeps the ranking stream free of
			// items that could never carry a drill-down identity.
			continue
		}
		hits = append(hits, hit)
	}
	return hits
}

// refs returns the (file, qualified name) pairs to resolve to symbol rows.
func hitRefs(hits []searchHit) []store.SymbolRef {
	refs := make([]store.SymbolRef, 0, len(hits))
	for _, h := range hits {
		refs = append(refs, store.SymbolRef{File: h.File, QualifiedName: h.QualifiedName})
	}
	return refs
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

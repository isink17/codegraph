package query

import (
	"context"
	"reflect"
	"testing"
)

// The adapter honours the documented keys and nothing else. This is the test the
// pre-P14 code lacked: a consumer read a key ("name") no producer emitted, and
// nothing failed until context_for_task returned an empty document.
func TestParseSearchHitsPinsProducerContract(t *testing.T) {
	hits := parseSearchHits([]map[string]any{
		// Current contract, as emitted by Store.SemanticSearch.
		{"file": "a.go", "symbol": "a.Alpha", "score": 2.5, "why": []string{"token_overlap"}},
		// Current contract with kind, as emitted by Store.HybridSearch.
		{"file": "b.go", "symbol": "b.Beta", "kind": "function", "score": 0.03, "why": []any{"fts", "vector_similarity"}},
		// Legacy shape: `name` stands in for `symbol`.
		{"file": "c.go", "name": "c.Gamma", "score": 1},
		// Both present: `symbol` wins.
		{"file": "d.go", "symbol": "d.Delta", "name": "d.Wrong"},
		// Producer that already resolved identity.
		{"file": "e.go", "symbol": "e.Epsilon", "symbol_id": float64(42)},
		// Unidentifiable rows are dropped rather than guessed at.
		{"file": "f.go", "score": 9},
		{"symbol": "g.Gamma"},
		// An unknown key is not a name source.
		{"file": "h.go", "identifier": "h.Eta"},
	})

	if len(hits) != 5 {
		t.Fatalf("parsed %d hits, want 5: %+v", len(hits), hits)
	}
	if hits[0].QualifiedName != "a.Alpha" || hits[0].Score != 2.5 || hits[0].Rank != 0 {
		t.Fatalf("first hit = %+v", hits[0])
	}
	if !reflect.DeepEqual(hits[1].Why, []string{"fts", "vector_similarity"}) {
		t.Fatalf("why = %v, want the []any values decoded", hits[1].Why)
	}
	if hits[1].Kind != "function" {
		t.Fatalf("kind = %q, want function", hits[1].Kind)
	}
	if hits[2].QualifiedName != "c.Gamma" {
		t.Fatalf("legacy name key not accepted: %+v", hits[2])
	}
	if hits[3].QualifiedName != "d.Delta" {
		t.Fatalf("precedence wrong: symbol must beat name, got %+v", hits[3])
	}
	if hits[4].SymbolID != 42 {
		t.Fatalf("symbol_id = %d, want 42", hits[4].SymbolID)
	}
	// Rank is the producer's position, and dropped rows must not shift it for the
	// hits that survive.
	for i, hit := range hits {
		if hit.Rank < i {
			t.Fatalf("hit %d has rank %d", i, hit.Rank)
		}
	}
}

func TestSearchHitContractKeysAreDocumented(t *testing.T) {
	for _, key := range []string{"file", "symbol", "name", "score", "why"} {
		found := false
		for _, documented := range searchHitContractKeys {
			if documented == key {
				found = true
			}
		}
		if !found {
			t.Fatalf("key %q is read by the adapter but not documented", key)
		}
	}
}

// Both real producers must satisfy the adapter, so pin their live output too.
func TestLiveProducersSatisfyTheAdapter(t *testing.T) {
	ctx := context.Background()
	for name, fx := range map[string]*contextFixture{
		"token_overlap": newContextFixture(t),
		"hybrid":        newContextFixtureWithEmbedder(t, bagOfWordsEmbedder{}),
	} {
		raw, err := fx.svc.SemanticSearch(ctx, fx.repoID, taskPayment, 30, 0)
		if err != nil {
			t.Fatalf("%s: SemanticSearch() error = %v", name, err)
		}
		hits := parseSearchHits(raw)
		if len(hits) == 0 {
			t.Fatalf("%s: adapter dropped every hit of %d results", name, len(raw))
		}
		for _, hit := range hits {
			if hit.File == "" || hit.QualifiedName == "" {
				t.Fatalf("%s: adapter produced an unusable hit %+v", name, hit)
			}
		}
	}
}

// Hybrid fusion merges through a map; its output order must not depend on
// iteration order.
func TestHybridSearchOrderIsStable(t *testing.T) {
	ctx := context.Background()
	fx := newContextFixtureWithEmbedder(t, bagOfWordsEmbedder{})
	var reference []string
	for i := 0; i < 10; i++ {
		raw, err := fx.svc.SemanticSearch(ctx, fx.repoID, taskPayment, 30, 0)
		if err != nil {
			t.Fatalf("SemanticSearch() error = %v", err)
		}
		order := make([]string, 0, len(raw))
		for _, hit := range parseSearchHits(raw) {
			order = append(order, hit.File+"::"+hit.QualifiedName)
		}
		if i == 0 {
			reference = order
			continue
		}
		if !reflect.DeepEqual(order, reference) {
			t.Fatalf("run %d order = %v, want %v", i, order, reference)
		}
	}
}

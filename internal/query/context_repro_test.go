package query

import (
	"context"
	"testing"
)

// TestSemanticSeedContractReproduction pins the producer contract of both
// semantic-search paths and proves that ContextForTask consumes it. Before the
// P14 fix, parseSeedSymbols read r["name"], a key neither producer emits, so
// every seed was dropped and ContextForTask returned {"files":null} while
// reporting success.
func TestSemanticSeedContractReproduction(t *testing.T) {
	ctx := context.Background()
	fx := newContextFixture(t)

	hits, err := fx.store.SemanticSearch(ctx, fx.repoID, "process payment", 30, 0)
	if err != nil {
		t.Fatalf("SemanticSearch() error = %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SemanticSearch() returned no hits; fixture cannot prove the bug")
	}
	// Producer contract: file + symbol (qualified name) + score, never "name".
	for _, h := range hits {
		if _, ok := h["file"].(string); !ok {
			t.Fatalf("hit missing string field %q: %v", "file", h)
		}
		if _, ok := h["symbol"].(string); !ok {
			t.Fatalf("hit missing string field %q: %v", "symbol", h)
		}
		if _, ok := h["name"]; ok {
			t.Fatalf("hit unexpectedly carries legacy field %q: %v", "name", h)
		}
	}

	res, err := fx.svc.ContextForTask(ctx, fx.repoID, "process payment", ContextForTaskOptions{})
	if err != nil {
		t.Fatalf("ContextForTask() error = %v", err)
	}
	if len(res.Files) == 0 {
		t.Fatalf("ContextForTask() returned no files for an indexed task; seeds were dropped: %+v", res)
	}
	found := false
	for _, f := range res.Files {
		for _, sym := range f.Symbols {
			if sym.Relevance == "direct_match" && sym.QualifiedName != "" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("ContextForTask() returned no direct_match symbol: %+v", res)
	}
}

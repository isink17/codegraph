package main

import (
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func file(path string, names ...string) graph.TaskContextFile {
	f := graph.TaskContextFile{Path: path, RelevanceScore: 1}
	for _, n := range names {
		f.Symbols = append(f.Symbols, graph.TaskContextSymbol{QualifiedName: n, Relevance: "direct_match"})
	}
	return f
}

func TestContextStageDeduplicatesAFileReturnedInBothSections(t *testing.T) {
	// One file can hold a directly matched symbol and a test-linked symbol,
	// so it appears in Files and in TestFiles. The scored list must not count
	// it twice.
	tc := &graph.TaskContext{
		Files:     []graph.TaskContextFile{file("pkg/a_test.go", "pkg.A.TestRun"), file("pkg/a.go", "pkg.A.run")},
		TestFiles: []graph.TaskContextFile{file("pkg/a_test.go", "pkg.A.TestOther")},
	}
	query := Query{ID: "q", Corpus: "c", Category: "test_discovery", Truth: TruthComplete, QueryText: "t",
		Relevant: []Label{{File: "pkg/a_test.go", Grade: 3, Evidence: "e"}}}

	got, err := contextStageResult(query, tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metrics.RecallAt10 != 1 {
		t.Fatalf("recall@10 = %v, want 1", got.Metrics.RecallAt10)
	}
	if got.Metrics.ReturnedFiles != 2 {
		t.Fatalf("returned files = %d, want 2 distinct", got.Metrics.ReturnedFiles)
	}
	if got.Metrics.TestFiles != 1 || got.Metrics.ProductionFiles != 1 {
		t.Fatalf("test/production = %d/%d, want 1/1", got.Metrics.TestFiles, got.Metrics.ProductionFiles)
	}
	if got.EstimatedTokens <= 0 || got.ResponseBytes <= 0 {
		t.Fatalf("response size must be measured: %d tokens / %d bytes", got.EstimatedTokens, got.ResponseBytes)
	}
}

func TestContextStageKeepsResponseOrderAndIdentities(t *testing.T) {
	tc := &graph.TaskContext{
		Files:     []graph.TaskContextFile{file("b.go", "pkg.B.run"), file("a.go", "pkg.A.run")},
		TestFiles: []graph.TaskContextFile{file("a_test.go", "pkg.A.TestRun")},
	}
	query := Query{ID: "q", Corpus: "c", Category: "k", Truth: TruthPartial, QueryText: "t",
		Relevant: []Label{{File: "a.go", Grade: 3, Symbols: []string{"pkg.A.run", "pkg.A.gone"}, Evidence: "e"}}}
	got, err := contextStageResult(query, tc, []string{"a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Files, []string{"b.go", "a.go", "a_test.go"}) {
		t.Fatalf("files = %v; production section first, response order preserved", got.Files)
	}
	if !reflect.DeepEqual(got.Metrics.MissingExpectedSymbols, []string{"pkg.A.gone"}) {
		t.Fatalf("missing symbols = %v", got.Metrics.MissingExpectedSymbols)
	}
	if len(got.Symbols) != 3 || got.Symbols[0].QualifiedName != "pkg.B.run" || got.Symbols[0].Rank != 0 {
		t.Fatalf("symbol identities = %+v", got.Symbols)
	}
}

func TestSearchStageDeduplicatesFilesAndKeepsHitOrder(t *testing.T) {
	hits := []map[string]any{
		{"file": "a.go", "symbol": "pkg.A.one", "score": 9.0, "symbol_id": int64(1)},
		{"file": "a.go", "symbol": "pkg.A.two", "score": 8.0, "symbol_id": int64(2)},
		{"file": "b.go", "symbol": "pkg.B.one", "score": 7.0, "symbol_id": int64(3)},
	}
	query := Query{ID: "q", Corpus: "c", Category: "k", Truth: TruthPartial, QueryText: "t",
		Relevant: []Label{{File: "b.go", Grade: 2, Evidence: "e"}}}
	got, err := searchStageResult(query, hits)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Files, []string{"a.go", "b.go"}) {
		t.Fatalf("files = %v", got.Files)
	}
	if *got.Metrics.FirstRelevantRank != 1 {
		t.Fatalf("first relevant rank = %d, want 1", *got.Metrics.FirstRelevantRank)
	}
	if got.Symbols[0].SymbolID != 1 || got.Symbols[0].Score != 9 {
		t.Fatalf("hit identity lost: %+v", got.Symbols[0])
	}
}

func TestSearchStageOnNoHits(t *testing.T) {
	query := Query{ID: "q", Corpus: "c", Category: "k", Truth: TruthPartial, QueryText: "t",
		Relevant: []Label{{File: "b.go", Grade: 2, Evidence: "e"}}}
	got, err := searchStageResult(query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 0 || got.Metrics.FirstRelevantRank != nil || got.Metrics.RecallAt10 != 0 {
		t.Fatalf("an empty result must report a clean miss: %+v", got.Metrics)
	}
}

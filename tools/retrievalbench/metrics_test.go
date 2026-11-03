package main

import (
	"reflect"
	"testing"
)

// deref reads an optional metric in a test assertion. -1 marks "withheld",
// which no real metric can equal.
func deref(v *float64) float64 {
	if v == nil {
		return -1
	}
	return *v
}

func q(truth string, relevant []Label, irrelevant ...Label) Query {
	return Query{ID: "q", Corpus: "c", Category: "k", Truth: truth, QueryText: "t",
		Relevant: relevant, Irrelevant: irrelevant}
}

func TestEvaluateWithholdsPrecisionAndNDCGForPartialTruth(t *testing.T) {
	query := q(TruthPartial, []Label{{File: "a.go", Grade: 3, Evidence: "e"}})
	m := Evaluate(query, []string{"a.go", "z.go"}, nil)
	if m.PrecisionAt5 != nil || m.PrecisionAt10 != nil || m.NDCGAt10 != nil {
		t.Fatalf("partial truth must withhold precision and nDCG, got %+v", m)
	}
	if deref(m.RecallAt10) != 1 {
		t.Fatalf("recall against the labelled set is still defined: got %v", deref(m.RecallAt10))
	}
	if len(m.FalsePositivesAt10) != 0 {
		t.Fatalf("an unlabelled file is not a false positive under partial truth: %v", m.FalsePositivesAt10)
	}
}

func TestEvaluateDenominators(t *testing.T) {
	query := q(TruthComplete, []Label{
		{File: "a.go", Grade: 3, Evidence: "e"},
		{File: "b.go", Grade: 1, Evidence: "e"},
	})
	m := Evaluate(query, []string{"a.go", "x.go", "b.go"}, nil)
	if m.PrecisionDenom5 != 3 || m.PrecisionDenom10 != 3 {
		t.Fatalf("denominator must be the files actually returned: %d/%d", m.PrecisionDenom5, m.PrecisionDenom10)
	}
	if *m.PrecisionAt5 != 2.0/3.0 {
		t.Fatalf("precision@5 = %v, want 2/3", *m.PrecisionAt5)
	}
	if deref(m.RecallAt5) != 1 {
		t.Fatalf("recall@5 = %v, want 1", m.RecallAt5)
	}
	if !reflect.DeepEqual(m.FalsePositivesAt10, []string{"x.go"}) {
		t.Fatalf("false positives = %v", m.FalsePositivesAt10)
	}
}

func TestEvaluateFirstRelevantRankAndReciprocal(t *testing.T) {
	query := q(TruthPartial, []Label{{File: "b.go", Grade: 2, Evidence: "e"}})
	m := Evaluate(query, []string{"a.go", "b.go"}, nil)
	if m.FirstRelevantRank == nil || *m.FirstRelevantRank != 1 {
		t.Fatalf("first relevant rank = %v, want 1", m.FirstRelevantRank)
	}
	if deref(m.ReciprocalRank) != 0.5 {
		t.Fatalf("reciprocal rank = %v, want 0.5", deref(m.ReciprocalRank))
	}
	empty := Evaluate(query, []string{"a.go"}, nil)
	if empty.FirstRelevantRank != nil || deref(empty.ReciprocalRank) != 0 {
		t.Fatalf("a miss must report no rank, got %+v", empty)
	}
}

func TestEvaluateMissingAndAmbiguousSymbols(t *testing.T) {
	query := q(TruthPartial, []Label{
		{File: "a.go", Grade: 3, Symbols: []string{"pkg.A.run", "pkg.A.stop"}, Evidence: "e"},
	})
	m := Evaluate(query, []string{"a.go"}, map[string]bool{"pkg.A.run": true, "other.A.stop": true})
	// other.A.stop is a different qualified identity; a same-short-name symbol
	// from another container must not satisfy the expectation.
	if !reflect.DeepEqual(m.MissingExpectedSymbols, []string{"pkg.A.stop"}) {
		t.Fatalf("missing expected symbols = %v", m.MissingExpectedSymbols)
	}
	if len(m.MissingRelevantFiles) != 0 {
		t.Fatalf("file was returned; it is not missing: %v", m.MissingRelevantFiles)
	}
}

func TestEvaluateReportsVerifiedIrrelevantHits(t *testing.T) {
	query := q(TruthPartial,
		[]Label{{File: "a.go", Grade: 3, Evidence: "e"}},
		Label{File: "a_test.go", Evidence: "test"})
	m := Evaluate(query, []string{"a.go", "a_test.go"}, nil)
	if !reflect.DeepEqual(m.LabelledIrrelevantReturned, []string{"a_test.go"}) {
		t.Fatalf("labelled irrelevant returned = %v", m.LabelledIrrelevantReturned)
	}
	if m.TestFiles != 1 || m.ProductionFiles != 1 {
		t.Fatalf("test/production split = %d/%d, want 1/1", m.TestFiles, m.ProductionFiles)
	}
}

func TestEvaluateIsDeterministicAndOrderSensitive(t *testing.T) {
	query := q(TruthComplete, []Label{
		{File: "a.go", Grade: 3, Evidence: "e"},
		{File: "b.go", Grade: 1, Evidence: "e"},
	})
	files := []string{"x.go", "b.go", "a.go"}
	first := Evaluate(query, files, nil)
	for i := 0; i < 5; i++ {
		if !reflect.DeepEqual(Evaluate(query, files, nil), first) {
			t.Fatalf("repeated evaluation differed on run %d", i)
		}
	}
	better := Evaluate(query, []string{"a.go", "b.go", "x.go"}, nil)
	if !(*better.NDCGAt10 > *first.NDCGAt10) {
		t.Fatalf("a better ordering must score higher: %v vs %v", *better.NDCGAt10, *first.NDCGAt10)
	}
	if deref(better.ReciprocalRank) <= deref(first.ReciprocalRank) {
		t.Fatalf("reciprocal rank must reward the earlier hit")
	}
}

func TestNDCGIsOneForTheIdealOrdering(t *testing.T) {
	query := q(TruthComplete, []Label{
		{File: "a.go", Grade: 3, Evidence: "e"},
		{File: "b.go", Grade: 2, Evidence: "e"},
		{File: "c.go", Grade: 1, Evidence: "e"},
	})
	m := Evaluate(query, []string{"a.go", "b.go", "c.go"}, nil)
	if *m.NDCGAt10 != 1 {
		t.Fatalf("ideal ordering nDCG = %v, want 1", *m.NDCGAt10)
	}
}

func TestIsTestPath(t *testing.T) {
	tests := map[string]bool{
		"command.go":           false,
		"command_test.go":      true,
		"tests/test_utils.py":  true,
		"src/click/testing.py": false,
		"okhttp/src/test/java/okhttp3/KotlinSourceModernTest.kt": true,
		"okhttp/src/main/kotlin/okhttp3/Cache.kt":                false,
		"a\\b\\foo_test.go": true,
		"pkg/FooSpec.scala": true,
	}
	for path, want := range tests {
		if got := isTestPath(path); got != want {
			t.Errorf("isTestPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestAttributeMissingSeparatesRankingFromCandidacy(t *testing.T) {
	got := AttributeMissing([]string{"a.go", "b.go"}, []string{"b.go"})
	want := map[string]string{"a.go": AttributionAbsentFromSearch, "b.go": AttributionRankedOut}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attribution = %v, want %v", got, want)
	}
}

func TestEvaluateCountsARepeatedFileOnce(t *testing.T) {
	// ContextForTask returns a file in both its production and its test
	// section when one of its symbols matched directly and another arrived
	// through a test link. Counting it twice pushes recall above 1.
	query := q(TruthComplete, []Label{{File: "a.go", Grade: 3, Evidence: "e"}})
	m := Evaluate(query, []string{"a.go", "a.go"}, nil)
	if deref(m.RecallAt10) != 1 {
		t.Fatalf("recall@10 = %v, want 1", deref(m.RecallAt10))
	}
	if m.RelevantAt10 != 1 || m.ReturnedFiles != 1 || m.ProductionFiles != 1 {
		t.Fatalf("a repeated file must be counted once: %+v", m)
	}
	if *m.PrecisionAt10 != 1 || m.PrecisionDenom10 != 1 {
		t.Fatalf("precision denominator must be distinct files: %v/%d", *m.PrecisionAt10, m.PrecisionDenom10)
	}
	if *m.NDCGAt10 != 1 {
		t.Fatalf("nDCG = %v, want 1", *m.NDCGAt10)
	}
}

func TestEvaluateRanksByFirstOccurrenceOfARepeatedFile(t *testing.T) {
	query := q(TruthPartial, []Label{{File: "b.go", Grade: 2, Evidence: "e"}})
	m := Evaluate(query, []string{"a.go", "b.go", "a.go", "b.go"}, nil)
	if *m.FirstRelevantRank != 1 || deref(m.ReciprocalRank) != 0.5 {
		t.Fatalf("rank must ignore repeats: %+v", m)
	}
	if m.ReturnedFiles != 2 {
		t.Fatalf("returned files = %d, want 2 distinct", m.ReturnedFiles)
	}
}

func TestEvaluateRecallNeverExceedsOne(t *testing.T) {
	query := q(TruthPartial, []Label{
		{File: "a.go", Grade: 3, Evidence: "e"},
		{File: "b.go", Grade: 1, Evidence: "e"},
	})
	m := Evaluate(query, []string{"a.go", "b.go", "a.go", "b.go", "a.go"}, nil)
	if deref(m.RecallAt5) > 1 || deref(m.RecallAt10) > 1 {
		t.Fatalf("recall above 1: %+v", m)
	}
}

func TestEvaluateDeduplicatesMissingExpectedSymbols(t *testing.T) {
	query := q(TruthPartial, []Label{
		{File: "a.go", Grade: 3, Symbols: []string{"pkg.A.run"}, Evidence: "e"},
		{File: "b.go", Grade: 1, Symbols: []string{"pkg.A.run"}, Evidence: "e"},
	})
	m := Evaluate(query, []string{"a.go", "b.go"}, nil)
	if !reflect.DeepEqual(m.MissingExpectedSymbols, []string{"pkg.A.run"}) {
		t.Fatalf("missing expected symbols = %v, want one entry", m.MissingExpectedSymbols)
	}
}

func TestEvaluateWithholdsEveryRatioForANegativeQuery(t *testing.T) {
	query := Query{ID: "n", Corpus: "c", Category: "negative", Truth: TruthNegative,
		QueryText:  "where is the GraphQL resolver",
		Irrelevant: []Label{{File: "lib/core/Axios.js", Evidence: "no GraphQL anywhere in this corpus"}}}
	m := Evaluate(query, []string{"lib/core/Axios.js", "lib/utils.js"}, nil)
	if m.RecallAt5 != nil || m.RecallAt10 != nil || m.ReciprocalRank != nil ||
		m.PrecisionAt10 != nil || m.NDCGAt10 != nil || m.FirstRelevantRank != nil {
		t.Fatalf("a negative query has no defined ratio: %+v", m)
	}
	if m.ReturnedFiles != 2 {
		t.Fatalf("returned files = %d, want 2", m.ReturnedFiles)
	}
	if !reflect.DeepEqual(m.LabelledIrrelevantReturned, []string{"lib/core/Axios.js"}) {
		t.Fatalf("a verified-irrelevant hit is still reported: %v", m.LabelledIrrelevantReturned)
	}
}

func TestAggregateExcludesNegativeQueriesFromTheMeans(t *testing.T) {
	one := 1.0
	scored := Metrics{RelevantLabeled: 1, RecallAt5: &one, RecallAt10: &one, ReciprocalRank: &one}
	negative := Metrics{ReturnedFiles: 10}
	a := aggregate([]Metrics{scored, negative}, 0)
	if a.Queries != 2 || a.ScoredQueries != 1 || a.NegativeQueries != 1 {
		t.Fatalf("counts = %+v", a)
	}
	if a.MeanRecallAt10 != 1 {
		t.Fatalf("mean recall = %v; a negative query must not halve it", a.MeanRecallAt10)
	}
}

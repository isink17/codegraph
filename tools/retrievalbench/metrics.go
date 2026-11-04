package main

import (
	"math"
	"path"
	"sort"
	"strings"
)

// Metrics is one stage's retrieval quality for one query.
//
// Pointer fields are withheld rather than zeroed when the labels do not
// support them: a nil PrecisionAt5 means "not computable from these
// judgements", which is a different statement from 0.0.
type Metrics struct {
	ReturnedFiles   int `json:"returned_files"`
	RelevantLabeled int `json:"relevant_labeled"`

	RelevantAt5  int `json:"relevant_at_5"`
	RelevantAt10 int `json:"relevant_at_10"`

	// Recall is nil for a negative query, where the denominator is zero.
	RecallAt5  *float64 `json:"recall_at_5"`
	RecallAt10 *float64 `json:"recall_at_10"`

	// Precision is defined only for a complete truth set. Denominator is the
	// number of files actually returned in the window, reported beside it.
	PrecisionAt5     *float64 `json:"precision_at_5"`
	PrecisionAt10    *float64 `json:"precision_at_10"`
	PrecisionDenom5  int      `json:"precision_denominator_5,omitempty"`
	PrecisionDenom10 int      `json:"precision_denominator_10,omitempty"`

	// NDCGAt10 needs graded labels and a complete truth set.
	NDCGAt10 *float64 `json:"ndcg_at_10"`

	// FirstRelevantRank is zero-based; nil when no relevant file was returned
	// and on a negative query, where there is nothing to find.
	FirstRelevantRank *int     `json:"first_relevant_rank"`
	ReciprocalRank    *float64 `json:"reciprocal_rank"`

	// ProductionFiles and TestFiles count distinct returned files over the
	// whole stage output, not over a top-k window: 30 candidates for the
	// search stage, about 10 for the context stage. Every other metric here
	// is @5 or @10, so the two are not comparable across stages.
	ProductionFiles int `json:"production_files_whole_response"`
	TestFiles       int `json:"test_files_whole_response"`

	// FalsePositivesAt10 is populated only for a complete truth set.
	FalsePositivesAt10 []string `json:"false_positives_at_10"`
	// LabelledIrrelevantReturned holds returned files a human verified as
	// irrelevant, over the whole response rather than the top 10. It is
	// available whatever the truth completeness, because each entry carries
	// its own evidence.
	LabelledIrrelevantReturned []string `json:"labelled_irrelevant_returned_whole_response"`

	MissingRelevantFiles   []string `json:"missing_relevant_files"`
	MissingExpectedSymbols []string `json:"missing_expected_symbols"`
}

// isTestPath classifies a repository-relative path as test or production.
//
// It is a fixed path rule, not a guess per language: a path segment named
// test/tests/testing/androidTest, a `_test.go` suffix, a `test_` prefix or a
// `_test`/`Test`/`Tests`/`Spec` basename suffix. The rule is reported with the
// numbers it produces so a reader can disagree with a specific file.
func isTestPath(p string) bool {
	p = strings.ReplaceAll(p, "\\", "/")
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "test", "tests", "testing", "androidTest", "testFixtures":
			return true
		}
	}
	base := path.Base(p)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	switch {
	case strings.HasSuffix(stem, "_test"), strings.HasSuffix(stem, "Test"),
		strings.HasSuffix(stem, "Tests"), strings.HasSuffix(stem, "Spec"):
		return true
	case strings.HasPrefix(stem, "test_"):
		return true
	}
	return false
}

// Evaluate scores one ordered file list against a query's labels.
// returnedSymbols is the set of qualified names the stage returned; it is what
// MissingExpectedSymbols is computed against.
//
// A repeated path is counted once, at its first position. ContextForTask can
// legitimately return one file in both its production and its test section --
// candidates dedup by symbol id, not by file, so a file holding one directly
// matched symbol and one test-linked symbol lands in both. Counting it twice
// would push recall above 1 and inflate the file counts, so the guard lives
// here, where every caller passes through, rather than only in the producer.
//
// It is a pure function of its arguments: the same inputs always produce the
// same Metrics, which is what makes a repeated benchmark run comparable.
func Evaluate(q Query, files []string, returnedSymbols map[string]bool) Metrics {
	relevantGrade := map[string]int{}
	var relevantOrder []string
	for _, l := range q.Relevant {
		relevantGrade[l.File] = l.Grade
		relevantOrder = append(relevantOrder, l.File)
	}
	irrelevant := map[string]bool{}
	for _, l := range q.Irrelevant {
		irrelevant[l.File] = true
	}

	m := Metrics{
		ReturnedFiles:              distinct(files),
		RelevantLabeled:            len(q.Relevant),
		FalsePositivesAt10:         []string{},
		LabelledIrrelevantReturned: []string{},
		MissingRelevantFiles:       []string{},
		MissingExpectedSymbols:     []string{},
	}

	seenRelevant := map[string]bool{}
	seenFile := map[string]bool{}
	i := -1
	for _, f := range files {
		if seenFile[f] {
			continue
		}
		seenFile[f] = true
		i++
		if isTestPath(f) {
			m.TestFiles++
		} else {
			m.ProductionFiles++
		}
		if irrelevant[f] {
			m.LabelledIrrelevantReturned = append(m.LabelledIrrelevantReturned, f)
		}
		_, rel := relevantGrade[f]
		if !rel {
			if q.Truth == TruthComplete && i < 10 {
				m.FalsePositivesAt10 = append(m.FalsePositivesAt10, f)
			}
			continue
		}
		seenRelevant[f] = true
		if i < 5 {
			m.RelevantAt5++
		}
		if i < 10 {
			m.RelevantAt10++
		}
		if m.FirstRelevantRank == nil {
			rank := i
			rr := 1 / float64(i+1)
			m.FirstRelevantRank, m.ReciprocalRank = &rank, &rr
		}
	}

	if m.RelevantLabeled > 0 {
		r5 := float64(m.RelevantAt5) / float64(m.RelevantLabeled)
		r10 := float64(m.RelevantAt10) / float64(m.RelevantLabeled)
		m.RecallAt5, m.RecallAt10 = &r5, &r10
		if m.ReciprocalRank == nil {
			zero := 0.0
			m.ReciprocalRank = &zero
		}
	}

	if q.Truth == TruthComplete && m.RelevantLabeled > 0 {
		m.PrecisionDenom5 = min(5, m.ReturnedFiles)
		m.PrecisionDenom10 = min(10, m.ReturnedFiles)
		if m.PrecisionDenom5 > 0 {
			v := float64(m.RelevantAt5) / float64(m.PrecisionDenom5)
			m.PrecisionAt5 = &v
		}
		if m.PrecisionDenom10 > 0 {
			v := float64(m.RelevantAt10) / float64(m.PrecisionDenom10)
			m.PrecisionAt10 = &v
		}
		if ndcg, ok := ndcgAt10(files, relevantGrade); ok {
			m.NDCGAt10 = &ndcg
		}
	}

	for _, f := range relevantOrder {
		if !seenRelevant[f] {
			m.MissingRelevantFiles = append(m.MissingRelevantFiles, f)
		}
	}
	missing := map[string]bool{}
	for _, l := range q.Relevant {
		for _, sym := range l.Symbols {
			if !returnedSymbols[sym] && !missing[sym] {
				missing[sym] = true
				m.MissingExpectedSymbols = append(m.MissingExpectedSymbols, sym)
			}
		}
	}
	sort.Strings(m.MissingExpectedSymbols)
	return m
}

// distinct counts unique paths, preserving nothing else.
func distinct(files []string) int {
	seen := map[string]bool{}
	for _, f := range files {
		seen[f] = true
	}
	return len(seen)
}

// ndcgAt10 uses gain 2^grade-1 and the standard log2(rank+2) discount. The
// ideal ranking is the labelled grades sorted descending, so a query whose
// labelled set is smaller than ten is not penalised for the empty tail.
func ndcgAt10(files []string, grades map[string]int) (float64, bool) {
	if len(grades) == 0 {
		return 0, false
	}
	dcg := 0.0
	seen := map[string]bool{}
	rank := 0
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		if rank >= 10 {
			break
		}
		if g, ok := grades[f]; ok {
			dcg += (math.Pow(2, float64(g)) - 1) / math.Log2(float64(rank)+2)
		}
		rank++
	}
	ideal := make([]int, 0, len(grades))
	for _, g := range grades {
		ideal = append(ideal, g)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ideal)))
	idcg := 0.0
	for i, g := range ideal {
		if i >= 10 {
			break
		}
		idcg += (math.Pow(2, float64(g)) - 1) / math.Log2(float64(i)+2)
	}
	if idcg == 0 {
		return 0, false
	}
	return dcg / idcg, true
}

// MissingAttribution explains, for each relevant file the final selection
// missed, whether it was ever a search candidate. Attributing such a miss to
// ranking is only honest for a file the search stage actually produced.
const (
	AttributionAbsentFromSearch = "absent_from_search_candidates"
	AttributionRankedOut        = "in_search_candidates_but_not_selected"
)

// AttributeMissing maps each file in missing to one of the attribution
// constants, using the search stage's candidate files.
func AttributeMissing(missing []string, searchFiles []string) map[string]string {
	inSearch := map[string]bool{}
	for _, f := range searchFiles {
		inSearch[f] = true
	}
	out := map[string]string{}
	for _, f := range missing {
		if inSearch[f] {
			out[f] = AttributionRankedOut
			continue
		}
		out[f] = AttributionAbsentFromSearch
	}
	return out
}

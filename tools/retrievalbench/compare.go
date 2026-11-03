package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
)

// Comparison is the A/B of two runs of the same frozen dataset.
type Comparison struct {
	Schema    string            `json:"schema"`
	DatasetID string            `json:"dataset_id"`
	Base      string            `json:"base_binary_sha256"`
	Candidate string            `json:"candidate_binary_sha256"`
	Queries   []QueryComparison `json:"queries"`
	Delta     struct {
		SearchRecallAt10  float64 `json:"search_recall_at_10"`
		ContextRecallAt10 float64 `json:"context_recall_at_10"`
		ContextMRR        float64 `json:"context_reciprocal_rank"`
		HeldOutRecallAt10 float64 `json:"held_out_context_recall_at_10"`
	} `json:"delta"`
}

// QueryComparison is one query's before/after, including the ordered file
// identities, so a change in ranking is visible and not only a change in a mean.
type QueryComparison struct {
	ID               string   `json:"id"`
	HeldOut          bool     `json:"held_out"`
	BaseFiles        []string `json:"base_context_files"`
	CandidateFiles   []string `json:"candidate_context_files"`
	OrderingChanged  bool     `json:"ordering_changed"`
	RecallAt10Delta  float64  `json:"context_recall_at_10_delta"`
	ReciprocalDelta  float64  `json:"context_reciprocal_rank_delta"`
	SearchRecallAt10 float64  `json:"search_recall_at_10_delta"`
}

// CompareReports refuses any pair that is not actually comparable.
//
// Two runs may be compared only when they scored the same frozen dataset over
// the same corpus revisions with the same run configuration. A different
// dataset, a re-indexed corpus or a widened MaxFiles all move the numbers
// without anything about scoring having changed, so reading such a pair as a
// scoring delta is the exact mistake this refusal exists to prevent.
func CompareReports(base, candidate Report) (Comparison, error) {
	if base.Dataset.SHA256 != candidate.Dataset.SHA256 {
		return Comparison{}, fmt.Errorf("dataset mismatch: base %s, candidate %s",
			base.Dataset.SHA256, candidate.Dataset.SHA256)
	}
	if base.Config != candidate.Config {
		return Comparison{}, fmt.Errorf("run configuration differs: %+v vs %+v; a configuration change is not a scoring change",
			base.Config, candidate.Config)
	}
	if base.Binary.OSArch != candidate.Binary.OSArch {
		return Comparison{}, fmt.Errorf("platform differs: base %s, candidate %s; a platform change is not a scoring change",
			base.Binary.OSArch, candidate.Binary.OSArch)
	}
	if err := sameCorpora(base, candidate); err != nil {
		return Comparison{}, err
	}
	if len(base.Queries) != len(candidate.Queries) {
		return Comparison{}, fmt.Errorf("query count differs: %d vs %d", len(base.Queries), len(candidate.Queries))
	}

	c := Comparison{Schema: "codegraph.retrieval_bench_comparison/v1", DatasetID: base.Dataset.ID,
		Base: base.Binary.SHA256, Candidate: candidate.Binary.SHA256}
	var heldOut, heldOutN float64
	for i, b := range base.Queries {
		a := candidate.Queries[i]
		if a.ID != b.ID {
			return Comparison{}, fmt.Errorf("query order differs at %d: %s vs %s", i, b.ID, a.ID)
		}
		qc := QueryComparison{
			ID: b.ID, HeldOut: b.HeldOut,
			BaseFiles: b.Context.Files, CandidateFiles: a.Context.Files,
			OrderingChanged:  !reflect.DeepEqual(b.Context.Files, a.Context.Files),
			RecallAt10Delta:  delta(a.Context.Metrics.RecallAt10, b.Context.Metrics.RecallAt10),
			ReciprocalDelta:  delta(a.Context.Metrics.ReciprocalRank, b.Context.Metrics.ReciprocalRank),
			SearchRecallAt10: delta(a.Search.Metrics.RecallAt10, b.Search.Metrics.RecallAt10),
		}
		c.Queries = append(c.Queries, qc)
		if b.HeldOut {
			heldOut += qc.RecallAt10Delta
			heldOutN++
		}
	}
	c.Delta.SearchRecallAt10 = candidate.Summary.ByStage[StageSearch].MeanRecallAt10 - base.Summary.ByStage[StageSearch].MeanRecallAt10
	c.Delta.ContextRecallAt10 = candidate.Summary.ByStage[StageContext].MeanRecallAt10 - base.Summary.ByStage[StageContext].MeanRecallAt10
	c.Delta.ContextMRR = candidate.Summary.ByStage[StageContext].MeanReciprocal - base.Summary.ByStage[StageContext].MeanReciprocal
	if heldOutN > 0 {
		c.Delta.HeldOutRecallAt10 = heldOut / heldOutN
	}
	return c, nil
}

// delta subtracts two optional metrics. A metric undefined on either side --
// a negative query has no recall -- contributes zero rather than a number
// invented from a nil.
func delta(candidate, base *float64) float64 {
	if candidate == nil || base == nil {
		return 0
	}
	return *candidate - *base
}

func sameCorpora(base, candidate Report) error {
	if len(base.Corpora) != len(candidate.Corpora) {
		return fmt.Errorf("corpus count differs: %d vs %d", len(base.Corpora), len(candidate.Corpora))
	}
	for i, b := range base.Corpora {
		a := candidate.Corpora[i]
		if a.Name != b.Name || a.Observed != b.Observed {
			return fmt.Errorf("corpus %s was re-indexed or replaced between the two runs", b.Name)
		}
	}
	return nil
}

func loadReport(path string) (Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return Report{}, fmt.Errorf("decode report %s: %w", path, err)
	}
	if r.Schema != ReportSchema {
		return Report{}, fmt.Errorf("report %s has schema %q, want %q", path, r.Schema, ReportSchema)
	}
	for _, c := range r.Corpora {
		// A report produced against a corpus that failed its identity check
		// was never comparable in the first place; the run that wrote it
		// stopped, so such a file is truncated or hand-edited.
		if !c.Match {
			return Report{}, fmt.Errorf("report %s records corpus %q as not matching its pinned identity", path, c.Name)
		}
	}
	return r, nil
}

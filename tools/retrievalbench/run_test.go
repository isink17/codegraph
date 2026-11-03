package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func okCheck() CorpusCheck {
	c := CorpusCheck{Expected: Corpus{Commit: "c1", Tree: "t1", Files: 10, Symbols: 20, References: 30, Edges: 40}}
	c.Observed.Commit, c.Observed.Tree = "c1", "t1"
	c.Observed.Files, c.Observed.Symbols, c.Observed.References, c.Observed.Edges = 10, 20, 30, 40
	return c
}

func TestCorpusIdentityMustMatchOnEveryField(t *testing.T) {
	if !okCheck().matches() {
		t.Fatal("an identical corpus must match")
	}
	mutations := map[string]func(*CorpusCheck){
		"commit":     func(c *CorpusCheck) { c.Observed.Commit = "c2" },
		"tree":       func(c *CorpusCheck) { c.Observed.Tree = "t2" },
		"files":      func(c *CorpusCheck) { c.Observed.Files = 11 },
		"symbols":    func(c *CorpusCheck) { c.Observed.Symbols = 21 },
		"references": func(c *CorpusCheck) { c.Observed.References = 31 },
		"edges":      func(c *CorpusCheck) { c.Observed.Edges = 41 },
		"no git":     func(c *CorpusCheck) { c.Observed.Commit = "" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c := okCheck()
			mutate(&c)
			if c.matches() {
				t.Fatalf("a differing %s must refuse to match", name)
			}
		})
	}
}

func TestRunRefusesBadInvocation(t *testing.T) {
	cases := map[string][]string{
		"no corpora root": {"-dataset", "datasets/v1.json"},
		"zero runs":       {"-dataset", "datasets/v1.json", "-corpora-root", t.TempDir(), "-runs", "0"},
		"negative warmup": {"-dataset", "datasets/v1.json", "-corpora-root", t.TempDir(), "-warmup", "-1"},
		"missing dataset": {"-dataset", "datasets/none.json", "-corpora-root", t.TempDir()},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(args, io.Discard); err == nil {
				t.Fatalf("expected refusal for %s", name)
			}
		})
	}
}

func TestRunRefusesBinaryIdentityMismatch(t *testing.T) {
	err := run([]string{"-dataset", "datasets/v1.json", "-corpora-root", t.TempDir(),
		"-expect-binary-sha256", strings.Repeat("0", 64)}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "binary identity mismatch") {
		t.Fatalf("err = %v, want a binary identity refusal", err)
	}
}

// The mismatch test above would also pass against an empty digest, so assert
// the digest is really computed: the matching case must get past the binary
// gate and fail on the corpus instead.
func TestRunAcceptsItsOwnBinaryDigest(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("no executable path: %v", err)
	}
	sum, err := fileSHA256(exe)
	if err != nil {
		t.Skipf("cannot digest %s: %v", exe, err)
	}
	err = run([]string{"-dataset", "datasets/v1.json", "-corpora-root", t.TempDir(),
		"-only", "cobra_execute_callers", "-expect-binary-sha256", sum}, io.Discard)
	if err == nil || strings.Contains(err.Error(), "binary identity mismatch") {
		t.Fatalf("err = %v, want the run to pass the binary gate and fail on the corpus", err)
	}
	if !strings.Contains(err.Error(), "no index at") {
		t.Fatalf("err = %v, want the missing-index refusal", err)
	}
}

func TestRunPrintsUsageForHelp(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"-h"}, &out); err != nil {
		t.Fatalf("-h returned %v, want nil", err)
	}
	if !strings.Contains(out.String(), "-corpora-root") {
		t.Fatalf("usage did not mention the flags: %q", out.String())
	}
}

func TestRunRefusesUnknownQueryID(t *testing.T) {
	err := run([]string{"-dataset", "datasets/v1.json", "-corpora-root", t.TempDir(),
		"-only", "no_such_query"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unknown query id") {
		t.Fatalf("err = %v, want an unknown query id refusal", err)
	}
}

func TestRunRefusesAnUnindexedCorpus(t *testing.T) {
	err := run([]string{"-dataset", "datasets/v1.json", "-corpora-root", t.TempDir(),
		"-only", "cobra_execute_callers"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no index at") {
		t.Fatalf("err = %v, want a missing-index refusal", err)
	}
}

func TestSelectQueriesKeepsDatasetOrder(t *testing.T) {
	ds, err := LoadDataset("datasets/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	all, err := selectQueries(ds, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(ds.Queries) {
		t.Fatalf("selected %d of %d", len(all), len(ds.Queries))
	}
	picked, err := selectQueries(ds, ds.Queries[2].ID+" , "+ds.Queries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if picked[0].ID != ds.Queries[0].ID || picked[1].ID != ds.Queries[2].ID {
		t.Fatalf("selection must follow dataset order, got %s,%s", picked[0].ID, picked[1].ID)
	}
}

func TestSummarizeIsDeterministicAndSeparatesHeldOut(t *testing.T) {
	mk := func(id string, heldOut bool, recall float64) QueryResult {
		m := Metrics{RecallAt5: recall, RecallAt10: recall, RelevantLabeled: 1}
		return QueryResult{ID: id, Category: "execution_path", HeldOut: heldOut,
			Search: StageResult{Metrics: m}, Context: StageResult{Metrics: m}}
	}
	in := []QueryResult{mk("a", false, 1), mk("b", true, 0), mk("c", false, 0.5)}
	first := summarize(in)
	for i := 0; i < 3; i++ {
		if !reflect.DeepEqual(summarize(in), first) {
			t.Fatalf("summarize differed on run %d", i)
		}
	}
	if first.HeldOut.Queries != 1 || first.Tuning.Queries != 2 {
		t.Fatalf("held-out/tuning split = %d/%d", first.HeldOut.Queries, first.Tuning.Queries)
	}
	if first.Tuning.MeanRecallAt10 != 0.75 {
		t.Fatalf("tuning mean recall = %v, want 0.75", first.Tuning.MeanRecallAt10)
	}
	if first.HeldOut.MeanRecallAt10 != 0 {
		t.Fatalf("held-out mean must not absorb tuning queries: %v", first.HeldOut.MeanRecallAt10)
	}
}

func TestAggregateWithholdsUndefinedMeans(t *testing.T) {
	a := aggregate([]Metrics{{RelevantLabeled: 1}}, 0)
	if a.ScoredPrecision != 0 || a.MeanPrecisionAt10 != 0 || a.ScoredNDCG != 0 {
		t.Fatalf("a partial-truth metric set must contribute no precision or nDCG: %+v", a)
	}
	if a.QueriesWithNoHit != 1 {
		t.Fatalf("a query with no relevant result must be counted: %+v", a)
	}
}

func baseReport() Report {
	r := Report{Schema: ReportSchema}
	r.Dataset.SHA256 = "d1"
	r.Binary.OSArch = "darwin/arm64"
	r.Config = RunConfig{SearchLimit: 30, MaxFiles: 10, MaxSymbols: 30, MaxTokens: 9000, Runs: 10,
		SelectedQueries: 14, DatasetQueries: 14}
	c := CorpusCheck{Name: "cobra", Match: true}
	c.Observed.Commit = "c1"
	r.Corpora = []CorpusCheck{c}
	m := Metrics{RecallAt10: 0.5, ReciprocalRank: 0.5}
	r.Queries = []QueryResult{{ID: "q1", Search: StageResult{Metrics: m},
		Context: StageResult{Files: []string{"a.go", "b.go"}, Metrics: m}}}
	r.Summary = summarize(r.Queries)
	return r
}

func TestCompareReportsRefusesIncomparablePairs(t *testing.T) {
	cases := map[string]func(*Report){
		"different dataset":  func(r *Report) { r.Dataset.SHA256 = "d2" },
		"different config":   func(r *Report) { r.Config.MaxFiles = 20 },
		"reindexed corpus":   func(r *Report) { r.Corpora[0].Observed.Commit = "c2" },
		"query set changed":  func(r *Report) { r.Queries = nil },
		"different platform": func(r *Report) { r.Binary.OSArch = "linux/amd64" },
		"subset of the dataset": func(r *Report) {
			r.Config.SelectedQueries = 1
			r.Config.DatasetQueries = 14
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := baseReport()
			mutate(&candidate)
			if _, err := CompareReports(baseReport(), candidate); err == nil {
				t.Fatalf("expected refusal for %s", name)
			}
		})
	}
}

func TestCompareReportsReportsRankingChange(t *testing.T) {
	candidate := baseReport()
	candidate.Queries[0].Context.Files = []string{"b.go", "a.go"}
	candidate.Queries[0].Context.Metrics.ReciprocalRank = 1
	candidate.Summary = summarize(candidate.Queries)
	cmp, err := CompareReports(baseReport(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Queries[0].OrderingChanged {
		t.Fatal("a reordered file list must be reported as an ordering change")
	}
	if cmp.Queries[0].ReciprocalDelta != 0.5 {
		t.Fatalf("reciprocal delta = %v, want 0.5", cmp.Queries[0].ReciprocalDelta)
	}
	same, err := CompareReports(baseReport(), baseReport())
	if err != nil {
		t.Fatal(err)
	}
	if same.Queries[0].OrderingChanged || same.Delta.ContextRecallAt10 != 0 {
		t.Fatalf("an identical rerun must show no change: %+v", same)
	}
}

func TestCompareReportsRefusesADifferentQueryAtTheSamePosition(t *testing.T) {
	candidate := baseReport()
	candidate.Queries[0].ID = "q2"
	if _, err := CompareReports(baseReport(), candidate); err == nil ||
		!strings.Contains(err.Error(), "query order differs") {
		t.Fatalf("err = %v, want a query-order refusal", err)
	}
}

func TestLoadReportRefusesWrongSchemaAndUnmatchedCorpus(t *testing.T) {
	write := func(r Report) string {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "r.json")
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bad := baseReport()
	bad.Schema = "other/v9"
	if _, err := loadReport(write(bad)); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("err = %v, want a schema refusal", err)
	}
	unmatched := baseReport()
	unmatched.Corpora[0].Match = false
	if _, err := loadReport(write(unmatched)); err == nil || !strings.Contains(err.Error(), "not matching") {
		t.Fatalf("err = %v, want an unmatched-corpus refusal", err)
	}
	if _, err := loadReport(write(baseReport())); err != nil {
		t.Fatalf("a well-formed report must load: %v", err)
	}
}

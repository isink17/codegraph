package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testSHA = "9ca4701b6504834e3bb8a266e32c01bba554410d"

func validRecord() *Record {
	return &Record{
		Schema:    SchemaVersion,
		Benchmark: "correctness+context",
		CodeGraph: CodeGraph{SHA: testSHA, Version: "dev"},
		DateUTC:   "2026-10-09T00:00:00Z",
		Platform:  Platform{OS: "linux", Arch: "amd64", Go: "go1.26.0"},
		Fixtures:  []Fixture{{Name: "go-calls", SHA: strings.Repeat("a", 64)}},
		Commands:  []string{"codegraph index <workdir>/repo"},
		Methodology: Methodology{Version: MethodologyVersion, Repetitions: 1, Aggregation: "sum",
			GroundTruth: "hand-labeled", Tokenizer: "ceil(bytes/4)", CompleteEnumeration: true},
		Metrics: []Metric{
			ratio("coverage", 3, 4, "all pairs", "measured corpus"),
			count("candidate_opportunities", 4, "measured corpus"),
		},
		Observations: []Observation{{Kind: "pair", ID: "a->b", Outcome: "not_resolved"}},
		Limitations:  []string{"small fixture"},
	}
}

func TestValidateAcceptsComplete(t *testing.T) {
	if err := Validate(validRecord()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(r *Record){
		"missing denominator":            func(r *Record) { r.Metrics[0].Denominator = nil },
		"missing denominator definition": func(r *Record) { r.Metrics[0].DenominatorDefinition = "" },
		"value disagrees with counts":    func(r *Record) { *r.Metrics[0].Value = 1 },
		"missing source sha":             func(r *Record) { r.CodeGraph.SHA = "" },
		"short source sha":               func(r *Record) { r.CodeGraph.SHA = "9ca4701" },
		"missing fixture identity":       func(r *Record) { r.Fixtures = nil },
		"missing fixture sha":            func(r *Record) { r.Fixtures[0].SHA = "" },
		"missing methodology":            func(r *Record) { r.Methodology = Methodology{} },
		"missing ground truth":           func(r *Record) { r.Methodology.GroundTruth = "" },
		"no limitations":                 func(r *Record) { r.Limitations = nil },
		"no observations":                func(r *Record) { r.Observations = nil },
		"accuracy claim":                 func(r *Record) { r.Metrics[1].Name = "accuracy" },
		"f1 claim":                       func(r *Record) { r.Metrics[1].Name = "F1" },
		"recall without full truth": func(r *Record) {
			r.Methodology.CompleteEnumeration = false
			r.Metrics[1].Name = "recall"
		},
		"recall variant without full truth": func(r *Record) {
			r.Methodology.CompleteEnumeration = false
			r.Metrics[1].Name = "pair_recall"
		},
		"coverage without full truth": func(r *Record) { r.Methodology.CompleteEnumeration = false },
		"unresolved_share without full truth": func(r *Record) {
			r.Methodology.CompleteEnumeration = false
			r.Metrics[0].Name = "unresolved_share"
		},
		"precision with unit count": func(r *Record) {
			r.Metrics[1].Name = "audited_resolved_precision"
		},
		"coverage with unit count": func(r *Record) {
			r.Metrics[1].Name = "coverage"
			*r.Metrics[1].Value = 0.95
		},
		"pair_coverage without full truth": func(r *Record) {
			r.Methodology.CompleteEnumeration = false
			r.Metrics[0].Name = "pair_coverage"
		},
		"percent unit": func(r *Record) { r.Metrics[1].Unit = "percent" },
		"empty unit":   func(r *Record) { r.Metrics[1].Unit = "" },
		"wrong schema": func(r *Record) { r.Schema = "codegraph.benchmark/v0" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := validRecord()
			mut(r)
			if Validate(r) == nil {
				t.Fatal("accepted invalid record")
			}
		})
	}
}

func TestRatioZeroDenominatorIsNull(t *testing.T) {
	m := ratio("coverage", 0, 0, "def", "scope")
	if m.Value != nil {
		t.Fatalf("value = %v, want nil", *m.Value)
	}
	r := validRecord()
	r.Metrics = append(r.Metrics, m)
	if err := Validate(r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(Summarize(r), "| coverage | n/a |") {
		t.Fatal("null metric not rendered as n/a")
	}
}

func TestCheckCurrentStale(t *testing.T) {
	r := validRecord()
	if err := CheckCurrent(r, testSHA, ""); err != nil {
		t.Fatal(err)
	}
	if CheckCurrent(r, strings.Repeat("b", 40), "") == nil {
		t.Fatal("stale codegraph sha accepted")
	}
	dir := filepath.Join("testdata", "go-calls")
	sha, err := HashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.Fixtures[0].SHA = sha
	if err := CheckCurrent(r, "", dir); err != nil {
		t.Fatal(err)
	}
	r.Fixtures[0].SHA = strings.Repeat("c", 64)
	if CheckCurrent(r, "", dir) == nil {
		t.Fatal("stale fixture sha accepted")
	}
	r.Fixtures = []Fixture{{Name: "x", SHA: sha}, {Name: "y", SHA: sha}}
	if CheckCurrent(r, "", dir) == nil {
		t.Fatal("multi-fixture record checked against one directory")
	}
}

func TestCompareIncompatible(t *testing.T) {
	if err := Compare(validRecord(), validRecord()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(r *Record){
		"fixture sha": func(r *Record) { r.Fixtures[0].SHA = strings.Repeat("d", 64) },
		"methodology": func(r *Record) { r.Methodology.Repetitions = 5 },
		"invalid":     func(r *Record) { r.Limitations = nil },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			b := validRecord()
			mut(b)
			if Compare(validRecord(), b) == nil {
				t.Fatal("incompatible records compared")
			}
		})
	}
}

func TestSortObservationsDeterministic(t *testing.T) {
	obs := []Observation{
		{Kind: "pair", ID: "z"}, {Kind: "context", ID: "t", Mode: "b"},
		{Kind: "context", ID: "t", Mode: "a"}, {Kind: "pair", ID: "a"},
	}
	sortObservations(obs)
	var got []string
	for _, o := range obs {
		got = append(got, o.Kind+"/"+o.ID+"/"+o.Mode)
	}
	if want := "context/t/a context/t/b pair/a/ pair/z/"; strings.Join(got, " ") != want {
		t.Fatalf("order = %v", got)
	}
}

func TestMarshalSummarizeRoundTripReproducible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	b1, err := Marshal(validRecord())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b1, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := Marshal(loaded)
	if !bytes.Equal(b1, b2) {
		t.Fatal("record does not round-trip byte-identically")
	}
	if Summarize(loaded) != Summarize(validRecord()) {
		t.Fatal("summary not deterministic")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(path, []byte(`{"schema":"codegraph.benchmark/v1","accuracy":1}`), 0o644)
	if _, err := load(path); err == nil {
		t.Fatal("unknown field accepted")
	}
}

// TestRunFixtureReproducible builds codegraph from this checkout and runs the
// fixture twice; the records must be byte-identical.
func TestRunFixtureReproducible(t *testing.T) {
	if testing.Short() {
		t.Skip("builds codegraph")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "codegraph")
	if runtime.GOOS == "windows" {
		bin += ".exe" // exec resolves explicit paths only with the executable suffix
	}
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/codegraph").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	var outs [2][]byte
	for i := range outs {
		rec, err := Run(RunConfig{FixtureDir: filepath.Join("testdata", "go-calls"), Binary: bin,
			CodeGraphSHA: testSHA, DateUTC: "2026-10-09T00:00:00Z", WorkDir: filepath.Join(tmp, "w", string(rune('a'+i)))})
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(rec); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"candidate_opportunities": "7", "resolved_to_truth_target": "7", "not_resolved": "0",
			"resolved_outside_truth":                "0",
			"evidence_completeness/" + modeBaseline: "6/6",
			"evidence_completeness/" + modeCLI:      "6/6",
		}
		for _, m := range rec.Metrics {
			w, ok := want[m.Name]
			if !ok {
				continue
			}
			got := fmtValue(m)
			if m.Unit == "ratio" {
				got = fmt.Sprintf("%d/%d", *m.Numerator, *m.Denominator)
			}
			if got != w {
				t.Errorf("%s = %s, want %s", m.Name, got, w)
			}
			delete(want, m.Name)
		}
		if len(want) != 0 {
			t.Errorf("missing metrics: %v", want)
		}
		outs[i], _ = Marshal(rec)
	}
	if !bytes.Equal(outs[0], outs[1]) {
		t.Fatalf("runs differ:\n%s\n---\n%s", outs[0], outs[1])
	}
}

func TestFactsPresentBoundaries(t *testing.T) {
	ctx := []byte(`"name": "Add", "x": "Adder", op: a.b+`)
	if got := factsPresent(ctx, []string{"Add", "Adde", "a.b+", "dd"}); got != 2 {
		t.Fatalf("factsPresent = %d, want 2", got)
	}
}

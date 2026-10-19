package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	calibrationFixture = "testdata/calibration/calibration.json"
	calibrationSession = "testdata/calibration/session.jsonl"
	calibrationSparse  = "testdata/calibration/sparse.jsonl"
)

func TestCalibrateFixtureDeterministic(t *testing.T) {
	want, err := os.ReadFile(calibrationFixture)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for i, args := range [][]string{{calibrationSession}, {calibrationSession}, {calibrationSparse, calibrationSession}, {calibrationSession, calibrationSparse}} {
		out := filepath.Join(dir, "cal"+itoa(i)+".json")
		if err := run(append([]string{"calibrate", "--out", out}, args...), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(out)
		switch {
		case i < 2 && !bytes.Equal(got, want):
			t.Fatalf("calibrate run %d differs from the committed fixture:\n%s", i, got)
		case i == 3:
			prev, _ := os.ReadFile(filepath.Join(dir, "cal2.json"))
			if !bytes.Equal(got, prev) {
				t.Fatal("calibration depends on argument order")
			}
		}
	}
	c, _, err := loadCalibration(calibrationFixture)
	if err != nil {
		t.Fatal(err)
	}
	f, src := c.fit(catFileRead)
	if !c.Sufficient || src != "category" || f.Samples != 6 || f.Q1 > f.Median || f.Median > f.Q3 {
		t.Fatalf("file_read fit %+v from %s", f, src)
	}
	if _, src := c.fit(catShell); src != "global" {
		t.Fatal("a category with too few samples must use the global fit")
	}
	dup := filepath.Join(dir, "dup.json")
	if err := run([]string{"calibrate", "--out", dup, calibrationSession, calibrationSession}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dup); !bytes.Equal(got, want) {
		t.Fatal("a byte-identical input must count once")
	}
	loose := filepath.Join(dir, "loose.json")
	_ = os.WriteFile(loose, bytes.ReplaceAll(want, []byte("\n  "), []byte("\n ")), 0o644)
	if _, _, err := loadCalibration(loose); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("non-canonical calibration: %v", err)
	}
}

func TestCalibrationSkipsResetsAndSidechains(t *testing.T) {
	big := strings.Repeat("x", 400)
	tr, err := parse(t,
		asstLine("a", false, 1, 0, 1000, 1, `{"type":"tool_use","id":"r1","name":"Read","input":{}}`),
		toolResultLine("r1", big),
		asstLine("b", false, 1, 0, 1100, 1, `{"type":"tool_use","id":"r2","name":"Read","input":{}}`),
		toolResultLine("r2", big),
		`{"type":"system","subtype":"compact_boundary"}`,
		asstLine("c", false, 1, 0, 300, 1, `{"type":"tool_use","id":"r3","name":"Read","input":{}}`),
		asstLine("s", true, 1, 0, 9000, 1, `{"type":"text","text":"side"}`),
		toolResultLine("r3", big),
		asstLine("d", false, 1, 0, 200, 1, `{"type":"text","text":"drop"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	global, byCat, requests := tr.calibrationSamples(map[string]bool{})
	// Only a->b qualifies: b->c is a compaction and c->d a drop.
	if requests != 4 || len(global) != 1 || len(byCat[catFileRead]) != 1 || global[0] < 4000 || global[0] > 4300 {
		t.Fatalf("samples global=%v byCat=%v requests=%d", global, byCat, requests)
	}
}

func TestIngestCalibrated(t *testing.T) {
	rec := ingestSubagents(t, fixtureManifest, calibrationFixture)
	for _, s := range rec.Sessions {
		r := s.Residual
		if r.Method != CalibratedMethod || r.Uncalibrated {
			t.Fatalf("session %s/%d: method %s uncalibrated=%t", s.Arm, s.Rep, r.Method, r.Uncalibrated)
		}
		sum := 0
		for _, c := range r.Categories {
			sum += c.Tokens
			if e := c.Calibrated; c.Basis == basisEstimated && (e == nil || e.Low > e.Estimate || e.Estimate > e.High) {
				t.Fatalf("category %s: %+v", c.Category, e)
			}
		}
		if sum != s.FinalContext {
			t.Fatalf("categories sum %d, final context %d", sum, s.FinalContext)
		}
	}
	if !strings.Contains(strings.Join(rec.Limitations, "\n"), "fitted on recorded provider usage") {
		t.Fatalf("limitations: %v", rec.Limitations)
	}

	sparse := filepath.Join(t.TempDir(), "sparse.json")
	if err := run([]string{"calibrate", "--out", sparse, calibrationSparse}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	rec = ingestSubagents(t, fixtureManifest, sparse)
	if r := rec.Sessions[0].Residual; r.Method != ResidualMethod || !r.Uncalibrated || rec.Calibration.Sufficient {
		t.Fatalf("insufficient calibration must fall back: %+v", r)
	}
	if !strings.Contains(strings.Join(rec.Limitations, "\n"), "too few samples") {
		t.Fatalf("limitations: %v", rec.Limitations)
	}
	if r := ingestSubagents(t, fixtureManifest, "").Sessions[0].Residual; r.Method != ResidualMethod || !r.Uncalibrated {
		t.Fatalf("no calibration: %+v", r)
	}
}

func TestPriceGap(t *testing.T) {
	cal := &Calibration{Sufficient: true, Global: RatioFit{Samples: 5, Median: 4000, Q1: 3000, Q3: 5000, Min: 1, Max: 9000},
		Categories: []CategoryFit{{Category: catFileRead, Source: "category", RatioFit: RatioFit{Samples: 5, Median: 2000, Q1: 2000, Q3: 2000, Min: 2000, Max: 2000}}}}
	segs := []segment{{cat: catFileRead, chars: 200}, {cat: catShell, chars: 400}}
	// 200/2 + 400/4 = 200 priced tokens.
	if got := priceGap(500, segs, cal); got[0] != 100 || got[1] != 100 {
		t.Fatalf("under-priced gap: %v", got)
	}
	if got := priceGap(50, segs, cal); got[0]+got[1] != 50 || got[0] != 25 {
		t.Fatalf("over-priced gap must scale to growth: %v", got)
	}
	if priceGap(10, []segment{{cat: catShell}}, cal) != nil {
		t.Fatal("empty gap must return nil")
	}
	tr, err := parse(t,
		asstLine("a", false, 1, 0, 99, 1, `{"type":"tool_use","id":"r","name":"Read","input":{}}`),
		toolResultLine("r", strings.Repeat("r", 196)),
		asstLine("b", false, 1, 0, 599, 1, `{"type":"text","text":"x"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	r := tr.Residual(cal)
	// The tool_use block adds "Read{}" (6 assistant chars, 2 tokens at 4000).
	if got := residualTokens(r); got[catFileRead] != 98 || got[catAssistant] != 2 || got[catUnattributed] != 400 || got[catInitial] != 100 {
		t.Fatalf("calibrated residual %v", got)
	}
}

func TestValidateRejectsCalibrationTampering(t *testing.T) {
	for name, edit := range map[string]func(*IngestRecord){
		"estimate":       func(r *IngestRecord) { r.Sessions[0].Residual.Categories[5].Calibrated.Estimate++ },
		"estimate gone":  func(r *IngestRecord) { r.Sessions[0].Residual.Categories[5].Calibrated = nil },
		"ratio":          func(r *IngestRecord) { r.Calibration.Global.Median++ },
		"sufficiency":    func(r *IngestRecord) { r.Calibration.Sufficient = false },
		"method flag":    func(r *IngestRecord) { r.Sessions[0].Residual.Uncalibrated = true },
		"method":         func(r *IngestRecord) { r.Sessions[0].Residual.Method = ResidualMethod },
		"calibration":    func(r *IngestRecord) { r.Calibration = nil },
		"source order":   func(r *IngestRecord) { r.Calibration.Sources = append(r.Calibration.Sources, r.Calibration.Sources[0]) },
		"fit order":      func(r *IngestRecord) { r.Calibration.Categories[1].Q1 = r.Calibration.Categories[1].Median + 1 },
		"category src":   func(r *IngestRecord) { r.Calibration.Categories[1].Source = "global" },
		"residual sum":   func(r *IngestRecord) { r.Sessions[0].Residual.Categories[8].Tokens++ },
		"threshold":      func(r *IngestRecord) { r.Calibration.MinSamples = 1 },
		"sha":            func(r *IngestRecord) { r.CalibrationSHA256 = "x" },
		"v2 calibration": func(r *IngestRecord) { r.Schema, r.Methodology = IngestSchemaV2, IngestMethodologyV2 },
	} {
		rec := ingestSubagents(t, fixtureManifest, calibrationFixture)
		edit(rec)
		if ValidateIngest(rec) == nil {
			t.Errorf("%s: tampered record accepted", name)
		}
	}
}

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/isink17/codegraph/internal/latency"
)

const (
	CalibrationSchema = "codegraph.residual_calibration/v1"
	CalibratedMethod  = "context-delta-calibrated/v1"

	// A gap is a category sample when that category holds at least this many
	// permille of the gap's visible characters.
	calDominantPermille = 900
	calMinGapChars      = 100
	calMinSamples       = 5
)

// RatioFit summarizes observed characters per 1000 provider tokens over
// request gaps: nearest-rank quartiles, minimum and maximum.
type RatioFit struct {
	Samples int `json:"samples"`
	Median  int `json:"chars_per_1000_tokens_median"`
	Q1      int `json:"chars_per_1000_tokens_q1"`
	Q3      int `json:"chars_per_1000_tokens_q3"`
	Min     int `json:"chars_per_1000_tokens_min"`
	Max     int `json:"chars_per_1000_tokens_max"`
}

// CategoryFit is a category's own fit; Source says which fit prices it:
// "category" with at least MinSamples samples, otherwise "global".
type CategoryFit struct {
	Category string `json:"category"`
	Source   string `json:"source"`
	RatioFit
}

type CalibrationSource struct {
	SHA256   string `json:"sha256"`
	Requests int    `json:"requests"`
	Samples  int    `json:"samples"`
}

// Calibration is a deterministic chars-per-token fit from recorded provider
// usage. Sufficient is false when the global fit has fewer than MinSamples
// samples; ingest then keeps the uncalibrated method.
type Calibration struct {
	Schema           string              `json:"schema"`
	Method           string              `json:"method"`
	DominantPermille int                 `json:"dominant_share_permille"`
	MinGapChars      int                 `json:"min_gap_chars"`
	MinSamples       int                 `json:"min_samples"`
	Sources          []CalibrationSource `json:"sources"`
	Sufficient       bool                `json:"sufficient"`
	Global           RatioFit            `json:"global"`
	Categories       []CategoryFit       `json:"categories"`
}

var estimatedCategories = func() []string {
	var out []string
	for _, c := range residualCategories {
		if c.basis == basisEstimated {
			out = append(out, c.name)
		}
	}
	return out
}()

// calibrationSamples returns chars*1000/delta for every main-session gap
// without a reset whose growth is positive and whose visible content has at
// least calMinGapChars characters, and the same ratio per dominant category.
// A request whose message.id is in seen (a fork's copy of another file's
// history) is not sampled again.
func (t *Transcript) calibrationSamples(seen map[string]bool) (global []int, byCat map[string][]int, requests int) {
	byCat = map[string][]int{}
	prev, first := 0, true
	for _, r := range t.requests {
		if r.sidechain || r.dup {
			continue
		}
		requests++
		ctx := r.context()
		delta := ctx - prev
		if !first && !r.reset && delta > 0 && !seen[r.id] {
			chars := map[string]int{}
			total := 0
			for _, sg := range r.gap {
				chars[sg.cat] += sg.chars
				total += sg.chars
			}
			if total >= calMinGapChars {
				ratio := int((int64(total)*1000 + int64(delta)/2) / int64(delta))
				global = append(global, ratio)
				for _, c := range estimatedCategories {
					if chars[c]*1000 >= calDominantPermille*total {
						byCat[c] = append(byCat[c], ratio)
					}
				}
			}
		}
		seen[r.id] = true
		prev, first = ctx, false
	}
	return global, byCat, requests
}

func fitRatios(xs []int) RatioFit {
	if len(xs) == 0 {
		return RatioFit{}
	}
	var ds latency.Samples
	for _, x := range xs {
		ds = append(ds, time.Duration(x))
	}
	s := ds.Sorted()
	return RatioFit{Samples: len(xs), Median: int(s.SortedPercentile(50)), Q1: int(s.SortedPercentile(25)), Q3: int(s.SortedPercentile(75)), Min: int(s[0]), Max: int(s[len(s)-1])}
}

// Calibrate fits chars-per-token ratios from transcript files. A file with no
// main-session request is read as a subagent file; byte-identical files count
// once.
func Calibrate(paths []string) (*Calibration, error) {
	if len(paths) == 0 {
		return nil, errors.New("calibrate: at least one transcript is required")
	}
	cal := &Calibration{Schema: CalibrationSchema, Method: CalibratedMethod, DominantPermille: calDominantPermille, MinGapChars: calMinGapChars, MinSamples: calMinSamples, Sources: []CalibrationSource{}}
	type src struct {
		sha string
		t   *Transcript
	}
	var srcs []src
	seen := map[string]bool{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		sha := sha256Hex(b)
		if seen[sha] {
			continue
		}
		seen[sha] = true
		t, err := ParseTranscript(bytes.NewReader(b))
		if err != nil {
			if t, err = parseTranscript(bytes.NewReader(b), true); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
		}
		srcs = append(srcs, src{sha, t})
	}
	// Sources are read in hash order so the artifact does not depend on paths
	// or argument order.
	sort.Slice(srcs, func(i, j int) bool { return srcs[i].sha < srcs[j].sha })
	var global []int
	byCat := map[string][]int{}
	ids := map[string]bool{}
	for _, s := range srcs {
		g, c, n := s.t.calibrationSamples(ids)
		cal.Sources = append(cal.Sources, CalibrationSource{s.sha, n, len(g)})
		global = append(global, g...)
		for k, v := range c {
			byCat[k] = append(byCat[k], v...)
		}
	}
	cal.Global = fitRatios(global)
	cal.Sufficient = cal.Global.Samples >= calMinSamples
	for _, c := range estimatedCategories {
		f := CategoryFit{Category: c, Source: "global", RatioFit: fitRatios(byCat[c])}
		if f.Samples >= calMinSamples {
			f.Source = "category"
		}
		cal.Categories = append(cal.Categories, f)
	}
	return cal, nil
}

func validRatioFit(f RatioFit) bool {
	if f.Samples == 0 {
		return f == RatioFit{}
	}
	return f.Samples > 0 && f.Min >= 1 && f.Min <= f.Q1 && f.Q1 <= f.Median && f.Median <= f.Q3 && f.Q3 <= f.Max
}

func validateCalibration(c *Calibration) error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if c.Schema != CalibrationSchema || c.Method != CalibratedMethod {
		bad("calibration schema/method %q/%q, want %q/%q", c.Schema, c.Method, CalibrationSchema, CalibratedMethod)
	}
	if c.DominantPermille != calDominantPermille || c.MinGapChars != calMinGapChars || c.MinSamples != calMinSamples {
		bad("calibration thresholds %d/%d/%d, want %d/%d/%d", c.DominantPermille, c.MinGapChars, c.MinSamples, calDominantPermille, calMinGapChars, calMinSamples)
	}
	if len(c.Sources) == 0 {
		bad("calibration has no sources")
	}
	samples := 0
	for i, s := range c.Sources {
		if !sha64RE.MatchString(s.SHA256) || s.Requests < 0 || s.Samples < 0 || s.Samples > s.Requests || (i > 0 && c.Sources[i-1].SHA256 >= s.SHA256) {
			bad("calibration source %d: bad sha256, counts or order", i)
		}
		samples += s.Samples
	}
	if !validRatioFit(c.Global) || samples != c.Global.Samples || c.Sufficient != (c.Global.Samples >= c.MinSamples) {
		bad("calibration global fit inconsistent with its sources or sufficiency")
	}
	if len(c.Categories) != len(estimatedCategories) {
		bad("calibration has %d categories, want %d", len(c.Categories), len(estimatedCategories))
		return errors.Join(errs...)
	}
	for i, f := range c.Categories {
		want := "global"
		if f.Samples >= c.MinSamples {
			want = "category"
		}
		if f.Category != estimatedCategories[i] || f.Source != want || !validRatioFit(f.RatioFit) || f.Samples > c.Global.Samples {
			bad("calibration category %d (%s): wrong name, source or fit", i, f.Category)
		}
	}
	return errors.Join(errs...)
}

// fit returns the ratios that price a category.
func (c *Calibration) fit(cat string) (RatioFit, string) {
	for _, f := range c.Categories {
		if f.Category == cat && f.Source == "category" {
			return f.RatioFit, f.Source
		}
	}
	return c.Global, "global"
}

func loadCalibration(path string) (*Calibration, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var c Calibration
	if err := decodeStrictBytes(path, b, &c); err != nil {
		return nil, "", err
	}
	if err := validateCalibration(&c); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	// Canonical bytes let validate tie the embedded copy to its hash.
	if canon, err := marshalJSON(&c); err != nil || !bytes.Equal(canon, b) {
		return nil, "", fmt.Errorf("%s: not in the canonical encoding written by calibrate", path)
	}
	return &c, sha256Hex(b), nil
}

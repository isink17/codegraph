package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const ResidualMethod = "context-delta-char-share/v1"

const (
	catCodegraph    = "codegraph"
	catFileRead     = "file_read"
	catSearch       = "search"
	catShell        = "shell"
	catOtherTool    = "other_tool"
	catAssistant    = "assistant"
	catUserText     = "user_text"
	catInitial      = "initial_request"
	catUnattributed = "unattributed"
)

const (
	basisMeasured    = "measured"
	basisEstimated   = "estimated"
	basisUnavailable = "unavailable"
)

// residualCategories is the fixed output order and the basis of each category.
var residualCategories = []struct{ name, basis string }{
	{catCodegraph, basisEstimated},
	{catFileRead, basisEstimated},
	{catSearch, basisEstimated},
	{catShell, basisEstimated},
	{catOtherTool, basisEstimated},
	{catAssistant, basisEstimated},
	{catUserText, basisEstimated},
	{catInitial, basisMeasured},
	{catUnattributed, basisUnavailable},
}

type segment struct {
	cat    string
	chars  int
	result bool
}

func runes(s string) int { return utf8.RuneCountInString(s) }

func (t *Transcript) addSegment(cat string, chars int, result bool) {
	if chars > 0 || result {
		t.pending = append(t.pending, segment{cat, chars, result})
	}
}

func residualCategory(u toolUse) string {
	if kind, _ := classifyUse(u); kind == "mcp" || kind == "cli" {
		return catCodegraph
	}
	switch u.Name {
	case "Read", "NotebookRead":
		return catFileRead
	case "Grep", "Glob", "LS":
		return catSearch
	case "Bash":
		return catShell
	}
	return catOtherTool
}

// Residual attributes the final main-session context to content categories.
// The first request's context is measured exactly; each later growth between
// consecutive main requests is split over the content written in between by
// character share. A compaction boundary or a context drop resets the
// attribution and the whole context at that request becomes unattributed.
type Residual struct {
	Method string `json:"method"`
	// Uncalibrated marks the character-share method in v3 records.
	Uncalibrated bool               `json:"uncalibrated,omitempty"`
	FinalContext int                `json:"final_context_tokens"`
	Resets       int                `json:"resets"`
	Categories   []ResidualCategory `json:"categories"`
}

type ResidualCategory struct {
	Category string `json:"category"`
	Basis    string `json:"basis"`
	Tokens   int    `json:"tokens"`
	Chars    int    `json:"chars"`
	Results  int    `json:"tool_results"`
	// Calibrated prices the category's characters at the calibration's
	// ratios: Estimate at the median, Low and High at the upper and lower
	// quartile. It is not capped by measured growth, so Tokens can fall
	// outside [Low, High].
	Calibrated *CalibratedEstimate `json:"calibrated,omitempty"`
}

type CalibratedEstimate struct {
	RatioSource string `json:"ratio_source"`
	Estimate    int    `json:"estimate_tokens"`
	Low         int    `json:"low_tokens"`
	High        int    `json:"high_tokens"`
}

func calibratedEstimate(chars int, f RatioFit, source string) *CalibratedEstimate {
	c := int64(chars) * 1000
	return &CalibratedEstimate{RatioSource: source,
		Estimate: int((c + int64(f.Median)/2) / int64(f.Median)),
		Low:      int(c / int64(f.Q3)),
		High:     int((c + int64(f.Q1) - 1) / int64(f.Q1))}
}

// With a sufficient calibration each gap's segments are priced at their
// category's median ratio instead: priced tokens up to the measured growth go
// to the categories, any growth beyond them is unattributed, and a gap priced
// above its growth is scaled down to it.
func (t *Transcript) Residual(cal *Calibration) Residual {
	if cal != nil && !cal.Sufficient {
		cal = nil
	}
	idx := map[string]int{}
	cats := make([]ResidualCategory, len(residualCategories))
	resetCats := func() {
		for i, c := range residualCategories {
			idx[c.name] = i
			cats[i] = ResidualCategory{Category: c.name, Basis: c.basis}
		}
	}
	resetCats()
	res := Residual{Method: ResidualMethod, Uncalibrated: true}
	if cal != nil {
		res.Method, res.Uncalibrated = CalibratedMethod, false
	}
	prev, first := 0, true
	for _, r := range t.requests {
		if r.sidechain {
			continue
		}
		ctx := r.context()
		switch {
		case first:
			cats[idx[catInitial]].Tokens = ctx
			first = false
		case r.reset || ctx < prev:
			resetCats()
			cats[idx[catUnattributed]].Tokens = ctx
			res.Resets++
		default:
			delta := ctx - prev
			var shares []int
			if cal == nil {
				shares = apportion(delta, r.gap)
			} else {
				shares = priceGap(delta, r.gap, cal)
			}
			if shares == nil {
				cats[idx[catUnattributed]].Tokens += delta
			}
			for _, v := range shares {
				delta -= v
			}
			if shares != nil {
				cats[idx[catUnattributed]].Tokens += delta
			}
			for i, sg := range r.gap {
				c := &cats[idx[sg.cat]]
				if shares != nil {
					c.Tokens += shares[i]
				}
				c.Chars += sg.chars
				if sg.result {
					c.Results++
				}
			}
		}
		prev = ctx
	}
	res.FinalContext = prev
	if cal != nil {
		for i := range cats {
			if cats[i].Basis == basisEstimated {
				f, src := cal.fit(cats[i].Category)
				cats[i].Calibrated = calibratedEstimate(cats[i].Chars, f, src)
			}
		}
	}
	res.Categories = cats
	return res
}

// priceGap prices each segment at its category's median chars-per-token
// ratio, in milli-tokens, and returns integer shares that sum to the priced
// total or to delta when the priced total exceeds it. It returns nil when the
// segments carry no characters.
func priceGap(delta int, segs []segment, cal *Calibration) []int {
	w := make([]int64, len(segs))
	var sum int64
	for i, sg := range segs {
		f, _ := cal.fit(sg.cat)
		w[i] = int64(sg.chars) * 1_000_000 / int64(f.Median)
		sum += w[i]
	}
	if sum == 0 {
		return nil
	}
	return apportionWeights(min(delta, int((sum+500)/1000)), w)
}

// apportion splits delta over segments by character share with the largest
// remainder method, so the shares sum to delta exactly. It returns nil when
// the segments carry no characters.
func apportion(delta int, segs []segment) []int {
	w := make([]int64, len(segs))
	for i, s := range segs {
		w[i] = int64(s.chars)
	}
	return apportionWeights(delta, w)
}

func apportionWeights(delta int, w []int64) []int {
	var total int64
	for _, x := range w {
		total += x
	}
	if total == 0 {
		return nil
	}
	out := make([]int, len(w))
	rem := make([]int64, len(w))
	left := delta
	for i, x := range w {
		// ponytail: int64 product; overflows past ~9e18 (delta*weight), far above transcript sizes.
		p := int64(delta) * x
		out[i] = int(p / total)
		rem[i] = p % total
		left -= out[i]
	}
	for ; left > 0; left-- {
		best := 0
		for i := range rem {
			if rem[i] > rem[best] {
				best = i
			}
		}
		out[best]++
		rem[best] = -1
	}
	return out
}

// validateResidual checks a residual's categories and sum. v3 records also
// carry the method flag, and calibrated estimates that must equal the
// calibration's pricing of each category's characters; cal is nil for older
// records.
func validateResidual(r *Residual, finalContext int, v3 bool, cal *Calibration) error {
	switch {
	case !v3 && r.Method != ResidualMethod:
		return fmt.Errorf("residual method %q, want %q", r.Method, ResidualMethod)
	case v3 && r.Method == ResidualMethod && !r.Uncalibrated:
		return fmt.Errorf("residual method %s must be flagged uncalibrated", ResidualMethod)
	case v3 && r.Method == CalibratedMethod && (r.Uncalibrated || cal == nil || !cal.Sufficient):
		return fmt.Errorf("residual method %s needs a sufficient calibration and no uncalibrated flag", CalibratedMethod)
	case v3 && r.Method != ResidualMethod && r.Method != CalibratedMethod:
		return fmt.Errorf("residual method %q unknown", r.Method)
	case !v3 && r.Uncalibrated:
		return fmt.Errorf("uncalibrated flag in a pre-v3 record")
	}
	if len(r.Categories) != len(residualCategories) {
		return fmt.Errorf("residual has %d categories, want %d", len(r.Categories), len(residualCategories))
	}
	sum := 0
	for i, c := range r.Categories {
		want := residualCategories[i]
		if c.Category != want.name || c.Basis != want.basis || c.Tokens < 0 || c.Chars < 0 || c.Results < 0 {
			return fmt.Errorf("residual category %d: %+v, want %s/%s with non-negative counts", i, c, want.name, want.basis)
		}
		var wantCal *CalibratedEstimate
		if r.Method == CalibratedMethod && want.basis == basisEstimated {
			f, src := cal.fit(c.Category)
			wantCal = calibratedEstimate(c.Chars, f, src)
		}
		if (wantCal == nil) != (c.Calibrated == nil) || (wantCal != nil && *wantCal != *c.Calibrated) {
			return fmt.Errorf("residual category %s: calibrated estimate does not match the calibration", c.Category)
		}
		sum += c.Tokens
	}
	if sum != r.FinalContext || r.FinalContext != finalContext {
		return fmt.Errorf("residual categories sum to %d, final context %d, session final context %d", sum, r.FinalContext, finalContext)
	}
	return nil
}

func residualUnit(basis string) string {
	switch basis {
	case basisMeasured:
		return "provider_tokens"
	case basisEstimated:
		return "estimated_provider_tokens"
	}
	return "unattributed_provider_tokens"
}

func residualStatName(cat string) string { return "residual_" + cat + "_tokens" }

func isResidualStat(name string) bool { return strings.HasPrefix(name, "residual_") }

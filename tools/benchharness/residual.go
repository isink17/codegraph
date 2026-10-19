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
	Method       string             `json:"method"`
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
}

func (t *Transcript) Residual() Residual {
	idx := map[string]int{}
	cats := make([]ResidualCategory, len(residualCategories))
	resetCats := func() {
		for i, c := range residualCategories {
			idx[c.name] = i
			cats[i] = ResidualCategory{Category: c.name, Basis: c.basis}
		}
	}
	resetCats()
	res := Residual{Method: ResidualMethod}
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
			shares := apportion(ctx-prev, r.gap)
			if shares == nil {
				cats[idx[catUnattributed]].Tokens += ctx - prev
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
	res.Categories = cats
	return res
}

// apportion splits delta over segments by character share with the largest
// remainder method, so the shares sum to delta exactly. It returns nil when
// the segments carry no characters.
func apportion(delta int, segs []segment) []int {
	total := 0
	for _, s := range segs {
		total += s.chars
	}
	if total == 0 {
		return nil
	}
	out := make([]int, len(segs))
	rem := make([]int64, len(segs))
	left := delta
	for i, s := range segs {
		p := int64(delta) * int64(s.chars)
		out[i] = int(p / int64(total))
		rem[i] = p % int64(total)
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

func validateResidual(r *Residual, finalContext int) error {
	if r.Method != ResidualMethod {
		return fmt.Errorf("residual method %q, want %q", r.Method, ResidualMethod)
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

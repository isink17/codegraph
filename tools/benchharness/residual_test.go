package main

import (
	"strings"
	"testing"
)

func toolResultLine(id, text string) string {
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + jsonString(text) + `}]}}`
}

func residualTokens(r Residual) map[string]int {
	out := map[string]int{}
	for _, c := range r.Categories {
		out[c.Category] = c.Tokens
	}
	return out
}

func TestResidualAttribution(t *testing.T) {
	tr, err := parse(t,
		`{"type":"user","message":{"content":"question"}}`,
		asstLine("a", false, 10, 0, 90, 1, `{"type":"tool_use","id":"r","name":"Read","input":{}}`),
		asstLine("a", false, 10, 0, 90, 1, `{"type":"tool_use","id":"g","name":"mcp__codegraph__find_symbol","input":{}}`),
		asstLine("a", false, 10, 0, 90, 1, `{"type":"tool_use","id":"b","name":"Bash","input":{"command":"codegraph stats"}}`),
		toolResultLine("r", strings.Repeat("r", 600)),
		toolResultLine("g", strings.Repeat("g", 300)),
		toolResultLine("b", strings.Repeat("c", 100)),
		asstLine("s", true, 1, 0, 5000, 1, `{"type":"tool_use","id":"x","name":"Grep","input":{}}`),
		toolResultLine("x", strings.Repeat("x", 9000)),
		asstLine("b2", false, 1, 100, 1100, 1, `{"type":"text","text":"done"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	r := tr.Residual()
	got := residualTokens(r)
	if r.FinalContext != 1201 || got[catInitial] != 100 || got[catFileRead] < 600 || got[catCodegraph] < 400 || got[catSearch] != 0 || got[catOtherTool] != 0 || r.Resets != 0 {
		t.Fatalf("residual: %+v", r)
	}
	if err := validateResidual(&r, 1201); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Categories {
		if c.Category == catCodegraph && (c.Results != 2 || c.Chars != 400) {
			t.Fatalf("codegraph category counts: %+v", c)
		}
	}
}

func TestResidualResetsOnCompactionAndDrop(t *testing.T) {
	tr, err := parse(t,
		asstLine("a", false, 10, 0, 90, 1, `{"type":"tool_use","id":"r","name":"Read","input":{}}`),
		toolResultLine("r", "xxxx"),
		asstLine("b", false, 10, 0, 190, 1, `{"type":"text","text":""}`),
		`{"type":"system","subtype":"compact_boundary"}`,
		toolResultLine("r", "late"),
		asstLine("c", false, 10, 0, 40, 1, `{"type":"tool_use","id":"g","name":"Grep","input":{}}`),
		toolResultLine("g", "hits"),
		asstLine("d", false, 10, 0, 90, 1, `{"type":"text","text":"x"}`),
		asstLine("e", false, 10, 0, 30, 1, `{"type":"text","text":"y"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	r := tr.Residual()
	if got := residualTokens(r); r.Resets != 2 || r.FinalContext != 40 || got[catUnattributed] != 40 || got[catSearch] != 0 || got[catInitial] != 0 {
		t.Fatalf("after compaction and drop: %+v", r)
	}

	beforeDrop, _ := parse(t,
		asstLine("a", false, 10, 0, 90, 1, `{"type":"tool_use","id":"r","name":"Read","input":{}}`),
		toolResultLine("r", "xxxx"),
		asstLine("b", false, 10, 0, 40, 1, `{"type":"text","text":""}`),
		asstLine("c", false, 10, 0, 70, 1, `{"type":"text","text":""}`),
	)
	r = beforeDrop.Residual()
	if got := residualTokens(r); r.Resets != 1 || got[catUnattributed] != 80 || got[catFileRead] != 0 {
		t.Fatalf("empty gap must stay unattributed: %+v", r)
	}
}

func TestApportionSumsExactly(t *testing.T) {
	for _, tc := range []struct {
		delta int
		chars []int
	}{{10, []int{1, 1, 1}}, {7, []int{3, 0, 5}}, {1, []int{2, 2}}, {1000003, []int{1 << 30, 7, 1}}} {
		segs := make([]segment, len(tc.chars))
		for i, c := range tc.chars {
			segs[i].chars = c
		}
		sum := 0
		for _, v := range apportion(tc.delta, segs) {
			sum += v
		}
		if sum != tc.delta {
			t.Errorf("apportion(%d, %v) sums to %d", tc.delta, tc.chars, sum)
		}
	}
	if got := apportion(10, []segment{{chars: 1}, {chars: 1}, {chars: 1}}); got[0] != 4 || got[1] != 3 {
		t.Fatalf("ties go to the earliest segment: %v", got)
	}
	if apportion(5, []segment{{chars: 0}}) != nil {
		t.Fatal("no characters must return nil")
	}
}

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func num(v float64) *float64 { return &v }

func TestFactMatching(t *testing.T) {
	answer := "Start is called from Main.\nIt reads 1,024 rows from main.go:3."
	for _, tc := range []struct {
		f    Fact
		want bool
	}{
		{Fact{Kind: "substring", Pattern: "called from Main"}, true},
		{Fact{Kind: "substring", Pattern: "called from main"}, false},
		{Fact{Kind: "substring", Pattern: "CALLED FROM main", IgnoreCase: true}, true},
		{Fact{Kind: "regex", Pattern: `main\.go:\d+`}, true},
		{Fact{Kind: "regex", Pattern: `^It reads`}, false},
		{Fact{Kind: "regex", Pattern: `(?m)^It reads`}, true},
		{Fact{Kind: "number", Pattern: `([\d,]+) rows`, Value: num(1024)}, true},
		{Fact{Kind: "number", Pattern: `main\.go:(\d+)`, Value: num(4)}, false},
	} {
		if err := validateFact(tc.f); err != nil {
			t.Fatalf("%+v: %v", tc.f, err)
		}
		if got := tc.f.matches(answer); got != tc.want {
			t.Errorf("%+v: matched=%v, want %v", tc.f, got, tc.want)
		}
	}
}

func TestValidateTaskSetRejects(t *testing.T) {
	ok := Fact{ID: "a", Kind: "substring", Pattern: "x"}
	for name, ts := range map[string]TaskSet{
		"schema":         {Schema: "x", Tasks: []Task{{ID: "t", Required: []Fact{ok}}}},
		"no tasks":       {Schema: TaskSetSchema},
		"duplicate task": {Schema: TaskSetSchema, Tasks: []Task{{ID: "t", Required: []Fact{ok}}, {ID: "t", Required: []Fact{ok}}}},
		"no required":    {Schema: TaskSetSchema, Tasks: []Task{{ID: "t"}}},
		"duplicate fact": {Schema: TaskSetSchema, Tasks: []Task{{ID: "t", Required: []Fact{ok}, Forbidden: []Fact{ok}}}},
		"bad kind":       {Schema: TaskSetSchema, Tasks: []Task{{ID: "t", Required: []Fact{{ID: "a", Kind: "llm", Pattern: "x"}}}}},
		"bad regex":      {Schema: TaskSetSchema, Tasks: []Task{{ID: "t", Required: []Fact{{ID: "a", Kind: "regex", Pattern: "("}}}}},
		"number no grp":  {Schema: TaskSetSchema, Tasks: []Task{{ID: "t", Required: []Fact{{ID: "a", Kind: "number", Pattern: `\d+`, Value: num(1)}}}}},
		"number no val":  {Schema: TaskSetSchema, Tasks: []Task{{ID: "t", Required: []Fact{{ID: "a", Kind: "number", Pattern: `(\d+)`}}}}},
		"empty pattern":  {Schema: TaskSetSchema, Tasks: []Task{{ID: "t", Required: []Fact{{ID: "a", Kind: "substring"}}}}},
	} {
		if validateTaskSet(&ts) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFinalAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		lines  []string
		answer string
		reason string
	}{
		"text": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"one"}`), asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"two"}`)}, "one\ntwo", ""},
		"tool use": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"tool_use","id":"t","name":"Read","input":{}}`), toolResultLine("t", "x")},
			"", "ended_with_tool_use"},
		"user after": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"one"}`), `{"type":"user","message":{"content":"more?"}}`},
			"", "ended_with_user_message"},
		"repeated block": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"answer 42"}`), asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"answer 42"}`)},
			"answer 42", ""},
		"meta after": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"one"}`), `{"type":"user","isMeta":true,"message":{"content":"Caveat: local"}}`},
			"one", ""},
		"local command after": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"one"}`),
			`{"type":"user","message":{"content":"<command-name>/exit</command-name>\n<command-message>exit</command-message>"}}`,
			`{"type":"user","message":{"content":[{"type":"text","text":"<local-command-stdout>Bye!</local-command-stdout>"}]}}`},
			"one", ""},
		"text after local command": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"one"}`),
			`{"type":"user","message":{"content":"<command-name>/clear</command-name> and also why?"}}`},
			"", "ended_with_user_message"},
		"unclosed wrapper after": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"one"}`),
			`{"type":"user","message":{"content":"<command-name>/clear and why?"}}`},
			"", "ended_with_user_message"},
		"mismatched wrapper after": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"one"}`),
			`{"type":"user","message":{"content":"<command-name>/clear</command-message>"}}`},
			"", "ended_with_user_message"},
		"command args after": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"one"}`),
			`{"type":"user","message":{"content":"<command-message>model</command-message>\n<command-name>/model</command-name>\n<command-args>opus</command-args>"}}`},
			"one", ""},
		"thinking only": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"thinking","thinking":"hmm"}`)}, "", "no_answer_text"},
		"sidechain after": {[]string{asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"main"}`), asstLine("s", true, 1, 0, 0, 1, `{"type":"text","text":"side"}`)},
			"main", ""},
	} {
		tr, err := parse(t, tc.lines...)
		if err != nil {
			t.Fatal(err)
		}
		if a, r := tr.FinalAnswer(); a != tc.answer || r != tc.reason {
			t.Errorf("%s: got %q/%q, want %q/%q", name, a, r, tc.answer, tc.reason)
		}
	}
}

func TestIngestEvaluation(t *testing.T) {
	rec, err := Ingest(fixtureManifest, "2026-01-02T00:00:00Z", "")
	if err != nil {
		t.Fatal(err)
	}
	clean, treat, unfinished := rec.Sessions[0].Evaluation, rec.Sessions[2].Evaluation, rec.Sessions[3].Evaluation
	if clean.Status != "scored" || clean.RequiredMatched != 2 || clean.RequiredTotal != 3 || clean.Required[2].Matched || clean.ForbiddenHits != 0 {
		t.Fatalf("clean baseline evaluation: %+v", clean)
	}
	if treat.RequiredMatched != 3 || unfinished.Status != "unscored" || unfinished.Reason != "ended_with_tool_use" {
		t.Fatalf("treatment evaluations: %+v %+v", treat, unfinished)
	}
	if c := rec.Arms[1].Correctness; c == nil || c.Scored != 1 || c.Unscored != 1 || c.AllRequiredMet != 1 {
		t.Fatalf("treatment arm correctness: %+v", c)
	}
	if c := rec.Arms[0].Correctness; c.Scored != 1 || c.RequiredMatched != 2 {
		t.Fatalf("baseline arm correctness must exclude the contaminated session: %+v", c)
	}
	if !strings.Contains(strings.Join(rec.Limitations, "\n"), "task t2 has no session") {
		t.Fatalf("unused task not listed: %v", rec.Limitations)
	}
	sum := SummarizeIngest(rec, false)
	c, a, p, res := strings.Index(sum, "## Contamination"), strings.Index(sum, "## Answer correctness"), strings.Index(sum, "## Per arm"), strings.Index(sum, "## Residual context per arm")
	if c < 0 || a < c || p < a || res < p || !strings.Contains(sum, "not a semantic judgment") {
		t.Fatalf("summary order or labels:\n%s", sum)
	}
	if !strings.Contains(sum, "| codegraph-mcp | 1 | 1 | 3/3 | 0 | 1 |") || !strings.Contains(sum, "unattributed (unavailable)") {
		t.Fatalf("summary:\n%s", sum)
	}
}

func TestIngestRejectsTaskSetMismatch(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Manifest)
		want string
	}{
		"sha":              {func(m *Manifest) { m.TaskSetSHA256 = strings.Repeat("0", 64) }, "task set sha256"},
		"unknown task":     {func(m *Manifest) { m.Sessions[0].Task = "t9" }, "not in the task set"},
		"absolute":         {func(m *Manifest) { m.TaskSet = "/etc/tasks.json" }, "relative path"},
		"windows absolute": {func(m *Manifest) { m.TaskSet = `C:\etc\tasks.json` }, "relative path"},
		"missing file":     {func(m *Manifest) { m.TaskSet = "nope.json" }, "nope.json"},
	} {
		_, err := Ingest(copyFixture(t, tc.edit), "2026-01-02T00:00:00Z", "")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
	noTasks, err := Ingest(copyFixture(t, func(m *Manifest) { m.TaskSet = "" }), "2026-01-02T00:00:00Z", "")
	if err != nil {
		t.Fatal(err)
	}
	if noTasks.TaskSetJSON != "" || noTasks.Sessions[0].Evaluation != nil || noTasks.Arms[0].Correctness != nil || noTasks.Limitations[0] != limitNoEvaluation {
		t.Fatal("ingest without a task set must not score")
	}
	if err := ValidateIngest(noTasks); err != nil {
		t.Fatal(err)
	}
}

func TestValidateIngestRejectsEvaluationAndResidualTampering(t *testing.T) {
	for name, edit := range map[string]func(*IngestRecord){
		"matched count":   func(r *IngestRecord) { r.Sessions[0].Evaluation.RequiredMatched = 3 },
		"fact flipped":    func(r *IngestRecord) { r.Sessions[0].Evaluation.Required[2].Matched = true },
		"forbidden count": func(r *IngestRecord) { r.Sessions[0].Evaluation.ForbiddenHits = 1 },
		"unscored reason": func(r *IngestRecord) { r.Sessions[3].Evaluation.Reason = "judge said so" },
		"evaluation gone": func(r *IngestRecord) { r.Sessions[0].Evaluation = nil },
		"task set gone":   func(r *IngestRecord) { r.TaskSetJSON = "" },
		"fact pattern": func(r *IngestRecord) {
			edited := strings.Replace(r.TaskSetJSON, `"called from\\s+main"`, `"called from"`, 1)
			if edited == r.TaskSetJSON {
				t.Fatal("fact pattern not found in stored task set")
			}
			r.TaskSetJSON = edited
		},
		"arm correctness": func(r *IngestRecord) { r.Arms[1].Correctness.AllRequiredMet = 2 },
		"residual sum":    func(r *IngestRecord) { r.Sessions[2].Residual.Categories[0].Tokens++ },
		"residual basis":  func(r *IngestRecord) { r.Sessions[2].Residual.Categories[0].Basis = basisMeasured },
		"residual gone":   func(r *IngestRecord) { r.Sessions[2].Residual = nil },
		"v1 with extras":  func(r *IngestRecord) { r.Schema, r.Methodology = IngestSchemaV1, IngestMethodologyV1 },
	} {
		rec, err := Ingest(fixtureManifest, "2026-01-02T00:00:00Z", "")
		if err != nil {
			t.Fatal(err)
		}
		edit(rec)
		if ValidateIngest(rec) == nil {
			t.Errorf("%s: tampered record accepted", name)
		}
	}
}

func TestV1IngestRecordStillValidates(t *testing.T) {
	const v1 = "testdata/ingest-v1.json"
	var out bytes.Buffer
	if err := run([]string{"validate", v1}, &out); err != nil || out.String() != "valid\n" {
		t.Fatalf("validate v1: %v %q", err, out.String())
	}
	out.Reset()
	if err := run([]string{"summarize", v1}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, IngestSchemaV1) || strings.Contains(s, "## Residual") || !strings.Contains(s, "Not measured") {
		t.Fatalf("v1 summary:\n%s", s)
	}
	b, err := os.ReadFile(v1)
	if err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(t.TempDir(), "v1.json")
	if err := os.WriteFile(tampered, bytes.Replace(b, []byte(`"processed_tokens": 750`), []byte(`"processed_tokens": 751`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"validate", tampered}, &bytes.Buffer{}); err == nil {
		t.Fatal("tampered v1 record accepted")
	}
}

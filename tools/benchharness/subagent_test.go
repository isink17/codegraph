package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const subagentManifest = "testdata/subagents/manifest.json"

func ingestSubagents(t *testing.T, manifest, cal string) *IngestRecord {
	t.Helper()
	rec, err := Ingest(manifest, "2026-01-02T00:00:00Z", cal)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateIngest(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestIngestSubagents(t *testing.T) {
	rec := ingestSubagents(t, subagentManifest, "")
	if rec.Schema != IngestSchema || len(rec.Sessions) != 4 {
		t.Fatalf("schema %s, %d sessions", rec.Schema, len(rec.Sessions))
	}
	clean, viaAgent, missing, treat := rec.Sessions[0], rec.Sessions[1], rec.Sessions[2], rec.Sessions[3]

	var got []string
	for _, a := range clean.Subagents {
		got = append(got, a.AgentID+"<"+a.Parent+"@"+itoa(a.Depth)+":"+itoa(a.Requests)+"/"+itoa(a.ProcessedTokens)+"/"+itoa(a.FinalContext)+"/"+itoa(a.DuplicateRequests))
	}
	// aaa repeats a1 within its file; bbb is a fork whose copy of m1 and the
	// in-main sidechain b1 count once; ccc is nested; ddd is unlinked.
	if want := "aaa<main@1:3/688/248/0 bbb<main@1:2/1027/524/1 ccc<aaa@2:2/215/113/0 ddd<@0:1/51/51/0"; strings.Join(got, " ") != want {
		t.Fatalf("subagents %v, want %s", got, want)
	}
	if clean.ProcessedTokens != 1080 || clean.FinalContext != 415 || clean.PeakContext != 415 || clean.OutputTokens != 75 || clean.ToolCallsTotal != 2 || clean.Sidechain.Requests != 0 {
		t.Fatalf("main totals must stay main-only: %+v", clean.SessionMetrics)
	}
	if w := *clean.Workload; w != (Workload{MainProcessed: 1080, SubagentProcessed: 1981, TotalProcessed: 3061, LinkedSubagents: 3, UnlinkedSubagents: 1}) {
		t.Fatalf("workload %+v", w)
	}
	if c := clean.Contamination; !c.Checked || c.Contaminated || len(c.Calls) != 0 {
		t.Fatalf("fully linked clean subagents must not contaminate: %+v", c)
	}
	if c := viaAgent.Contamination; !c.Contaminated || len(c.Calls) != 1 || c.Calls[0].Agent != "eee" || c.Calls[0].Kind != "cli" || c.Calls[0].Outcome != "succeeded" {
		t.Fatalf("subagent codegraph use must contaminate the baseline: %+v", c)
	}
	if c := missing.Contamination; !c.Contaminated || c.Calls[0].Agent != "main" || c.Calls[0].Outcome != "unverified" || missing.Workload.UnlinkedAgentCalls != 1 {
		t.Fatalf("Agent call without a subagent file must stay unverified: %+v %+v", c, missing.Workload)
	}
	if treat.Contamination.Checked || len(treat.Subagents) != 1 || treat.Workload.TotalProcessed != 880 {
		t.Fatalf("treatment: %+v", treat)
	}
	if a := rec.Arms[0]; a.Contaminated != 2 || a.Included != 1 {
		t.Fatalf("baseline arm: %+v", a)
	}
	sum := SummarizeIngest(rec, false)
	for _, s := range []string{"## Subagents", "| baseline | t1 | 1 | ddd | unlinked | 0 |", "baseline/t1/2 [eee]: succeeded cli", "context sizes are per agent and never summed"} {
		if !strings.Contains(sum, s) {
			t.Fatalf("summary lacks %q:\n%s", s, sum)
		}
	}
}

func TestLinkNestedUnlinkedCall(t *testing.T) {
	main, err := parse(t,
		asstLine("m1", false, 1, 0, 10, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`),
		`{"type":"user","toolUseResult":{"agentId":"x1"},"message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := parseTranscript(strings.NewReader(asstLine("s1", true, 1, 0, 10, 1, `{"type":"tool_use","id":"tu2","name":"Task","input":{}}`)+"\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	subs := []*agentFile{{res: SubagentResult{AgentID: "x1"}, t: sub}}
	checked, unlinked, threads, err := linkSubagents(main, subs, true)
	if err != nil {
		t.Fatal(err)
	}
	calls := sessionContamination(threads, checked).Calls
	if !checked["tu1"] || unlinked != 1 || len(calls) != 1 || calls[0].Agent != "x1" || calls[0].Outcome != "unverified" {
		t.Fatalf("nested call without a file: checked=%v unlinked=%d calls=%+v", checked, unlinked, calls)
	}
}

// copySubagentFixture copies the subagent fixture, applies edit to the copy's
// directory, then rewrites declared subagent hashes unless keepHashes.
func copySubagentFixture(t *testing.T, edit func(dir string), keepHashes bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "f")
	if err := os.CopyFS(dir, os.DirFS("testdata/subagents")); err != nil {
		t.Fatal(err)
	}
	edit(dir)
	path := filepath.Join(dir, "manifest.json")
	var m Manifest
	if err := decodeStrict(path, &m); err != nil {
		t.Fatal(err)
	}
	if !keepHashes {
		for _, s := range m.Sessions {
			if s.Subagents == nil {
				continue
			}
			for i := range *s.Subagents {
				ref := &(*s.Subagents)[i]
				if b, err := os.ReadFile(filepath.Join(dir, ref.Transcript)); err == nil {
					ref.TranscriptSHA256 = sha256Hex(b)
				}
			}
		}
	}
	b, _ := marshalJSON(&m)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func rewrite(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(old)) {
		t.Fatalf("%s: %q not found", path, old)
	}
	if err := os.WriteFile(path, bytes.Replace(b, []byte(old), []byte(new), 1), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIngestRejectsSubagentMismatch(t *testing.T) {
	sub := func(dir, session, name string) string { return filepath.Join(dir, session, "subagents", name) }
	for name, tc := range map[string]struct {
		edit       func(dir string)
		keepHashes bool
		want       string
	}{
		"extra file": {func(d string) {
			_ = os.WriteFile(sub(d, "baseline-agents", "agent-zzz.jsonl"), []byte(asstLine("z1", true, 1, 0, 1, 1, `{"type":"text","text":"z"}`)+"\n"), 0o644)
		}, false, "is not listed in the manifest"},
		"extra file in declared-empty session": {func(d string) {
			_ = os.MkdirAll(filepath.Join(d, "baseline-missing", "subagents"), 0o755)
			_ = os.WriteFile(sub(d, "baseline-missing", "agent-fff.jsonl"), []byte(asstLine("f1", true, 1, 0, 1, 1, `{"type":"text","text":"f"}`)+"\n"), 0o644)
		}, false, "is not listed in the manifest"},
		"missing file": {func(d string) { _ = os.Remove(sub(d, "baseline-agents", "agent-ddd.jsonl")) }, false, "listed in the manifest but missing"},
		"hash mismatch": {func(d string) {
			rewrite(t, sub(d, "baseline-agents", "agent-aaa.jsonl"), `"Start in main.go"`, `"Start in main.go!"`)
		}, true, "sha256"},
		"conflicting usage": {func(d string) {
			rewrite(t, sub(d, "baseline-agents", "agent-bbb.jsonl"), `"cache_creation_input_tokens": 300`, `"cache_creation_input_tokens": 301`)
		}, false, "conflicting usage across transcript files"},
		"agent id mismatch": {func(d string) {
			rewrite(t, sub(d, "baseline-agents", "agent-ccc.jsonl"), `"agentId": "ccc"`, `"agentId": "cc2"`)
		}, false, `agentId "cc2"`},
		"client version": {func(d string) {
			rewrite(t, sub(d, "baseline-agents", "agent-ddd.jsonl"), `"version": "9.9.9-test"`, `"version": "0.0.1"`)
		}, false, "client version"},
		"claimed twice": {func(d string) {
			rewrite(t, filepath.Join(d, "baseline-agents.jsonl"), `"agentId": "bbb"`, `"agentId": "aaa"`)
			b, _ := os.ReadFile(filepath.Join(d, "baseline-agents.jsonl"))
			var m Manifest
			_ = decodeStrict(filepath.Join(d, "manifest.json"), &m)
			m.Sessions[0].TranscriptSHA256 = sha256Hex(b)
			out, _ := marshalJSON(&m)
			_ = os.WriteFile(filepath.Join(d, "manifest.json"), out, 0o644)
		}, false, "more than one Task/Agent result"},
	} {
		_, err := Ingest(copySubagentFixture(t, tc.edit, tc.keepHashes), "2026-01-02T00:00:00Z", "")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}

	// Without a declared list, discovered files are recorded with their hash.
	path := copySubagentFixture(t, func(d string) {
		rewrite(t, sub(d, "baseline-subagent-cg", "agent-eee.jsonl"), `"text": "main"`, `"text": "main!"`)
	}, false)
	rec := ingestSubagents(t, path, "")
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(path), rec.Sessions[1].Subagents[0].Transcript))
	if rec.Sessions[1].Subagents[0].TranscriptSHA256 != sha256Hex(b) {
		t.Fatal("discovered subagent hash not recorded")
	}
}

func TestValidateIngestRejectsSubagentTampering(t *testing.T) {
	for name, edit := range map[string]func(*IngestRecord){
		"dropped subagent": func(r *IngestRecord) { r.Sessions[0].Subagents = r.Sessions[0].Subagents[:3] },
		"dropped undeclared subagent": func(r *IngestRecord) {
			r.Sessions[1].Subagents = nil
			r.Sessions[1].Contamination = Contamination{Checked: true, Calls: []ContaminationCall{}}
		},
		"parent":          func(r *IngestRecord) { r.Sessions[0].Subagents[2].Parent = "main" },
		"depth":           func(r *IngestRecord) { r.Sessions[0].Subagents[2].Depth = 3 },
		"unlinked parent": func(r *IngestRecord) { r.Sessions[0].Subagents[2].Parent = "ddd" },
		"subagent sha":    func(r *IngestRecord) { r.Sessions[0].Subagents[0].TranscriptSHA256 = strings.Repeat("0", 64) },
		"subagent path":   func(r *IngestRecord) { r.Sessions[0].Subagents[0].Transcript = "elsewhere/agent-aaa.jsonl" },
		"subagent tokens": func(r *IngestRecord) { r.Sessions[0].Subagents[0].ProcessedTokens++ },
		"workload total":  func(r *IngestRecord) { r.Sessions[0].Workload.TotalProcessed++ },
		"workload gone":   func(r *IngestRecord) { r.Sessions[0].Workload = nil },
		"hidden unverified call": func(r *IngestRecord) {
			r.Sessions[2].Contamination = Contamination{Checked: true, Calls: []ContaminationCall{}}
		},
		"unverified flipped": func(r *IngestRecord) {
			r.Sessions[2].Contamination.Calls[0].Outcome = "blocked"
			r.Sessions[2].Contamination.Contaminated = false
			r.Sessions[2].Contamination.BlockedAttempts = 1
		},
		"unknown agent": func(r *IngestRecord) { r.Sessions[1].Contamination.Calls[0].Agent = "zzz" },
		"v2 with subagents": func(r *IngestRecord) {
			r.Schema, r.Methodology = IngestSchemaV2, IngestMethodologyV2
		},
	} {
		rec := ingestSubagents(t, subagentManifest, "")
		edit(rec)
		if ValidateIngest(rec) == nil {
			t.Errorf("%s: tampered record accepted", name)
		}
	}
}

func TestV2IngestRecordStillValidates(t *testing.T) {
	const v2 = "testdata/ingest-v2.json"
	var out bytes.Buffer
	if err := run([]string{"validate", v2}, &out); err != nil || out.String() != "valid\n" {
		t.Fatalf("validate v2: %v %q", err, out.String())
	}
	out.Reset()
	if err := run([]string{"summarize", v2}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, IngestSchemaV2) || !strings.Contains(s, "## Residual context per arm") || strings.Contains(s, "## Subagents") {
		t.Fatalf("v2 summary:\n%s", s)
	}
	b, err := os.ReadFile(v2)
	if err != nil {
		t.Fatal(err)
	}
	var r IngestRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	r.Sessions[0].Residual.Uncalibrated = true
	if ValidateIngest(&r) == nil {
		t.Fatal("v2 record with a v3 residual flag accepted")
	}
}

func TestIngestSubagentsDeterministic(t *testing.T) {
	dir := t.TempDir()
	var outs [2][]byte
	for i := range outs {
		out := filepath.Join(dir, "ingest"+itoa(i)+".json")
		if err := run([]string{"ingest", "--manifest", subagentManifest, "--calibration", calibrationFixture, "--date", "2026-01-02T00:00:00Z", "--out", out}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		outs[i], _ = os.ReadFile(out)
	}
	if !bytes.Equal(outs[0], outs[1]) || len(outs[0]) == 0 {
		t.Fatal("ingest output differs between runs")
	}
}

func spawnResult(id, agent string) string {
	return `{"type":"user","toolUseResult":{"agentId":"` + agent + `"},"message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"ok"}]}}`
}

func ownFile(t *testing.T, lines ...string) *Transcript {
	t.Helper()
	tr, err := parseTranscript(strings.NewReader(strings.Join(lines, "\n")+"\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func sessionCalls(t *testing.T, main *Transcript, subs ...*agentFile) (Contamination, int) {
	t.Helper()
	checked, unlinked, threads, err := linkSubagents(main, subs, true)
	if err != nil {
		t.Fatal(err)
	}
	return sessionContamination(threads, checked), unlinked
}

func TestContaminationReadsDuplicateCopies(t *testing.T) {
	cg := `{"type":"tool_use","id":"cg1","name":"mcp__codegraph__find_symbol","input":{}}`
	// A subagent's copy of a main message.id carries a codegraph call that
	// the owning copy does not.
	main, err := parse(t,
		asstLine("m0", false, 1, 0, 10, 1, `{"type":"text","text":"hi"}`),
		asstLine("m1", false, 1, 10, 5, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`),
		spawnResult("tu1", "x1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	x1 := ownFile(t, asstLine("m0", true, 1, 0, 10, 1, cg), toolResultLine("cg1", "Start main.go:3"), asstLine("s1", true, 1, 0, 30, 1, `{"type":"text","text":"done"}`))
	c, _ := sessionCalls(t, main, &agentFile{res: SubagentResult{AgentID: "x1"}, t: x1})
	if !c.Contaminated || len(c.Calls) != 1 || c.Calls[0].Agent != "x1" || c.Calls[0].Outcome != "succeeded" {
		t.Fatalf("codegraph call on a dropped copy: %+v", c)
	}

	// An in-file sidechain call stays checked when a subagent file owns the
	// request with text only.
	main, err = parse(t,
		asstLine("m0", false, 1, 0, 10, 1, `{"type":"text","text":"hi"}`),
		asstLine("s1", true, 1, 0, 30, 1, cg),
		toolResultLine("cg1", "Start main.go:3"),
		asstLine("m1", false, 1, 10, 5, 1, `{"type":"text","text":"done"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	x1 = ownFile(t, asstLine("s1", true, 1, 0, 30, 1, `{"type":"text","text":"side"}`))
	c, _ = sessionCalls(t, main, &agentFile{res: SubagentResult{AgentID: "x1"}, t: x1})
	if !c.Contaminated || len(c.Calls) != 1 || c.Calls[0].Agent != "sidechain" {
		t.Fatalf("in-file sidechain call hidden by a text-only copy: %+v", c)
	}

	// A copy that ran is not hidden by a blocked copy of the same tool_use.
	b := ownFile(t, asstLine("b0", true, 1, 0, 10, 1, `{"type":"tool_use","id":"cli","name":"Bash","input":{"command":"codegraph stats"}}`),
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"cli","is_error":true,"content":"bash: codegraph: command not found"}]}}`)
	r := ownFile(t, asstLine("b0", true, 1, 0, 10, 1, `{"type":"tool_use","id":"cli","name":"Bash","input":{"command":"codegraph stats"}}`), toolResultLine("cli", "files: 3"))
	c = sessionContamination([]thread{{"b", b, false}, {"r", r, false}}, nil)
	if !c.Contaminated || len(c.Calls) != 1 || c.Calls[0].Outcome != "succeeded" || c.BlockedAttempts != 0 {
		t.Fatalf("blocked copy masks a run: %+v", c)
	}
}

func TestLinksCallsOnDroppedCopies(t *testing.T) {
	main, err := parse(t,
		asstLine("m0", false, 1, 0, 10, 1, `{"type":"tool_use","id":"tu1","name":"Agent","input":{}}`),
		spawnResult("tu1", "y1"),
		asstLine("s9", true, 1, 0, 30, 1, `{"type":"tool_use","id":"tu9","name":"Task","input":{}}`),
		asstLine("m1", false, 1, 10, 5, 1, `{"type":"text","text":"done"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	// y1 holds a copy of main's m0 that also spawns z2; x1 owns s9 as text.
	y1 := ownFile(t, asstLine("m0", true, 1, 0, 10, 1, `{"type":"tool_use","id":"tu5","name":"Agent","input":{}}`), spawnResult("tu5", "z2"))
	z2 := ownFile(t, asstLine("z", true, 1, 0, 5, 1, `{"type":"text","text":"z"}`))
	x1 := ownFile(t, asstLine("s9", true, 1, 0, 30, 1, `{"type":"text","text":"side"}`))
	subs := []*agentFile{{res: SubagentResult{AgentID: "x1"}, t: x1}, {res: SubagentResult{AgentID: "y1"}, t: y1}, {res: SubagentResult{AgentID: "z2"}, t: z2}}
	c, unlinked := sessionCalls(t, main, subs...)
	if subs[2].res.Parent != "y1" || subs[2].res.Depth != 2 || unlinked != 1 || len(c.Calls) != 1 || c.Calls[0].ToolUseID != "tu9" || c.Calls[0].Outcome != "unverified" {
		t.Fatalf("z2 parent %q depth %d, unlinked %d, calls %+v", subs[2].res.Parent, subs[2].res.Depth, unlinked, c.Calls)
	}
}

func TestValidateRejectsZeroRatioCalibration(t *testing.T) {
	rec := ingestSubagents(t, fixtureManifest, calibrationFixture)
	rec.Calibration.Global.Median, rec.Calibration.Global.Q1, rec.Calibration.Global.Min, rec.Calibration.Global.Q3 = 0, 0, 0, 0
	for i := range rec.Calibration.Categories {
		rec.Calibration.Categories[i].Source = "global"
	}
	if ValidateIngest(rec) == nil {
		t.Fatal("zero-ratio calibration accepted")
	}
	if e := calibratedEstimate(10, RatioFit{}, "global"); e.Estimate != 10000 {
		t.Fatalf("zero ratio guard: %+v", e)
	}
}

func TestDiscoverRejectsBadAgentName(t *testing.T) {
	path := copySubagentFixture(t, func(d string) {
		_ = os.WriteFile(filepath.Join(d, "baseline-agents", "subagents", "agent-a.b.jsonl"), []byte("\n"), 0o644)
	}, false)
	if _, err := Ingest(path, "2026-01-02T00:00:00Z", ""); err == nil || !strings.Contains(err.Error(), "agent-a.b.jsonl") {
		t.Fatalf("bad agent file name: %v", err)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureManifest = "testdata/transcripts/manifest.json"

func parse(t *testing.T, lines ...string) (*Transcript, error) {
	t.Helper()
	return ParseTranscript(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

func asstLine(id string, side bool, in, cr, cc, out int, block string) string {
	sc := "false"
	if side {
		sc = "true"
	}
	return `{"type":"assistant","isSidechain":` + sc + `,"version":"1","message":{"id":"` + id + `","model":"m","content":[` + block +
		`],"usage":{"input_tokens":` + itoa(in) + `,"cache_read_input_tokens":` + itoa(cr) + `,"cache_creation_input_tokens":` + itoa(cc) + `,"output_tokens":` + itoa(out) + `}}}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestParseDeduplicatesByMessageID(t *testing.T) {
	tr, err := parse(t,
		asstLine("a", false, 10, 0, 100, 3, `{"type":"text","text":"x"}`),
		asstLine("a", false, 10, 0, 100, 40, `{"type":"tool_use","id":"t1","name":"Read","input":{}}`),
		asstLine("a", false, 10, 0, 100, 7, `{"type":"tool_use","id":"t1","name":"Read","input":{}}`),
		asstLine("b", false, 1, 110, 5, 9, `{"type":"text","text":"y"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	m := tr.Metrics()
	if m.Requests != 2 || m.ProcessedTokens != 226 || m.OutputTokens != 49 || m.ToolCalls["Read"] != 1 {
		t.Fatalf("got %+v", m)
	}
}

func TestParseRejectsConflictingUsage(t *testing.T) {
	_, err := parse(t,
		asstLine("a", false, 10, 0, 100, 3, `{"type":"text","text":"x"}`),
		asstLine("a", false, 11, 0, 100, 3, `{"type":"text","text":"x"}`),
	)
	if err == nil || !strings.Contains(err.Error(), "conflicting usage") {
		t.Fatalf("want conflicting usage error, got %v", err)
	}
}

func TestParseRejectsInvalidInput(t *testing.T) {
	for name, lines := range map[string][]string{
		"malformed json":   {`{"type":`},
		"missing id":       {`{"type":"assistant","message":{"model":"m","usage":{}}}`},
		"missing usage":    {`{"type":"assistant","message":{"id":"a","model":"m"}}`},
		"bad timestamp":    {`{"type":"user","timestamp":"yesterday"}`},
		"no main request":  {asstLine("s", true, 1, 0, 0, 1, `{"type":"text","text":"x"}`)},
		"side and main id": {asstLine("a", false, 1, 0, 0, 1, `{"type":"text","text":"x"}`), asstLine("a", true, 1, 0, 0, 1, `{"type":"text","text":"x"}`)},
	} {
		if _, err := parse(t, lines...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSidechainExcludedFromMainTotals(t *testing.T) {
	tr, err := parse(t,
		asstLine("a", false, 10, 0, 100, 5, `{"type":"text","text":"x"}`),
		asstLine("s", true, 50, 0, 500, 30, `{"type":"tool_use","id":"t9","name":"Grep","input":{}}`),
		`{"type":"assistant","message":{"id":"syn","model":"<synthetic>","content":[],"usage":{"input_tokens":0,"output_tokens":0}}}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	m := tr.Metrics()
	if m.Requests != 1 || m.ProcessedTokens != 110 || m.ToolCallsTotal != 0 || len(m.Models) != 1 {
		t.Fatalf("main totals include sidechain: %+v", m)
	}
	if s := m.Sidechain; s.Requests != 1 || s.ProcessedTokens != 550 || s.ToolCalls != 1 || s.OutputTokens != 30 {
		t.Fatalf("sidechain stats: %+v", s)
	}
}

func TestProcessedResidualPeakAndCompaction(t *testing.T) {
	tr, err := parse(t,
		`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"content":"q"}}`,
		asstLine("a", false, 10, 0, 300, 1, `{"type":"text","text":"x"}`),
		`{"type":"system","subtype":"compact_boundary"}`,
		asstLine("b", false, 20, 0, 80, 1, `{"type":"text","text":"y"}`),
		`{"type":"user","timestamp":"2026-01-01T00:00:01.500Z","message":{"content":"q"}}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	m := tr.Metrics()
	if m.ProcessedTokens != 410 || m.FinalContext != 100 || m.PeakContext != 310 || m.Compactions != 1 {
		t.Fatalf("got processed=%d final=%d peak=%d compactions=%d", m.ProcessedTokens, m.FinalContext, m.PeakContext, m.Compactions)
	}
	if m.WallClockMS == nil || *m.WallClockMS != 1500 {
		t.Fatalf("wall clock %v", m.WallClockMS)
	}
}

func TestCLIPattern(t *testing.T) {
	hits := []string{
		"codegraph find_symbol Foo",
		"/usr/local/bin/codegraph stats",
		"FOO=1 BAR=x codegraph index .",
		"cd /src && codegraph stats",
		"x=$(codegraph stats)",
		"(cd repo; ./bin/codegraph callers X)",
		"env codegraph stats",
		"ls\ncodegraph stats",
		"go run ./cmd/codegraph index .",
		"echo x | codegraph",
		"env X=1 codegraph stats",
		`bash -c "codegraph stats"`,
		`bash -c 'codegraph stats'`,
		`sh -c 'cd /x && codegraph stats'`,
		"timeout 60 codegraph stats",
		"nice codegraph stats",
		`"$HOME/go/bin/codegraph" stats`,
		`'codegraph' stats`,
		`\codegraph stats`,
		"{ codegraph stats; }",
		"if codegraph stats; then :; fi",
		"! codegraph stats",
		"time -p codegraph",
		"sudo -u x codegraph",
		"xargs -n1 codegraph",
		"xargs -n 1 codegraph",
		"codegraph.exe stats",
		`C:\\tools\\codegraph.exe stats`,
		"npx codegraph stats",
		"npx -y @isink17/codegraph@latest stats",
		"go run github.com/isink17/codegraph/cmd/codegraph@latest stats",
		`echo "$(codegraph stats)"`,
		"echo `codegraph stats`",
		"while true; do codegraph stats; done",
	}
	misses := []string{
		"grep -rn codegraph .",
		"ls .codegraph",
		"cd /work/src/codegraph && go test ./...",
		"cd ~/Projekti/codegraph-worker-a",
		"which codegraph",
		"cat codegraph.txt",
		"./bin/notcodegraph stats",
		"go test ./cmd/codegraph/...",
		`cd "/x/codegraph"`,
		"git -C /x/codegraph status",
		`echo "codegraph stats"`,
		"bash ./codegraph.sh",
		"npx codegraph-other",
		"command -v codegraph",
		"command -V codegraph",
	}
	for _, c := range hits {
		if !runsCodegraph(c) {
			t.Errorf("not detected: %q", c)
		}
	}
	for _, c := range misses {
		if runsCodegraph(c) {
			t.Errorf("false positive: %q", c)
		}
	}
}

func TestContaminationOutcomes(t *testing.T) {
	use := func(id, name, input string) string {
		return asstLine("m"+id, false, 1, 0, 0, 1, `{"type":"tool_use","id":"`+id+`","name":"`+name+`","input":`+input+`}`)
	}
	result := func(id, text string, isErr bool) string {
		e := ""
		if isErr {
			e = `,"is_error":true`
		}
		return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + jsonString(text) + e + `}]}}`
	}
	tr, err := parse(t,
		use("1", "Bash", `{"command":"codegraph stats"}`), result("1", "PreToolUse:Bash hook error: blocked", true),
		use("2", "Bash", `{"command":"codegraph stats"}`), result("2", "bash: codegraph: command not found", true),
		use("3", "Bash", `{"command":"codegraph stats"}`), result("3", "error: not indexed", true),
		use("4", "mcp__codegraph__find_symbol", `{}`), result("4", "ok", false),
		use("5", "Skill", `{"skill":"codegraph-repo-nav"}`),
		use("6", "mcp__plugin_codegraph_cg__stats", `{}`), result("6", "Permission to use it has been denied.", true),
		use("7", "mcp__other__codegraph_lookalike", `{}`), result("7", "ok", false),
		use("8", "Bash", `{"command":"grep codegraph README.md"}`), result("8", "ok", false),
		use("9", "Bash", `{"command":"codegraph find_symbol useAuth && npm test"}`), result("9", "FAIL src/hooks/useAuth.test.ts", true),
		use("10", "Bash", `{"command":"codegraph stats && rg foo"}`), result("10", "files: 3\nbash: rg: command not found", true),
		use("11", "Bash", `{"command":"codegraph stats"}`), result("11", "zsh: command not found: codegraph", true),
		use("12", "Task", `{"prompt":"explore"}`), result("12", "done", false),
		use("13", "Bash", `{"command":"~/go/bin/codegraph stats; codegraph index"}`), result("13", "files: 3\nbash: line 1: codegraph: command not found", true),
		use("14", "Bash", `{"command":"codegraph stats"}`), result("14", "Exit code 127\nbash: line 1: codegraph: command not found", true),
		use("15", "Bash", `{"command":"codegraph stats"}`), result("15", "Exit code 1\nbash: line 1: codegraph: command not found\nfiles: 3\nerror: bad flag", true),
		use("16", "Bash", `{"command":"codegraph stats"}`), result("16", "files: 3\nbash: line 2: codegraph: command not found", true),
		use("17", "Bash", `{"command":"codegraph stats"}`), result("17", "symbols: 10\nlog: codegraph: command not found", true),
		use("18", "Bash", `{"command":"codegraph stats"}`), result("18", "sh: 1: codegraph: not found", true),
	)
	if err != nil {
		t.Fatal(err)
	}
	c := tr.Contamination()
	var got []string
	for _, call := range c.Calls {
		got = append(got, call.ToolUseID+":"+call.Kind+":"+call.Outcome)
	}
	want := "1:cli:blocked 2:cli:blocked 3:cli:error 4:mcp:succeeded 5:skill:no_result 6:mcp:blocked 9:cli:error 10:cli:error 11:cli:blocked 12:subagent:unverified" +
		" 13:cli:error 14:cli:blocked 15:cli:error 16:cli:error 17:cli:error 18:cli:blocked"
	if strings.Join(got, " ") != want || !c.Contaminated || c.BlockedAttempts != 6 {
		t.Fatalf("got %v contaminated=%v blocked=%d", got, c.Contaminated, c.BlockedAttempts)
	}

	blockedOnly, err := parse(t, use("1", "Bash", `{"command":"codegraph stats"}`), result("1", "Hook PreToolUse:Bash denied this tool", true))
	if err != nil {
		t.Fatal(err)
	}
	if c := blockedOnly.Contamination(); c.Contaminated || c.BlockedAttempts != 1 {
		t.Fatalf("blocked-only attempt must not contaminate: %+v", c)
	}

	subagentOnly, err := parse(t, use("1", "Agent", `{"prompt":"look around"}`), result("1", "done", false))
	if err != nil {
		t.Fatal(err)
	}
	if c := subagentOnly.Contamination(); !c.Contaminated || c.Calls[0].Outcome != "unverified" {
		t.Fatalf("unchecked subagent must contaminate: %+v", c)
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestIngestFixture(t *testing.T) {
	rec, err := Ingest(fixtureManifest, "2026-01-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateIngest(rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Sessions) != 3 || rec.Sessions[0].Arm != "baseline" || rec.Sessions[0].Rep != 1 {
		t.Fatalf("sessions not sorted: %+v", rec.Sessions)
	}
	clean, dirty, treat := rec.Sessions[0], rec.Sessions[1], rec.Sessions[2]
	if clean.ProcessedTokens != 750 || clean.FinalContext != 100 || clean.PeakContext != 202 || clean.Compactions != 1 ||
		clean.Sidechain.ProcessedTokens != 550 || clean.Contamination.Contaminated || clean.Contamination.BlockedAttempts != 1 {
		t.Fatalf("clean baseline: %+v", clean)
	}
	if !dirty.Contamination.Contaminated || treat.Contamination.Checked {
		t.Fatalf("contamination flags: dirty=%+v treatment=%+v", dirty.Contamination, treat.Contamination)
	}
	base := rec.Arms[0]
	if base.Sessions != 2 || base.Contaminated != 1 || base.BlockedOnly != 1 || base.Included != 1 || base.Stats[0].Median != 750 {
		t.Fatalf("baseline arm: %+v", base)
	}
	withAll := armSummaries(rec.Sessions, true)[0]
	if withAll.Included != 2 || withAll.Stats[0].Min != 427 || withAll.Stats[0].Max != 750 || withAll.Stats[0].Median != 427 {
		t.Fatalf("include contaminated: %+v", withAll)
	}
}

func TestIngestDeterministic(t *testing.T) {
	dir := t.TempDir()
	var outs [2][]byte
	for i := range outs {
		out := filepath.Join(dir, "ingest"+itoa(i)+".json")
		if err := run([]string{"ingest", "--manifest", fixtureManifest, "--date", "2026-01-02T00:00:00Z", "--out", out}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		outs[i] = b
	}
	if !bytes.Equal(outs[0], outs[1]) {
		t.Fatal("ingest output differs between runs")
	}
	out := filepath.Join(dir, "ingest0.json")
	var v bytes.Buffer
	if err := run([]string{"validate", out}, &v); err != nil || v.String() != "valid\n" {
		t.Fatalf("validate: %v %q", err, v.String())
	}
	if err := run([]string{"validate", "--expect-sha", strings.Repeat("1", 40), out}, &bytes.Buffer{}); err != nil {
		t.Fatalf("validate --expect-sha matching: %v", err)
	}
	if err := run([]string{"validate", "--expect-sha", strings.Repeat("9", 40), out}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("validate --expect-sha mismatch: %v", err)
	}
	if err := run([]string{"validate", "--fixture", "testdata/go-calls", out}, &bytes.Buffer{}); err == nil {
		t.Fatal("validate --fixture accepted for an ingest record")
	}
	if err := run([]string{"compare", out, out}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("compare on ingest record: %v", err)
	}
	var s1, s2 bytes.Buffer
	if err := run([]string{"summarize", out}, &s1); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"summarize", out}, &s2); err != nil || s1.String() != s2.String() {
		t.Fatalf("summarize not reproducible: %v", err)
	}
	sum := s1.String()
	if c, p := strings.Index(sum, "## Contamination"), strings.Index(sum, "## Per arm"); c < 0 || p < c {
		t.Fatal("contamination section must come before per-arm statistics")
	}
	if !strings.Contains(sum, "| baseline | false | 2 | 1 | 1 | 1 |") || !strings.Contains(sum, "not a published claim") {
		t.Fatalf("summary:\n%s", sum)
	}
}

func copyFixture(t *testing.T, edit func(*Manifest)) string {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir("testdata/transcripts")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("testdata/transcripts", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var m Manifest
	if err := decodeStrict(filepath.Join(dir, "manifest.json"), &m); err != nil {
		t.Fatal(err)
	}
	edit(&m)
	b, _ := marshalJSON(&m)
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIngestRejectsManifestMismatch(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Manifest)
		want string
	}{
		"model":          {func(m *Manifest) { m.Model = "claude-other" }, "manifest model"},
		"client version": {func(m *Manifest) { m.Client.Version = "1.0.0" }, "manifest version"},
		"transcript sha": {func(m *Manifest) { m.Sessions[0].TranscriptSHA256 = strings.Repeat("0", 64) }, "transcript sha256"},
		"unknown arm":    {func(m *Manifest) { m.Sessions[0].Arm = "nope" }, "unknown arm"},
		"short sha":      {func(m *Manifest) { m.CodeGraph.SHA = "abc" }, "codegraph sha"},
		"absolute path":  {func(m *Manifest) { m.Sessions[0].Transcript = "/etc/x.jsonl" }, "relative path"},
		"duplicate":      {func(m *Manifest) { m.Sessions[1] = m.Sessions[2] }, "duplicate"},
	} {
		_, err := Ingest(copyFixture(t, tc.edit), "2026-01-02T00:00:00Z")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
	if _, err := Ingest(fixtureManifest, "not-a-date"); err == nil {
		t.Error("bad date accepted")
	}
}

func TestValidateIngestRejectsTampering(t *testing.T) {
	for name, edit := range map[string]func(*IngestRecord){
		"processed":     func(r *IngestRecord) { r.Sessions[0].ProcessedTokens++ },
		"arm summary":   func(r *IngestRecord) { r.Arms[0].Stats[0].Median = 1 },
		"contamination": func(r *IngestRecord) { r.Sessions[1].Contamination.Contaminated = false },
		"unchecked":     func(r *IngestRecord) { r.Sessions[0].Contamination.Checked = false },
		"schema":        func(r *IngestRecord) { r.Schema = SchemaVersion },
		"limitations":   func(r *IngestRecord) { r.Limitations = nil },
		"dropped":       func(r *IngestRecord) { r.Sessions = r.Sessions[:2] },
		"blocked count": func(r *IngestRecord) { r.Sessions[0].Contamination.BlockedAttempts = 0 },
		"swapped sha":   func(r *IngestRecord) { r.Sessions[0].TranscriptSHA256 = r.Sessions[1].TranscriptSHA256 },
		"duplicated":    func(r *IngestRecord) { r.Sessions[2] = r.Sessions[1] },
		"call outcome": func(r *IngestRecord) {
			r.Sessions[1].Contamination.Calls[0].Outcome = "blocked"
			r.Sessions[1].Contamination.Calls[1].Outcome = "blocked"
		},
	} {
		rec, err := Ingest(fixtureManifest, "2026-01-02T00:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
		edit(rec)
		if ValidateIngest(rec) == nil {
			t.Errorf("%s: tampered record accepted", name)
		}
	}
}

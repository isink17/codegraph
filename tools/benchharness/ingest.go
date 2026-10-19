package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/isink17/codegraph/internal/latency"
)

const (
	ManifestSchema    = "codegraph.agent_run_manifest/v1"
	IngestSchema      = "codegraph.agent_transcript_ingest/v3"
	IngestMethodology = "agent-transcript-ab/v3"
	// v2 records (no subagent files or calibration) and v1 records (no
	// residual attribution or evaluation) still validate.
	IngestSchemaV2      = "codegraph.agent_transcript_ingest/v2"
	IngestMethodologyV2 = "agent-transcript-ab/v2"
	IngestSchemaV1      = "codegraph.agent_transcript_ingest/v1"
	IngestMethodologyV1 = "agent-transcript-ab/v1"
)

// Manifest is written by the operator for one A/B campaign. Hashes and
// isolation flags are recorded claims; ingest verifies only transcript hashes
// and the model/client version seen in each transcript.
type Manifest struct {
	Schema        string            `json:"schema"`
	Client        Client            `json:"client"`
	Model         string            `json:"model"`
	Effort        string            `json:"effort"`
	CodeGraph     ManifestCodeGraph `json:"codegraph"`
	Corpus        Corpus            `json:"corpus"`
	TaskSetSHA256 string            `json:"task_set_sha256"`
	TaskSet       string            `json:"task_set,omitempty"`
	Arms          []Arm             `json:"arms"`
	Sessions      []SessionRef      `json:"sessions"`
	Uncertainties []string          `json:"uncertainties"`
}

type Client struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ManifestCodeGraph struct {
	SHA          string `json:"sha"`
	BinarySHA256 string `json:"binary_sha256"`
}

type Corpus struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
}

type Arm struct {
	Name             string    `json:"name"`
	CodeGraphEnabled bool      `json:"codegraph_enabled"`
	ToolMode         string    `json:"tool_mode"`
	MCPConfigSHA256  string    `json:"mcp_config_sha256"`
	SettingsSHA256   string    `json:"settings_sha256"`
	Isolation        Isolation `json:"isolation"`
}

type Isolation struct {
	StrictMCPConfig          bool `json:"strict_mcp_config"`
	PathSanitized            bool `json:"path_sanitized"`
	CLIBlockHook             bool `json:"cli_block_hook"`
	UserInstructionsIsolated bool `json:"user_instructions_isolated"`
	UserSkillsIsolated       bool `json:"user_skills_isolated"`
}

type SessionRef struct {
	Arm              string `json:"arm"`
	Task             string `json:"task"`
	Rep              int    `json:"rep"`
	Transcript       string `json:"transcript"`
	TranscriptSHA256 string `json:"transcript_sha256"`
	// Subagents, when present (even empty), must list exactly the
	// agent-*.jsonl files next to the transcript.
	Subagents *[]SubagentRef `json:"subagents,omitempty"`
}

var (
	sha40RE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha64RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validateManifest(m *Manifest) error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if m.Schema != ManifestSchema {
		bad("manifest schema %q, want %q", m.Schema, ManifestSchema)
	}
	if m.Client.Name == "" || m.Client.Version == "" || m.Model == "" || m.Effort == "" {
		bad("manifest client name/version, model and effort are required")
	}
	if !sha40RE.MatchString(m.CodeGraph.SHA) || !sha64RE.MatchString(m.CodeGraph.BinarySHA256) {
		bad("manifest codegraph sha (40 hex) and binary_sha256 (64 hex) are required")
	}
	if m.Corpus.Repo == "" || !sha40RE.MatchString(m.Corpus.Commit) {
		bad("manifest corpus repo and commit (40 hex) are required")
	}
	if !sha64RE.MatchString(m.TaskSetSHA256) {
		bad("manifest task_set_sha256 must be 64 hex")
	}
	if filepath.IsAbs(m.TaskSet) {
		bad("manifest task_set must be a relative path")
	}
	arms := map[string]bool{}
	for _, a := range m.Arms {
		if a.Name == "" || arms[a.Name] {
			bad("arm name %q empty or duplicated", a.Name)
		}
		arms[a.Name] = true
		if a.ToolMode == "" || !sha64RE.MatchString(a.MCPConfigSHA256) || !sha64RE.MatchString(a.SettingsSHA256) {
			bad("arm %q: tool_mode, mcp_config_sha256 and settings_sha256 are required", a.Name)
		}
	}
	if len(m.Arms) == 0 || len(m.Sessions) == 0 {
		bad("manifest needs at least one arm and one session")
	}
	keys := map[string]bool{}
	for _, s := range m.Sessions {
		k := fmt.Sprintf("%s/%s/%d", s.Arm, s.Task, s.Rep)
		if !arms[s.Arm] || s.Task == "" || s.Rep < 1 || keys[k] {
			bad("session %s: unknown arm, empty task, rep < 1 or duplicate", k)
		}
		keys[k] = true
		if s.Transcript == "" || filepath.IsAbs(s.Transcript) || !sha64RE.MatchString(s.TranscriptSHA256) {
			bad("session %s: transcript must be a relative path with a 64-hex sha256", k)
		}
		if s.Subagents != nil {
			paths := map[string]bool{}
			for _, a := range *s.Subagents {
				if paths[a.Transcript] || !strings.HasPrefix(a.Transcript, subagentDir(s.Transcript)+"/") || !sha64RE.MatchString(a.TranscriptSHA256) {
					bad("session %s: subagent %q must be a unique path under %s/ with a 64-hex sha256", k, a.Transcript, subagentDir(s.Transcript))
				}
				paths[a.Transcript] = true
			}
		}
	}
	return errors.Join(errs...)
}

// IngestRecord is the deterministic output of ingest. Arms are recomputable
// from Sessions; validate checks that they are.
type IngestRecord struct {
	Schema         string   `json:"schema"`
	Methodology    string   `json:"methodology"`
	DateUTC        string   `json:"date_utc"`
	ManifestSHA256 string   `json:"manifest_sha256"`
	Manifest       Manifest `json:"manifest"`
	// TaskSetJSON is the task-set file exactly as ingest read and hashed it.
	TaskSetJSON string `json:"task_set_json,omitempty"`
	// Calibration is the residual calibration artifact ingest read, with the
	// sha256 of its bytes.
	CalibrationSHA256 string          `json:"calibration_sha256,omitempty"`
	Calibration       *Calibration    `json:"calibration,omitempty"`
	Sessions          []SessionResult `json:"sessions"`
	Arms              []ArmSummary    `json:"arms"`
	Limitations       []string        `json:"limitations"`
}

type SessionResult struct {
	Arm              string `json:"arm"`
	Task             string `json:"task"`
	Rep              int    `json:"rep"`
	TranscriptSHA256 string `json:"transcript_sha256"`
	SessionMetrics
	Contamination Contamination    `json:"contamination"`
	Residual      *Residual        `json:"residual,omitempty"`
	Evaluation    *Evaluation      `json:"evaluation,omitempty"`
	Subagents     []SubagentResult `json:"subagents,omitempty"`
	Workload      *Workload        `json:"workload,omitempty"`
}

type ArmSummary struct {
	Arm             string          `json:"arm"`
	Sessions        int             `json:"sessions"`
	Contaminated    int             `json:"contaminated"`
	BlockedOnly     int             `json:"blocked_attempts_only"`
	Included        int             `json:"included"`
	IncludesTainted bool            `json:"includes_contaminated"`
	Stats           []Stat          `json:"stats"`
	Correctness     *ArmCorrectness `json:"correctness,omitempty"`
}

// Stat is a nearest-rank median (always an observed session value), min and
// max over the included sessions of one arm.
type Stat struct {
	Name   string `json:"name"`
	Unit   string `json:"unit"`
	N      int    `json:"n"`
	Median int    `json:"median"`
	Min    int    `json:"min"`
	Max    int    `json:"max"`
}

const (
	limitNoEvaluation = "Answer quality is not measured; token or time differences are not improvements on their own."
	limitEvaluation   = "Answer correctness is rubric matching of the final main-session answer against the task set's required and forbidden substrings, regexes and numbers; it is not a semantic judgment, and token or time differences are not improvements on their own."
	limitResidual     = "Residual attribution: the first request's context and every total are measured provider tokens; per-category splits of later context growth are estimated by character share of the visible content written between requests (hidden or redacted content, attachments and tokenizer density differences move tokens between categories); context after a compaction or a context drop is unattributed."
	limitCalibrated   = "Residual attribution: the first request's context and every total are measured provider tokens; per-category splits of later context growth are estimated by pricing the visible content written between requests at chars-per-token ratios fitted on recorded provider usage (see calibration), with growth beyond the priced content unattributed; the fit spreads over gaps that mix content types and hidden content, so the bounds are the interquartile range of observed ratios, not a confidence interval; context after a compaction or a context drop is unattributed."
)

var baseLimitations = []string{
	"Subagent transcripts are read from the session's subagents/ directory (agent-*.jsonl) and linked to the Task/Agent call whose result names their agent id; agents spawned without such a result (teammates, resumed agents) are listed as unlinked, and in arms without CodeGraph a Task/Agent call without a linked subagent file is unverified and contaminates the session. Main-session totals and context exclude subagents; processed tokens are also reported per subagent and as a session total, context sizes per agent only.",
	"output_tokens is the largest streamed snapshot per message.id and may undercount.",
	"Contamination detection is pattern-based (MCP server name, Skill input, shell command word); aliases, functions, eval, scripts and variables used as commands are not detected.",
	"Config hashes and isolation flags are recorded by the operator in the manifest and are not verified by ingest.",
	"Records are measurements of the listed sessions only, not published claims.",
}

// Ingest parses every transcript named by the manifest at manifestPath.
// calibrationPath may be empty.
func Ingest(manifestPath, date, calibrationPath string) (*IngestRecord, error) {
	if _, err := time.Parse(time.RFC3339, date); err != nil {
		return nil, fmt.Errorf("--date: %w", err)
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := decodeStrictBytes(manifestPath, raw, &m); err != nil {
		return nil, err
	}
	if err := validateManifest(&m); err != nil {
		return nil, err
	}
	arms := map[string]Arm{}
	for _, a := range m.Arms {
		arms[a.Name] = a
	}
	rec := &IngestRecord{Schema: IngestSchema, Methodology: IngestMethodology, DateUTC: date, ManifestSHA256: sha256Hex(raw), Manifest: m}
	dir := filepath.Dir(manifestPath)
	tasks, raw, err := loadTaskSet(dir, &m)
	if err != nil {
		return nil, err
	}
	rec.TaskSetJSON = string(raw)
	if calibrationPath != "" {
		if rec.Calibration, rec.CalibrationSHA256, err = loadCalibration(calibrationPath); err != nil {
			return nil, err
		}
	}
	var errs []error
	for _, ref := range m.Sessions {
		s, err := ingestSession(dir, ref, &m, arms[ref.Arm], tasks, rec.Calibration)
		if err != nil {
			errs = append(errs, fmt.Errorf("session %s/%s/%d: %w", ref.Arm, ref.Task, ref.Rep, err))
			continue
		}
		rec.Sessions = append(rec.Sessions, s)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	sort.Slice(rec.Sessions, func(i, j int) bool {
		a, b := rec.Sessions[i], rec.Sessions[j]
		if a.Arm != b.Arm {
			return a.Arm < b.Arm
		}
		if a.Task != b.Task {
			return a.Task < b.Task
		}
		return a.Rep < b.Rep
	})
	rec.Arms = armSummaries(rec.Sessions, false)
	rec.Limitations = limitations(&m, tasks, rec.Calibration)
	return rec, nil
}

// loadTaskSet reads the manifest's task set, checks it against
// task_set_sha256 and requires a task for every session. It returns nil when
// the manifest names no task set.
func loadTaskSet(dir string, m *Manifest) (*TaskSet, []byte, error) {
	if m.TaskSet == "" {
		return nil, nil, nil
	}
	path := filepath.Join(dir, filepath.FromSlash(m.TaskSet))
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	ts, err := parseTaskSet(path, raw, m)
	return ts, raw, err
}

// parseTaskSet checks raw against the manifest's task_set_sha256, decodes and
// validates it, and requires a task for every session.
func parseTaskSet(path string, raw []byte, m *Manifest) (*TaskSet, error) {
	if got := sha256Hex(raw); got != m.TaskSetSHA256 {
		return nil, fmt.Errorf("task set sha256 %s, manifest says %s", got, m.TaskSetSHA256)
	}
	// The bytes are stored as a JSON string, which cannot carry invalid UTF-8.
	if !utf8.Valid(raw) {
		return nil, errors.New("task set is not valid UTF-8")
	}
	var ts TaskSet
	if err := decodeStrictBytes(path, raw, &ts); err != nil {
		return nil, err
	}
	if err := validateTaskSet(&ts); err != nil {
		return nil, err
	}
	if err := checkSessionTasks(m, &ts); err != nil {
		return nil, err
	}
	return &ts, nil
}

func checkSessionTasks(m *Manifest, ts *TaskSet) error {
	var errs []error
	for _, s := range m.Sessions {
		if findTask(ts, s.Task) == nil {
			errs = append(errs, fmt.Errorf("session %s/%s/%d: task %q is not in the task set", s.Arm, s.Task, s.Rep, s.Task))
		}
	}
	return errors.Join(errs...)
}

func findTask(ts *TaskSet, id string) *Task {
	for i := range ts.Tasks {
		if ts.Tasks[i].ID == id {
			return &ts.Tasks[i]
		}
	}
	return nil
}

func ingestSession(dir string, ref SessionRef, m *Manifest, arm Arm, ts *TaskSet, cal *Calibration) (SessionResult, error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref.Transcript)))
	if err != nil {
		return SessionResult{}, err
	}
	if got := sha256Hex(b); got != ref.TranscriptSHA256 {
		return SessionResult{}, fmt.Errorf("transcript sha256 %s, manifest says %s", got, ref.TranscriptSHA256)
	}
	t, err := ParseTranscript(bytes.NewReader(b))
	if err != nil {
		return SessionResult{}, err
	}
	subs, checked, unlinked, err := sessionAgents(dir, ref, m, t)
	if err != nil {
		return SessionResult{}, err
	}
	sm := t.Metrics()
	if len(sm.Models) != 1 || sm.Models[0] != m.Model {
		return SessionResult{}, fmt.Errorf("transcript models %v, manifest model %q", sm.Models, m.Model)
	}
	if len(sm.ClientVersions) != 1 || sm.ClientVersions[0] != m.Client.Version {
		return SessionResult{}, fmt.Errorf("transcript client versions %v, manifest version %q", sm.ClientVersions, m.Client.Version)
	}
	c := Contamination{Calls: []ContaminationCall{}}
	if !arm.CodeGraphEnabled {
		c = Contamination{Checked: true, Calls: t.contaminationCalls("main", checked)}
		for _, s := range subs {
			c.Calls = append(c.Calls, s.t.contaminationCalls(s.res.AgentID, checked)...)
		}
		c.Contaminated, c.BlockedAttempts = contaminationTotals(c.Calls)
	}
	res := t.Residual(cal)
	out := SessionResult{Arm: ref.Arm, Task: ref.Task, Rep: ref.Rep, TranscriptSHA256: ref.TranscriptSHA256, SessionMetrics: sm, Contamination: c, Residual: &res}
	for _, s := range subs {
		out.Subagents = append(out.Subagents, s.res)
	}
	out.Workload = workload(&out, unlinked)
	if ts != nil {
		answer, reason := t.FinalAnswer()
		e := evaluate(*findTask(ts, ref.Task), answer, reason)
		out.Evaluation = &e
	}
	return out, nil
}

func limitations(m *Manifest, ts *TaskSet, cal *Calibration) []string {
	out := []string{limitNoEvaluation}
	if ts != nil {
		out[0] = limitEvaluation
	}
	switch {
	case cal != nil && cal.Sufficient:
		out = append(out, limitCalibrated)
	case cal != nil:
		out = append(out, limitResidual, "Residual calibration has too few samples; attribution is uncalibrated.")
	default:
		out = append(out, limitResidual, "Residual attribution is uncalibrated (no calibration given).")
	}
	out = append(out, baseLimitations...)
	for _, a := range m.Arms {
		iso := a.Isolation
		for _, f := range []struct {
			ok   bool
			what string
		}{
			{iso.StrictMCPConfig, "MCP configuration not strict"},
			{iso.UserInstructionsIsolated, "user-level instructions (CLAUDE.md) not isolated"},
			{iso.UserSkillsIsolated, "user-level skills not isolated"},
			{iso.PathSanitized || a.CodeGraphEnabled, "codegraph binary not removed from PATH"},
			{iso.CLIBlockHook || a.CodeGraphEnabled, "no hook blocking the codegraph CLI"},
		} {
			if !f.ok {
				out = append(out, fmt.Sprintf("arm %s: %s.", a.Name, f.what))
			}
		}
	}
	if ts != nil {
		run := map[string]bool{}
		for _, s := range m.Sessions {
			run[s.Task] = true
		}
		for _, t := range ts.Tasks {
			if !run[t.ID] {
				out = append(out, fmt.Sprintf("task set: task %s has no session.", t.ID))
			}
		}
	}
	for _, u := range m.Uncertainties {
		out = append(out, "manifest: "+u)
	}
	return out
}

var statDefs = []struct {
	name, unit string
	get        func(SessionResult) *int
}{
	{"processed_tokens", "provider_tokens", func(s SessionResult) *int { return &s.ProcessedTokens }},
	{"final_context_tokens", "provider_tokens", func(s SessionResult) *int { return &s.FinalContext }},
	{"peak_context_tokens", "provider_tokens", func(s SessionResult) *int { return &s.PeakContext }},
	{"output_tokens", "provider_tokens", func(s SessionResult) *int { return &s.OutputTokens }},
	{"requests", "count", func(s SessionResult) *int { return &s.Requests }},
	{"tool_calls_total", "count", func(s SessionResult) *int { return &s.ToolCallsTotal }},
	{"compactions", "count", func(s SessionResult) *int { return &s.Compactions }},
	{"wall_clock_ms", "milliseconds", func(s SessionResult) *int { return s.WallClockMS }},
	{"subagent_processed_tokens", "provider_tokens", func(s SessionResult) *int {
		if s.Workload == nil {
			return nil
		}
		return &s.Workload.SubagentProcessed
	}},
	{"total_processed_tokens", "provider_tokens", func(s SessionResult) *int {
		if s.Workload == nil {
			return nil
		}
		return &s.Workload.TotalProcessed
	}},
}

func armSummaries(sessions []SessionResult, includeContaminated bool) []ArmSummary {
	byArm := map[string][]SessionResult{}
	for _, s := range sessions {
		byArm[s.Arm] = append(byArm[s.Arm], s)
	}
	names := make([]string, 0, len(byArm))
	for n := range byArm {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []ArmSummary{}
	for _, n := range names {
		a := ArmSummary{Arm: n, IncludesTainted: includeContaminated, Stats: []Stat{}}
		var included []SessionResult
		for _, s := range byArm[n] {
			a.Sessions++
			switch {
			case s.Contamination.Contaminated:
				a.Contaminated++
			case s.Contamination.BlockedAttempts > 0:
				a.BlockedOnly++
			}
			if includeContaminated || !s.Contamination.Contaminated {
				included = append(included, s)
			}
		}
		a.Included = len(included)
		for _, d := range statDefs {
			var xs []int
			for _, s := range included {
				if v := d.get(s); v != nil {
					xs = append(xs, *v)
				}
			}
			if len(xs) > 0 {
				a.Stats = append(a.Stats, intStat(d.name, d.unit, xs))
			}
		}
		for i, c := range residualCategories {
			var xs []int
			for _, s := range included {
				if s.Residual != nil {
					xs = append(xs, s.Residual.Categories[i].Tokens)
				}
			}
			if len(xs) > 0 {
				a.Stats = append(a.Stats, intStat(residualStatName(c.name), residualUnit(c.basis), xs))
			}
		}
		a.Correctness = armCorrectness(included)
		out = append(out, a)
	}
	return out
}

func intStat(name, unit string, xs []int) Stat {
	// latency.Samples supplies the shared nearest-rank arithmetic; the values
	// are plain integers, not durations.
	var ds latency.Samples
	for _, x := range xs {
		ds = append(ds, time.Duration(x))
	}
	sorted := ds.Sorted()
	return Stat{Name: name, Unit: unit, N: len(xs), Median: int(sorted.SortedPercentile(50)), Min: int(sorted[0]), Max: int(sorted[len(sorted)-1])}
}

// ValidateIngest checks an ingest record's identity, per-session arithmetic
// and that the stored arm summaries equal the ones recomputed from sessions.
func ValidateIngest(r *IngestRecord) error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	v1, v3 := r.Schema == IngestSchemaV1, r.Schema == IngestSchema
	if !(v3 && r.Methodology == IngestMethodology) && !(r.Schema == IngestSchemaV2 && r.Methodology == IngestMethodologyV2) && !(v1 && r.Methodology == IngestMethodologyV1) {
		bad("schema/methodology %q/%q, want %q/%q, %q/%q or %q/%q", r.Schema, r.Methodology, IngestSchema, IngestMethodology,
			IngestSchemaV2, IngestMethodologyV2, IngestSchemaV1, IngestMethodologyV1)
	}
	switch {
	case !v3 && (r.Calibration != nil || r.CalibrationSHA256 != ""):
		bad("%s records carry no calibration", r.Schema)
	case (r.Calibration != nil) != (r.CalibrationSHA256 != ""):
		bad("calibration and calibration_sha256 must be present together")
	case r.Calibration != nil:
		if !sha64RE.MatchString(r.CalibrationSHA256) {
			bad("calibration_sha256 must be 64 hex")
		}
		if err := validateCalibration(r.Calibration); err != nil {
			bad("%v", err)
		}
		if b, err := marshalJSON(r.Calibration); err != nil || sha256Hex(b) != r.CalibrationSHA256 {
			bad("calibration does not match calibration_sha256")
		}
	}
	calibrated := r.Calibration != nil && r.Calibration.Sufficient
	refs := map[string]*SessionRef{}
	for i, ref := range r.Manifest.Sessions {
		refs[fmt.Sprintf("%s/%s/%d", ref.Arm, ref.Task, ref.Rep)] = &r.Manifest.Sessions[i]
		if !v3 && ref.Subagents != nil {
			bad("%s manifests declare no subagents", r.Schema)
		}
	}
	var tasks *TaskSet
	switch {
	case v1 && (r.TaskSetJSON != "" || r.Manifest.TaskSet != ""):
		bad("%s records carry no task set", IngestSchemaV1)
	case !v1 && (r.TaskSetJSON != "") != (r.Manifest.TaskSet != ""):
		bad("task_set_json must be present exactly when the manifest names a task set")
	case r.TaskSetJSON != "":
		ts, err := parseTaskSet("task_set_json", []byte(r.TaskSetJSON), &r.Manifest)
		if err != nil {
			bad("%v", err)
		}
		tasks = ts
	}
	if _, err := time.Parse(time.RFC3339, r.DateUTC); err != nil {
		bad("date_utc: %v", err)
	}
	if !sha64RE.MatchString(r.ManifestSHA256) {
		bad("manifest_sha256 must be 64 hex")
	}
	if err := validateManifest(&r.Manifest); err != nil {
		bad("%v", err)
	}
	if len(r.Sessions) != len(r.Manifest.Sessions) {
		bad("%d sessions, manifest lists %d", len(r.Sessions), len(r.Manifest.Sessions))
	}
	listed := map[string]bool{}
	for _, ref := range r.Manifest.Sessions {
		listed[fmt.Sprintf("%s/%s/%d/%s", ref.Arm, ref.Task, ref.Rep, ref.TranscriptSHA256)] = true
	}
	enabled := map[string]bool{}
	for _, a := range r.Manifest.Arms {
		enabled[a.Name] = a.CodeGraphEnabled
	}
	for _, s := range r.Sessions {
		id := fmt.Sprintf("%s/%s/%d", s.Arm, s.Task, s.Rep)
		if s.ProcessedTokens < s.FinalContext || s.PeakContext < s.FinalContext || s.Requests < 1 {
			bad("session %s: inconsistent token counts", id)
		}
		if s.InputTokens+s.CacheReadTokens+s.CacheCreation != s.ProcessedTokens {
			bad("session %s: processed_tokens != input + cache_read + cache_creation", id)
		}
		if !listed[id+"/"+s.TranscriptSHA256] {
			bad("session %s: no manifest entry with transcript sha256 %s", id, s.TranscriptSHA256)
		}
		delete(listed, id+"/"+s.TranscriptSHA256)
		if s.Contamination.Checked == enabled[s.Arm] {
			bad("session %s: contamination must be checked exactly for arms without CodeGraph", id)
		}
		if c, b := contaminationTotals(s.Contamination.Calls); c != s.Contamination.Contaminated || b != s.Contamination.BlockedAttempts {
			bad("session %s: contamination totals do not match its calls", id)
		}
		switch {
		case v3:
			if ref := refs[id]; ref != nil {
				if err := validateSubagents(&s, ref); err != nil {
					bad("session %s: %v", id, err)
				}
			}
		case s.Subagents != nil || s.Workload != nil:
			bad("session %s: %s records carry no subagents or workload", id, r.Schema)
		default:
			for _, c := range s.Contamination.Calls {
				if c.Agent != "" {
					bad("session %s: %s contamination calls carry no agent", id, r.Schema)
				}
			}
		}
		switch {
		case v1:
			if s.Residual != nil || s.Evaluation != nil {
				bad("session %s: %s records carry no residual or evaluation", id, IngestSchemaV1)
			}
			continue
		case s.Residual == nil:
			bad("session %s: residual attribution missing", id)
		case v3 && (s.Residual.Method == CalibratedMethod) != calibrated:
			bad("session %s: residual method %s, but the record's calibration sufficient=%t", id, s.Residual.Method, calibrated)
		default:
			if err := validateResidual(s.Residual, s.FinalContext, v3, r.Calibration); err != nil {
				bad("session %s: %v", id, err)
			}
		}
		var task *Task
		if tasks != nil {
			task = findTask(tasks, s.Task)
		}
		switch {
		case r.TaskSetJSON != "" && tasks == nil:
		case (task != nil) != (s.Evaluation != nil):
			bad("session %s: evaluation must be present exactly when its task is in the task set", id)
		case task != nil:
			if err := validateEvaluation(s.Evaluation, *task); err != nil {
				bad("session %s: evaluation: %v", id, err)
			}
		}
	}
	if len(r.Limitations) == 0 {
		bad("limitations must be explicit")
	}
	if want := armSummaries(r.Sessions, false); !reflect.DeepEqual(want, r.Arms) {
		bad("arm summaries do not match sessions")
	}
	return errors.Join(errs...)
}

// SummarizeIngest renders an ingest record; contamination comes first because
// it decides which sessions the per-arm numbers cover.
func SummarizeIngest(r *IngestRecord, includeContaminated bool) string {
	var b strings.Builder
	m := r.Manifest
	fmt.Fprintf(&b, "# Agent transcript ingest\n\n- Schema: %s\n- Methodology: %s\n- Date (UTC): %s\n- Manifest sha256: %s\n",
		r.Schema, r.Methodology, r.DateUTC, r.ManifestSHA256)
	fmt.Fprintf(&b, "- Client: %s %s; model %s; effort %s\n- CodeGraph: %s (binary sha256 %s)\n- Corpus: %s @ %s\n- Task set sha256: %s\n",
		m.Client.Name, m.Client.Version, m.Model, m.Effort, m.CodeGraph.SHA, m.CodeGraph.BinarySHA256, m.Corpus.Repo, m.Corpus.Commit, m.TaskSetSHA256)
	if c := r.Calibration; c != nil {
		fmt.Fprintf(&b, "- Residual calibration: sha256 %s, %s, sufficient %t, %d global samples from %d transcripts\n",
			r.CalibrationSHA256, c.Method, c.Sufficient, c.Global.Samples, len(c.Sources))
	}
	b.WriteString("\nMeasurements of the listed sessions only; not a published claim.\n")

	arms := armSummaries(r.Sessions, includeContaminated)
	b.WriteString("\n## Contamination\n\n| Arm | CodeGraph enabled | Sessions | Contaminated | Blocked attempts only | Included |\n|---|---|---|---|---|---|\n")
	enabled := map[string]bool{}
	for _, a := range m.Arms {
		enabled[a.Name] = a.CodeGraphEnabled
	}
	for _, a := range arms {
		fmt.Fprintf(&b, "| %s | %t | %d | %d | %d | %d |\n", a.Arm, enabled[a.Arm], a.Sessions, a.Contaminated, a.BlockedOnly, a.Included)
	}
	if includeContaminated {
		b.WriteString("\nContaminated sessions are INCLUDED in the per-arm statistics below.\n")
	} else {
		b.WriteString("\nContaminated sessions are excluded from the per-arm statistics below.\n")
	}
	for _, s := range r.Sessions {
		for _, c := range s.Contamination.Calls {
			agent := ""
			if c.Agent != "" {
				agent = " [" + c.Agent + "]"
			}
			fmt.Fprintf(&b, "- %s/%s/%d%s: %s %s %s %q\n", s.Arm, s.Task, s.Rep, agent, c.Outcome, c.Kind, c.Tool, c.Detail)
		}
	}

	writeCorrectness(&b, r, arms)

	statTable := func(title string, residual bool) {
		fmt.Fprintf(&b, "\n## %s (nearest-rank median, min, max)\n\n| Arm | Metric | Unit | N | Median | Min | Max |\n|---|---|---|---|---|---|---|\n", title)
		for _, a := range arms {
			for _, st := range a.Stats {
				if isResidualStat(st.Name) == residual {
					fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d |\n", a.Arm, st.Name, st.Unit, st.N, st.Median, st.Min, st.Max)
				}
			}
		}
	}
	statTable("Per arm", false)
	if r.Schema != IngestSchemaV1 {
		statTable("Residual context per arm", true)
		writeResidualSessions(&b, r.Sessions)
	}
	if r.Schema == IngestSchema {
		writeAgents(&b, r)
	}

	b.WriteString("\n## Sessions\n\n| Arm | Task | Rep | Processed | Final ctx | Peak ctx | Output | Requests | Compactions | Tool calls | Wall ms | Sidechain processed | Contaminated |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range r.Sessions {
		wall := "n/a"
		if s.WallClockMS != nil {
			wall = strconv.Itoa(*s.WallClockMS)
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %d | %d | %d | %d | %s | %d | %t |\n", s.Arm, s.Task, s.Rep,
			s.ProcessedTokens, s.FinalContext, s.PeakContext, s.OutputTokens, s.Requests, s.Compactions, s.ToolCallsTotal, wall,
			s.Sidechain.ProcessedTokens, s.Contamination.Contaminated)
	}

	b.WriteString("\n## Arms\n\n| Arm | Tool mode | MCP config sha256 | Settings sha256 | Strict MCP | PATH sanitized | CLI hook | User instructions isolated | User skills isolated |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, a := range m.Arms {
		i := a.Isolation
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %t | %t | %t | %t | %t |\n", a.Name, a.ToolMode, a.MCPConfigSHA256, a.SettingsSHA256,
			i.StrictMCPConfig, i.PathSanitized, i.CLIBlockHook, i.UserInstructionsIsolated, i.UserSkillsIsolated)
	}
	b.WriteString("\n## Limitations\n\n")
	for _, l := range r.Limitations {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	return b.String()
}

func writeCorrectness(b *strings.Builder, r *IngestRecord, arms []ArmSummary) {
	b.WriteString("\n## Answer correctness (rubric matching)\n\n")
	if r.TaskSetJSON == "" {
		b.WriteString("Not measured: the manifest names no task set.\n")
		return
	}
	b.WriteString("Final main-session answer matched against the task set's required and forbidden facts; not a semantic judgment. Subagent answers are not scored.\n\n" +
		"| Arm | Scored | Unscored | Required matched | Forbidden hits | Sessions with all required and no forbidden |\n|---|---|---|---|---|---|\n")
	for _, a := range arms {
		c := a.Correctness
		if c == nil {
			c = &ArmCorrectness{}
		}
		fmt.Fprintf(b, "| %s | %d | %d | %d/%d | %d | %d |\n", a.Arm, c.Scored, c.Unscored, c.RequiredMatched, c.RequiredTotal, c.ForbiddenHits, c.AllRequiredMet)
	}
	b.WriteString("\n")
	for _, s := range r.Sessions {
		e := s.Evaluation
		tag := ""
		if s.Contamination.Contaminated {
			tag = " (contaminated)"
		}
		if e.Status != "scored" {
			fmt.Fprintf(b, "- %s/%s/%d%s: unscored (%s)\n", s.Arm, s.Task, s.Rep, tag, e.Reason)
			continue
		}
		var missed, hit []string
		for _, f := range e.Required {
			if !f.Matched {
				missed = append(missed, f.ID)
			}
		}
		for _, f := range e.Forbidden {
			if f.Matched {
				hit = append(hit, f.ID)
			}
		}
		fmt.Fprintf(b, "- %s/%s/%d%s: %d/%d required; missed [%s]; forbidden hit [%s]\n", s.Arm, s.Task, s.Rep, tag,
			e.RequiredMatched, e.RequiredTotal, strings.Join(missed, " "), strings.Join(hit, " "))
	}
}

func writeResidualSessions(b *strings.Builder, sessions []SessionResult) {
	b.WriteString("\n## Residual context per session (provider tokens; basis per column)\n\n| Arm | Task | Rep | Final ctx (measured) | Resets |")
	for _, c := range residualCategories {
		fmt.Fprintf(b, " %s (%s) |", c.name, c.basis)
	}
	b.WriteString("\n|---|---|---|---|---|" + strings.Repeat("---|", len(residualCategories)) + "\n")
	for _, s := range sessions {
		fmt.Fprintf(b, "| %s | %s | %d | %d | %d |", s.Arm, s.Task, s.Rep, s.Residual.FinalContext, s.Residual.Resets)
		for _, c := range s.Residual.Categories {
			fmt.Fprintf(b, " %d |", c.Tokens)
		}
		b.WriteString("\n")
	}
}

func writeAgents(b *strings.Builder, r *IngestRecord) {
	b.WriteString("\n## Workload per session (provider tokens)\n\nProcessed tokens summed across agents; context sizes are per agent and never summed.\n\n" +
		"| Arm | Task | Rep | Main processed | Subagent processed | In-file sidechain processed | Total processed | Linked subagents | Unlinked subagents | Unlinked Task/Agent calls |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range r.Sessions {
		w := s.Workload
		fmt.Fprintf(b, "| %s | %s | %d | %d | %d | %d | %d | %d | %d | %d |\n", s.Arm, s.Task, s.Rep, w.MainProcessed, w.SubagentProcessed,
			w.SidechainProcessed, w.TotalProcessed, w.LinkedSubagents, w.UnlinkedSubagents, w.UnlinkedAgentCalls)
	}
	b.WriteString("\n## Subagents\n\n| Arm | Task | Rep | Agent | Parent | Depth | Requests | Processed | Final ctx | Peak ctx | Tool calls | Duplicate requests | Transcript sha256 |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range r.Sessions {
		for _, a := range s.Subagents {
			parent := a.Parent
			if parent == "" {
				parent = "unlinked"
			}
			fmt.Fprintf(b, "| %s | %s | %d | %s | %s | %d | %d | %d | %d | %d | %d | %d | %s |\n", s.Arm, s.Task, s.Rep, a.AgentID, parent, a.Depth,
				a.Requests, a.ProcessedTokens, a.FinalContext, a.PeakContext, a.ToolCallsTotal, a.DuplicateRequests, a.TranscriptSHA256)
		}
	}
	if r.Calibration == nil || !r.Calibration.Sufficient {
		return
	}
	b.WriteString("\n## Calibrated residual estimates (estimated provider tokens)\n\nReconciled tokens sum to the measured final context; the estimate prices each category's characters at the median ratio and the range at the interquartile ratios, uncapped.\n\n" +
		"| Arm | Task | Rep | Category | Reconciled | Estimate | Low | High | Ratio source |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range r.Sessions {
		for _, c := range s.Residual.Categories {
			if e := c.Calibrated; e != nil && c.Chars > 0 {
				fmt.Fprintf(b, "| %s | %s | %d | %s | %d | %d | %d | %d | %s |\n", s.Arm, s.Task, s.Rep, c.Category, c.Tokens, e.Estimate, e.Low, e.High, e.RatioSource)
			}
		}
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

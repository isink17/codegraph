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

	"github.com/isink17/codegraph/internal/latency"
)

const (
	ManifestSchema    = "codegraph.agent_run_manifest/v1"
	IngestSchema      = "codegraph.agent_transcript_ingest/v1"
	IngestMethodology = "agent-transcript-ab/v1"
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
	}
	return errors.Join(errs...)
}

// IngestRecord is the deterministic output of ingest. Arms are recomputable
// from Sessions; validate checks that they are.
type IngestRecord struct {
	Schema         string          `json:"schema"`
	Methodology    string          `json:"methodology"`
	DateUTC        string          `json:"date_utc"`
	ManifestSHA256 string          `json:"manifest_sha256"`
	Manifest       Manifest        `json:"manifest"`
	Sessions       []SessionResult `json:"sessions"`
	Arms           []ArmSummary    `json:"arms"`
	Limitations    []string        `json:"limitations"`
}

type SessionResult struct {
	Arm              string `json:"arm"`
	Task             string `json:"task"`
	Rep              int    `json:"rep"`
	TranscriptSHA256 string `json:"transcript_sha256"`
	SessionMetrics
	Contamination Contamination `json:"contamination"`
}

type ArmSummary struct {
	Arm             string `json:"arm"`
	Sessions        int    `json:"sessions"`
	Contaminated    int    `json:"contaminated"`
	BlockedOnly     int    `json:"blocked_attempts_only"`
	Included        int    `json:"included"`
	IncludesTainted bool   `json:"includes_contaminated"`
	Stats           []Stat `json:"stats"`
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

var baseLimitations = []string{
	"Answer quality is not measured; token or time differences are not improvements on their own.",
	"Subagent transcripts stored in separate files are not ingested; sidechain events inside a transcript are reported separately and excluded from main-session totals; in arms without CodeGraph every Task/Agent call is unverified and contaminates the session.",
	"output_tokens is the largest streamed snapshot per message.id and may undercount.",
	"Contamination detection is pattern-based (MCP server name, Skill input, shell command word); aliases, functions, eval, scripts and variables used as commands are not detected.",
	"Config hashes and isolation flags are recorded by the operator in the manifest and are not verified by ingest.",
	"Records are measurements of the listed sessions only, not published claims.",
}

// Ingest parses every transcript named by the manifest at manifestPath.
func Ingest(manifestPath, date string) (*IngestRecord, error) {
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
	var errs []error
	for _, ref := range m.Sessions {
		s, err := ingestSession(dir, ref, &m, arms[ref.Arm])
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
	rec.Limitations = limitations(&m)
	return rec, nil
}

func ingestSession(dir string, ref SessionRef, m *Manifest, arm Arm) (SessionResult, error) {
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
	sm := t.Metrics()
	if len(sm.Models) != 1 || sm.Models[0] != m.Model {
		return SessionResult{}, fmt.Errorf("transcript models %v, manifest model %q", sm.Models, m.Model)
	}
	if len(sm.ClientVersions) != 1 || sm.ClientVersions[0] != m.Client.Version {
		return SessionResult{}, fmt.Errorf("transcript client versions %v, manifest version %q", sm.ClientVersions, m.Client.Version)
	}
	c := Contamination{Calls: []ContaminationCall{}}
	if !arm.CodeGraphEnabled {
		c = t.Contamination()
	}
	return SessionResult{Arm: ref.Arm, Task: ref.Task, Rep: ref.Rep, TranscriptSHA256: ref.TranscriptSHA256, SessionMetrics: sm, Contamination: c}, nil
}

func limitations(m *Manifest) []string {
	out := append([]string{}, baseLimitations...)
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
			// latency.Samples supplies the shared nearest-rank arithmetic; the
			// values are plain integers, not durations.
			var xs latency.Samples
			for _, s := range included {
				if v := d.get(s); v != nil {
					xs = append(xs, time.Duration(*v))
				}
			}
			if len(xs) == 0 {
				continue
			}
			sorted := xs.Sorted()
			a.Stats = append(a.Stats, Stat{Name: d.name, Unit: d.unit, N: len(xs),
				Median: int(sorted.SortedPercentile(50)), Min: int(sorted[0]), Max: int(sorted[len(sorted)-1])})
		}
		out = append(out, a)
	}
	return out
}

// ValidateIngest checks an ingest record's identity, per-session arithmetic
// and that the stored arm summaries equal the ones recomputed from sessions.
func ValidateIngest(r *IngestRecord) error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if r.Schema != IngestSchema || r.Methodology != IngestMethodology {
		bad("schema/methodology %q/%q, want %q/%q", r.Schema, r.Methodology, IngestSchema, IngestMethodology)
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
			fmt.Fprintf(&b, "- %s/%s/%d: %s %s %s %q\n", s.Arm, s.Task, s.Rep, c.Outcome, c.Kind, c.Tool, c.Detail)
		}
	}

	b.WriteString("\n## Per arm (nearest-rank median, min, max)\n\n| Arm | Metric | Unit | N | Median | Min | Max |\n|---|---|---|---|---|---|---|\n")
	for _, a := range arms {
		for _, st := range a.Stats {
			fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d |\n", a.Arm, st.Name, st.Unit, st.N, st.Median, st.Min, st.Max)
		}
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

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

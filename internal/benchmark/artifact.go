package benchmark

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	ArtifactSchema   = "codegraph.benchmark_artifact/v1"
	ComparisonSchema = "codegraph.benchmark_comparison/v1"
	MaxArtifactBytes = 16 << 20
)

// Artifact preserves raw samples and the identity needed to decide whether
// another artifact measures the same workload.
type Artifact struct {
	Schema     string                `json:"schema"`
	Identity   RunIdentity           `json:"identity"`
	PublicMode bool                  `json:"public_mode"`
	Scenarios  []ScenarioMeasurement `json:"scenarios"`
	Output     OutputAccounting      `json:"output_accounting"`
	Quality    *QualityResult        `json:"quality,omitempty"`
}

type ScenarioMeasurement struct {
	Name         string        `json:"name"`
	RawSamplesNS []int64       `json:"raw_samples_ns"`
	WarmupCount  int           `json:"warmup_count"`
	Denominators []Denominator `json:"denominators"`
}

// A denominator with no positive value must explain why its metric is absent.
type Denominator struct {
	Name          string `json:"name"`
	Unit          string `json:"unit"`
	Value         *int64 `json:"value,omitempty"`
	MissingReason string `json:"missing_reason,omitempty"`
}

type OutputAccounting struct {
	SerializedBytes int64  `json:"serialized_bytes"`
	EstimatedTokens int64  `json:"estimated_tokens"`
	ProviderTokens  *int64 `json:"provider_tokens,omitempty"`
}

type QualityResult struct {
	TaskSetSHA string `json:"task_set_sha256"`
	RubricSHA  string `json:"rubric_sha256"`
	Correct    int    `json:"correct"`
	Partial    int    `json:"partial"`
	Incorrect  int    `json:"incorrect"`
	Failed     int    `json:"failed"`
}

type ComparisonArtifact struct {
	Schema        string        `json:"schema"`
	BaselineSHA   string        `json:"baseline_artifact_sha256"`
	CandidateSHA  string        `json:"candidate_artifact_sha256"`
	Comparability Comparability `json:"comparability"`
}

func (a Artifact) Validate() error {
	if a.Schema != ArtifactSchema {
		return fmt.Errorf("unsupported benchmark artifact schema %q", a.Schema)
	}
	if err := a.Identity.Validate(); err != nil {
		return err
	}
	if !validGitSHA(a.Identity.CodeGraphSHA) || !isSHA256(a.Identity.FixtureSHA) || !isSHA256(a.Identity.EnvironmentFingerprint) || !isSHA256(a.Identity.ConfigFingerprint) {
		return fmt.Errorf("codegraph SHA must be a Git object ID and fixture, environment, and config fingerprints must be SHA-256")
	}
	if len(a.Scenarios) == 0 {
		return fmt.Errorf("benchmark artifact requires scenarios")
	}
	if a.Output.SerializedBytes < 0 || a.Output.EstimatedTokens < 0 || a.Output.EstimatedTokens != a.Output.SerializedBytes/4+btoi(a.Output.SerializedBytes%4 != 0) {
		return fmt.Errorf("output token estimate must equal ceil(serialized bytes/4)")
	}
	if a.Output.ProviderTokens != nil && *a.Output.ProviderTokens < 0 {
		return fmt.Errorf("provider tokens must be non-negative")
	}
	seenScenarios := map[string]bool{}
	for _, s := range a.Scenarios {
		if s.Name == "" || seenScenarios[s.Name] {
			return fmt.Errorf("scenario names must be non-empty and unique")
		}
		seenScenarios[s.Name] = true
		if s.WarmupCount < 0 || len(s.RawSamplesNS) == 0 {
			return fmt.Errorf("scenario %q requires raw samples and non-negative warmup", s.Name)
		}
		if a.PublicMode && len(s.RawSamplesNS) < 10 {
			return fmt.Errorf("public scenario %q requires at least 10 raw samples", s.Name)
		}
		for _, sample := range s.RawSamplesNS {
			if sample < 0 {
				return fmt.Errorf("scenario %q has negative timing sample", s.Name)
			}
		}
		if len(s.Denominators) == 0 {
			return fmt.Errorf("scenario %q requires denominator definitions", s.Name)
		}
		seenDenominators := map[string]bool{}
		for _, d := range s.Denominators {
			if d.Name == "" || d.Unit == "" || seenDenominators[d.Name] {
				return fmt.Errorf("scenario %q has invalid or duplicate denominator", s.Name)
			}
			seenDenominators[d.Name] = true
			if d.Value == nil || *d.Value <= 0 {
				if d.MissingReason == "" {
					return fmt.Errorf("scenario %q denominator %q missing or zero without a refusal reason", s.Name, d.Name)
				}
			} else if d.MissingReason != "" {
				return fmt.Errorf("scenario %q denominator %q has both value and missing reason", s.Name, d.Name)
			}
		}
	}
	if a.Quality != nil {
		q := a.Quality
		if !isSHA256(q.TaskSetSHA) || !isSHA256(q.RubricSHA) || q.Correct < 0 || q.Partial < 0 || q.Incorrect < 0 || q.Failed < 0 || q.Correct == 0 && q.Partial == 0 && q.Incorrect == 0 && q.Failed == 0 {
			return fmt.Errorf("quality result requires task/rubric SHA-256 and non-negative non-empty counts")
		}
	}
	return nil
}

func (a Artifact) Marshal() ([]byte, error) {
	a = a.canonicalize()
	if err := a.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxArtifactBytes {
		return nil, fmt.Errorf("benchmark artifact exceeds %d bytes", MaxArtifactBytes)
	}
	return b, nil
}

func DecodeArtifact(data []byte) (Artifact, error) {
	if len(data) > MaxArtifactBytes {
		return Artifact{}, fmt.Errorf("benchmark artifact exceeds %d bytes", MaxArtifactBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var a Artifact
	if err := dec.Decode(&a); err != nil {
		return Artifact{}, fmt.Errorf("decode benchmark artifact: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Artifact{}, fmt.Errorf("benchmark artifact has trailing JSON value")
		}
		return Artifact{}, err
	}
	a = a.canonicalize()
	if err := a.Validate(); err != nil {
		return Artifact{}, err
	}
	return a, nil
}

func (a Artifact) canonicalize() Artifact {
	a.Scenarios = append([]ScenarioMeasurement(nil), a.Scenarios...)
	if a.Scenarios == nil {
		a.Scenarios = []ScenarioMeasurement{}
	}
	sort.Slice(a.Scenarios, func(i, j int) bool { return a.Scenarios[i].Name < a.Scenarios[j].Name })
	for i := range a.Scenarios {
		s := &a.Scenarios[i]
		s.RawSamplesNS = append([]int64(nil), s.RawSamplesNS...)
		s.Denominators = append([]Denominator(nil), s.Denominators...)
		if s.Denominators == nil {
			s.Denominators = []Denominator{}
		}
		sort.Slice(s.Denominators, func(i, j int) bool { return s.Denominators[i].Name < s.Denominators[j].Name })
	}
	return a
}

func CompareArtifacts(baseline, candidate Artifact) ComparisonArtifact {
	baseBytes, baseErr := baseline.Marshal()
	candidateBytes, candidateErr := candidate.Marshal()
	reasons := []string{}
	if baseErr != nil {
		reasons = append(reasons, "baseline_invalid")
	}
	if candidateErr != nil {
		reasons = append(reasons, "candidate_invalid")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, Compare(baseline.Identity, candidate.Identity).Reasons...)
		reasons = append(reasons, scenarioMismatchReasons(baseline.Scenarios, candidate.Scenarios)...)
	}
	return ComparisonArtifact{Schema: ComparisonSchema, BaselineSHA: digest(baseBytes), CandidateSHA: digest(candidateBytes), Comparability: Comparability{Comparable: len(reasons) == 0, Reasons: reasons}}
}

func scenarioMismatchReasons(a, b []ScenarioMeasurement) []string {
	if len(a) != len(b) {
		return []string{"scenario_set_mismatch"}
	}
	byName := map[string]ScenarioMeasurement{}
	for _, s := range a {
		byName[s.Name] = s
	}
	for _, s := range b {
		base, ok := byName[s.Name]
		if !ok {
			return []string{"scenario_set_mismatch"}
		}
		if len(base.Denominators) != len(s.Denominators) {
			return []string{"denominator_definition_mismatch:" + s.Name}
		}
		units := map[string]string{}
		for _, d := range base.Denominators {
			units[d.Name] = d.Unit
		}
		for _, d := range s.Denominators {
			if units[d.Name] != d.Unit {
				return []string{"denominator_definition_mismatch:" + s.Name + ":" + d.Name}
			}
		}
	}
	return nil
}

func digest(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RenderMarkdown emits a compact view of the stored identity and raw samples.
// It does not calculate a separate latency or quality claim.
func (a Artifact) RenderMarkdown() (string, error) {
	a = a.canonicalize()
	if err := a.Validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Benchmark artifact\n\n- CodeGraph SHA: `%s`\n- Fixture: `%s` (`%s`)\n- Lifecycle: `%s`\n- Environment: `%s`\n- Config: `%s`\n- Public mode: `%t`\n\n| Scenario | Warmups | Raw timing samples (ns) | Denominators |\n| --- | ---: | --- | --- |\n", a.Identity.CodeGraphSHA, a.Identity.FixtureID, a.Identity.FixtureSHA, a.Identity.Lifecycle, a.Identity.EnvironmentFingerprint, a.Identity.ConfigFingerprint, a.PublicMode)
	for _, s := range a.Scenarios {
		samples, _ := json.Marshal(s.RawSamplesNS)
		denominators := make([]string, 0, len(s.Denominators))
		for _, d := range s.Denominators {
			if d.Value == nil || *d.Value <= 0 {
				denominators = append(denominators, d.Name+": absent ("+d.MissingReason+")")
			} else {
				denominators = append(denominators, fmt.Sprintf("%s=%d %s", d.Name, *d.Value, d.Unit))
			}
		}
		fmt.Fprintf(&b, "| %s | %d | `%s` | %s |\n", s.Name, s.WarmupCount, samples, strings.Join(denominators, "; "))
	}
	fmt.Fprintf(&b, "\nOutput accounting: %d serialized bytes, %d estimated tokens", a.Output.SerializedBytes, a.Output.EstimatedTokens)
	if a.Output.ProviderTokens != nil {
		fmt.Fprintf(&b, ", %d provider tokens", *a.Output.ProviderTokens)
	}
	b.WriteString(".\n")
	if q := a.Quality; q != nil {
		fmt.Fprintf(&b, "\nQuality (separate result; task set `%s`, rubric `%s`): correct %d, partial %d, incorrect %d, failed %d.\n", q.TaskSetSHA, q.RubricSHA, q.Correct, q.Partial, q.Incorrect, q.Failed)
	}
	return b.String(), nil
}

func validGitSHA(value string) bool {
	return (len(value) == 40 || len(value) == 64) && func() bool { _, err := hex.DecodeString(value); return err == nil }()
}
func btoi(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

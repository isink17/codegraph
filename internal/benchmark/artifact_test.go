package benchmark

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func artifact() Artifact {
	value := int64(100)
	provider := int64(120)
	id := testIdentity()
	id.CodeGraphSHA = strings.Repeat("c", 40)
	id.FixtureSHA = strings.Repeat("d", 64)
	id.EnvironmentFingerprint = strings.Repeat("e", 64)
	id.ConfigFingerprint = strings.Repeat("f", 64)
	return Artifact{
		Schema: ArtifactSchema, Identity: id,
		Scenarios: []ScenarioMeasurement{{Name: "symbol_lookup", RawSamplesNS: []int64{10, 11}, SampleCount: 2, WarmupCount: 2, OperationsPerSample: 1, AggregationPolicy: BenchstatAggregation, Denominators: []Denominator{{Name: "results", Unit: "symbols", Value: &value}}}},
		Output:    OutputAccounting{SerializedBytes: 9, EstimatedTokens: 3, ProviderTokens: &provider},
		Quality:   &QualityResult{TaskSetSHA: strings.Repeat("a", 64), RubricSHA: strings.Repeat("b", 64), Correct: 1},
	}
}

func TestRenderMarkdownUsesOnlyStoredRawFields(t *testing.T) {
	a := artifact()
	got, err := a.RenderMarkdown()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Raw timing samples (ns)", "`[10,11]`", "provider tokens", "Quality (separate result", "results=100 symbols"} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q: %s", want, got)
		}
	}
}

func TestArtifactRoundTripDeterministic(t *testing.T) {
	a := artifact()
	a.Scenarios = append(a.Scenarios, ScenarioMeasurement{Name: "callers", RawSamplesNS: []int64{20}, SampleCount: 1, OperationsPerSample: 1, AggregationPolicy: BenchstatAggregation, Denominators: []Denominator{{Name: "rows", Unit: "rows", Value: func() *int64 { v := int64(2); return &v }()}}})
	first, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("serialization differs: %s != %s", first, second)
	}
	got, err := DecodeArtifact(first)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := got.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, encoded) {
		t.Fatalf("round-trip differs: %s != %s", first, encoded)
	}
}

func TestArtifactRequiresSamplesDenominatorsAndValidAccounting(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Artifact)
	}{
		{"missing denominator", func(a *Artifact) { a.Scenarios[0].Denominators = nil }},
		{"zero denominator without reason", func(a *Artifact) { v := int64(0); a.Scenarios[0].Denominators[0].Value = &v }},
		{"missing denominator without reason", func(a *Artifact) { a.Scenarios[0].Denominators[0].Value = nil }},
		{"token accounting mismatch", func(a *Artifact) { a.Output.EstimatedTokens = 2 }},
		{"no samples", func(a *Artifact) { a.Scenarios[0].RawSamplesNS = nil }},
		{"sample count mismatch", func(a *Artifact) { a.Scenarios[0].SampleCount++ }},
		{"missing aggregation policy", func(a *Artifact) { a.Scenarios[0].AggregationPolicy = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := artifact()
			tt.mutate(&a)
			if err := a.Validate(); err == nil {
				t.Fatal("invalid artifact accepted")
			}
		})
	}
}

func TestArtifactRejectsMalformedLifecycleFingerprints(t *testing.T) {
	a := artifact()
	a.Identity.Lifecycle = LifecycleIncremental
	a.Identity.PriorStateSHA = "not-a-git-sha"
	a.Identity.ChangeSetSHA = strings.Repeat("a", 64)
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "prior state") {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestArtifactPublicModeNeedsTenSamples(t *testing.T) {
	a := artifact()
	a.PublicMode = true
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "10 raw samples") {
		t.Fatalf("Validate() = %v", err)
	}
	for len(a.Scenarios[0].RawSamplesNS) < 10 {
		a.Scenarios[0].RawSamplesNS = append(a.Scenarios[0].RawSamplesNS, 1)
	}
	a.Scenarios[0].SampleCount = len(a.Scenarios[0].RawSamplesNS)
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	benchstat, err := a.RenderBenchstat()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(benchstat, "Benchmarksymbol_lookup-1"); got != 10 {
		t.Fatalf("benchstat has %d samples, want 10: %s", got, benchstat)
	}
}

func TestCompareArtifactsRefusesDifferentDenominatorValues(t *testing.T) {
	base, candidate := artifact(), artifact()
	value := int64(101)
	candidate.Scenarios[0].Denominators[0].Value = &value
	got := CompareArtifacts(base, candidate)
	if got.Comparability.Comparable || !contains(got.Comparability.Reasons, "denominator_value_mismatch:symbol_lookup:results") {
		t.Fatalf("denominator mismatch = %+v", got.Comparability)
	}
}

func TestDecodeArtifactRejectsMalformedUnknownAndOversize(t *testing.T) {
	for _, data := range [][]byte{[]byte("{"), []byte(`{"schema":"future/v99","unknown":true}`), append(bytes.Repeat([]byte{' '}, MaxArtifactBytes+1), '{')} {
		if _, err := DecodeArtifact(data); err == nil {
			t.Fatal("invalid artifact accepted")
		}
	}
	good, _ := artifact().Marshal()
	if _, err := DecodeArtifact(append(good, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	var doc map[string]any
	if err := json.Unmarshal(good, &doc); err != nil {
		t.Fatal(err)
	}
}

func TestCompareArtifactsRefusesStaleAndEnvironmentMismatch(t *testing.T) {
	base, candidate := artifact(), artifact()
	if got := CompareArtifacts(base, candidate); !got.Comparability.Comparable {
		t.Fatalf("same run refused: %+v", got)
	}
	candidate.Identity.FixtureSHA = strings.Repeat("b", 64)
	if got := CompareArtifacts(base, candidate); got.Comparability.Comparable || !contains(got.Comparability.Reasons, "fixture_sha_mismatch") {
		t.Fatalf("fixture mismatch = %+v", got)
	}
	candidate = artifact()
	candidate.Identity.EnvironmentFingerprint = strings.Repeat("c", 64)
	if got := CompareArtifacts(base, candidate); got.Comparability.Comparable || !contains(got.Comparability.Reasons, "environment_fingerprint_mismatch") {
		t.Fatalf("environment mismatch = %+v", got)
	}
	candidate = artifact()
	candidate.Scenarios[0].Name = "other"
	if got := CompareArtifacts(base, candidate); got.Comparability.Comparable || !contains(got.Comparability.Reasons, "scenario_set_mismatch") {
		t.Fatalf("scenario mismatch = %+v", got)
	}
}

func TestCompareArtifactsUsesStableInvalidReasons(t *testing.T) {
	base, candidate := artifact(), artifact()
	candidate.Identity.FixtureSHA = "invalid"
	got := CompareArtifacts(base, candidate)
	if got.Comparability.Comparable || len(got.Comparability.Reasons) != 1 || got.Comparability.Reasons[0] != "candidate_invalid" {
		t.Fatalf("invalid artifact reasons = %+v", got.Comparability.Reasons)
	}
}

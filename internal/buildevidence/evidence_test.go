package buildevidence

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func testArtifact() Artifact {
	sha := strings.Repeat("a", 64)
	dimension := func() Dimension { return Dimension{State: Complete, Provenance: []string{"gradle-model:v1"}} }
	return Artifact{
		Schema:           SchemaV1,
		Producer:         Producer{Name: "codegraph-gradle-exporter", Version: "0.1.0"},
		Repository:       Repository{Identity: "repo-identity", RootMarker: "logical-slash-v1", BuildRoot: "."},
		Compilation:      Compilation{GradleProject: ":app", KotlinCompilation: "main"},
		Tools:            Tools{GradleVersion: "8.10", KotlinPluginVersion: "2.0.20", KotlinCompilerVersion: "2.0.20", JDKVendor: "Temurin", JDKVersion: "21"},
		InputFingerprint: sha,
		Evidence: Evidence{
			Dimensions:       Dimensions{Source: dimension(), Generated: dimension(), Excluded: dimension(), Dependency: dimension(), ExternalMetadata: dimension(), CompilerIdentity: dimension()},
			SourceRoots:      []string{"app/src/main/kotlin"},
			GeneratedRoots:   []string{},
			ExcludedRoots:    []string{},
			Dependencies:     []Dependency{{Coordinate: "org.example:lib:1.0", State: "positive", Provenance: "resolved compile classpath"}},
			ExternalMetadata: []Metadata{{Identity: "org.example:lib:1.0", SHA256: sha, Provenance: "artifact metadata"}},
		},
	}
}

func testProof() *Proof {
	return &Proof{
		TaskOutcome: "TASK_REPORTED_WORK", TaskDidWork: true,
		TaskInputSHA256: []string{}, CompilerArtifacts: []FileIdentity{},
		OutputFiles: []OutputFile{{Path: "app/build/classes/kotlin/main/Hello.class", SHA256: strings.Repeat("b", 64)}},
		OutputRoots: []string{"app/build/classes/kotlin/main"}, TaskInputsComplete: true, TaskInputsStable: true, OutputsComplete: true, OutputsContained: true,
		Dimensions: ProofDimensions{
			GradleRuntimeJDK:  Dimension{State: Incomplete, Provenance: []string{"runtime JDK content identity incomplete"}},
			TaskExecution:     Dimension{State: Incomplete, Provenance: []string{"Gradle task outcome only"}},
			CompilerRuntime:   Dimension{State: Unknown, Provenance: []string{"not observed"}},
			SelectedToolchain: Dimension{State: Unknown, Provenance: []string{"not observed"}},
			ABIInputs:         Dimension{State: Incomplete, Provenance: []string{"input closure incomplete"}},
			OutputProvenance:  Dimension{State: Incomplete, Provenance: []string{"clean output state unknown"}},
		},
	}
}

func TestArtifactRoundTripAndCurrentFingerprint(t *testing.T) {
	a := testArtifact()
	first, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("artifact serialization is not deterministic")
	}
	decoded, err := Decode(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateCurrent(a.InputFingerprint); err != nil {
		t.Fatalf("ValidateCurrent(): %v", err)
	}
	if err := decoded.ValidateCurrent(strings.Repeat("b", 64)); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale fingerprint error = %v", err)
	}
}

func TestArtifactRejectsMalformedVersionAndUnknownFields(t *testing.T) {
	if _, err := Decode([]byte("{")); err == nil {
		t.Fatal("malformed JSON was accepted")
	}
	a := testArtifact()
	a.Schema = "codegraph.gradle_kotlin_evidence/v99"
	if _, err := a.Marshal(); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("version error = %v", err)
	}
	data := []byte(`{"schema":"codegraph.gradle_kotlin_evidence/v1","secret":"should-not-be-captured"}`)
	if _, err := Decode(data); err == nil {
		t.Fatal("unknown/secret field was accepted")
	}
}

func TestOmittedDimensionsStayUnknown(t *testing.T) {
	a := testArtifact()
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	dimensions := document["evidence"].(map[string]any)["dimensions"].(map[string]any)
	delete(dimensions, "generated")
	delete(dimensions, "excluded")
	data, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	for name, dimension := range map[string]Dimension{"generated": decoded.Evidence.Dimensions.Generated, "excluded": decoded.Evidence.Dimensions.Excluded} {
		if dimension.State != Unknown || len(dimension.Provenance) == 0 {
			t.Errorf("%s = %+v, want explicit UNKNOWN with provenance", name, dimension)
		}
	}
}

func TestPartialDimensionRemainsPartial(t *testing.T) {
	a := testArtifact()
	a.Evidence.Dimensions.Generated = Dimension{State: Incomplete, Provenance: []string{"task graph omitted generated roots"}}
	data, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.Evidence.Dimensions.Generated.State; got != Incomplete {
		t.Fatalf("generated state = %q, want incomplete", got)
	}
}

func TestArtifactRejectsEscapingRootAndUnsupportedState(t *testing.T) {
	for _, root := range []string{"../../outside", "C:/outside"} {
		a := testArtifact()
		a.Evidence.SourceRoots = []string{root}
		if err := a.Validate(); err == nil {
			t.Fatalf("unsafe source root %q was accepted", root)
		}
	}
	a := testArtifact()
	a.Evidence.Dimensions.Source.State = "maybe"
	if err := a.Validate(); err == nil {
		t.Fatal("unknown dimension state was accepted")
	}
}

func TestCompleteCompilerIdentityRequiresToolVersions(t *testing.T) {
	a := testArtifact()
	a.Tools.KotlinCompilerVersion = ""
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "compiler versions") {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestV1ArtifactsRemainDiagnosticAndCannotCarryV2Proof(t *testing.T) {
	a := testArtifact()
	if a.Schema != SchemaV1 || a.Proof != nil {
		t.Fatal("legacy fixture no longer represents v1")
	}
	if _, err := a.Marshal(); err != nil {
		t.Fatalf("v1 artifact rejected: %v", err)
	}
	a.Proof = testProof()
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "v1") {
		t.Fatalf("v1 artifact accepted v2 proof fields: %v", err)
	}
}

func TestV2ProofRequiresExplicitIncompleteTrustDimensions(t *testing.T) {
	a := testArtifact()
	a.Schema = Schema
	a.Proof = testProof()
	if _, err := a.Marshal(); err != nil {
		t.Fatalf("valid diagnostic v2 artifact rejected: %v", err)
	}
	a.Proof.Dimensions.OutputProvenance.State = Complete
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "remain incomplete") {
		t.Fatalf("unproven output provenance was promoted: %v", err)
	}
}

func TestV2CacheOutcomeCannotAuthorizeFreshExecution(t *testing.T) {
	p := testProof()
	p.TaskOutcome = "FROM_CACHE"
	p.TaskDidWork = false
	p.TaskSkipped = true
	p.SkipReasonCode = "FROM_CACHE"
	if err := validateProof(*p); err != nil {
		t.Fatalf("cache diagnostic proof rejected: %v", err)
	}
	p.Dimensions.TaskExecution.State = Complete
	if err := validateProof(*p); err == nil {
		t.Fatal("cache-restored output accepted as new compiler execution")
	}
}

func TestV2NonFreshTaskOutcomesRemainIncomplete(t *testing.T) {
	for _, outcome := range []string{"UP_TO_DATE", "FROM_CACHE", "NO_SOURCE", "SKIPPED", "COMPLETED_NO_WORK"} {
		t.Run(outcome, func(t *testing.T) {
			p := testProof()
			p.TaskOutcome = outcome
			p.TaskDidWork = false
			p.TaskSkipped = outcome != "COMPLETED_NO_WORK"
			p.SkipReasonCode = ""
			if p.TaskSkipped {
				p.SkipReasonCode = outcome
			}
			if err := validateProof(*p); err != nil {
				t.Fatalf("diagnostic task outcome rejected: %v", err)
			}
			if p.Dimensions.TaskExecution.State == Complete || p.Dimensions.OutputProvenance.State == Complete {
				t.Fatal("non-fresh outcome promoted execution or output trust")
			}
		})
	}
}

func TestCompleteDimensionsRequireExplicitPayloads(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Artifact)
	}{
		{name: "source roots", mutate: func(a *Artifact) { a.Evidence.SourceRoots = nil }},
		{name: "generated roots", mutate: func(a *Artifact) { a.Evidence.GeneratedRoots = nil }},
		{name: "excluded roots", mutate: func(a *Artifact) { a.Evidence.ExcludedRoots = nil }},
		{name: "dependencies", mutate: func(a *Artifact) { a.Evidence.Dependencies = nil }},
		{name: "external metadata", mutate: func(a *Artifact) { a.Evidence.ExternalMetadata = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := testArtifact()
			test.mutate(&a)
			if err := a.Validate(); err == nil {
				t.Fatal("complete dimension accepted omitted payload")
			}
		})
	}
}

func TestNegativeDependencyRequiresCompleteScope(t *testing.T) {
	a := testArtifact()
	a.Evidence.Dimensions.Dependency = Dimension{State: Incomplete, Provenance: []string{"partial classpath"}}
	a.Evidence.Dependencies = []Dependency{{Coordinate: "org.example:missing:1.0", State: "negative", Provenance: "partial classpath"}}
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "requires complete") {
		t.Fatalf("Validate() = %v", err)
	}
}

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
		Schema:           Schema,
		Producer:         Producer{Name: "codegraph-gradle-exporter", Version: "0.1.0"},
		Repository:       Repository{Identity: "repo-identity", RootMarker: "logical-slash-v1", BuildRoot: "."},
		Compilation:      Compilation{GradleProject: ":app", KotlinCompilation: "main"},
		Tools:            Tools{GradleVersion: "8.10", KotlinPluginVersion: "2.0.20", KotlinCompilerVersion: "2.0.20", JDKVendor: "Temurin", JDKVersion: "21"},
		InputFingerprint: sha,
		Evidence: Evidence{
			Dimensions:       Dimensions{Source: dimension(), Generated: dimension(), Excluded: dimension(), Dependency: dimension(), ExternalMetadata: dimension(), CompilerIdentity: dimension()},
			SourceRoots:      []string{"app/src/main/kotlin"},
			Dependencies:     []Dependency{{Coordinate: "org.example:lib:1.0", State: "positive", Provenance: "resolved compile classpath"}},
			ExternalMetadata: []Metadata{{Identity: "org.example:lib:1.0", SHA256: sha, Provenance: "artifact metadata"}},
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

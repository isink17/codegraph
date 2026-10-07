package benchmark

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func testIdentity() RunIdentity {
	dirty, cgo := false, true
	return RunIdentity{
		SchemaVersion: IdentitySchema, CodeGraphSHA: "abc123", Dirty: &dirty,
		Lifecycle: LifecycleFresh,
		FixtureID: "query-100k-v1", FixtureSHA: "fixture-sha",
		CanonicalRootMarker: "logical-slash-v1", EnvironmentFingerprint: "machine-sha", OS: "darwin", Arch: "arm64",
		GoVersion: "go1.26.1", CGOEnabled: &cgo, ConfigFingerprint: "config-sha",
	}
}

func TestCompareRunIdentity(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*RunIdentity)
		wantReason string
	}{
		{name: "same identity"},
		{name: "fixture sha differs", mutate: func(id *RunIdentity) { id.FixtureSHA = "other" }, wantReason: "fixture_sha_mismatch"},
		{name: "codegraph sha differs", mutate: func(id *RunIdentity) { id.CodeGraphSHA = "other" }, wantReason: "codegraph_sha_mismatch"},
		{name: "dirty versus clean", mutate: func(id *RunIdentity) { clean := true; id.Dirty = &clean }, wantReason: "dirty_state_mismatch"},
		{name: "dirty runs refuse without diff fingerprint", mutate: func(id *RunIdentity) { dirty := true; id.Dirty = &dirty }, wantReason: "dirty_tree_not_comparable"},
		{name: "config differs", mutate: func(id *RunIdentity) { id.ConfigFingerprint = "other" }, wantReason: "config_fingerprint_mismatch"},
		{name: "lifecycle differs", mutate: func(id *RunIdentity) { id.Lifecycle = LifecycleNoOp; id.PriorStateSHA = "state-sha" }, wantReason: "lifecycle_mismatch"},
		{name: "missing required identity", mutate: func(id *RunIdentity) { id.FixtureSHA = "" }, wantReason: "right_invalid:benchmark identity missing required fields: [fixture_sha]"},
		{name: "unknown schema", mutate: func(id *RunIdentity) { id.SchemaVersion = "future/v99" }, wantReason: `right_invalid:unsupported benchmark identity schema "future/v99"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := testIdentity(), testIdentity()
			if tt.mutate != nil {
				tt.mutate(&b)
			}
			if tt.wantReason == "dirty_tree_not_comparable" {
				a.Dirty = b.Dirty
			}
			got := Compare(a, b)
			if tt.wantReason == "" {
				if !got.Comparable || len(got.Reasons) != 0 {
					t.Fatalf("Compare() = %+v, want comparable", got)
				}
				return
			}
			if got.Comparable || !contains(got.Reasons, tt.wantReason) {
				t.Fatalf("Compare() = %+v, want refusal reason %q", got, tt.wantReason)
			}
		})
	}
}

func TestMarshalIdentityDeterministicAndRoundTrips(t *testing.T) {
	id := testIdentity()
	first, err := MarshalIdentity(id)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalIdentity(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("MarshalIdentity differs:\n%s\n%s", first, second)
	}
	var got RunIdentity
	if err := json.Unmarshal(first, &got); err != nil {
		t.Fatal(err)
	}
	if result := Compare(id, got); !result.Comparable {
		t.Fatalf("round-trip identity mismatch: %+v", result)
	}
}

func TestMissingBooleansRemainUnknown(t *testing.T) {
	values := map[string]any{}
	data, err := json.Marshal(testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	delete(values, "dirty")
	b, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	var got RunIdentity
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	got.Dirty = nil
	if err := got.Validate(); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("Validate() = %v, want missing dirty", err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

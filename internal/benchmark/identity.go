// Package benchmark defines the identity needed to decide whether two
// CodeGraph benchmark runs can be compared.
package benchmark

import (
	"encoding/json"
	"fmt"
)

const IdentitySchema = "codegraph.benchmark_identity/v1"

const (
	LifecycleFresh       = "fresh"
	LifecycleIncremental = "incremental"
	LifecycleNoOp        = "no_op"
)

// RunIdentity records inputs that can change a measured result. Optional
// booleans are pointers so a missing dirty or CGO value cannot look like false.
type RunIdentity struct {
	SchemaVersion          string `json:"schema_version"`
	CodeGraphSHA           string `json:"codegraph_sha"`
	Dirty                  *bool  `json:"dirty"`
	Lifecycle              string `json:"lifecycle"`
	PriorStateSHA          string `json:"prior_state_sha,omitempty"`
	ChangeSetSHA           string `json:"change_set_sha,omitempty"`
	FixtureID              string `json:"fixture_id"`
	FixtureSHA             string `json:"fixture_sha"`
	CanonicalRootMarker    string `json:"canonical_root_marker"`
	EnvironmentFingerprint string `json:"environment_fingerprint"`
	OS                     string `json:"os"`
	Arch                   string `json:"arch"`
	GoVersion              string `json:"go_version"`
	CGOEnabled             *bool  `json:"cgo_enabled"`
	ConfigFingerprint      string `json:"config_fingerprint"`
}

// Comparability states whether both identities describe the same benchmark
// inputs. Reasons are stable, field-oriented codes suitable for artifacts.
type Comparability struct {
	Comparable bool     `json:"comparable"`
	Reasons    []string `json:"reasons"`
}

// Validate rejects an identity with an unknown schema or missing required
// values. It does not infer defaults for absent fields.
func (id RunIdentity) Validate() error {
	missing := make([]string, 0, 11)
	if id.SchemaVersion == "" {
		missing = append(missing, "schema_version")
	} else if id.SchemaVersion != IdentitySchema {
		return fmt.Errorf("unsupported benchmark identity schema %q", id.SchemaVersion)
	}
	if id.CodeGraphSHA == "" {
		missing = append(missing, "codegraph_sha")
	}
	if id.Dirty == nil {
		missing = append(missing, "dirty")
	}
	switch id.Lifecycle {
	case LifecycleFresh:
		if id.PriorStateSHA != "" || id.ChangeSetSHA != "" {
			return fmt.Errorf("fresh lifecycle cannot include prior state or change set")
		}
	case LifecycleIncremental:
		if id.PriorStateSHA == "" {
			missing = append(missing, "prior_state_sha")
		}
		if id.ChangeSetSHA == "" {
			missing = append(missing, "change_set_sha")
		}
	case LifecycleNoOp:
		if id.PriorStateSHA == "" {
			missing = append(missing, "prior_state_sha")
		}
		if id.ChangeSetSHA != "" {
			return fmt.Errorf("no_op lifecycle cannot include change_set_sha")
		}
	case "":
		missing = append(missing, "lifecycle")
	default:
		return fmt.Errorf("unsupported benchmark lifecycle %q", id.Lifecycle)
	}
	if id.FixtureID == "" {
		missing = append(missing, "fixture_id")
	}
	if id.FixtureSHA == "" {
		missing = append(missing, "fixture_sha")
	}
	if id.CanonicalRootMarker == "" {
		missing = append(missing, "canonical_root_marker")
	}
	if id.EnvironmentFingerprint == "" {
		missing = append(missing, "environment_fingerprint")
	}
	if id.OS == "" {
		missing = append(missing, "os")
	}
	if id.Arch == "" {
		missing = append(missing, "arch")
	}
	if id.GoVersion == "" {
		missing = append(missing, "go_version")
	}
	if id.CGOEnabled == nil {
		missing = append(missing, "cgo_enabled")
	}
	if id.ConfigFingerprint == "" {
		missing = append(missing, "config_fingerprint")
	}
	if len(missing) != 0 {
		return fmt.Errorf("benchmark identity missing required fields: %v", missing)
	}
	return nil
}

// Compare returns explicit reasons whenever either identity is incomplete or
// any measured input differs. A malformed identity never compares equal.
func Compare(a, b RunIdentity) Comparability {
	reasons := make([]string, 0)
	if err := a.Validate(); err != nil {
		reasons = append(reasons, "left_invalid:"+err.Error())
	}
	if err := b.Validate(); err != nil {
		reasons = append(reasons, "right_invalid:"+err.Error())
	}
	if len(reasons) != 0 {
		return Comparability{Reasons: reasons}
	}
	if a.CodeGraphSHA != b.CodeGraphSHA {
		reasons = append(reasons, "codegraph_sha_mismatch")
	}
	if *a.Dirty != *b.Dirty {
		reasons = append(reasons, "dirty_state_mismatch")
	} else if *a.Dirty {
		// The identity records the base commit, not an arbitrary working-tree
		// diff. Until a producer fingerprints that diff, never compare dirty runs.
		reasons = append(reasons, "dirty_tree_not_comparable")
	}
	if a.Lifecycle != b.Lifecycle {
		reasons = append(reasons, "lifecycle_mismatch")
	}
	if a.PriorStateSHA != b.PriorStateSHA {
		reasons = append(reasons, "prior_state_sha_mismatch")
	}
	if a.ChangeSetSHA != b.ChangeSetSHA {
		reasons = append(reasons, "change_set_sha_mismatch")
	}
	if a.FixtureID != b.FixtureID {
		reasons = append(reasons, "fixture_id_mismatch")
	}
	if a.FixtureSHA != b.FixtureSHA {
		reasons = append(reasons, "fixture_sha_mismatch")
	}
	if a.CanonicalRootMarker != b.CanonicalRootMarker {
		reasons = append(reasons, "canonical_root_marker_mismatch")
	}
	if a.EnvironmentFingerprint != b.EnvironmentFingerprint {
		reasons = append(reasons, "environment_fingerprint_mismatch")
	}
	if a.OS != b.OS {
		reasons = append(reasons, "os_mismatch")
	}
	if a.Arch != b.Arch {
		reasons = append(reasons, "arch_mismatch")
	}
	if a.GoVersion != b.GoVersion {
		reasons = append(reasons, "go_version_mismatch")
	}
	if *a.CGOEnabled != *b.CGOEnabled {
		reasons = append(reasons, "cgo_enabled_mismatch")
	}
	if a.ConfigFingerprint != b.ConfigFingerprint {
		reasons = append(reasons, "config_fingerprint_mismatch")
	}
	return Comparability{Comparable: len(reasons) == 0, Reasons: reasons}
}

// MarshalIdentity validates before producing deterministic struct-ordered
// JSON. It intentionally does not add timestamps or machine-local paths.
func MarshalIdentity(id RunIdentity) ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(id)
}

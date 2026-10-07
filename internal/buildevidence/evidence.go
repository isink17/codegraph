// Package buildevidence validates explicitly generated Gradle/Kotlin build
// evidence. It has no build execution or indexing side effects.
package buildevidence

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
)

const (
	Schema          = "codegraph.gradle_kotlin_evidence/v1"
	MaxArtifactSize = 16 << 20
)

type State string

const (
	Complete   State = "complete"
	Incomplete State = "incomplete"
	Unknown    State = "unknown"
)

type Dimension struct {
	State      State    `json:"state"`
	Provenance []string `json:"provenance"`
}

type Dimensions struct {
	Source           Dimension `json:"source"`
	Generated        Dimension `json:"generated"`
	Excluded         Dimension `json:"excluded"`
	Dependency       Dimension `json:"dependency"`
	ExternalMetadata Dimension `json:"external_metadata"`
	CompilerIdentity Dimension `json:"compiler_identity"`
}

type Producer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Repository struct {
	Identity   string `json:"identity"`
	RootMarker string `json:"root_marker"`
	BuildRoot  string `json:"build_root"`
}

type Compilation struct {
	GradleProject     string `json:"gradle_project"`
	KotlinCompilation string `json:"kotlin_compilation"`
}

type Tools struct {
	GradleVersion         string `json:"gradle_version,omitempty"`
	KotlinPluginVersion   string `json:"kotlin_plugin_version,omitempty"`
	KotlinCompilerVersion string `json:"kotlin_compiler_version,omitempty"`
	JDKVendor             string `json:"jdk_vendor,omitempty"`
	JDKVersion            string `json:"jdk_version,omitempty"`
}

type Dependency struct {
	Coordinate string `json:"coordinate"`
	State      string `json:"state"`
	Provenance string `json:"provenance"`
}

type Metadata struct {
	Identity   string `json:"identity"`
	SHA256     string `json:"sha256"`
	Provenance string `json:"provenance"`
}

type Evidence struct {
	Dimensions       Dimensions   `json:"dimensions"`
	SourceRoots      []string     `json:"source_roots"`
	GeneratedRoots   []string     `json:"generated_roots"`
	ExcludedRoots    []string     `json:"excluded_roots"`
	Dependencies     []Dependency `json:"dependencies"`
	ExternalMetadata []Metadata   `json:"external_metadata"`
}

// Artifact is the versioned output for one selected Kotlin Gradle compilation.
// InputFingerprint covers the complete relevant input set, including explicit
// unknown/incomplete evidence states; it is never a timestamp.
type Artifact struct {
	Schema           string      `json:"schema"`
	Producer         Producer    `json:"producer"`
	Repository       Repository  `json:"repository"`
	Compilation      Compilation `json:"compilation"`
	Tools            Tools       `json:"tools"`
	InputFingerprint string      `json:"input_fingerprint"`
	Evidence         Evidence    `json:"evidence"`
}

// Decode accepts bounded JSON, rejects unknown fields and trailing values, and
// turns omitted evidence dimensions into explicit UNKNOWN with provenance.
func Decode(data []byte) (Artifact, error) {
	if len(data) > MaxArtifactSize {
		return Artifact{}, fmt.Errorf("build evidence exceeds %d bytes", MaxArtifactSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var artifact Artifact
	if err := dec.Decode(&artifact); err != nil {
		return Artifact{}, fmt.Errorf("decode build evidence: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Artifact{}, fmt.Errorf("decode build evidence: multiple JSON values")
		}
		return Artifact{}, fmt.Errorf("decode build evidence trailer: %w", err)
	}
	artifact = artifact.withUnknownOmissions()
	if err := artifact.Validate(); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

// Marshal validates and emits deterministic struct-ordered JSON.
func (a Artifact) Marshal() ([]byte, error) {
	a = a.withUnknownOmissions()
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(a)
}

// Validate checks artifact identity, scope structure, and evidence provenance.
// It does not promote any dimension to complete.
func (a Artifact) Validate() error {
	if a.Schema != Schema {
		return fmt.Errorf("unsupported build evidence schema %q", a.Schema)
	}
	if a.Producer.Name == "" || a.Producer.Version == "" {
		return fmt.Errorf("build evidence producer name and version are required")
	}
	if a.Repository.Identity == "" || a.Repository.RootMarker == "" {
		return fmt.Errorf("build evidence repository identity and root marker are required")
	}
	if err := validateRepoPath(a.Repository.BuildRoot); err != nil {
		return fmt.Errorf("invalid build root: %w", err)
	}
	if !validGradleProject(a.Compilation.GradleProject) || !validCompilation(a.Compilation.KotlinCompilation) {
		return fmt.Errorf("selected Gradle project and Kotlin compilation are required")
	}
	if !isSHA256(a.InputFingerprint) {
		return fmt.Errorf("input_fingerprint must be a SHA-256 digest")
	}
	if err := validateDimension("source", a.Evidence.Dimensions.Source); err != nil {
		return err
	}
	if err := validateDimension("generated", a.Evidence.Dimensions.Generated); err != nil {
		return err
	}
	if err := validateDimension("excluded", a.Evidence.Dimensions.Excluded); err != nil {
		return err
	}
	if err := validateDimension("dependency", a.Evidence.Dimensions.Dependency); err != nil {
		return err
	}
	if err := validateDimension("external_metadata", a.Evidence.Dimensions.ExternalMetadata); err != nil {
		return err
	}
	if err := validateDimension("compiler_identity", a.Evidence.Dimensions.CompilerIdentity); err != nil {
		return err
	}
	if a.Evidence.Dimensions.CompilerIdentity.State == Complete && (a.Tools.GradleVersion == "" || a.Tools.KotlinPluginVersion == "" || a.Tools.KotlinCompilerVersion == "") {
		return fmt.Errorf("complete compiler identity requires Gradle, Kotlin plugin, and Kotlin compiler versions")
	}
	if a.Evidence.Dimensions.Source.State == Complete && a.Evidence.SourceRoots == nil {
		return fmt.Errorf("complete source evidence requires an explicit source_roots list")
	}
	if a.Evidence.Dimensions.Generated.State == Complete && a.Evidence.GeneratedRoots == nil {
		return fmt.Errorf("complete generated evidence requires an explicit generated_roots list")
	}
	if a.Evidence.Dimensions.Excluded.State == Complete && a.Evidence.ExcludedRoots == nil {
		return fmt.Errorf("complete excluded evidence requires an explicit excluded_roots list")
	}
	if a.Evidence.Dimensions.Dependency.State == Complete && a.Evidence.Dependencies == nil {
		return fmt.Errorf("complete dependency evidence requires an explicit dependencies list")
	}
	if a.Evidence.Dimensions.ExternalMetadata.State == Complete && a.Evidence.ExternalMetadata == nil {
		return fmt.Errorf("complete external metadata evidence requires an explicit external_metadata list")
	}
	for _, roots := range [][]string{a.Evidence.SourceRoots, a.Evidence.GeneratedRoots, a.Evidence.ExcludedRoots} {
		for _, root := range roots {
			if err := validateRepoPath(root); err != nil {
				return fmt.Errorf("invalid evidence root %q: %w", root, err)
			}
		}
	}
	for _, dep := range a.Evidence.Dependencies {
		if dep.Coordinate == "" || dep.Provenance == "" {
			return fmt.Errorf("dependency coordinate and provenance are required")
		}
		switch dep.State {
		case "positive", "negative", "unknown", "ambiguous":
		default:
			return fmt.Errorf("unsupported dependency state %q", dep.State)
		}
		if dep.State == "negative" && a.Evidence.Dimensions.Dependency.State != Complete {
			return fmt.Errorf("negative dependency evidence requires complete dependency scope")
		}
	}
	for _, metadata := range a.Evidence.ExternalMetadata {
		if metadata.Identity == "" || metadata.Provenance == "" || !isSHA256(metadata.SHA256) {
			return fmt.Errorf("external metadata requires identity, SHA-256, and provenance")
		}
	}
	return nil
}

// ValidateCurrent refuses evidence computed from any other build input set.
func (a Artifact) ValidateCurrent(currentFingerprint string) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if !isSHA256(currentFingerprint) {
		return fmt.Errorf("current input fingerprint must be a SHA-256 digest")
	}
	if a.InputFingerprint != currentFingerprint {
		return fmt.Errorf("build evidence is stale: input fingerprint mismatch")
	}
	return nil
}

func (a Artifact) withUnknownOmissions() Artifact {
	set := func(d *Dimension) {
		if d.State == "" {
			d.State = Unknown
			d.Provenance = []string{"dimension omitted from artifact"}
		}
	}
	set(&a.Evidence.Dimensions.Source)
	set(&a.Evidence.Dimensions.Generated)
	set(&a.Evidence.Dimensions.Excluded)
	set(&a.Evidence.Dimensions.Dependency)
	set(&a.Evidence.Dimensions.ExternalMetadata)
	set(&a.Evidence.Dimensions.CompilerIdentity)
	return a
}

func validateDimension(name string, d Dimension) error {
	switch d.State {
	case Complete, Incomplete, Unknown:
	default:
		return fmt.Errorf("%s evidence has invalid state %q", name, d.State)
	}
	if len(d.Provenance) == 0 {
		return fmt.Errorf("%s evidence requires provenance", name)
	}
	for _, item := range d.Provenance {
		if strings.TrimSpace(item) == "" {
			return fmt.Errorf("%s evidence has empty provenance", name)
		}
	}
	return nil
}

func validateRepoPath(value string) error {
	driveQualified := len(value) >= 3 && value[1] == ':' && value[2] == '/' && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z'))
	if value == "" || strings.Contains(value, "\\") || path.IsAbs(value) || driveQualified || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("path must be normalized and repository-relative")
	}
	return nil
}

func validGradleProject(value string) bool {
	if value == ":" {
		return true
	}
	if !strings.HasPrefix(value, ":") {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(value, ":"), ":") {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "/\\") {
			return false
		}
	}
	return true
}

func validCompilation(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, ":/\\")
}

func isSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

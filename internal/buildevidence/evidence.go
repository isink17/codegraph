// Package buildevidence produces and validates explicitly requested
// Gradle/Kotlin build evidence. It has no indexing side effects.
package buildevidence

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

const (
	SchemaV1        = "codegraph.gradle_kotlin_evidence/v1"
	Schema          = "codegraph.gradle_kotlin_evidence/v2"
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
	GradleTask        string `json:"gradle_task,omitempty"`
}

type Tools struct {
	GradleVersion            string `json:"gradle_version,omitempty"`
	KotlinPluginVersion      string `json:"kotlin_plugin_version,omitempty"`
	KotlinCompilerVersion    string `json:"kotlin_compiler_version,omitempty"`
	JDKVendor                string `json:"jdk_vendor,omitempty"`
	JDKVersion               string `json:"jdk_version,omitempty"`
	GradleRuntimeJDKVendor   string `json:"gradle_runtime_jdk_vendor,omitempty"`
	GradleRuntimeJDKVersion  string `json:"gradle_runtime_jdk_version,omitempty"`
	SelectedToolchainVendor  string `json:"selected_toolchain_vendor,omitempty"`
	SelectedToolchainVersion string `json:"selected_toolchain_version,omitempty"`
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

// Proof contains observations added in v2. It is diagnostic evidence only:
// Gradle task events and file hashes do not attest the compiler process that
// loaded the implementation or establish a complete ABI-affecting input set.
type Proof struct {
	TaskOutcome        string          `json:"task_outcome"`
	TaskDidWork        bool            `json:"task_did_work"`
	TaskSkipped        bool            `json:"task_skipped"`
	SkipReasonCode     string          `json:"skip_reason_code,omitempty"`
	TaskInputSHA256    []string        `json:"task_input_sha256"`
	CompilerArtifacts  []FileIdentity  `json:"compiler_artifact_candidates"`
	OutputFiles        []OutputFile    `json:"output_files"`
	OutputRoots        []string        `json:"output_roots"`
	JVMTarget          string          `json:"jvm_target,omitempty"`
	TaskInputsComplete bool            `json:"task_inputs_complete"`
	TaskInputsStable   bool            `json:"task_inputs_stable"`
	OutputRootsClean   bool            `json:"output_roots_clean_before_task"`
	OutputsComplete    bool            `json:"outputs_complete"`
	OutputsContained   bool            `json:"outputs_contained"`
	Dimensions         ProofDimensions `json:"dimensions"`
}

type ProofDimensions struct {
	GradleRuntimeJDK  Dimension `json:"gradle_runtime_jdk"`
	TaskExecution     Dimension `json:"task_execution"`
	CompilerRuntime   Dimension `json:"compiler_runtime"`
	SelectedToolchain Dimension `json:"selected_toolchain"`
	ABIInputs         Dimension `json:"abi_inputs"`
	OutputProvenance  Dimension `json:"output_provenance"`
}

type FileIdentity struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type OutputFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
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
	Proof            *Proof      `json:"proof,omitempty"`
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
	if a.Schema != Schema && a.Schema != SchemaV1 {
		return fmt.Errorf("unsupported build evidence schema %q", a.Schema)
	}
	if a.Schema == SchemaV1 && a.Proof != nil {
		return fmt.Errorf("v1 build evidence cannot carry v2 proof fields")
	}
	if a.Schema == SchemaV1 && (a.Tools.GradleRuntimeJDKVendor != "" || a.Tools.GradleRuntimeJDKVersion != "" || a.Tools.SelectedToolchainVendor != "" || a.Tools.SelectedToolchainVersion != "") {
		return fmt.Errorf("v1 build evidence cannot carry v2 runtime JDK fields")
	}
	if a.Schema == Schema && a.Proof == nil {
		return fmt.Errorf("v2 build evidence requires an explicit proof section")
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
	if a.Proof != nil {
		if err := validateProof(*a.Proof); err != nil {
			return err
		}
	}
	return nil
}

func validateProof(p Proof) error {
	switch p.TaskOutcome {
	case "TASK_REPORTED_WORK", "UP_TO_DATE", "FROM_CACHE", "NO_SOURCE", "SKIPPED", "COMPLETED_NO_WORK", "FAILED", "UNKNOWN":
	default:
		return fmt.Errorf("unsupported Gradle task outcome %q", p.TaskOutcome)
	}
	switch p.SkipReasonCode {
	case "", "UP_TO_DATE", "FROM_CACHE", "NO_SOURCE", "SKIPPED":
	default:
		return fmt.Errorf("unsupported Gradle skip reason code %q", p.SkipReasonCode)
	}
	switch p.TaskOutcome {
	case "TASK_REPORTED_WORK":
		if !p.TaskDidWork || p.TaskSkipped {
			return fmt.Errorf("task-reported-work outcome conflicts with did_work/skipped flags")
		}
	case "UP_TO_DATE", "FROM_CACHE", "NO_SOURCE", "SKIPPED":
		if p.TaskDidWork || !p.TaskSkipped || p.SkipReasonCode != p.TaskOutcome {
			return fmt.Errorf("skipped task outcome conflicts with did_work/skipped flags or skip reason")
		}
	case "COMPLETED_NO_WORK":
		if p.TaskDidWork || p.TaskSkipped {
			return fmt.Errorf("completed-no-work outcome conflicts with did_work/skipped flags")
		}
	}
	if p.TaskInputSHA256 == nil || p.CompilerArtifacts == nil || p.OutputFiles == nil || p.OutputRoots == nil {
		return fmt.Errorf("v2 proof requires explicit task-input, compiler-candidate, output-file, and output-root lists")
	}
	if p.TaskInputsStable && !p.TaskInputsComplete {
		return fmt.Errorf("stable task inputs require a complete bounded input inventory")
	}
	if p.OutputRootsClean && (!p.OutputsContained || !p.OutputsComplete || len(p.OutputRoots) == 0) {
		return fmt.Errorf("clean output roots require a complete contained output inventory")
	}
	if !safeToolIdentity(p.JVMTarget) {
		return fmt.Errorf("JVM target contains unsupported text")
	}
	for _, item := range []struct {
		name string
		dim  Dimension
	}{
		{"gradle_runtime_jdk", p.Dimensions.GradleRuntimeJDK},
		{"task_execution", p.Dimensions.TaskExecution},
		{"compiler_runtime", p.Dimensions.CompilerRuntime},
		{"selected_toolchain", p.Dimensions.SelectedToolchain},
		{"abi_inputs", p.Dimensions.ABIInputs},
		{"output_provenance", p.Dimensions.OutputProvenance},
	} {
		if err := validateDimension(item.name, item.dim); err != nil {
			return err
		}
	}
	if p.Dimensions.GradleRuntimeJDK.State == Complete || p.Dimensions.TaskExecution.State == Complete || p.Dimensions.CompilerRuntime.State == Complete || p.Dimensions.SelectedToolchain.State == Complete || p.Dimensions.ABIInputs.State == Complete || p.Dimensions.OutputProvenance.State == Complete {
		return fmt.Errorf("v2 proof dimensions remain incomplete until execution, clean outputs, and ABI input closure are independently proven")
	}
	for _, digest := range p.TaskInputSHA256 {
		if !isSHA256(digest) {
			return fmt.Errorf("task input identity must be a SHA-256 digest")
		}
	}
	for _, file := range p.CompilerArtifacts {
		if file.Name == "" || !safeArtifactName(file.Name) || !isSHA256(file.SHA256) {
			return fmt.Errorf("compiler artifact candidate requires a safe name and SHA-256")
		}
	}
	outputRoots := append([]string(nil), p.OutputRoots...)
	sort.Strings(outputRoots)
	for i := 1; i < len(outputRoots); i++ {
		if outputRoots[i] == outputRoots[i-1] {
			return fmt.Errorf("duplicate output root %q", outputRoots[i])
		}
	}
	seenOutputs := make(map[string]struct{}, len(p.OutputFiles))
	for _, file := range p.OutputFiles {
		if err := validateRepoPath(file.Path); err != nil {
			return fmt.Errorf("invalid output path %q: %w", file.Path, err)
		}
		if !strings.HasSuffix(file.Path, ".class") {
			return fmt.Errorf("selected output inventory only accepts class files: %q", file.Path)
		}
		if _, exists := seenOutputs[file.Path]; exists {
			return fmt.Errorf("duplicate selected output path %q", file.Path)
		}
		seenOutputs[file.Path] = struct{}{}
		attributed := false
		for _, root := range p.OutputRoots {
			if root == "." || file.Path == root || strings.HasPrefix(file.Path, strings.TrimSuffix(root, "/")+"/") {
				attributed = true
				break
			}
		}
		if !attributed {
			return fmt.Errorf("selected class output %q is outside declared output roots", file.Path)
		}
		if !isSHA256(file.SHA256) {
			return fmt.Errorf("output file %q requires a SHA-256 digest", file.Path)
		}
	}
	for _, root := range p.OutputRoots {
		if err := validateRepoPath(root); err != nil {
			return fmt.Errorf("invalid output root %q: %w", root, err)
		}
	}
	if p.TaskOutcome == "FROM_CACHE" && (p.Dimensions.TaskExecution.State == Complete || p.Dimensions.OutputProvenance.State == Complete) {
		return fmt.Errorf("cache-restored outputs cannot prove new execution provenance")
	}
	return nil
}

func safeArtifactName(value string) bool {
	if len(value) > 255 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "/\\\r\n\x00") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._+-()", r) {
			continue
		}
		return false
	}
	return value != "" && value != "." && value != ".."
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

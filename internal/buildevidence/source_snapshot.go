package buildevidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	SourceSnapshotSchema       = "codegraph.kotlin_task_source_snapshot/v1"
	SupportedSourceSnapshotKGP = "2.3.20"
	MaxSourceSnapshotFiles     = 10000
	MaxSourceCandidates        = 20000
	MaxSourceSnapshotBytes     = int64(1 << 30)
	MaxSourceSnapshotFileSize  = int64(256 << 20)
	MaxSourcePathBytes         = 4 << 20
	MaxSourceFilterPatterns    = 1000
	MaxSourcePatternBytes      = 512
)

type rawSourceObservation struct {
	API                  string   `json:"api"`
	CandidateAPI         string   `json:"candidate_api"`
	GeneratedAPI         string   `json:"generated_api"`
	Phase                string   `json:"phase"`
	KotlinPluginVersion  string   `json:"kotlin_plugin_version"`
	TaskFilesBefore      []string `json:"task_files_before"`
	TaskFilesAfter       []string `json:"task_files_after"`
	CandidateFilesBefore []string `json:"candidate_files_before"`
	CandidateFilesAfter  []string `json:"candidate_files_after"`
	GeneratedFilesBefore []string `json:"generated_files_before"`
	GeneratedFilesAfter  []string `json:"generated_files_after"`
	SourceRootsBefore    []string `json:"source_roots_before"`
	SourceRootsAfter     []string `json:"source_roots_after"`
	GeneratedRootsBefore []string `json:"generated_roots_before"`
	GeneratedRootsAfter  []string `json:"generated_roots_after"`
	Includes             []string `json:"includes"`
	Excludes             []string `json:"excludes"`
	ObservationMissing   bool     `json:"observation_missing"`
}

type SourceEntry struct {
	Path             string `json:"path,omitempty"`
	Language         string `json:"language"`
	SHA256           string `json:"sha256,omitempty"`
	State            State  `json:"state"`
	Origin           string `json:"origin"`
	OriginProvenance string `json:"origin_provenance"`
	SelectionSource  string `json:"selection_source"`
	Reason           string `json:"reason,omitempty"`
}

type SourceCandidate struct {
	Path      string `json:"path,omitempty"`
	State     State  `json:"state"`
	Origin    string `json:"origin"`
	Selection string `json:"selection"`
	Reason    string `json:"reason,omitempty"`
}

// SourceSnapshot records the Kotlin source files exposed by the selected
// KGP task. It does not claim that the compiler consumed those files.
type SourceSnapshot struct {
	Schema                      string            `json:"schema"`
	Producer                    Producer          `json:"producer"`
	RepositoryIdentity          string            `json:"repository_identity"`
	BuildRoot                   string            `json:"build_root"`
	GradleProject               string            `json:"gradle_project"`
	KotlinCompilation           string            `json:"kotlin_compilation"`
	CompileTask                 string            `json:"compile_task"`
	V2InputFingerprint          string            `json:"v2_input_fingerprint"`
	ObservationAPI              string            `json:"observation_api"`
	CandidateAPI                string            `json:"candidate_api"`
	GeneratedAPI                string            `json:"generated_api"`
	ObservationPhase            string            `json:"observation_phase"`
	KotlinPluginVersion         string            `json:"kotlin_plugin_version"`
	TaskOutcome                 string            `json:"task_outcome"`
	TaskInputsStable            bool              `json:"task_inputs_stable"`
	TaskMembershipState         State             `json:"declared_task_membership_observed"`
	RecordedBytesState          State             `json:"recorded_source_bytes_observed"`
	CandidateState              State             `json:"source_candidates_observed"`
	GeneratedSetState           State             `json:"generated_source_set_membership_observed"`
	GeneratedOwnershipState     State             `json:"generated_producer_ownership"`
	ExclusionCoverageState      State             `json:"exclusion_coverage"`
	CompilerConsumedSourceState State             `json:"compiler_consumed_source_set"`
	SourceRoots                 []string          `json:"source_roots"`
	GeneratedRoots              []string          `json:"generated_roots"`
	Includes                    []string          `json:"include_patterns,omitempty"`
	Excludes                    []string          `json:"exclude_patterns,omitempty"`
	FilterProvenance            []string          `json:"filter_provenance"`
	Entries                     []SourceEntry     `json:"selected_sources"`
	Candidates                  []SourceCandidate `json:"source_candidates"`
	SelectedContentSHA256       string            `json:"selected_content_sha256"`
	CandidateMembershipSHA256   string            `json:"candidate_membership_sha256"`
	SnapshotSHA256              string            `json:"snapshot_sha256"`
}

type SourceSnapshotFreshness struct {
	BuildEvidence        State    `json:"build_evidence_freshness"`
	RecordedContent      State    `json:"recorded_source_bytes_freshness"`
	RecordedMembership   State    `json:"recorded_membership_freshness"`
	CurrentTaskSelection State    `json:"current_gradle_task_selection"`
	GeneratedOwnership   State    `json:"generated_source_ownership"`
	CompilerConsumed     State    `json:"compiler_consumed_source_set"`
	Reasons              []string `json:"reasons"`
}

type sourceSnapshotLimits struct {
	bytes   int64
	files   int
	entries int
}

func buildSourceSnapshotContext(ctx context.Context, root string, artifact Artifact, raw rawSourceObservation, model gradleModel) (SourceSnapshot, error) {
	return buildSourceSnapshotContextWithLimits(ctx, root, artifact, raw, model, sourceSnapshotLimits{
		bytes: MaxSourceSnapshotBytes, files: MaxSourceSnapshotFiles, entries: MaxSourceSnapshotFiles,
	})
}

func buildSourceSnapshotContextWithLimits(ctx context.Context, root string, artifact Artifact, raw rawSourceObservation, model gradleModel, limits sourceSnapshotLimits) (SourceSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return SourceSnapshot{}, err
	}
	if limits.bytes < 0 || limits.files < 0 || limits.entries < 0 {
		return SourceSnapshot{}, errors.New("invalid source snapshot limits")
	}
	if err := artifact.Validate(); err != nil {
		return SourceSnapshot{}, fmt.Errorf("validate related build evidence: %w", err)
	}
	if model.KotlinPluginVersion != SupportedSourceSnapshotKGP || raw.KotlinPluginVersion != SupportedSourceSnapshotKGP {
		return SourceSnapshot{}, fmt.Errorf("source snapshot supports Kotlin Gradle Plugin %s only", SupportedSourceSnapshotKGP)
	}
	if raw.API != "org.jetbrains.kotlin.gradle.tasks.KotlinCompileTool.sources" || raw.CandidateAPI != "KotlinSourceSet.allKotlinSources" || raw.GeneratedAPI != "KotlinSourceSet.generatedKotlin" || raw.Phase != "beforeExecute_and_afterExecute" {
		return SourceSnapshot{}, errors.New("Gradle source observation API or phase is unsupported")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return SourceSnapshot{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return SourceSnapshot{}, errors.New("repository root cannot be resolved")
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return SourceSnapshot{}, errors.New("repository root cannot be opened safely")
	}
	defer rootFS.Close()

	selectedBefore := normalizedRawPaths(raw.TaskFilesBefore)
	selectedAfter := normalizedRawPaths(raw.TaskFilesAfter)
	candidateBefore := normalizedRawPaths(raw.CandidateFilesBefore)
	candidateAfter := normalizedRawPaths(raw.CandidateFilesAfter)
	generatedBefore := normalizedRawPaths(raw.GeneratedFilesBefore)
	generatedAfter := normalizedRawPaths(raw.GeneratedFilesAfter)
	selectedStable := equalStrings(selectedBefore, selectedAfter)
	candidateStable := equalStrings(candidateBefore, candidateAfter)
	generatedStable := equalStrings(generatedBefore, generatedAfter)

	snapshot := SourceSnapshot{
		Schema:                      SourceSnapshotSchema,
		Producer:                    Producer{Name: "codegraph-gradle-source-snapshot", Version: "1"},
		RepositoryIdentity:          artifact.Repository.Identity,
		BuildRoot:                   artifact.Repository.BuildRoot,
		GradleProject:               artifact.Compilation.GradleProject,
		KotlinCompilation:           artifact.Compilation.KotlinCompilation,
		CompileTask:                 artifact.Compilation.GradleTask,
		V2InputFingerprint:          artifact.InputFingerprint,
		ObservationAPI:              raw.API,
		CandidateAPI:                raw.CandidateAPI,
		GeneratedAPI:                raw.GeneratedAPI,
		ObservationPhase:            raw.Phase,
		KotlinPluginVersion:         raw.KotlinPluginVersion,
		TaskOutcome:                 model.TaskOutcome,
		TaskInputsStable:            model.TaskInputsStable,
		TaskMembershipState:         Complete,
		RecordedBytesState:          Complete,
		CandidateState:              Complete,
		GeneratedSetState:           Complete,
		GeneratedOwnershipState:     Unknown,
		ExclusionCoverageState:      Incomplete,
		CompilerConsumedSourceState: Unknown,
		SourceRoots:                 make([]string, 0),
		GeneratedRoots:              make([]string, 0),
		FilterProvenance:            []string{"KotlinCompileTool.sources applies KGP source filters; Gradle exposes include/exclude patterns but arbitrary predicates are not enumerable"},
		Entries:                     make([]SourceEntry, 0, min(len(selectedAfter), MaxSourceSnapshotFiles)),
		Candidates:                  make([]SourceCandidate, 0, min(len(candidateAfter), MaxSourceCandidates)),
	}
	if model.TaskOutcome == "FAILED" || !model.TaskInputsStable || !selectedStable || len(raw.TaskFilesBefore) > MaxSourceSnapshotFiles || len(raw.TaskFilesAfter) > MaxSourceSnapshotFiles {
		snapshot.TaskMembershipState = Incomplete
	}
	if !candidateStable || len(raw.CandidateFilesBefore) > MaxSourceCandidates || len(raw.CandidateFilesAfter) > MaxSourceCandidates {
		snapshot.CandidateState = Incomplete
	}
	if !generatedStable {
		snapshot.GeneratedSetState = Incomplete
	}
	if raw.ObservationMissing {
		snapshot.TaskMembershipState = Incomplete
		snapshot.CandidateState = Incomplete
		snapshot.GeneratedSetState = Incomplete
		snapshot.FilterProvenance = append(snapshot.FilterProvenance, "selected task had no beforeExecute source observation")
	}
	if !selectedStable {
		snapshot.FilterProvenance = append(snapshot.FilterProvenance, "selected task source membership changed between beforeExecute and afterExecute")
	}
	if !candidateStable {
		snapshot.FilterProvenance = append(snapshot.FilterProvenance, "Kotlin source-set candidate membership changed during selected task execution")
	}
	if !generatedStable {
		snapshot.FilterProvenance = append(snapshot.FilterProvenance, "generated source-set membership changed during selected task execution")
	}

	var pathBytes int
	for _, pattern := range raw.Includes {
		if len(snapshot.Includes) == MaxSourceFilterPatterns || pathBytes+len(pattern) > MaxSourcePathBytes || !safeSourcePattern(pattern) {
			snapshot.ExclusionCoverageState = Incomplete
			continue
		}
		snapshot.Includes = append(snapshot.Includes, pattern)
		pathBytes += len(pattern)
	}
	for _, pattern := range raw.Excludes {
		if len(snapshot.Excludes) == MaxSourceFilterPatterns || pathBytes+len(pattern) > MaxSourcePathBytes || !safeSourcePattern(pattern) {
			snapshot.ExclusionCoverageState = Incomplete
			continue
		}
		snapshot.Excludes = append(snapshot.Excludes, pattern)
		pathBytes += len(pattern)
	}
	if !equalStrings(snapshot.Includes, raw.Includes) || !equalStrings(snapshot.Excludes, raw.Excludes) {
		snapshot.FilterProvenance = append(snapshot.FilterProvenance, "one or more include/exclude patterns were omitted by the safe pattern limit")
	}
	sort.Strings(snapshot.Includes)
	sort.Strings(snapshot.Excludes)

	rootState := Complete
	for _, rawRoot := range raw.SourceRootsAfter {
		if rel, e := sourceDirectoryLocator(root, rawRoot); e == nil {
			snapshot.SourceRoots = append(snapshot.SourceRoots, rel)
		} else {
			rootState = Incomplete
		}
	}
	for _, rawRoot := range raw.GeneratedRootsAfter {
		if rel, e := sourceDirectoryLocator(root, rawRoot); e == nil {
			snapshot.GeneratedRoots = append(snapshot.GeneratedRoots, rel)
		} else {
			rootState = Incomplete
		}
	}
	if rootState == Incomplete {
		snapshot.FilterProvenance = append(snapshot.FilterProvenance, "one or more declared source roots could not be represented safely")
	}
	snapshot.SourceRoots = uniqueSorted(snapshot.SourceRoots)
	snapshot.GeneratedRoots = uniqueSorted(snapshot.GeneratedRoots)

	generatedSet := stringSet(generatedAfter)
	candidateSet := stringSet(candidateAfter)
	selectedAfter = uniqueSorted(selectedAfter)
	if len(selectedAfter) > MaxSourceSnapshotFiles {
		selectedAfter = selectedAfter[:MaxSourceSnapshotFiles]
		snapshot.TaskMembershipState = Incomplete
	}
	budget := classpathReadBudget{limits: classpathRevalidationLimits{
		bytes: limits.bytes, files: limits.files, entries: limits.entries,
	}}
	var totalPathBytes int
	for _, rawPath := range selectedAfter {
		if err := ctx.Err(); err != nil {
			return SourceSnapshot{}, err
		}
		entry := SourceEntry{Language: sourceLanguage(rawPath), State: Complete, Origin: "unknown", OriginProvenance: "Kotlin source-set origin is not available for this selected task file", SelectionSource: "KotlinCompileTool.sources"}
		if generatedSet[rawPath] && candidateSet[rawPath] {
			entry.Origin = "registered_generated_source_set"
			entry.OriginProvenance = "selected file is present in KotlinSourceSet.generatedKotlin; producer-task ownership is not established"
		} else if candidateSet[rawPath] {
			entry.Origin = "ordinary_source_set"
			entry.OriginProvenance = "selected file is present in KotlinSourceSet.allKotlinSources and absent from generatedKotlin"
		}
		locator, pathErr := sourceFileLocator(root, rootFS, rawPath)
		pathBudgetExceeded := len(locator)+totalPathBytes > MaxSourcePathBytes
		if pathErr != nil || pathBudgetExceeded {
			entry.State, entry.Reason = Incomplete, sourcePathReason(pathErr, pathBudgetExceeded)
			entry.Path = ""
			snapshot.TaskMembershipState = Incomplete
			snapshot.RecordedBytesState = Incomplete
			if pathBudgetExceeded {
				budget.exhausted = true
			}
			snapshot.Entries = append(snapshot.Entries, entry)
			continue
		}
		entry.Path = locator
		totalPathBytes += len(locator)
		entry.SHA256, _, pathErr = stableFileDigestRootBudgetedContext(ctx, rootFS, filepath.FromSlash(locator), min(MaxSourceSnapshotFileSize, limits.bytes), &budget)
		if err := ctx.Err(); err != nil {
			return SourceSnapshot{}, err
		}
		if pathErr != nil {
			entry.State, entry.Reason, entry.SHA256 = Incomplete, sourcePathReason(pathErr, false), ""
			snapshot.RecordedBytesState = Incomplete
		}
		snapshot.Entries = append(snapshot.Entries, entry)
	}
	sort.Slice(snapshot.Entries, func(i, j int) bool {
		if snapshot.Entries[i].Path != snapshot.Entries[j].Path {
			return snapshot.Entries[i].Path < snapshot.Entries[j].Path
		}
		return snapshot.Entries[i].Origin < snapshot.Entries[j].Origin
	})
	if budget.exhausted {
		snapshot.RecordedBytesState = Incomplete
		snapshot.FilterProvenance = append(snapshot.FilterProvenance, "selected source hashing exceeded its aggregate byte/file/entry budget")
	}

	selectedPaths := stringSet(selectedAfter)
	candidates := uniqueSorted(candidateAfter)
	if len(candidates) > MaxSourceCandidates {
		candidates = candidates[:MaxSourceCandidates]
		snapshot.CandidateState = Incomplete
	}
	for _, rawPath := range candidates {
		candidate := SourceCandidate{State: Complete, Selection: "not_selected", Origin: "ordinary_source_set", Reason: "present in a compilation source set but absent from KotlinCompileTool.sources; exact exclusion cause is unknown"}
		if generatedSet[rawPath] {
			candidate.Origin = "registered_generated_source_set"
		}
		if selectedPaths[rawPath] {
			candidate.Selection, candidate.Reason = "selected", ""
		}
		locator, pathErr := sourceFileLocator(root, rootFS, rawPath)
		if pathErr != nil {
			candidate.State, candidate.Reason = Incomplete, sourcePathReason(pathErr, false)
			snapshot.CandidateState = Incomplete
		} else {
			candidate.Path = locator
		}
		snapshot.Candidates = append(snapshot.Candidates, candidate)
	}
	sort.Slice(snapshot.Candidates, func(i, j int) bool {
		if snapshot.Candidates[i].Path != snapshot.Candidates[j].Path {
			return snapshot.Candidates[i].Path < snapshot.Candidates[j].Path
		}
		return snapshot.Candidates[i].Origin < snapshot.Candidates[j].Origin
	})
	snapshot.SelectedContentSHA256, err = selectedContentDigest(snapshot.Entries)
	if err != nil {
		return SourceSnapshot{}, err
	}
	snapshot.CandidateMembershipSHA256, err = candidateMembershipDigest(snapshot.Candidates)
	if err != nil {
		return SourceSnapshot{}, err
	}
	snapshot.SnapshotSHA256, err = sourceSnapshotDigest(snapshot)
	if err != nil {
		return SourceSnapshot{}, err
	}
	return snapshot, nil
}

func (s SourceSnapshot) Validate() error {
	if s.Schema != SourceSnapshotSchema || s.Producer.Name != "codegraph-gradle-source-snapshot" || s.Producer.Version != "1" {
		return errors.New("unsupported Kotlin source snapshot schema or producer")
	}
	if !strings.HasPrefix(s.RepositoryIdentity, "sha256:") || !isSHA256(strings.TrimPrefix(s.RepositoryIdentity, "sha256:")) || !isSHA256(s.V2InputFingerprint) {
		return errors.New("source snapshot repository identity or v2 fingerprint is invalid")
	}
	if err := validateRepoPath(s.BuildRoot); err != nil {
		return fmt.Errorf("invalid source snapshot build root: %w", err)
	}
	if !validGradleProject(s.GradleProject) || !validCompilation(s.KotlinCompilation) || !validGradleProject(s.CompileTask) {
		return errors.New("source snapshot selected project, compilation, and task are required")
	}
	if s.ObservationAPI != "org.jetbrains.kotlin.gradle.tasks.KotlinCompileTool.sources" || s.CandidateAPI != "KotlinSourceSet.allKotlinSources" || s.GeneratedAPI != "KotlinSourceSet.generatedKotlin" || s.ObservationPhase != "beforeExecute_and_afterExecute" || s.KotlinPluginVersion != SupportedSourceSnapshotKGP {
		return errors.New("source snapshot observation contract is unsupported")
	}
	if !validSourceOutcome(s.TaskOutcome) || len(s.FilterProvenance) == 0 || s.Entries == nil || s.Candidates == nil {
		return errors.New("source snapshot must include outcome, provenance, and explicit source lists")
	}
	if s.GeneratedOwnershipState != Unknown || s.ExclusionCoverageState != Incomplete || s.CompilerConsumedSourceState != Unknown {
		return errors.New("source snapshot cannot promote generated ownership, exclusion coverage, or compiler consumption")
	}
	for _, state := range []State{s.TaskMembershipState, s.RecordedBytesState, s.CandidateState, s.GeneratedSetState} {
		if state != Complete && state != Incomplete {
			return errors.New("source snapshot contains an unsupported observation state")
		}
	}
	if s.TaskMembershipState == Complete && !s.TaskInputsStable {
		return errors.New("complete task membership requires stable task inputs")
	}
	if s.RecordedBytesState == Complete && s.TaskMembershipState != Complete {
		return errors.New("complete recorded source bytes require complete task membership observation")
	}
	for _, roots := range [][]string{s.SourceRoots, s.GeneratedRoots} {
		if roots == nil {
			return errors.New("source snapshot must include explicit source root lists")
		}
		for _, root := range roots {
			if err := validateRepoPath(root); err != nil {
				return fmt.Errorf("invalid source snapshot root: %w", err)
			}
		}
	}
	if len(s.Entries) > MaxSourceSnapshotFiles || len(s.Candidates) > MaxSourceCandidates || len(s.Includes)+len(s.Excludes) > 2*MaxSourceFilterPatterns {
		return errors.New("source snapshot exceeds its entry or filter limit")
	}
	previous := ""
	allEntriesComplete := true
	for _, entry := range s.Entries {
		if entry.Language != "kotlin" && entry.Language != "kotlin_source_unknown_extension" || entry.SelectionSource != "KotlinCompileTool.sources" {
			return errors.New("source snapshot entry contains unsupported language or selection provenance")
		}
		if entry.Origin != "ordinary_source_set" && entry.Origin != "registered_generated_source_set" && entry.Origin != "unknown" {
			return errors.New("source snapshot entry contains unsupported origin classification")
		}
		if entry.OriginProvenance == "" {
			return errors.New("source snapshot entry omits origin provenance")
		}
		if entry.State == Complete {
			if err := validateRepoPath(entry.Path); err != nil || entry.Path == "." || !isSHA256(entry.SHA256) || entry.Reason != "" {
				return errors.New("source snapshot entry contains an invalid path or content hash")
			}
			if previous != "" && entry.Path <= previous {
				return errors.New("source snapshot selected entries are not uniquely sorted")
			}
			previous = entry.Path
		} else if entry.State == Incomplete {
			allEntriesComplete = false
			if entry.Reason == "" || entry.SHA256 != "" || entry.Path != "" && validateRepoPath(entry.Path) != nil {
				return errors.New("incomplete source snapshot entry must carry a safe reason and no hash")
			}
		} else {
			return errors.New("source snapshot entry has unsupported state")
		}
	}
	if s.RecordedBytesState == Complete && !allEntriesComplete {
		return errors.New("complete source byte observation contains incomplete entries")
	}
	previous = ""
	for _, candidate := range s.Candidates {
		if candidate.Origin != "ordinary_source_set" && candidate.Origin != "registered_generated_source_set" {
			return errors.New("source candidate contains unsupported origin classification")
		}
		if candidate.Selection != "selected" && candidate.Selection != "not_selected" {
			return errors.New("source candidate contains unsupported selection state")
		}
		if candidate.State == Complete {
			if err := validateRepoPath(candidate.Path); err != nil || candidate.Path == "." || previous != "" && candidate.Path <= previous {
				return errors.New("source candidates are not uniquely sorted safe paths")
			}
			previous = candidate.Path
		} else if candidate.State != Incomplete || candidate.Reason == "" || candidate.Path != "" {
			return errors.New("incomplete source candidate must carry a reason and no path")
		}
	}
	for _, pattern := range append(append([]string(nil), s.Includes...), s.Excludes...) {
		if !safeSourcePattern(pattern) {
			return errors.New("source snapshot contains an unsafe filter pattern")
		}
	}
	if !isSHA256(s.SelectedContentSHA256) || !isSHA256(s.CandidateMembershipSHA256) {
		return errors.New("source snapshot content or candidate digest is invalid")
	}
	digest, err := sourceSnapshotDigest(s)
	if err != nil {
		return err
	}
	if digest != s.SnapshotSHA256 {
		return errors.New("source snapshot digest mismatch")
	}
	return nil
}

func (s SourceSnapshot) Marshal() ([]byte, error) {
	s.SnapshotSHA256 = ""
	digest, err := sourceSnapshotDigest(s)
	if err != nil {
		return nil, err
	}
	s.SnapshotSHA256 = digest
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

func DecodeSourceSnapshot(data []byte) (SourceSnapshot, error) {
	if len(data) > MaxArtifactSize {
		return SourceSnapshot{}, fmt.Errorf("Kotlin source snapshot exceeds %d bytes", MaxArtifactSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var snapshot SourceSnapshot
	if err := dec.Decode(&snapshot); err != nil {
		return SourceSnapshot{}, fmt.Errorf("decode Kotlin source snapshot: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return SourceSnapshot{}, errors.New("Kotlin source snapshot contains trailing JSON")
	}
	if err := snapshot.Validate(); err != nil {
		return SourceSnapshot{}, err
	}
	return snapshot, nil
}

func ValidateSourceSnapshotCurrentContext(ctx context.Context, repositoryRoot string, artifact Artifact, snapshot SourceSnapshot) (SourceSnapshotFreshness, error) {
	if err := ctx.Err(); err != nil {
		return SourceSnapshotFreshness{}, err
	}
	if err := artifact.Validate(); err != nil {
		return SourceSnapshotFreshness{}, fmt.Errorf("validate related build evidence: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return SourceSnapshotFreshness{}, err
	}
	root, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return SourceSnapshotFreshness{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return SourceSnapshotFreshness{}, fmt.Errorf("resolve repository root: %w", err)
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return SourceSnapshotFreshness{}, errors.New("repository root cannot be opened safely")
	}
	defer rootFS.Close()
	wantIdentity := sha256.Sum256([]byte(root))
	want := "sha256:" + hex.EncodeToString(wantIdentity[:])
	if snapshot.RepositoryIdentity != want || snapshot.RepositoryIdentity != artifact.Repository.Identity || snapshot.BuildRoot != artifact.Repository.BuildRoot || snapshot.GradleProject != artifact.Compilation.GradleProject || snapshot.KotlinCompilation != artifact.Compilation.KotlinCompilation || snapshot.CompileTask != artifact.Compilation.GradleTask || snapshot.V2InputFingerprint != artifact.InputFingerprint {
		return SourceSnapshotFreshness{}, errors.New("source snapshot does not match its related repository and v2 evidence")
	}
	result := SourceSnapshotFreshness{BuildEvidence: Incomplete, RecordedContent: Complete, RecordedMembership: Unknown, CurrentTaskSelection: Unknown, GeneratedOwnership: Unknown, CompilerConsumed: Unknown, Reasons: []string{"read-only validation hashes only previously recorded files; it does not execute Gradle or discover new selected sources", "generated source producer ownership and compiler-consumed source membership are not established"}}
	budget := classpathReadBudget{limits: classpathRevalidationLimits{bytes: MaxClasspathRevalidationBytes, files: MaxClasspathRevalidationFiles, entries: MaxClasspathRevalidationEntries}}
	if fingerprint, e := currentFingerprintContext(ctx, root, artifact, &budget); e != nil {
		if ctx.Err() != nil {
			return SourceSnapshotFreshness{}, ctx.Err()
		}
		result.Reasons = append(result.Reasons, "related v2 build evidence could not be fully revalidated")
	} else if fingerprint != artifact.InputFingerprint {
		result.Reasons = append(result.Reasons, "related v2 build evidence is stale")
	} else {
		result.BuildEvidence = Complete
	}
	if snapshot.TaskMembershipState != Complete {
		result.RecordedMembership = Incomplete
		result.CurrentTaskSelection = Incomplete
		result.Reasons = append(result.Reasons, "source membership was unstable or incomplete at export")
	}
	missingRecordedMember := false
	for _, entry := range snapshot.Entries {
		if err := ctx.Err(); err != nil {
			return SourceSnapshotFreshness{}, err
		}
		if entry.State != Complete {
			result.RecordedContent = Incomplete
			result.RecordedMembership = Incomplete
			result.Reasons = append(result.Reasons, "one or more selected source entries were incomplete at export")
			continue
		}
		digest, _, digestErr := stableFileDigestRootBudgetedContext(ctx, rootFS, filepath.FromSlash(entry.Path), MaxSourceSnapshotFileSize, &budget)
		if ctx.Err() != nil {
			return SourceSnapshotFreshness{}, ctx.Err()
		}
		if digestErr != nil {
			result.RecordedContent = Incomplete
			result.RecordedMembership = Incomplete
			missingRecordedMember = true
			result.Reasons = append(result.Reasons, fmt.Sprintf("recorded source %q is missing or unsafe", entry.Path))
		} else if digest != entry.SHA256 {
			result.RecordedContent = Incomplete
			result.Reasons = append(result.Reasons, fmt.Sprintf("recorded source %q changed", entry.Path))
		}
	}
	if budget.exhausted {
		result.RecordedContent = Incomplete
		result.Reasons = append(result.Reasons, "aggregate source and related evidence revalidation budget exceeded")
	}
	if result.RecordedMembership == Unknown && !missingRecordedMember {
		result.Reasons = append(result.Reasons, "all recorded selected paths remain readable, but additions and today's filtered task membership are unknown")
	}
	return result, nil
}

func sourceSnapshotDigest(snapshot SourceSnapshot) (string, error) {
	snapshot.SnapshotSHA256 = ""
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func selectedContentDigest(entries []SourceEntry) (string, error) {
	type item struct{ Path, SHA256 string }
	items := make([]item, 0, len(entries))
	for _, entry := range entries {
		if entry.State == Complete {
			items = append(items, item{entry.Path, entry.SHA256})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	data, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func candidateMembershipDigest(candidates []SourceCandidate) (string, error) {
	type item struct{ Path, Origin, Selection string }
	items := make([]item, 0, len(candidates))
	for _, candidate := range candidates {
		items = append(items, item{candidate.Path, candidate.Origin, candidate.Selection})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Path != items[j].Path {
			return items[i].Path < items[j].Path
		}
		if items[i].Origin != items[j].Origin {
			return items[i].Origin < items[j].Origin
		}
		return items[i].Selection < items[j].Selection
	})
	data, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func sourceFileLocator(root string, rootFS *os.Root, rawPath string) (string, error) {
	absolute, err := filepath.Abs(rawPath)
	if err != nil {
		return "", errors.New("source path cannot be resolved")
	}
	rel, ok := relativeWithin(root, absolute)
	if !ok || rel == "." {
		return "", errors.New("source path is outside repository")
	}
	if err := validateRepoPath(filepath.ToSlash(rel)); err != nil {
		return "", errors.New("source path is not a safe repository-relative path")
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	current := ""
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("source path is not safe")
		}
		if current == "" {
			current = part
		} else {
			current = filepath.Join(current, part)
		}
		info, err := rootFS.Lstat(current)
		if err != nil {
			return "", errors.New("source path cannot be inspected")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("source path contains a symlink")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", errors.New("source path parent is not a directory")
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return "", errors.New("source path is not a regular file")
		}
	}
	return filepath.ToSlash(rel), nil
}

func sourceDirectoryLocator(root, rawPath string) (string, error) {
	absolute, err := filepath.Abs(rawPath)
	if err != nil {
		return "", err
	}
	rel, ok := relativeWithin(root, absolute)
	if !ok || rel == "." {
		return "", errors.New("source root is outside repository")
	}
	if err := validateRepoPath(filepath.ToSlash(rel)); err != nil {
		return "", err
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return "", errors.New("source root cannot be opened safely")
	}
	defer rootFS.Close()
	current := ""
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("source root is not safe")
		}
		if current == "" {
			current = part
		} else {
			current = filepath.Join(current, part)
		}
		info, err := rootFS.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("source root is missing or contains a symlink")
		}
	}
	return filepath.ToSlash(rel), nil
}

func sourcePathReason(err error, budget bool) string {
	if budget {
		return "source snapshot path or byte budget exceeded"
	}
	if err == nil {
		return "source snapshot content could not be hashed"
	}
	switch {
	case strings.Contains(err.Error(), "outside repository"):
		return "source path is outside repository"
	case strings.Contains(err.Error(), "symlink"):
		return "source path contains a symlink"
	case strings.Contains(err.Error(), "regular file"):
		return "source path is not a regular file"
	case strings.Contains(err.Error(), "budget"):
		return "source snapshot byte/file budget exceeded"
	default:
		return "source path is missing, unreadable, unstable, or unsafe"
	}
}

func sourceLanguage(path string) string {
	extension := strings.ToLower(filepath.Ext(path))
	if extension == ".kt" || extension == ".kts" {
		return "kotlin"
	}
	return "kotlin_source_unknown_extension"
}

func safeSourcePattern(pattern string) bool {
	if pattern == "" || len(pattern) > MaxSourcePatternBytes || filepath.IsAbs(pattern) || strings.Contains(pattern, "..") {
		return false
	}
	for _, r := range pattern {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-/\\*?[]{}!@+", r) {
			continue
		}
		return false
	}
	return true
}

func validSourceOutcome(outcome string) bool {
	switch outcome {
	case "TASK_REPORTED_WORK", "UP_TO_DATE", "FROM_CACHE", "NO_SOURCE", "SKIPPED", "COMPLETED_NO_WORK", "FAILED":
		return true
	default:
		return false
	}
}

func normalizedRawPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if absolute, err := filepath.Abs(path); err == nil {
			out = append(out, filepath.Clean(absolute))
		} else {
			out = append(out, path)
		}
	}
	return uniqueSorted(out)
}

func uniqueSorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	if len(out) < 2 {
		return out
	}
	write := 1
	for read := 1; read < len(out); read++ {
		if out[read] != out[write-1] {
			out[write] = out[read]
			write++
		}
	}
	return out[:write]
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func equalStrings(a, b []string) bool {
	a, b = uniqueSorted(a), uniqueSorted(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

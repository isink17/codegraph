package buildevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type cancelDuringHashContext struct {
	parent   context.Context
	done     chan struct{}
	checks   int
	cancelAt int
	once     sync.Once
}

func (c *cancelDuringHashContext) Deadline() (time.Time, bool) { return c.parent.Deadline() }
func (c *cancelDuringHashContext) Done() <-chan struct{}       { return c.done }
func (c *cancelDuringHashContext) Value(key any) any           { return c.parent.Value(key) }
func (c *cancelDuringHashContext) Err() error {
	c.checks++
	if c.checks >= c.cancelAt {
		c.once.Do(func() { close(c.done) })
	}
	select {
	case <-c.done:
		return context.Canceled
	default:
		return c.parent.Err()
	}
}

func sourceSnapshotFixture(t *testing.T) (string, Artifact, rawSourceObservation, gradleModel) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(root, "app", "src", "main", "kotlin")
	generatedDir := filepath.Join(root, "app", "build", "generated", "sources", "kotlin")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(generatedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(sourceDir, "Selected.kt")
	excluded := filepath.Join(sourceDir, "Excluded.kt")
	generated := filepath.Join(generatedDir, "Generated.kt")
	for path, content := range map[string]string{selected: "fun selected() = 1\n", excluded: "fun excluded() = 1\n", generated: "fun generated() = 1\n"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	identityHash := sha256.Sum256([]byte(root))
	identity := "sha256:" + hex.EncodeToString(identityHash[:])
	a := Artifact{
		Schema:           SchemaV1,
		Producer:         Producer{Name: "test", Version: "1"},
		Repository:       Repository{Identity: identity, RootMarker: identity, BuildRoot: "."},
		Compilation:      Compilation{GradleProject: ":app", KotlinCompilation: "main", GradleTask: ":app:compileKotlin"},
		Tools:            Tools{GradleVersion: "9.3.1", KotlinPluginVersion: SupportedSourceSnapshotKGP},
		InputFingerprint: strings.Repeat("a", 64),
		Evidence: Evidence{
			Dimensions: Dimensions{
				Source:           Dimension{State: Incomplete, Provenance: []string{"source root is not a selected file list"}},
				Generated:        Dimension{State: Unknown, Provenance: []string{"generated producer ownership unknown"}},
				Excluded:         Dimension{State: Unknown, Provenance: []string{"exclusion coverage incomplete"}},
				Dependency:       Dimension{State: Unknown, Provenance: []string{"not observed"}},
				ExternalMetadata: Dimension{State: Unknown, Provenance: []string{"not observed"}},
				CompilerIdentity: Dimension{State: Unknown, Provenance: []string{"not observed"}},
			},
			SourceRoots: []string{"app/src/main/kotlin"}, GeneratedRoots: []string{}, ExcludedRoots: []string{},
		},
	}
	fingerprint, err := CurrentFingerprint(root, a)
	if err != nil {
		t.Fatal(err)
	}
	a.InputFingerprint = fingerprint
	raw := rawSourceObservation{
		API:                 "org.jetbrains.kotlin.gradle.tasks.KotlinCompileTool.sources",
		CandidateAPI:        "KotlinSourceSet.allKotlinSources",
		GeneratedAPI:        "KotlinSourceSet.generatedKotlin",
		Phase:               "beforeExecute_and_afterExecute",
		KotlinPluginVersion: SupportedSourceSnapshotKGP,
		TaskFilesBefore:     []string{selected, generated}, TaskFilesAfter: []string{generated, selected},
		CandidateFilesBefore: []string{selected, excluded, generated}, CandidateFilesAfter: []string{generated, excluded, selected},
		GeneratedFilesBefore: []string{generated}, GeneratedFilesAfter: []string{generated},
		SourceRootsBefore: []string{sourceDir}, SourceRootsAfter: []string{sourceDir},
		GeneratedRootsBefore: []string{generatedDir}, GeneratedRootsAfter: []string{generatedDir},
		Includes: []string{"**/*.kt"}, Excludes: []string{"**/Excluded.kt"},
	}
	model := gradleModel{KotlinPluginVersion: SupportedSourceSnapshotKGP, TaskOutcome: "TASK_REPORTED_WORK", TaskInputsStable: true}
	return root, a, raw, model
}

func TestBuildSourceSnapshotSeparatesSelectedGeneratedAndExcludedCandidates(t *testing.T) {
	root, artifact, raw, model := sourceSnapshotFixture(t)
	snapshot, err := buildSourceSnapshotContext(context.Background(), root, artifact, raw, model)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TaskMembershipState != Complete || snapshot.RecordedBytesState != Complete || snapshot.CandidateState != Complete || snapshot.GeneratedSetState != Complete {
		t.Fatalf("observed source dimensions = task %s bytes %s candidates %s generated %s", snapshot.TaskMembershipState, snapshot.RecordedBytesState, snapshot.CandidateState, snapshot.GeneratedSetState)
	}
	if snapshot.GeneratedOwnershipState != Unknown || snapshot.ExclusionCoverageState != Incomplete || snapshot.CompilerConsumedSourceState != Unknown {
		t.Fatalf("source trust was promoted: ownership=%s exclusion=%s consumed=%s", snapshot.GeneratedOwnershipState, snapshot.ExclusionCoverageState, snapshot.CompilerConsumedSourceState)
	}
	if len(snapshot.Entries) != 2 || snapshot.Entries[0].Path != "app/build/generated/sources/kotlin/Generated.kt" || snapshot.Entries[0].Origin != "registered_generated_source_set" || snapshot.Entries[1].Path != "app/src/main/kotlin/Selected.kt" {
		t.Fatalf("selected entries = %+v", snapshot.Entries)
	}
	if len(snapshot.Candidates) != 3 {
		t.Fatalf("candidate set = %+v", snapshot.Candidates)
	}
	var excludedFound bool
	for _, candidate := range snapshot.Candidates {
		if candidate.Path == "app/src/main/kotlin/Excluded.kt" {
			excludedFound = candidate.Selection == "not_selected" && strings.Contains(candidate.Reason, "cause is unknown")
		}
	}
	if !excludedFound {
		t.Fatalf("excluded candidate was not separated from selected files: %+v", snapshot.Candidates)
	}
	encoded, err := snapshot.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSourceSnapshot(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SelectedContentSHA256 != snapshot.SelectedContentSHA256 {
		t.Fatal("round trip changed selected content digest")
	}
}

func TestSourceSnapshotValidatorSeparatesRecordedBytesFromCurrentSelection(t *testing.T) {
	root, artifact, raw, model := sourceSnapshotFixture(t)
	snapshot, err := buildSourceSnapshotContext(context.Background(), root, artifact, raw, model)
	if err != nil {
		t.Fatal(err)
	}
	status, err := ValidateSourceSnapshotCurrentContext(context.Background(), root, artifact, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if status.BuildEvidence != Complete || status.RecordedContent != Complete || status.RecordedMembership != Unknown || status.CurrentTaskSelection != Unknown || status.CompilerConsumed != Unknown {
		t.Fatalf("fresh read-only report = %+v", status)
	}
	selectedPath := filepath.Join(root, "app", "src", "main", "kotlin", "Selected.kt")
	if err := os.WriteFile(selectedPath, []byte("fun selected() = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = ValidateSourceSnapshotCurrentContext(context.Background(), root, artifact, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if status.RecordedContent != Incomplete {
		t.Fatalf("modified selected bytes report = %+v", status)
	}
	if err := os.Remove(selectedPath); err != nil {
		t.Fatal(err)
	}
	status, err = ValidateSourceSnapshotCurrentContext(context.Background(), root, artifact, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if status.RecordedMembership != Incomplete || status.RecordedContent != Incomplete {
		t.Fatalf("removed selected member report = %+v", status)
	}
}

func TestSourceSnapshotExclusionMutationDoesNotChangeSelectedContentDigest(t *testing.T) {
	root, artifact, raw, model := sourceSnapshotFixture(t)
	first, err := buildSourceSnapshotContext(context.Background(), root, artifact, raw, model)
	if err != nil {
		t.Fatal(err)
	}
	excluded := filepath.Join(root, "app", "src", "main", "kotlin", "Excluded.kt")
	if err := os.WriteFile(excluded, []byte("fun excluded() = 99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := buildSourceSnapshotContext(context.Background(), root, artifact, raw, model)
	if err != nil {
		t.Fatal(err)
	}
	if first.SelectedContentSHA256 != second.SelectedContentSHA256 {
		t.Fatal("excluded candidate bytes changed selected source digest")
	}
	if first.CandidateMembershipSHA256 != second.CandidateMembershipSHA256 {
		t.Fatal("candidate content mutation unexpectedly changed candidate membership")
	}
	if first.SnapshotSHA256 != second.SnapshotSHA256 {
		t.Fatal("snapshot self digest should bind only recorded identity/state, not excluded file bytes")
	}
}

func TestSourceSnapshotSelectionDigestChangesOnAddedOrRemovedMember(t *testing.T) {
	root, artifact, raw, model := sourceSnapshotFixture(t)
	first, err := buildSourceSnapshotContext(context.Background(), root, artifact, raw, model)
	if err != nil {
		t.Fatal(err)
	}
	added := filepath.Join(root, "app", "src", "main", "kotlin", "Added.kt")
	if err := os.WriteFile(added, []byte("fun added() = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw.TaskFilesBefore = append(raw.TaskFilesBefore, added)
	raw.TaskFilesAfter = append(raw.TaskFilesAfter, added)
	raw.CandidateFilesBefore = append(raw.CandidateFilesBefore, added)
	raw.CandidateFilesAfter = append(raw.CandidateFilesAfter, added)
	addedSnapshot, err := buildSourceSnapshotContext(context.Background(), root, artifact, raw, model)
	if err != nil {
		t.Fatal(err)
	}
	if first.SelectedContentSHA256 == addedSnapshot.SelectedContentSHA256 {
		t.Fatal("added selected source did not change digest")
	}
	raw.TaskFilesBefore = raw.TaskFilesBefore[:len(raw.TaskFilesBefore)-1]
	raw.TaskFilesAfter = raw.TaskFilesAfter[:len(raw.TaskFilesAfter)-1]
	raw.CandidateFilesBefore = raw.CandidateFilesBefore[:len(raw.CandidateFilesBefore)-1]
	raw.CandidateFilesAfter = raw.CandidateFilesAfter[:len(raw.CandidateFilesAfter)-1]
	removedSnapshot, err := buildSourceSnapshotContext(context.Background(), root, artifact, raw, model)
	if err != nil {
		t.Fatal(err)
	}
	if addedSnapshot.SelectedContentSHA256 == removedSnapshot.SelectedContentSHA256 {
		t.Fatal("removed selected source did not change digest")
	}
}

func TestSourceSnapshotRejectsSymlinksAndAggregateBudgetExhaustion(t *testing.T) {
	root, artifact, raw, model := sourceSnapshotFixture(t)
	external := filepath.Join(t.TempDir(), "Escape.kt")
	if err := os.WriteFile(external, []byte("fun escape() = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "app", "src", "main", "kotlin", "Escape.kt")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	raw.TaskFilesBefore = append(raw.TaskFilesBefore, link)
	raw.TaskFilesAfter = append(raw.TaskFilesAfter, link)
	snapshot, err := buildSourceSnapshotContextWithLimits(context.Background(), root, artifact, raw, model, sourceSnapshotLimits{bytes: 1, files: 10, entries: 10})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RecordedBytesState != Incomplete || snapshot.TaskMembershipState != Incomplete {
		t.Fatalf("unsafe or over-budget source snapshot = %+v", snapshot)
	}
	for _, entry := range snapshot.Entries {
		if entry.Path == "" && entry.State == Incomplete && entry.Reason == "" {
			t.Fatal("incomplete entry has no safe reason")
		}
		if strings.Contains(entry.Path, "Escape.kt") && entry.State == Complete {
			t.Fatal("symlink source was accepted")
		}
	}
}

func TestSourceSnapshotCancellationDuringFingerprinting(t *testing.T) {
	root, artifact, raw, model := sourceSnapshotFixture(t)
	selected := raw.TaskFilesAfter[0]
	if err := os.WriteFile(selected, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	canceling := &cancelDuringHashContext{parent: context.Background(), done: make(chan struct{}), cancelAt: 5}

	_, err := buildSourceSnapshotContext(canceling, root, artifact, raw, model)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-fingerprint cancellation error = %v, want context.Canceled", err)
	}
	if canceling.checks != canceling.cancelAt+1 {
		t.Fatalf("cancellation occurred at context check %d, want second hashing read then caller check (%d)", canceling.checks, canceling.cancelAt+1)
	}
	select {
	case <-canceling.Done():
	default:
		t.Fatal("context returned canceled without closing Done")
	}
}

func TestDecodeSourceSnapshotRefusesTrustPromotion(t *testing.T) {
	root, artifact, raw, model := sourceSnapshotFixture(t)
	snapshot, err := buildSourceSnapshotContext(context.Background(), root, artifact, raw, model)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.CompilerConsumedSourceState = Complete
	data, err := snapshot.Marshal()
	if err == nil {
		t.Fatal("marshal promoted compiler-consumed source state")
	}
	_ = data
}

func TestSourceSnapshotMissingBeforeObservationIsIncomplete(t *testing.T) {
	root, artifact, raw, model := sourceSnapshotFixture(t)
	raw.ObservationMissing = true
	snapshot, err := buildSourceSnapshotContext(context.Background(), root, artifact, raw, model)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TaskMembershipState != Incomplete || snapshot.CandidateState != Incomplete || snapshot.GeneratedSetState != Incomplete {
		t.Fatalf("missing before observation was promoted: %+v", snapshot)
	}
}

func TestSourceSnapshotValidatorPropagatesCancellationWithEmptySelection(t *testing.T) {
	root, artifact, raw, model := sourceSnapshotFixture(t)
	raw.TaskFilesBefore = nil
	raw.TaskFilesAfter = nil
	raw.CandidateFilesBefore = nil
	raw.CandidateFilesAfter = nil
	raw.GeneratedFilesBefore = nil
	raw.GeneratedFilesAfter = nil
	snapshot, err := buildSourceSnapshotContext(context.Background(), root, artifact, raw, model)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ValidateSourceSnapshotCurrentContext(ctx, root, artifact, snapshot); err != context.Canceled {
		t.Fatalf("empty-selection validation error = %v, want context.Canceled", err)
	}
}

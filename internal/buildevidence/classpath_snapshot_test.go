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
)

func TestClasspathSnapshotTracksContentOrderDuplicatesAndFreshness(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "libs"), 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "libs", "same.jar")
	second := filepath.Join(root, "other", "same.jar")
	if err := os.MkdirAll(filepath.Dir(second), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("coordinate bytes one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("coordinate bytes two"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := snapshotTestArtifact(root)
	paths := []rawClasspathEntry{{Path: first, Identity: "file"}, {Path: second, Identity: "module:org.example:dep:1.0"}, {Path: first, Identity: "file"}}
	snapshot, err := buildClasspathSnapshot(root, home, artifact, paths, Complete, []string{"KotlinCompile.libraries FileCollection rendered through Gradle asPath in iteration order"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Entries) != 3 || snapshot.Entries[0].Order != 0 || snapshot.Entries[2].Order != 2 || snapshot.Entries[0].SHA256 != snapshot.Entries[2].SHA256 {
		t.Fatalf("ordered duplicate entries were not retained: %+v", snapshot.Entries)
	}
	if snapshot.Entries[0].Role != "local_file_dependency" || snapshot.Entries[1].Role != "external_jar" || snapshot.Entries[0].SHA256 == snapshot.Entries[1].SHA256 {
		t.Fatalf("same filename or local dependency identity was mishandled: %+v", snapshot.Entries)
	}
	encoded, err := snapshot.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeClasspathSnapshot(encoded)
	if err != nil {
		t.Fatal(err)
	}
	status, err := ValidateClasspathSnapshotCurrent(root, home, artifact, decoded)
	if err != nil || status.RecordedContent != Complete || status.Selection != Unknown || status.CompilerClosure != Unknown {
		t.Fatalf("fresh read-only validation = %+v, err %v", status, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ValidateClasspathSnapshotCurrentContext(canceled, root, home, artifact, decoded); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled validation returned %v, want context.Canceled", err)
	}
	newSelectionCandidate := filepath.Join(home, "caches", "modules-2", "new.jar")
	if err := os.MkdirAll(filepath.Dir(newSelectionCandidate), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newSelectionCandidate, []byte("not in the recorded selection"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = ValidateClasspathSnapshotCurrent(root, home, artifact, decoded)
	if err != nil || status.RecordedContent != Complete || status.Selection != Unknown {
		t.Fatalf("read-only validator overclaimed selection freshness: %+v, err %v", status, err)
	}
	if err := os.WriteFile(first, []byte("same coordinate, changed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = ValidateClasspathSnapshotCurrent(root, home, artifact, decoded)
	if err != nil || status.RecordedContent != Incomplete || !containsString(status.Reasons, "entry 0 content changed") {
		t.Fatalf("same-path content mutation was accepted: %+v, err %v", status, err)
	}
	reordered, err := buildClasspathSnapshot(root, home, artifact, []rawClasspathEntry{paths[1], paths[0], paths[2]}, Complete, []string{"KotlinCompile.libraries FileCollection rendered through Gradle asPath in iteration order"})
	if err != nil {
		t.Fatal(err)
	}
	if reordered.SnapshotSHA256 == decoded.SnapshotSHA256 {
		t.Fatal("classpath order change did not change snapshot identity")
	}
	withoutDuplicate, err := buildClasspathSnapshot(root, home, artifact, paths[:2], Complete, []string{"KotlinCompile.libraries FileCollection rendered through Gradle asPath in iteration order"})
	if err != nil {
		t.Fatal(err)
	}
	if withoutDuplicate.SnapshotSHA256 == reordered.SnapshotSHA256 {
		t.Fatal("duplicate classpath entry removal did not change snapshot identity")
	}
	third := filepath.Join(root, "libs", "added.jar")
	if err := os.WriteFile(third, []byte("added classpath file"), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := buildClasspathSnapshot(root, home, artifact, append(paths[:2], rawClasspathEntry{Path: third}), Complete, []string{"KotlinCompile.libraries FileCollection rendered through Gradle asPath in iteration order"})
	if err != nil {
		t.Fatal(err)
	}
	if added.SnapshotSHA256 == withoutDuplicate.SnapshotSHA256 || len(added.Entries) != 3 {
		t.Fatal("added classpath file did not change membership identity")
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	status, err = ValidateClasspathSnapshotCurrent(root, home, artifact, decoded)
	if err != nil || status.RecordedContent != Incomplete || !containsString(status.Reasons, "entry 1 cannot be revalidated: classpath path is missing") {
		t.Fatalf("deleted classpath file was accepted: %+v, err %v", status, err)
	}
}

func TestClasspathSnapshotBoundsAndCancellation(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	large := filepath.Join(root, "large.jar")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxClasspathFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := buildClasspathSnapshot(root, home, snapshotTestArtifact(root), []rawClasspathEntry{{Path: large}}, Complete, []string{"KotlinCompile.libraries FileCollection rendered through Gradle asPath in iteration order"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ObservationState != Incomplete || snapshot.Entries[0].Reason != "classpath snapshot limit exceeded" {
		t.Fatalf("oversized entry was not explicitly incomplete: %+v", snapshot.Entries[0])
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := buildClasspathSnapshotContext(ctx, root, home, snapshotTestArtifact(root), []rawClasspathEntry{{Path: large}}, Complete, []string{"KotlinCompile.libraries FileCollection rendered through Gradle asPath in iteration order"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot returned %v, want context.Canceled", err)
	}
}

func TestClasspathSnapshotDirectoryContentsAndMembership(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	dir := filepath.Join(root, "project", "build", "classes")
	if err := os.MkdirAll(filepath.Join(dir, "demo"), 0o700); err != nil {
		t.Fatal(err)
	}
	classFile := filepath.Join(dir, "demo", "A.class")
	if err := os.WriteFile(classFile, []byte("class A"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := snapshotTestArtifact(root)
	provenance := []string{"KotlinCompile.libraries FileCollection rendered through Gradle asPath in iteration order"}
	first, err := buildClasspathSnapshot(root, home, artifact, []rawClasspathEntry{{Path: dir, Identity: "project::dep"}}, Complete, provenance)
	if err != nil {
		t.Fatal(err)
	}
	if first.Entries[0].Role != "project_dependency_output" || first.Entries[0].Kind != "directory" {
		t.Fatalf("project output directory classification = %+v", first.Entries[0])
	}
	if err := os.WriteFile(classFile, []byte("class A changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	modified, err := buildClasspathSnapshot(root, home, artifact, []rawClasspathEntry{{Path: dir, Identity: "project::dep"}}, Complete, provenance)
	if err != nil {
		t.Fatal(err)
	}
	if modified.Entries[0].SHA256 == first.Entries[0].SHA256 {
		t.Fatalf("directory file modification did not change content identity: first=%+v modified=%+v", first.Entries[0], modified.Entries[0])
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "B.class"), []byte("class B"), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := buildClasspathSnapshot(root, home, artifact, []rawClasspathEntry{{Path: dir, Identity: "project::dep"}}, Complete, provenance)
	if err != nil {
		t.Fatal(err)
	}
	if added.Entries[0].SHA256 == modified.Entries[0].SHA256 {
		t.Fatal("directory addition did not change content identity")
	}
	if err := os.Remove(classFile); err != nil {
		t.Fatal(err)
	}
	deleted, err := buildClasspathSnapshot(root, home, artifact, []rawClasspathEntry{{Path: dir, Identity: "project::dep"}}, Complete, provenance)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Entries[0].SHA256 == added.Entries[0].SHA256 {
		t.Fatal("directory deletion did not change content identity")
	}
}

func TestClasspathSnapshotFailsClosedForUnsupportedPathsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.jar")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(root, "local.jar")
	if err := os.WriteFile(local, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := snapshotTestArtifact(root)
	link := filepath.Join(root, "escaping.jar")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	missing := filepath.Join(root, "missing.jar")
	snapshot, err := buildClasspathSnapshot(root, home, artifact, []rawClasspathEntry{{Path: link}, {Path: outside}, {Path: missing}}, Complete, []string{"KotlinCompile.libraries FileCollection rendered through Gradle asPath in iteration order"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ObservationState != Incomplete || len(snapshot.Entries) != 3 {
		t.Fatalf("unsupported paths were silently omitted: %+v", snapshot)
	}
	data, err := snapshot.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), outside) || strings.Contains(string(data), root) || strings.Contains(string(data), home) {
		t.Fatal("snapshot leaked an absolute machine path")
	}
	if snapshot.Entries[0].Reason != "classpath symlink is unsupported or escapes an allowed root" || snapshot.Entries[1].Reason != "classpath path is outside allowed roots" || snapshot.Entries[2].Reason == "" {
		t.Fatalf("unsupported path reasons are not explicit: %+v", snapshot.Entries)
	}
	outsideDir := filepath.Dir(outside)
	parentLink := filepath.Join(root, "linked-parent")
	if err := os.Symlink(outsideDir, parentLink); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rootFS.Close()
	if _, _, err := stableFileDigestRootContext(context.Background(), rootFS, "linked-parent/outside.jar", MaxClasspathFileBytes); err == nil {
		t.Fatal("rooted hashing escaped through a parent symlink")
	}
}

func TestClasspathSnapshotDoesNotInferProjectOrFileRoleFromRepositoryPath(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	file := filepath.Join(root, "build", "generated.jar")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("bytes only"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := buildClasspathSnapshot(root, home, snapshotTestArtifact(root), []rawClasspathEntry{{Path: file}}, Complete, []string{"KotlinCompile.libraries FileCollection rendered through Gradle asPath in iteration order"})
	if err != nil {
		t.Fatal(err)
	}
	entry := snapshot.Entries[0]
	if entry.Role != "unknown" || entry.ProvenanceState != Incomplete || entry.State != Complete || snapshot.EntryProvenanceState != Incomplete {
		t.Fatalf("filesystem location was used as dependency provenance: %+v / %+v", entry, snapshot)
	}
}

func TestClasspathSnapshotDirectoryAndArtifactTamperingRejected(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	file := filepath.Join(root, "dep.jar")
	if err := os.WriteFile(file, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := buildClasspathSnapshot(root, home, snapshotTestArtifact(root), []rawClasspathEntry{{Path: file}}, Complete, []string{"KotlinCompile.libraries FileCollection rendered through Gradle asPath in iteration order"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := snapshot.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"unrecognized":"value"}`)...)
	if _, err := DecodeClasspathSnapshot(data); err == nil {
		t.Fatal("unknown sidecar field was accepted")
	}
	snapshot.SnapshotSHA256 = strings.Repeat("0", 64)
	if err := snapshot.Validate(); err == nil {
		t.Fatal("tampered snapshot digest was accepted")
	}
}

func TestEvidenceBundlePreflightsAndReportsPartialSidecarPublication(t *testing.T) {
	t.Run("preflight collision leaves primary absent", func(t *testing.T) {
		root := t.TempDir()
		primary, sidecar := filepath.Join(root, "primary.json"), filepath.Join(root, "snapshot.json")
		if err := os.WriteFile(sidecar, []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}
		called := false
		err := publishEvidenceBundle(primary, []byte("primary"), []evidenceSidecar{{path: sidecar, data: []byte("snapshot"), label: "dependency snapshot"}}, func(path string, data []byte) error {
			called = true
			return writeNewFile(path, data)
		})
		if err == nil || called {
			t.Fatalf("collision error = %v, publisher called=%t", err, called)
		}
		if _, err := os.Lstat(primary); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("primary published despite sidecar collision: %v", err)
		}
	})
	t.Run("later sidecar failure keeps accurately reported primary", func(t *testing.T) {
		root := t.TempDir()
		primary, sidecar := filepath.Join(root, "primary.json"), filepath.Join(root, "snapshot.json")
		failure := errors.New("injected sidecar write failure")
		calls := 0
		err := publishEvidenceBundle(primary, []byte("primary"), []evidenceSidecar{{path: sidecar, data: []byte("snapshot"), label: "dependency snapshot"}}, func(path string, data []byte) error {
			calls++
			if calls == 2 {
				return failure
			}
			return writeNewFile(path, data)
		})
		if !errors.Is(err, failure) || !strings.Contains(err.Error(), "dependency snapshot publication failed") || calls != 2 {
			t.Fatalf("partial publication error=%v calls=%d", err, calls)
		}
		data, readErr := os.ReadFile(primary)
		if readErr != nil || string(data) != "primary" {
			t.Fatalf("primary bytes=%q err=%v", data, readErr)
		}
		if _, err := os.Lstat(sidecar); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed sidecar unexpectedly exists: %v", err)
		}
	})
}

func TestConcurrentEvidenceBundlePublicationDoesNotOverwrite(t *testing.T) {
	root := t.TempDir()
	primary, sidecar := filepath.Join(root, "primary.json"), filepath.Join(root, "snapshot.json")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- publishEvidenceBundle(primary, []byte("primary"), []evidenceSidecar{{path: sidecar, data: []byte("snapshot"), label: "dependency snapshot"}}, writeNewFile)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent bundle successes=%d, want exactly one", successes)
	}
	for _, path := range []string{primary, sidecar} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("published bundle missing %s: %v", filepath.Base(path), err)
		}
	}
}

func snapshotTestArtifact(root string) Artifact {
	root, _ = filepath.Abs(root)
	root, _ = filepath.EvalSymlinks(root)
	identity := sha256.Sum256([]byte(root))
	sha := strings.Repeat("a", 64)
	unknown := Dimension{State: Unknown, Provenance: []string{"test unknown"}}
	incomplete := Dimension{State: Incomplete, Provenance: []string{"test incomplete"}}
	artifact := Artifact{
		Schema:           Schema,
		Producer:         Producer{Name: "codegraph-gradle-exporter", Version: "2"},
		Repository:       Repository{Identity: "sha256:" + hex.EncodeToString(identity[:]), RootMarker: "sha256:" + hex.EncodeToString(identity[:]), BuildRoot: "."},
		Compilation:      Compilation{GradleProject: ":app", KotlinCompilation: "main", GradleTask: ":app:compileKotlin"},
		Tools:            Tools{GradleVersion: "9.3.1", KotlinPluginVersion: "2.3.20", KotlinCompilerVersion: "2.3.20"},
		InputFingerprint: sha,
		Evidence:         Evidence{Dimensions: Dimensions{Source: incomplete, Generated: unknown, Excluded: unknown, Dependency: incomplete, ExternalMetadata: unknown, CompilerIdentity: incomplete}, SourceRoots: []string{}, Dependencies: []Dependency{}},
		Proof: &Proof{
			TaskOutcome: "COMPLETED_NO_WORK", TaskInputSHA256: []string{}, CompilerArtifacts: []FileIdentity{}, OutputFiles: []OutputFile{}, OutputRoots: []string{},
			Dimensions: ProofDimensions{GradleRuntimeJDK: incomplete, TaskExecution: incomplete, CompilerRuntime: unknown, SelectedToolchain: unknown, ABIInputs: incomplete, OutputProvenance: incomplete},
		},
	}
	fingerprint, err := CurrentFingerprint(root, artifact)
	if err != nil {
		panic(err)
	}
	artifact.InputFingerprint = fingerprint
	return artifact
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

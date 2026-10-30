package buildevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCurrentFingerprintChangesWithRepositoryInputs(t *testing.T) {
	root := t.TempDir()
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "app", "src", "main", "kotlin"), 0o755); err != nil {
		t.Fatal(err)
	}
	buildFile := filepath.Join(root, "app", "build.gradle.kts")
	if err := os.WriteFile(buildFile, []byte("plugins { kotlin(\"jvm\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	buildDirInput := filepath.Join(root, "app", "build", "settings.properties")
	gradleDirInput := filepath.Join(root, "app", ".gradle", "settings.properties")
	for _, path := range []string{buildDirInput, gradleDirInput} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("setting=one\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(root, "app", "src", "main", "kotlin", "Hello.kt")
	if err := os.WriteFile(source, []byte("class Hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := "sha256:" + hashString(root)
	a := Artifact{
		Schema:           SchemaV1,
		Producer:         Producer{Name: "test", Version: "1"},
		Repository:       Repository{Identity: identity, RootMarker: identity, BuildRoot: "."},
		Compilation:      Compilation{GradleProject: ":app", KotlinCompilation: "main"},
		InputFingerprint: strings.Repeat("0", 64),
		Evidence: Evidence{
			Dimensions: Dimensions{
				Source:           Dimension{State: Incomplete, Provenance: []string{"test"}},
				Generated:        Dimension{State: Unknown, Provenance: []string{"test"}},
				Excluded:         Dimension{State: Unknown, Provenance: []string{"test"}},
				Dependency:       Dimension{State: Unknown, Provenance: []string{"test"}},
				ExternalMetadata: Dimension{State: Unknown, Provenance: []string{"test"}},
				CompilerIdentity: Dimension{State: Unknown, Provenance: []string{"test"}},
			},
			SourceRoots: []string{"app/src/main/kotlin"},
		},
	}
	fingerprint, err := CurrentFingerprint(root, a)
	if err != nil {
		t.Fatal(err)
	}
	a.InputFingerprint = fingerprint
	if err := a.ValidateCurrentInRepository(root); err != nil {
		t.Fatalf("fresh artifact rejected: %v", err)
	}
	if err := os.WriteFile(buildFile, []byte("plugins { kotlin(\"jvm\") version \"2.3.20\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateCurrentInRepository(root); err == nil {
		t.Fatal("artifact remained current after build input edit")
	}
	current, err := CurrentFingerprint(root, a)
	if err != nil {
		t.Fatal(err)
	}
	a.InputFingerprint = current
	if err := os.WriteFile(buildDirInput, []byte("setting=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateCurrentInRepository(root); err == nil {
		t.Fatal("artifact remained current after nested build-directory input edit")
	}
	current, err = CurrentFingerprint(root, a)
	if err != nil {
		t.Fatal(err)
	}
	a.InputFingerprint = current
	if err := os.WriteFile(gradleDirInput, []byte("setting=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateCurrentInRepository(root); err == nil {
		t.Fatal("artifact remained current after nested .gradle input edit")
	}
	if err := os.WriteFile(source, []byte("class Changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateCurrentInRepository(root); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed source accepted: %v", err)
	}
}

func TestV2FingerprintBindsTaskProofAndOutputBytes(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	outputRel := "app/build/classes/kotlin/main/Hello.class"
	outputPath := filepath.Join(root, filepath.FromSlash(outputRel))
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("class-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputHash := hashString("class-bytes")
	a := testArtifact()
	a.Schema = Schema
	a.Repository.Identity = "sha256:" + hashString(root)
	a.Repository.RootMarker = a.Repository.Identity
	a.Evidence.SourceRoots = []string{}
	a.Evidence.Dimensions = Dimensions{
		Source:           Dimension{State: Incomplete, Provenance: []string{"source scope incomplete"}},
		Generated:        Dimension{State: Unknown, Provenance: []string{"not observed"}},
		Excluded:         Dimension{State: Unknown, Provenance: []string{"not observed"}},
		Dependency:       Dimension{State: Incomplete, Provenance: []string{"dependency content freshness incomplete"}},
		ExternalMetadata: Dimension{State: Unknown, Provenance: []string{"not observed"}},
		CompilerIdentity: Dimension{State: Incomplete, Provenance: []string{"compiler implementation incomplete"}},
	}
	a.Tools = Tools{GradleVersion: "9.3.1", KotlinPluginVersion: "2.3.20", KotlinCompilerVersion: "2.3.20", GradleRuntimeJDKVendor: "Eclipse Adoptium", GradleRuntimeJDKVersion: "17.0.9"}
	a.Compilation.GradleTask = ":app:compileKotlin"
	a.Proof = testProof()
	a.Proof.OutputFiles = []OutputFile{{Path: outputRel, SHA256: outputHash}}
	a.Proof.TaskInputSHA256 = []string{hashString("compiler-input")}
	a.Proof.CompilerArtifacts = []FileIdentity{{Name: "kotlin-compiler-embeddable-2.3.20.jar", SHA256: hashString("compiler-jar")}}
	a.Proof.TaskInputsComplete = true
	a.Proof.OutputsContained = true
	a.InputFingerprint = strings.Repeat("0", 64)
	fingerprint, err := CurrentFingerprint(root, a)
	if err != nil {
		t.Fatal(err)
	}
	a.InputFingerprint = fingerprint
	if err := a.ValidateCurrentInRepository(root); err != nil {
		t.Fatalf("fresh v2 artifact rejected: %v", err)
	}

	changed := a
	proof := *a.Proof
	proof.CompilerArtifacts = append([]FileIdentity(nil), a.Proof.CompilerArtifacts...)
	proof.CompilerArtifacts[0].SHA256 = hashString("changed compiler bytes")
	changed.Proof = &proof
	if err := changed.ValidateCurrentInRepository(root); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed compiler artifact identity accepted: %v", err)
	}

	if err := os.WriteFile(filepath.Join(filepath.Dir(outputPath), "Added.class"), []byte("new-class-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateCurrentInRepository(root); err == nil || !strings.Contains(err.Error(), "unrecorded class output") {
		t.Fatalf("added class output was not detected: %v", err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(outputPath), "Added.class")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, []byte("tampered-class-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateCurrentInRepository(root); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("modified class output accepted: %v", err)
	}
	if err := os.Remove(outputPath); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateCurrentInRepository(root); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("deleted class output accepted: %v", err)
	}
}

func TestRepositoryFingerprintDistinguishesFileBoundaries(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	fileA := filepath.Join(root, "a")
	fileB := filepath.Join(root, "b")
	if err := os.WriteFile(fileA, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := testArtifact()
	a.Repository.Identity = "sha256:" + hashString(root)
	a.Repository.RootMarker = a.Repository.Identity
	a.Evidence.SourceRoots = []string{}
	before, err := CurrentFingerprint(root, a)
	if err != nil {
		t.Fatal(err)
	}
	a.InputFingerprint = before
	if err := os.Remove(fileB); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileA, []byte("x\x00b\x00600\x00y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateCurrentInRepository(root); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("different repository inputs accepted as current: %v", err)
	}
}

func TestV1FingerprintSemanticsIgnoreV2Fields(t *testing.T) {
	model := gradleModel{
		GradleVersion: "8.10", KotlinPluginVersion: "2.0.20", KotlinCompilerVersion: "2.0.20",
		CompileTask: ":app:compileKotlin", JDKVendor: "Temurin", JDKVersion: "21",
		Project: ":app", Compilation: "main", SourceRoots: []string{"app/src/main/kotlin"},
		Dependencies: []string{"org.example:lib:1.0"},
	}
	dimensions := Dimensions{Source: Dimension{State: Incomplete, Provenance: []string{"v1"}}}
	before := hashFingerprint("/repo", model, model.SourceRoots, strings.Repeat("a", 64), dimensions)
	model.TaskOutcome = "TASK_REPORTED_WORK"
	model.TaskInputSHA256 = []string{strings.Repeat("b", 64)}
	model.OutputFiles = []OutputFile{{Path: "app/build/Hello.class", SHA256: strings.Repeat("c", 64)}}
	model.GradleRuntimeJDKVersion = "99"
	after := hashFingerprint("/repo", model, model.SourceRoots, strings.Repeat("a", 64), dimensions)
	if before != after {
		t.Fatal("adding v2 observations changed the legacy v1 fingerprint semantics")
	}
}

func TestCanonicalRepoRootsRefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "source")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := canonicalRepoRoots(root, []string{link}); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escaping source root accepted: %v", err)
	}
	missingThroughLink := filepath.Join(link, "not-created-yet")
	if _, err := canonicalRepoRoots(root, []string{missingThroughLink}); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("missing source root under escaping symlink accepted: %v", err)
	}
}

func TestExportGradleRejectsSymlinkedOutputParentBeforeInvocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinked output-parent test is platform-specific")
	}
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(t.TempDir(), "repo-alias")
	if err := os.Symlink(root, aliasParent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "gradle-invoked")
	gradle := filepath.Join(bin, "gradle")
	if err := os.WriteFile(gradle, []byte("#!/bin/sh\nprintf invoked > \"$CODEGRAPH_GRADLE_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEGRAPH_GRADLE_MARKER", marker)
	_, err = ExportGradle(context.Background(), GradleRequest{
		RepositoryRoot: root,
		GradleCommand:  gradle,
		Project:        ":app",
		Compilation:    "main",
		OutputPath:     filepath.Join(aliasParent, "evidence.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Fatalf("symlinked in-repository output accepted: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Gradle ran before output containment refusal: %v", err)
	}
}

func TestProducerIdentityAllowsKnownValuesAndRejectsFreeFormText(t *testing.T) {
	for _, coordinate := range []string{"org.example:library:1.2.3", "project :shared"} {
		if !safeDependencyCoordinate(coordinate) {
			t.Errorf("safe dependency coordinate rejected: %q", coordinate)
		}
	}
	for _, coordinate := range []string{"https://user:secret@example.test/lib", "org.example:lib:1.0\nsecret"} {
		if safeDependencyCoordinate(coordinate) {
			t.Errorf("free-form dependency coordinate accepted: %q", coordinate)
		}
	}
	for _, identity := range []string{"9.3.1", "Eclipse Adoptium"} {
		if !safeToolIdentity(identity) {
			t.Errorf("safe tool identity rejected: %q", identity)
		}
	}
	if safeToolIdentity("vendor\nsecret") {
		t.Fatal("free-form tool identity accepted")
	}
}

func TestPublishEvidenceArtifactsRejectsExistingPrimaryWithoutPublishingSidecar(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "evidence.json")
	sidecar := primary + ".compiler-probe.json"
	original := []byte("user-owned primary")
	if err := os.WriteFile(primary, original, 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	err := publishEvidenceArtifacts(primary, []byte("new primary"), sidecar, []byte("new sidecar"), func(path string, data []byte) error {
		called = true
		return writeNewFile(path, data)
	})
	if err == nil || !strings.Contains(err.Error(), "primary evidence output is unavailable") {
		t.Fatalf("publish error = %v", err)
	}
	if called {
		t.Fatal("publisher ran despite pre-existing primary output")
	}
	got, err := os.ReadFile(primary)
	if err != nil || string(got) != string(original) {
		t.Fatalf("pre-existing primary changed: data=%q err=%v", got, err)
	}
	if _, err := os.Lstat(sidecar); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar was published or stat failed: %v", err)
	}
}

func TestPublishEvidenceArtifactsRejectsExistingSidecarWithoutPublishingPrimary(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "evidence.json")
	sidecar := primary + ".compiler-probe.json"
	original := []byte("user-owned sidecar")
	if err := os.WriteFile(sidecar, original, 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	err := publishEvidenceArtifacts(primary, []byte("new primary"), sidecar, []byte("new sidecar"), func(path string, data []byte) error {
		called = true
		return writeNewFile(path, data)
	})
	if err == nil || !strings.Contains(err.Error(), "compiler probe sidecar output is unavailable") {
		t.Fatalf("publish error = %v", err)
	}
	if called {
		t.Fatal("publisher ran despite pre-existing sidecar")
	}
	if _, err := os.Lstat(primary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("primary was published or stat failed: %v", err)
	}
	got, err := os.ReadFile(sidecar)
	if err != nil || string(got) != string(original) {
		t.Fatalf("pre-existing sidecar changed: data=%q err=%v", got, err)
	}
}

func TestPublishEvidenceArtifactsDoesNotPublishSidecarWhenPrimaryPublicationFails(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "evidence.json")
	sidecar := primary + ".compiler-probe.json"
	primaryFailure := errors.New("injected primary publication failure")
	calls := 0
	err := publishEvidenceArtifacts(primary, []byte("primary bytes"), sidecar, []byte("sidecar bytes"), func(path string, data []byte) error {
		calls++
		if path == primary {
			return primaryFailure
		}
		return writeNewFile(path, data)
	})
	if !errors.Is(err, primaryFailure) || !strings.Contains(err.Error(), "publish primary evidence artifact") {
		t.Fatalf("primary publication error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("publisher calls = %d, want only primary", calls)
	}
	for _, path := range []string{primary, sidecar} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact unexpectedly exists at %s: %v", path, err)
		}
	}
}

func TestPublishEvidenceArtifactsReportsPrimaryWhenSidecarPublicationFails(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "evidence.json")
	sidecar := primary + ".compiler-probe.json"
	sidecarFailure := errors.New("injected sidecar publication failure")
	calls := 0
	err := publishEvidenceArtifacts(primary, []byte("primary bytes"), sidecar, []byte("sidecar bytes"), func(path string, data []byte) error {
		calls++
		if path == sidecar {
			return sidecarFailure
		}
		return writeNewFile(path, data)
	})
	if !errors.Is(err, sidecarFailure) || !strings.Contains(err.Error(), "primary evidence artifact was published") || !strings.Contains(err.Error(), "remains in place") {
		t.Fatalf("publication error does not describe the partial pair: %v", err)
	}
	if calls != 2 {
		t.Fatalf("publisher calls = %d, want primary then sidecar", calls)
	}
	got, err := os.ReadFile(primary)
	if err != nil || string(got) != "primary bytes" {
		t.Fatalf("published primary = %q, err %v", got, err)
	}
	if _, err := os.Lstat(sidecar); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed sidecar unexpectedly exists: %v", err)
	}
}

func TestExportGradlePinnedFixture(t *testing.T) {
	gradle := os.Getenv("CODEGRAPH_TEST_GRADLE")
	if gradle == "" {
		t.Skip("set CODEGRAPH_TEST_GRADLE to run the pinned Gradle integration fixture")
	}
	if runtime.GOOS == "windows" {
		t.Skip("pinned Gradle fixture currently uses Unix-style paths")
	}
	root := copyGradleFixture(t)
	if _, err := os.Stat(filepath.Join(root, "app", "build")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture output root is not clean before selected compilation: stat error = %v", err)
	}
	output := filepath.Join(filepath.Dir(root), "evidence.json")
	var stderr strings.Builder
	a, err := ExportGradle(context.Background(), GradleRequest{
		RepositoryRoot: root,
		GradleCommand:  gradle,
		Project:        ":app",
		Compilation:    "main",
		OutputPath:     output,
		Stderr:         &stderr,
	})
	if err != nil {
		t.Fatalf("ExportGradle(): %v\nGradle output:\n%s", err, stderr.String())
	}
	if a.Schema != Schema || a.Proof == nil || a.Proof.TaskOutcome != "TASK_REPORTED_WORK" || !a.Proof.TaskDidWork {
		t.Fatalf("selected compile task observation = %+v", a.Proof)
	}
	if len(a.Proof.OutputFiles) == 0 || len(a.Proof.TaskInputSHA256) == 0 || len(a.Proof.CompilerArtifacts) == 0 {
		t.Fatalf("selected task input/output hash evidence is empty: %+v", a.Proof)
	}
	if !a.Proof.TaskInputsComplete || !a.Proof.TaskInputsStable {
		t.Fatalf("clean fixture selected-task inputs were incomplete or changed during compilation: %+v", a.Proof)
	}
	if !a.Proof.OutputRootsClean {
		t.Fatalf("clean fixture outputs were not proven empty before compilation: %+v", a.Proof)
	}
	if a.Tools.GradleRuntimeJDKVersion == "" || a.Tools.JDKVersion != "" {
		t.Fatalf("Gradle runtime JDK was not represented separately: %+v", a.Tools)
	}
	if a.Proof.Dimensions.TaskExecution.State != Incomplete || a.Proof.Dimensions.CompilerRuntime.State != Unknown || a.Proof.Dimensions.SelectedToolchain.State != Unknown || a.Proof.Dimensions.OutputProvenance.State != Incomplete || a.Proof.Dimensions.ABIInputs.State != Incomplete {
		t.Fatalf("producer overstated trust dimensions: %+v", a.Proof.Dimensions)
	}
	if a.Evidence.Dimensions.Source.State != Incomplete || a.Evidence.Dimensions.Dependency.State != Incomplete {
		t.Fatalf("producer overstated dimensions: %+v", a.Evidence.Dimensions)
	}
	if a.Tools.GradleVersion != "9.3.1" || a.Tools.KotlinPluginVersion == "" || a.Tools.KotlinCompilerVersion != "2.3.20" || a.Compilation.GradleTask != ":app:compileKotlin" {
		t.Fatalf("fixture tool identity = %+v; want Gradle 9.3.1 and pinned Kotlin plugin", a.Tools)
	}
	if a.Evidence.Dimensions.CompilerIdentity.State != Incomplete {
		t.Fatalf("compiler identity overstated: %+v", a.Evidence.Dimensions.CompilerIdentity)
	}
	if len(a.Evidence.Dependencies) == 0 {
		t.Fatal("Gradle classpath observations are empty")
	}
	if !contains(a.Evidence.SourceRoots, "app/src/main/kotlin") {
		t.Fatalf("source roots = %v", a.Evidence.SourceRoots)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(data); err != nil {
		t.Fatalf("generated artifact failed validation: %v", err)
	}
	if err := a.ValidateCurrentInRepository(root); err != nil {
		t.Fatalf("fresh generated artifact rejected: %v", err)
	}
	secondOutput := filepath.Join(filepath.Dir(root), "evidence-second.json")
	second, err := ExportGradle(context.Background(), GradleRequest{
		RepositoryRoot: root, GradleCommand: gradle, Project: ":app", Compilation: "main", OutputPath: secondOutput,
	})
	if err != nil {
		t.Fatalf("repeat ExportGradle(): %v", err)
	}
	if second.Proof.TaskOutcome != "UP_TO_DATE" || second.Proof.TaskDidWork || !second.Proof.TaskSkipped || second.Proof.SkipReasonCode != "UP_TO_DATE" {
		t.Fatalf("unchanged compile outcome = %+v, want UP_TO_DATE", second.Proof)
	}
	if second.Proof.Dimensions.OutputProvenance.State != Incomplete {
		t.Fatalf("up-to-date output promoted to trusted provenance: %+v", second.Proof.Dimensions.OutputProvenance)
	}
	if second.Proof.OutputRootsClean {
		t.Fatal("pre-existing outputs were incorrectly reported clean on the repeated invocation")
	}
	changed := a
	changed.Tools.KotlinCompilerVersion = "2.3.19"
	if err := changed.ValidateCurrentInRepository(root); err == nil {
		t.Fatal("mismatched Kotlin compiler version accepted as fresh")
	}
	changed = a
	changed.Compilation.GradleTask = ":app:compileTestKotlin"
	if err := changed.ValidateCurrentInRepository(root); err == nil {
		t.Fatal("mismatched Kotlin compile task accepted as fresh")
	}
	if err := os.WriteFile(filepath.Join(root, "app", "src", "main", "kotlin", "Hello.kt"), []byte("class Changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateCurrentInRepository(root); err == nil {
		t.Fatal("artifact remained current after source edit")
	}
}

func TestExportGradlePinnedSourceSnapshot(t *testing.T) {
	gradle := os.Getenv("CODEGRAPH_TEST_GRADLE")
	if gradle == "" {
		t.Skip("set CODEGRAPH_TEST_GRADLE to run the pinned Gradle source snapshot fixture")
	}
	if runtime.GOOS == "windows" {
		t.Skip("pinned Gradle fixture currently uses Unix-style paths")
	}
	root := copyGradleFixture(t)
	write := func(rel, content string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("app/src/main/kotlin/Included.kt", "package fixture\nfun included() = 1\n")
	write("app/src/main/kotlin/Excluded.kt", "package fixture\nfun excluded() = 1\n")
	write("app/src/main/kotlin/PredicateExcluded.kt", "package fixture\nfun predicateExcluded() = 1\n")
	write("app/src/main/kotlin/OutsideCandidate.kt", "package fixture\nfun outsideCandidate() = 1\n")
	write("app/src/unselected/kotlin/Unselected.kt", "package fixture\nfun unselected() = 1\n")
	customBuild := []byte(`import org.jetbrains.kotlin.gradle.ExperimentalKotlinGradlePluginApi
plugins { kotlin("jvm") }
dependencies { implementation(kotlin("stdlib")) }
val generator = tasks.register("generateFixtureKotlin") {
    val out = layout.buildDirectory.dir("generated/sources/custom")
    outputs.dir(out)
    doLast { out.get().file("GeneratedA.kt").asFile.apply { parentFile.mkdirs(); writeText("package fixture.generated\nfun generatedA() = 1\n") } }
}
@OptIn(ExperimentalKotlinGradlePluginApi::class)
kotlin.sourceSets.named("main") { generatedKotlin.srcDir(generator) }
tasks.named<org.jetbrains.kotlin.gradle.tasks.KotlinCompile>("compileKotlin") {
    exclude("**/Excluded.kt")
    exclude { element -> element.file.name == "PredicateExcluded.kt" }
}`)
	if err := os.WriteFile(filepath.Join(root, "app", "build.gradle.kts"), customBuild, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(root), "source-evidence.json")
	var stderr strings.Builder
	a, err := ExportGradle(context.Background(), GradleRequest{RepositoryRoot: root, GradleCommand: gradle, Project: ":app", Compilation: "main", OutputPath: output, SourceSnapshot: true, Stderr: &stderr})
	if err != nil {
		t.Fatalf("ExportGradle(): %v\\nGradle output:\\n%s", err, stderr.String())
	}
	data, err := os.ReadFile(output + ".sources.json")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := DecodeSourceSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TaskMembershipState != Complete || snapshot.RecordedBytesState != Complete || snapshot.GeneratedOwnershipState != Unknown || snapshot.ExclusionCoverageState != Incomplete || snapshot.CompilerConsumedSourceState != Unknown {
		t.Fatalf("source trust dimensions = %+v", snapshot)
	}
	selected := map[string]bool{}
	for _, entry := range snapshot.Entries {
		selected[entry.Path] = true
	}
	if !selected["app/src/main/kotlin/Included.kt"] || !selected["app/build/generated/sources/custom/GeneratedA.kt"] || selected["app/src/main/kotlin/Excluded.kt"] || selected["app/src/main/kotlin/PredicateExcluded.kt"] || !selected["app/src/main/kotlin/OutsideCandidate.kt"] || selected["app/src/unselected/kotlin/Unselected.kt"] || selected["app/build/generated/unregistered/Unregistered.kt"] {
		t.Fatalf("selected task Kotlin sources = %v", selected)
	}
	if a.Tools.KotlinPluginVersion != SupportedSourceSnapshotKGP {
		t.Fatalf("KGP=%q", a.Tools.KotlinPluginVersion)
	}
	status, err := ValidateSourceSnapshotCurrentContext(context.Background(), root, a, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if status.BuildEvidence != Incomplete || status.RecordedContent != Complete || status.RecordedMembership != Unknown || status.CurrentTaskSelection != Unknown {
		t.Fatalf("fresh source validation = %+v", status)
	}
	generatedPath := filepath.Join(root, "app", "build", "generated", "sources", "custom", "GeneratedA.kt")
	if err := os.WriteFile(generatedPath, []byte("package fixture.generated\nfun generatedA() = 55\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = ValidateSourceSnapshotCurrentContext(context.Background(), root, a, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if status.RecordedContent != Incomplete {
		t.Fatalf("mutated generated bytes remained fresh: %+v", status)
	}
}

func copyGradleFixture(t *testing.T) string {
	t.Helper()
	base := filepath.Join("testdata", "gradle-kotlin-jvm")
	root := filepath.Join(t.TempDir(), "repo")
	err := filepath.WalkDir(base, func(source string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, source)
		if err != nil {
			return err
		}
		target := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

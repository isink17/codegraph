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
		Schema:           Schema,
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

func TestExportGradlePinnedFixture(t *testing.T) {
	gradle := os.Getenv("CODEGRAPH_TEST_GRADLE")
	if gradle == "" {
		t.Skip("set CODEGRAPH_TEST_GRADLE to run the pinned Gradle integration fixture")
	}
	if runtime.GOOS == "windows" {
		t.Skip("pinned Gradle fixture currently uses Unix-style paths")
	}
	root := copyGradleFixture(t)
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
	if a.Evidence.Dimensions.Source.State != Incomplete || a.Evidence.Dimensions.Dependency.State != Incomplete {
		t.Fatalf("producer overstated dimensions: %+v", a.Evidence.Dimensions)
	}
	if a.Tools.GradleVersion != "9.3.1" || a.Tools.KotlinPluginVersion == "" {
		t.Fatalf("fixture tool identity = %+v; want Gradle 9.3.1 and pinned Kotlin plugin", a.Tools)
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
	if err := os.WriteFile(filepath.Join(root, "app", "src", "main", "kotlin", "Hello.kt"), []byte("class Changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ValidateCurrentInRepository(root); err == nil {
		t.Fatal("artifact remained current after source edit")
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

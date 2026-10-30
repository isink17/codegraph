package buildevidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type mutationPhase struct {
	name           string
	artifact       Artifact
	snapshot       ClasspathSnapshot
	artifactSHA256 string
	log            string
}

func TestExportGradleSameCoordinateClasspathMutation(t *testing.T) {
	gradle := os.Getenv("CODEGRAPH_TEST_GRADLE")
	if gradle == "" {
		t.Skip("set CODEGRAPH_TEST_GRADLE to run the pinned Gradle dependency mutation experiment")
	}
	if runtime.GOOS == "windows" {
		t.Skip("pinned Gradle mutation fixture currently uses Unix-style paths")
	}
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac is required to build the controlled JVM dependency")
	}
	jar, err := exec.LookPath("jar")
	if err != nil {
		t.Skip("jar is required to build the controlled JVM dependency")
	}
	root := copyGradleFixture(t)
	buildControlledDependency(t, root, javac, jar, "first-bytes")
	dependencyJar := filepath.Join(root, "repo", "dependency-1.0.jar")
	original, err := os.ReadFile(dependencyJar)
	if err != nil {
		t.Fatal(err)
	}
	if err := installControlledDependency(t, root); err != nil {
		t.Fatal(err)
	}
	gradleHome := gradleUserHome()
	outputs := t.TempDir()
	commands := []string{
		"CODEGRAPH_TEST_GRADLE=" + gradle + " go test ./internal/buildevidence -run TestExportGradleSameCoordinateClasspathMutation -count=1",
		"export-gradle-evidence: explicit Gradle 9.3.1, --project :app --compilation main --dependency-snapshot (exporter adds --no-daemon --no-build-cache and selected-task evidence init script)",
	}
	run := func(name string) mutationPhase {
		t.Helper()
		output := filepath.Join(outputs, name+".json")
		var log bytes.Buffer
		artifact, err := ExportGradle(t.Context(), GradleRequest{
			RepositoryRoot: root, GradleCommand: gradle, Project: ":app", Compilation: "main", OutputPath: output,
			DependencySnapshot: true, Stderr: &log,
		})
		if err != nil {
			t.Fatalf("ExportGradle(%s): %v\n%s", name, err, log.String())
		}
		data, err := os.ReadFile(output + ".classpath.json")
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := DecodeClasspathSnapshot(data)
		if err != nil {
			t.Fatalf("DecodeClasspathSnapshot(%s): %v", name, err)
		}
		return mutationPhase{name: name, artifact: artifact, snapshot: snapshot, artifactSHA256: fileSHA256(dependencyJar), log: log.String()}
	}
	first := run("first")
	if first.artifact.Tools.GradleVersion != "9.3.1" || first.artifact.Tools.KotlinCompilerVersion != "2.3.20" || first.artifact.Compilation.GradleTask != ":app:compileKotlin" {
		t.Fatalf("pinned compilation identity mismatch: %+v %+v", first.artifact.Tools, first.artifact.Compilation)
	}
	const coordinate = "com.example:dependency:1.0"
	if !evidenceHasCoordinate(first.artifact.Evidence.Dependencies, coordinate) {
		t.Fatalf("local dependency coordinate %q not observed: %v", coordinate, first.artifact.Evidence.Dependencies)
	}
	localFile, foundLocalFile := classpathEntryForPath(first.snapshot, "repo/local-file.jar")
	if !foundLocalFile || localFile.Role != "unknown" || localFile.Identity != "" || localFile.State != Complete || localFile.ProvenanceState != Incomplete {
		t.Fatalf("Gradle files(...) dependency was not content-hashed with unknown provenance: %+v", first.snapshot.Entries)
	}
	if first.artifact.Proof == nil || first.artifact.Proof.TaskOutcome != "TASK_REPORTED_WORK" {
		t.Fatalf("initial selected compile task did not report work: %+v", first.artifact.Proof)
	}
	firstValidation, err := ValidateClasspathSnapshotCurrent(root, gradleHome, first.artifact, first.snapshot)
	if err != nil || firstValidation.BuildEvidence != Complete || firstValidation.RecordedContent != Complete {
		t.Fatalf("initial known-good snapshot rejected: %+v, err %v", firstValidation, err)
	}
	secondBytes := buildControlledDependency(t, root, javac, jar, "second-bytes")
	if err := os.WriteFile(dependencyJar, secondBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	mutatedHash := fileSHA256(dependencyJar)
	if mutatedHash == first.artifactSHA256 {
		t.Fatal("controlled dependency mutation did not change artifact bytes")
	}
	staleFirst, err := ValidateClasspathSnapshotCurrent(root, gradleHome, first.artifact, first.snapshot)
	if err != nil || staleFirst.RecordedContent != Incomplete {
		t.Fatalf("original evidence accepted after same-coordinate byte mutation: %+v, err %v", staleFirst, err)
	}
	second := run("second")
	if second.artifact.Proof == nil || (second.artifact.Proof.TaskOutcome != "TASK_REPORTED_WORK" && second.artifact.Proof.TaskOutcome != "UP_TO_DATE") {
		t.Fatalf("post-mutation selected compile task had an unsupported outcome: %+v", second.artifact.Proof)
	}
	if !evidenceHasCoordinate(second.artifact.Evidence.Dependencies, coordinate) || dependencyCoordinates(first.artifact.Evidence.Dependencies) != dependencyCoordinates(second.artifact.Evidence.Dependencies) {
		t.Fatalf("dependency coordinates changed across byte substitution: before=%v after=%v", first.artifact.Evidence.Dependencies, second.artifact.Evidence.Dependencies)
	}
	firstDependency := controlledDependencyEntry(t, first.snapshot)
	secondDependency := controlledDependencyEntry(t, second.snapshot)
	if firstDependency.SHA256 == secondDependency.SHA256 {
		t.Fatalf("same-coordinate changed JAR content identity stayed constant: %s", firstDependency.SHA256)
	}
	secondValidation, err := ValidateClasspathSnapshotCurrent(root, gradleHome, second.artifact, second.snapshot)
	if err != nil || secondValidation.BuildEvidence != Complete || secondValidation.RecordedContent != Complete {
		t.Fatalf("fresh post-mutation snapshot rejected: %+v, err %v", secondValidation, err)
	}
	if err := os.WriteFile(dependencyJar, original, 0o600); err != nil {
		t.Fatal(err)
	}
	restoredHash := fileSHA256(dependencyJar)
	if restoredHash != first.artifactSHA256 {
		t.Fatalf("restored artifact hash = %s, want original %s", restoredHash, first.artifactSHA256)
	}
	staleSecond, err := ValidateClasspathSnapshotCurrent(root, gradleHome, second.artifact, second.snapshot)
	if err != nil || staleSecond.RecordedContent != Incomplete {
		t.Fatalf("post-mutation evidence accepted after original bytes restored: %+v, err %v", staleSecond, err)
	}
	writeMutationProof(t, first, second, firstValidation, staleFirst, secondValidation, staleSecond, coordinate, first.artifactSHA256, mutatedHash, restoredHash, commands)
}

func installControlledDependency(t *testing.T, root string) error {
	t.Helper()
	moduleArtifact := filepath.Join(root, "repo", "dependency-1.0.jar")
	moduleBytes, err := os.ReadFile(moduleArtifact)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "repo", "local-file.jar"), moduleBytes, 0o600); err != nil {
		return err
	}
	settingsPath := filepath.Join(root, "settings.gradle.kts")
	settings, err := os.ReadFile(settingsPath)
	if err != nil {
		return err
	}
	settingsText := strings.Replace(string(settings), "dependencyResolutionManagement {\n    repositories {\n        mavenCentral()", "dependencyResolutionManagement {\n    repositories {\n        mavenCentral()\n        flatDir { dirs(file(\"repo\")) }", 1)
	if settingsText == string(settings) {
		return fmt.Errorf("could not add the local artifact repository to the disposable settings fixture")
	}
	if err := os.WriteFile(settingsPath, []byte(settingsText), 0o600); err != nil {
		return err
	}
	appBuild := filepath.Join(root, "app", "build.gradle.kts")
	build, err := os.ReadFile(appBuild)
	if err != nil {
		return err
	}
	build = append(build, []byte("\ndependencies { implementation(\"com.example:dependency:1.0\"); implementation(files(rootProject.file(\"repo/local-file.jar\"))) }\n")...)
	if err := os.WriteFile(appBuild, build, 0o600); err != nil {
		return err
	}
	hello := filepath.Join(root, "app", "src", "main", "kotlin", "Hello.kt")
	return os.WriteFile(hello, []byte("class Hello { fun dependencyValue(): String = demo.Dependency.value() }\n"), 0o600)
}

func classpathEntryForPath(snapshot ClasspathSnapshot, path string) (ClasspathEntry, bool) {
	for _, entry := range snapshot.Entries {
		if entry.Path == path {
			return entry, true
		}
	}
	return ClasspathEntry{}, false
}

func buildControlledDependency(t *testing.T, root, javac, jar, marker string) []byte {
	t.Helper()
	work := filepath.Join(t.TempDir(), "dependency")
	source := filepath.Join(work, "src", "demo", "Dependency.java")
	classes := filepath.Join(work, "classes")
	resources := filepath.Join(work, "resources")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(classes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resources, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package demo; public final class Dependency { public static String value() { return \"ok\"; } }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resources, "marker.txt"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	runTool := func(binary string, args ...string) {
		t.Helper()
		command := exec.Command(binary, args...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", binary, strings.Join(args, " "), err, output)
		}
	}
	runTool(javac, "-d", classes, source)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(repo, "dependency-1.0.jar")
	runTool(jar, "--create", "--file", artifact, "-C", classes, ".", "-C", resources, "marker.txt")
	data, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fileSHA256(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func writeMutationProof(t *testing.T, first, second mutationPhase, firstStatus, staleFirst, secondStatus, staleSecond SnapshotFreshness, coordinate, firstHash, mutatedHash, restoredHash string, commands []string) {
	t.Helper()
	path := os.Getenv("CODEGRAPH_CG35_EVIDENCE_DIR")
	if path == "" {
		return
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	type record struct {
		PinnedToolchain  string                       `json:"pinned_toolchain"`
		SelectedTask     string                       `json:"selected_task"`
		Coordinate       string                       `json:"unchanged_coordinate"`
		ArtifactSHA256   map[string]string            `json:"artifact_sha256"`
		ClasspathSHA256  map[string]string            `json:"classpath_entry_sha256"`
		Entries          []ClasspathEntry             `json:"mutated_classpath_entries"`
		Validation       map[string]SnapshotFreshness `json:"validation"`
		ClasspathEntries int                          `json:"classpath_entries_after_mutation"`
		CompileOutcome   map[string]string            `json:"compile_task_outcome"`
		Commands         []string                     `json:"commands"`
	}
	proof := record{
		PinnedToolchain:  "Gradle 9.3.1 / Kotlin Gradle Plugin and compiler 2.3.20",
		SelectedTask:     first.artifact.Compilation.GradleTask,
		Coordinate:       coordinate,
		ArtifactSHA256:   map[string]string{"initial": firstHash, "mutated": mutatedHash, "restored": restoredHash},
		ClasspathSHA256:  map[string]string{"initial": controlledDependencyEntry(t, first.snapshot).SHA256, "mutated": controlledDependencyEntry(t, second.snapshot).SHA256},
		Entries:          second.snapshot.Entries,
		Validation:       map[string]SnapshotFreshness{"initial": firstStatus, "initial_after_mutation": staleFirst, "mutated": secondStatus, "mutated_after_restore": staleSecond},
		ClasspathEntries: len(second.snapshot.Entries), Commands: commands,
		CompileOutcome: map[string]string{"initial": first.artifact.Proof.TaskOutcome, "after_byte_mutation": second.artifact.Proof.TaskOutcome},
	}
	data, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "same-coordinate-mutation.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ name, value string }{{"initial.log", summarizeGradleLog(first.log)}, {"mutated.log", summarizeGradleLog(second.log)}} {
		if err := os.WriteFile(filepath.Join(path, item.name), []byte(item.value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func summarizeGradleLog(value string) string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if strings.HasPrefix(line, "> Task :app:compileKotlin") || strings.HasPrefix(line, "> Task :codegraphExportEvidence") || strings.HasPrefix(line, "BUILD SUCCESSFUL") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func controlledDependencyEntry(t *testing.T, snapshot ClasspathSnapshot) ClasspathEntry {
	t.Helper()
	for _, entry := range snapshot.Entries {
		if strings.HasSuffix(entry.Path, "repo/dependency-1.0.jar") {
			return entry
		}
	}
	t.Fatalf("controlled dependency JAR missing from selected task classpath: %+v", snapshot.Entries)
	return ClasspathEntry{}
}

func evidenceHasCoordinate(dependencies []Dependency, want string) bool {
	for _, dependency := range dependencies {
		if dependency.Coordinate == want {
			return true
		}
	}
	return false
}

func dependencyCoordinates(dependencies []Dependency) string {
	var values []string
	for _, dependency := range dependencies {
		values = append(values, dependency.Coordinate)
	}
	return strings.Join(values, "\x00")
}

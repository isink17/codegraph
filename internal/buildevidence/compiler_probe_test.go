package buildevidence

import (
	"archive/zip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestExportGradleCompilerProbePinnedFixture(t *testing.T) {
	gradle := os.Getenv("CODEGRAPH_TEST_GRADLE")
	probeJar := os.Getenv("CODEGRAPH_TEST_COMPILER_PROBE_JAR")
	if gradle == "" || probeJar == "" {
		t.Skip("set CODEGRAPH_TEST_GRADLE and CODEGRAPH_TEST_COMPILER_PROBE_JAR for the pinned compiler probe integration")
	}
	if runtime.GOOS == "windows" {
		t.Skip("pinned Gradle fixture uses Unix-style paths")
	}
	baseline, err := ExportGradle(context.Background(), GradleRequest{
		RepositoryRoot: copyGradleFixture(t), GradleCommand: gradle, Project: ":app", Compilation: "main",
		OutputPath: filepath.Join(t.TempDir(), "baseline.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var baselineFiles []OutputFile
	if baseline.Proof == nil {
		t.Fatal("baseline v2 artifact omitted proof")
	}
	baselineFiles = baseline.Proof.OutputFiles

	tokens := map[string]bool{}
	for _, strategy := range []struct{ name, relation string }{{"daemon", "SEPARATE_PROCESS"}, {"in-process", "SAME_PROCESS"}} {
		t.Run(strategy.name, func(t *testing.T) {
			root := copyGradleFixture(t)
			output := filepath.Join(t.TempDir(), "evidence.json")
			artifact, err := ExportGradle(context.Background(), GradleRequest{
				RepositoryRoot: root, GradleCommand: gradle, Project: ":app", Compilation: "main",
				OutputPath: output, CompilerProbeJar: probeJar, CompilerExecutionStrategy: strategy.name,
			})
			if err != nil {
				t.Fatal(err)
			}
			data, err := readBoundedFile(output+".compiler-probe.json", MaxCompilerProbeRecord)
			if err != nil {
				t.Fatal(err)
			}
			var observation CompilerProbe
			if err := json.Unmarshal(data, &observation); err != nil {
				t.Fatal(err)
			}
			if observation.Status != CompilerProbeStatus || !observation.CallbackExecuted {
				t.Fatalf("probe status = %+v", observation)
			}
			if observation.RequestedExecutionStrategy != strategy.name || observation.ProcessRelationship != strategy.relation {
				t.Fatalf("process/strategy observation = %+v", observation)
			}
			if observation.InvocationToken == "" || tokens[observation.InvocationToken] {
				t.Fatalf("missing or reused invocation token %q", observation.InvocationToken)
			}
			tokens[observation.InvocationToken] = true
			if observation.Project != ":app" || observation.Compilation != "main" || observation.Task != ":app:compileKotlin" || observation.EvidenceFingerprint != artifact.InputFingerprint {
				t.Fatalf("probe correlation mismatch: %+v", observation)
			}
			if observation.CompilerCodeSourceSHA256 == "" || observation.CompilerClassSHA256 == "" || observation.CompilerClass != "org.jetbrains.kotlin.cli.jvm.K2JVMCompiler" {
				t.Fatalf("compiler identity fields incomplete: %+v", observation)
			}
			if artifact.Evidence.Dimensions.CompilerIdentity.State != Incomplete || artifact.Proof.Dimensions.CompilerRuntime.State != Unknown {
				t.Fatalf("probe upgraded v2 trust dimensions: %+v", artifact.Evidence.Dimensions)
			}
			if !reflect.DeepEqual(normalizedOutputFiles(artifact.Proof.OutputFiles), normalizedOutputFiles(baselineFiles)) {
				t.Fatalf("probe changed class output inventory/bytes: probe=%v baseline=%v", artifact.Proof.OutputFiles, baselineFiles)
			}
			if !reflect.DeepEqual(artifact.Proof.OutputRoots, baseline.Proof.OutputRoots) {
				t.Fatalf("probe changed declared output roots: probe=%v baseline=%v", artifact.Proof.OutputRoots, baseline.Proof.OutputRoots)
			}
		})
	}
}

func normalizedOutputFiles(in []OutputFile) []OutputFile {
	out := append([]OutputFile(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func TestCompilerProbeFailClosedMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		change     func(*compilerProbeRecord, *gradleModel)
		wantStatus string
		wantErr    bool
	}{
		{name: "callback accepted", wantStatus: CompilerProbeStatus},
		{name: "wrong token", change: func(r *compilerProbeRecord, _ *gradleModel) { r.InvocationToken = strings.Repeat("0", 64) }, wantErr: true},
		{name: "wrong project", change: func(r *compilerProbeRecord, _ *gradleModel) { r.Project = ":other" }, wantErr: true},
		{name: "wrong compilation", change: func(r *compilerProbeRecord, _ *gradleModel) { r.Compilation = "test" }, wantErr: true},
		{name: "wrong task", change: func(r *compilerProbeRecord, _ *gradleModel) { r.Task = ":other:compileKotlin" }, wantErr: true},
		{name: "wrong protocol", change: func(r *compilerProbeRecord, _ *gradleModel) { r.ProtocolVersion++ }, wantErr: true},
		{name: "wrong execution strategy", change: func(r *compilerProbeRecord, _ *gradleModel) { r.RequestedExecutionStrategy = "in-process" }, wantErr: true},
		{name: "wrong probe artifact", change: func(r *compilerProbeRecord, _ *gradleModel) { r.ProbeJARSHA256 = strings.Repeat("f", 64) }, wantErr: true},
		{name: "wrong probe version", change: func(r *compilerProbeRecord, _ *gradleModel) { r.ProbeVersion = "9.9" }, wantErr: true},
		{name: "callback absent", change: func(r *compilerProbeRecord, _ *gradleModel) { r.CallbackExecuted = false }, wantErr: true},
		{name: "unexpected compiler class", change: func(r *compilerProbeRecord, _ *gradleModel) { r.CompilerClass = "unexpected.Compiler" }, wantStatus: "UNKNOWN"},
		{name: "code source unavailable", change: func(r *compilerProbeRecord, _ *gradleModel) { r.CompilerCodeSourceSHA256 = "" }, wantStatus: "UNKNOWN"},
		{name: "class resource unavailable", change: func(r *compilerProbeRecord, _ *gradleModel) { r.CompilerClassSHA256 = "" }, wantStatus: "UNKNOWN"},
		{name: "KGP outside compatibility boundary", change: func(_ *compilerProbeRecord, m *gradleModel) { m.KotlinPluginVersion = "2.3.21" }, wantStatus: "UNKNOWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record, invocation, model, artifact := validProbeFixture(t)
			if tc.change != nil {
				tc.change(&record, &model)
			}
			writeProbeFixture(t, invocation.recordPath, record)
			got, err := readCompilerProbe(invocation, model, artifact)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantStatus)
			}
		})
	}
}

func TestCompilerProbeMissingSkippedAndMalformedRecords(t *testing.T) {
	t.Run("missing callback", func(t *testing.T) {
		_, invocation, model, artifact := validProbeFixture(t)
		got, err := readCompilerProbe(invocation, model, artifact)
		if err != nil || got.Status != "UNKNOWN" || got.ReasonCode != "CALLBACK_MISSING" || got.CallbackExecuted {
			t.Fatalf("got %+v, err %v", got, err)
		}
	})
	t.Run("up to date never reuses observation", func(t *testing.T) {
		record, invocation, model, artifact := validProbeFixture(t)
		writeProbeFixture(t, invocation.recordPath, record)
		model.TaskOutcome, model.TaskSkipped = "UP_TO_DATE", true
		if err := os.Remove(invocation.recordPath); err != nil {
			t.Fatal(err)
		}
		got, err := readCompilerProbe(invocation, model, artifact)
		if err != nil || got.Status != "UNKNOWN" || got.ReasonCode != "TASK_SKIPPED_NO_FRESH_CALLBACK" {
			t.Fatalf("got %+v, err %v", got, err)
		}
	})
	for _, outcome := range []string{"FROM_CACHE", "NO_SOURCE", "SKIPPED", "COMPLETED_NO_WORK"} {
		t.Run(outcome+" has no fresh callback", func(t *testing.T) {
			_, invocation, model, artifact := validProbeFixture(t)
			model.TaskOutcome = outcome
			model.TaskSkipped = outcome != "COMPLETED_NO_WORK"
			got, err := readCompilerProbe(invocation, model, artifact)
			if err != nil || got.Status != "UNKNOWN" || got.CallbackExecuted {
				t.Fatalf("got %+v, err %v", got, err)
			}
		})
	}
	for _, tc := range []struct{ name, data string }{
		{"malformed JSON", "{"},
		{"duplicate record", `{"schema":"x","schema":"y"}`},
		{"trailing JSON", `{} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, invocation, model, artifact := validProbeFixture(t)
			if err := os.WriteFile(invocation.recordPath, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readCompilerProbe(invocation, model, artifact); err == nil {
				t.Fatal("expected malformed record rejection")
			}
		})
	}
}

func TestCompilerProbeUnexpectedProcessRelationshipStaysUnknown(t *testing.T) {
	record, invocation, model, artifact := validProbeFixture(t)
	invocation.strategy = "daemon"
	record.RequestedExecutionStrategy = "daemon"
	model.GradleProcessID = record.ProcessID
	writeProbeFixture(t, invocation.recordPath, record)
	got, err := readCompilerProbe(invocation, model, artifact)
	if err != nil || got.Status != "UNKNOWN" || got.ReasonCode != "EXECUTION_STRATEGY_PROCESS_RELATIONSHIP_MISMATCH" {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestCompilerProbeConcurrentInvocationsHaveDistinctTokens(t *testing.T) {
	root := t.TempDir()
	jar := filepath.Join(root, "probe.jar")
	writeValidProbeJar(t, jar)
	first, err := prepareCompilerProbe(filepath.Join(root, "repo"), jar, filepath.Join(root, "out.json"), ":app", "main", "daemon")
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareCompilerProbe(filepath.Join(root, "repo"), jar, filepath.Join(root, "other.json"), ":app", "main", "in-process")
	if err != nil {
		t.Fatal(err)
	}
	if first.token == second.token || !probeTokenPattern.MatchString(first.token) || !probeTokenPattern.MatchString(second.token) {
		t.Fatal("probe tokens were not unique 256-bit hex values")
	}
}

func TestPrepareCompilerProbeChecksPinnedJarManifest(t *testing.T) {
	root := t.TempDir()
	jar := filepath.Join(t.TempDir(), "wrong-probe.jar")
	writeProbeJarManifest(t, jar, "1.0.0", "1", "2.3.21")
	if _, err := prepareCompilerProbe(root, jar, filepath.Join(t.TempDir(), "evidence.json"), ":app", "main", ""); err == nil {
		t.Fatal("probe built for a different Kotlin compiler was accepted")
	}
	textJar := filepath.Join(t.TempDir(), "not-a-jar.jar")
	if err := os.WriteFile(textJar, []byte("not a jar"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareCompilerProbe(root, textJar, filepath.Join(t.TempDir(), "evidence.json"), ":app", "main", ""); err == nil {
		t.Fatal("non-JAR compiler probe accepted")
	}
}

func TestExportCompilerProbeRejectsUnsupportedStrategyBeforeGradle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Gradle command uses a Unix shell")
	}
	root, jarDir, outputDir := t.TempDir(), t.TempDir(), t.TempDir()
	jar := filepath.Join(jarDir, "probe.jar")
	writeValidProbeJar(t, jar)
	marker := filepath.Join(t.TempDir(), "gradle-ran")
	gradle := filepath.Join(t.TempDir(), "gradle")
	if err := os.WriteFile(gradle, []byte("#!/bin/sh\nprintf ran > \"$CODEGRAPH_GRADLE_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEGRAPH_GRADLE_MARKER", marker)
	_, err := ExportGradle(context.Background(), GradleRequest{
		RepositoryRoot: root, GradleCommand: gradle, Project: ":app", Compilation: "main",
		OutputPath: filepath.Join(outputDir, "evidence.json"), CompilerProbeJar: jar,
		CompilerExecutionStrategy: "out-of-process",
	})
	if err == nil {
		t.Fatal("unsupported strategy accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Gradle ran before unsupported strategy rejection: %v", err)
	}
}

func TestExportCompilerProbeDoesNotWriteEvidenceAfterGradleFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Gradle command uses a Unix shell")
	}
	root, jarDir, outputDir := t.TempDir(), t.TempDir(), t.TempDir()
	jar := filepath.Join(jarDir, "probe.jar")
	writeValidProbeJar(t, jar)
	gradle := filepath.Join(t.TempDir(), "gradle")
	if err := os.WriteFile(gradle, []byte("#!/bin/sh\nexit 17\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(outputDir, "evidence.json")
	_, err := ExportGradle(context.Background(), GradleRequest{
		RepositoryRoot: root, GradleCommand: gradle, Project: ":app", Compilation: "main",
		OutputPath: output, CompilerProbeJar: jar,
	})
	if err == nil {
		t.Fatal("Gradle failure accepted")
	}
	for _, path := range []string{output, output + ".compiler-probe.json"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("failure left a success artifact at %s: %v", path, err)
		}
	}
}

func writeValidProbeJar(t *testing.T, path string) {
	t.Helper()
	writeProbeJarManifest(t, path, "1.0.0", "1", "2.3.20")
}

func writeProbeJarManifest(t *testing.T, path, version, protocol, kotlinVersion string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	manifest, err := archive.Create("META-INF/MANIFEST.MF")
	if err != nil {
		t.Fatal(err)
	}
	_, err = manifest.Write([]byte("Manifest-Version: 1.0\r\nImplementation-Version: " + version + "\r\nCG35-Protocol-Version: " + protocol + "\r\nKotlin-Compiler-Version: " + kotlinVersion + "\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompilerProbeRejectsSymlinkAndOversizedRecord(t *testing.T) {
	record, invocation, model, artifact := validProbeFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	writeProbeFixture(t, outside, record)
	if err := os.Symlink(outside, invocation.recordPath); err != nil {
		t.Fatal(err)
	}
	if _, err := readCompilerProbe(invocation, model, artifact); err == nil {
		t.Fatal("expected symlink record rejection")
	}

	_, invocation, model, artifact = validProbeFixture(t)
	if err := os.WriteFile(invocation.recordPath, make([]byte, MaxCompilerProbeRecord+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCompilerProbe(invocation, model, artifact); err == nil {
		t.Fatal("expected oversized record rejection")
	}
}

func TestCompilerProbeDuplicateCallbackMarkerRejected(t *testing.T) {
	_, invocation, model, artifact := validProbeFixture(t)
	if err := os.WriteFile(invocation.recordPath+".duplicate", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCompilerProbe(invocation, model, artifact); err == nil {
		t.Fatal("duplicate callback marker accepted")
	}
}

func TestGradleProcessIdentityDoesNotChangeV2Fingerprint(t *testing.T) {
	model := gradleModel{GradleVersion: "9.3.1", KotlinPluginVersion: "2.3.20", Project: ":app", Compilation: "main", CompileTask: ":app:compileKotlin", GradleProcessID: 100}
	first := hashFingerprintV2("/repo", model, []string{"src"}, strings.Repeat("a", 64), Dimensions{}, Proof{})
	model.GradleProcessID = 200
	second := hashFingerprintV2("/repo", model, []string{"src"}, strings.Repeat("a", 64), Dimensions{}, Proof{})
	if first != second {
		t.Fatal("ephemeral process identity changed existing v2 fingerprint meaning")
	}
}

func validProbeFixture(t *testing.T) (compilerProbeRecord, *probeInvocation, gradleModel, Artifact) {
	t.Helper()
	dir := t.TempDir()
	token := strings.Repeat("a", 64)
	jarHash := strings.Repeat("b", 64)
	invocation := &probeInvocation{token: token, jarSHA256: jarHash, recordPath: filepath.Join(dir, "record.json"), project: ":app", compilation: "main", strategy: "default"}
	model := gradleModel{Project: ":app", Compilation: "main", CompileTask: ":app:compileKotlin", KotlinPluginVersion: "2.3.20", TaskOutcome: "TASK_REPORTED_WORK"}
	artifact := Artifact{InputFingerprint: strings.Repeat("c", 64)}
	record := compilerProbeRecord{
		Schema: CompilerProbeSchema, ProtocolVersion: CompilerProbeProtocol, Status: "CALLBACK_OBSERVED",
		InvocationToken: token, ProbeVersion: "1.0.0", ProbeJARSHA256: jarHash,
		CompilerClass: "org.jetbrains.kotlin.cli.jvm.K2JVMCompiler", CompilerClassloader: "java.net.URLClassLoader",
		CompilerCodeSourceName: "kotlin-compiler-embeddable-2.3.20.jar", CompilerCodeSourceSHA256: strings.Repeat("d", 64),
		CompilerClassSHA256: strings.Repeat("e", 64), Project: model.Project, Compilation: model.Compilation,
		Task: model.CompileTask, CallbackExecuted: true, ProcessID: 12345, ProcessStart: "2026-10-30T00:00:00Z", RuntimeVersion: "17.0.1", RequestedExecutionStrategy: "default",
	}
	return record, invocation, model, artifact
}

func writeProbeFixture(t *testing.T, path string, record compilerProbeRecord) {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

package buildevidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed gradle.init.gradle
var gradleInitScript embed.FS

// GradleRequest is an explicitly authorized invocation for one Kotlin/JVM
// compilation. GradleCommand must name the executable selected by the user;
// this function never searches for or invokes a project wrapper.
type GradleRequest struct {
	RepositoryRoot            string
	GradleCommand             string
	Project                   string
	Compilation               string
	OutputPath                string
	CompilerProbeJar          string
	CompilerExecutionStrategy string
	DependencySnapshot        bool
	Stderr                    io.Writer
}

type gradleModel struct {
	GradleVersion            string              `json:"gradle_version"`
	KotlinPluginVersion      string              `json:"kotlin_plugin_version"`
	KotlinCompilerVersion    string              `json:"kotlin_compiler_version,omitempty"`
	CompileTask              string              `json:"compile_task,omitempty"`
	JDKVendor                string              `json:"jdk_vendor"`
	JDKVersion               string              `json:"jdk_version"`
	GradleRuntimeJDKVendor   string              `json:"gradle_runtime_jdk_vendor"`
	GradleRuntimeJDKVersion  string              `json:"gradle_runtime_jdk_version"`
	SelectedToolchainVendor  string              `json:"selected_toolchain_vendor"`
	SelectedToolchainVersion string              `json:"selected_toolchain_version"`
	Project                  string              `json:"project"`
	Compilation              string              `json:"compilation"`
	SourceRoots              []string            `json:"source_roots"`
	Dependencies             []string            `json:"dependencies"`
	ClasspathObservation     string              `json:"classpath_observation,omitempty"`
	ClasspathState           State               `json:"classpath_state,omitempty"`
	ClasspathProvenance      []string            `json:"classpath_provenance,omitempty"`
	ClasspathEntries         []rawClasspathEntry `json:"classpath_entries,omitempty"`
	TaskOutcome              string              `json:"task_outcome"`
	TaskDidWork              bool                `json:"task_did_work"`
	TaskSkipped              bool                `json:"task_skipped"`
	SkipReasonCode           string              `json:"skip_reason_code"`
	TaskInputSHA256          []string            `json:"task_input_sha256"`
	CompilerArtifacts        []FileIdentity      `json:"compiler_artifact_candidates"`
	GradleProcessID          int64               `json:"gradle_process_id,omitempty"`
	OutputFiles              []OutputFile        `json:"output_files"`
	OutputRoots              []string            `json:"output_roots"`
	JVMTarget                string              `json:"jvm_target"`
	TaskInputsComplete       bool                `json:"task_inputs_complete"`
	TaskInputsStable         bool                `json:"task_inputs_stable"`
	OutputsComplete          bool                `json:"outputs_complete"`
	OutputRootsClean         bool                `json:"output_roots_clean_before_task"`
	OutputsContained         bool                `json:"outputs_contained"`
}

// ExportGradle invokes Gradle only at this explicit call site and writes a
// schema-valid artifact. Dimensions stay incomplete/unknown unless the model
// exposes evidence this first Kotlin/JVM slice can audit.
func ExportGradle(ctx context.Context, req GradleRequest) (Artifact, error) {
	root, err := filepath.Abs(req.RepositoryRoot)
	if err != nil {
		return Artifact{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Artifact{}, fmt.Errorf("resolve repository root: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return Artifact{}, fmt.Errorf("repository root is not a directory: %s", root)
	}
	if req.GradleCommand == "" || req.Project == "" || req.Compilation == "" || req.OutputPath == "" {
		return Artifact{}, errors.New("Gradle command, project, compilation, and output are required")
	}
	if !validGradleProject(req.Project) || !validCompilation(req.Compilation) {
		return Artifact{}, errors.New("invalid Gradle project or Kotlin compilation")
	}
	output, err := filepath.Abs(req.OutputPath)
	if err != nil {
		return Artifact{}, err
	}
	outputParent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return Artifact{}, fmt.Errorf("resolve evidence output parent (it must exist): %w", err)
	}
	parentInfo, err := os.Stat(outputParent)
	if err != nil || !parentInfo.IsDir() {
		return Artifact{}, errors.New("evidence output parent must be an existing directory")
	}
	output = filepath.Join(outputParent, filepath.Base(output))
	if _, inside := relativeWithin(root, output); inside {
		return Artifact{}, errors.New("artifact output must be outside the repository")
	}
	if err := requireAbsent(output); err != nil {
		return Artifact{}, fmt.Errorf("primary evidence output is unavailable: %w", err)
	}
	var probe *probeInvocation
	if req.CompilerProbeJar != "" {
		var err error
		probe, err = prepareCompilerProbe(root, req.CompilerProbeJar, output, req.Project, req.Compilation, req.CompilerExecutionStrategy)
		if err != nil {
			return Artifact{}, err
		}
		if err := requireAbsent(output + ".compiler-probe.json"); err != nil {
			return Artifact{}, fmt.Errorf("compiler probe sidecar output is unavailable: %w", err)
		}
	}
	if req.CompilerExecutionStrategy != "" && (probe == nil || (req.CompilerExecutionStrategy != "daemon" && req.CompilerExecutionStrategy != "in-process")) {
		return Artifact{}, errors.New("compiler execution strategy requires the opt-in probe and must be daemon or in-process")
	}

	tmpDir, err := os.MkdirTemp("", "codegraph-gradle-evidence-")
	if err != nil {
		return Artifact{}, err
	}
	defer os.RemoveAll(tmpDir)
	identity := sha256.Sum256([]byte(root))
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return Artifact{}, fmt.Errorf("resolve user cache directory: %w", err)
	}
	projectCacheDir := filepath.Join(cacheRoot, "codegraph", "gradle-project-cache", hex.EncodeToString(identity[:]))
	if err := os.MkdirAll(projectCacheDir, 0o700); err != nil {
		return Artifact{}, fmt.Errorf("create isolated Gradle project cache: %w", err)
	}
	initPath := filepath.Join(tmpDir, "evidence.init.gradle")
	script, err := gradleInitScript.ReadFile("gradle.init.gradle")
	if err != nil {
		return Artifact{}, err
	}
	if err := os.WriteFile(initPath, script, 0o600); err != nil {
		return Artifact{}, err
	}
	if probe != nil {
		probe.recordPath = filepath.Join(tmpDir, "compiler-probe.json")
	}
	modelPath := filepath.Join(tmpDir, "model.json")
	args := []string{
		"--no-daemon", "--project-cache-dir", projectCacheDir, "--init-script", initPath,
		"--no-build-cache",
		"-Pcg35.project=" + req.Project,
		"-Pcg35.compilation=" + req.Compilation,
		"-Pcg35.output=" + modelPath,
		":codegraphExportEvidence",
	}
	if req.DependencySnapshot {
		if err := requireAbsent(output + ".classpath.json"); err != nil {
			return Artifact{}, fmt.Errorf("dependency snapshot output is unavailable: %w", err)
		}
		args = append(args, "-Pcg35.dependency.snapshot=true")
	}
	if probe != nil {
		args = append(args,
			"-Pcg35.probe.jar="+probe.jarPath,
			"-Pcg35.probe.record="+probe.recordPath,
			"-Pcg35.probe.token="+probe.token,
			"-Pcg35.probe.strategy="+probe.strategy,
		)
		if req.CompilerExecutionStrategy != "" {
			args = append(args, "-Pkotlin.compiler.execution.strategy="+req.CompilerExecutionStrategy)
		}
	}
	if req.Stderr != nil {
		if _, err := fmt.Fprintf(req.Stderr, "Running explicitly selected Gradle executable and selected compilation task for %s/%s; build-cache reuse is disabled, existing build outputs are not cleaned\n", req.Project, req.Compilation); err != nil {
			return Artifact{}, err
		}
	}
	cmd := exec.CommandContext(ctx, req.GradleCommand, args...)
	cmd.Dir = root
	cmd.Stdout = req.Stderr
	cmd.Stderr = req.Stderr
	if err := cmd.Run(); err != nil {
		return Artifact{}, fmt.Errorf("run explicitly requested Gradle evidence task: %w", err)
	}
	modelBytes, err := readBoundedFile(modelPath, MaxArtifactSize)
	if err != nil {
		return Artifact{}, fmt.Errorf("read Gradle model output: %w", err)
	}
	var model gradleModel
	dec := json.NewDecoder(bytes.NewReader(modelBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&model); err != nil {
		return Artifact{}, fmt.Errorf("decode Gradle model output: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Artifact{}, errors.New("Gradle model output has trailing JSON")
	}
	if model.Project != req.Project || model.Compilation != req.Compilation || model.GradleVersion == "" || model.CompileTask == "" || !validGradleProject(model.CompileTask) || (req.Project != ":" && !strings.HasPrefix(model.CompileTask, req.Project+":")) {
		return Artifact{}, errors.New("Gradle model did not match the requested project and compilation")
	}
	if model.SourceRoots == nil {
		return Artifact{}, errors.New("Gradle model omitted Kotlin source roots")
	}
	if model.TaskOutcome == "" || model.TaskInputSHA256 == nil || model.CompilerArtifacts == nil || model.OutputFiles == nil || model.OutputRoots == nil {
		return Artifact{}, errors.New("Gradle model omitted selected-task execution or output observations")
	}
	sort.Strings(model.Dependencies)
	for _, item := range []struct{ name, value string }{
		{"Gradle version", model.GradleVersion}, {"Kotlin plugin version", model.KotlinPluginVersion},
		{"Kotlin compiler version", model.KotlinCompilerVersion}, {"Gradle runtime JDK version", model.GradleRuntimeJDKVersion},
		{"Gradle runtime JDK vendor", model.GradleRuntimeJDKVendor}, {"selected toolchain version", model.SelectedToolchainVersion},
		{"selected toolchain vendor", model.SelectedToolchainVendor},
	} {
		if !safeToolIdentity(item.value) || (item.name == "Kotlin compiler version" && item.value == "") {
			return Artifact{}, fmt.Errorf("Gradle reported unsupported %s identity text", item.name)
		}
	}
	for _, dependency := range model.Dependencies {
		if !safeDependencyCoordinate(dependency) {
			return Artifact{}, errors.New("Gradle reported a dependency coordinate outside the supported non-secret format")
		}
	}
	roots, err := canonicalRepoRoots(root, model.SourceRoots)
	if err != nil {
		return Artifact{}, err
	}
	model.SourceRoots = roots
	inputs, err := repositoryFingerprint(root, roots)
	if err != nil {
		return Artifact{}, fmt.Errorf("fingerprint selected Kotlin source roots: %w", err)
	}
	dimensions := Dimensions{
		Source:           Dimension{State: Incomplete, Provenance: []string{"Gradle Kotlin/JVM compilation source-set roots; generated inputs are not proven"}},
		Generated:        Dimension{State: Unknown, Provenance: []string{"first producer does not enumerate generated source ownership"}},
		Excluded:         Dimension{State: Unknown, Provenance: []string{"first producer does not observe excluded source rules"}},
		Dependency:       Dimension{State: Incomplete, Provenance: []string{"Gradle resolved compilation artifacts observed; file dependencies and artifact content freshness are not proven"}},
		ExternalMetadata: Dimension{State: Unknown, Provenance: []string{"external metadata is not captured"}},
		CompilerIdentity: Dimension{State: Incomplete, Provenance: []string{"configured Gradle/KGP/compiler versions and candidate compiler task-input hashes observed; loaded compiler implementation identity and selected toolchain content are not proven"}},
	}
	proof := proofFromModel(model)
	fingerprint := hashFingerprintV2(root, model, roots, inputs, dimensions, proof)
	artifact := Artifact{
		Schema:   Schema,
		Producer: Producer{Name: "codegraph-gradle-exporter", Version: "2"},
		Repository: Repository{
			Identity:   "sha256:" + hex.EncodeToString(identity[:]),
			RootMarker: "sha256:" + hex.EncodeToString(identity[:]),
			BuildRoot:  ".",
		},
		Compilation: Compilation{GradleProject: req.Project, KotlinCompilation: req.Compilation, GradleTask: model.CompileTask},
		Tools: Tools{
			GradleVersion:            model.GradleVersion,
			KotlinPluginVersion:      model.KotlinPluginVersion,
			KotlinCompilerVersion:    model.KotlinCompilerVersion,
			GradleRuntimeJDKVendor:   model.GradleRuntimeJDKVendor,
			GradleRuntimeJDKVersion:  model.GradleRuntimeJDKVersion,
			SelectedToolchainVendor:  model.SelectedToolchainVendor,
			SelectedToolchainVersion: model.SelectedToolchainVersion,
		},
		InputFingerprint: fingerprint,
		Evidence: Evidence{
			Dimensions:  dimensions,
			SourceRoots: roots,
			Dependencies: func() []Dependency {
				out := make([]Dependency, 0, len(model.Dependencies))
				for _, coordinate := range model.Dependencies {
					out = append(out, Dependency{Coordinate: coordinate, State: "positive", Provenance: "Gradle selected compilation classpath"})
				}
				return out
			}(),
		},
		Proof: &proof,
	}
	var probeOutput string
	var probeData []byte
	if probe != nil {
		observation, err := readCompilerProbe(probe, model, artifact)
		if err != nil {
			return Artifact{}, err
		}
		probeData, err = observation.Marshal()
		if err != nil {
			return Artifact{}, err
		}
		probeOutput = output + ".compiler-probe.json"
	}
	var dependencyOutput string
	var dependencyData []byte
	if req.DependencySnapshot {
		if model.ClasspathObservation != "KotlinCompile.libraries" || model.ClasspathState == "" || model.ClasspathEntries == nil || model.ClasspathProvenance == nil {
			return Artifact{}, errors.New("Gradle model omitted the selected task classpath observation")
		}
		snapshot, err := buildClasspathSnapshotContext(ctx, root, gradleUserHome(), artifact, model.ClasspathEntries, model.ClasspathState, model.ClasspathProvenance)
		if err != nil {
			return Artifact{}, fmt.Errorf("snapshot selected Kotlin task classpath: %w", err)
		}
		dependencyData, err = snapshot.Marshal()
		if err != nil {
			return Artifact{}, err
		}
		if len(dependencyData) > MaxArtifactSize {
			return Artifact{}, fmt.Errorf("dependency classpath snapshot exceeds %d bytes", MaxArtifactSize)
		}
		dependencyOutput = output + ".classpath.json"
	}
	data, err := artifact.Marshal()
	if err != nil {
		return Artifact{}, err
	}
	if len(data) > MaxArtifactSize {
		return Artifact{}, fmt.Errorf("generated build evidence exceeds %d bytes", MaxArtifactSize)
	}
	sidecars := make([]evidenceSidecar, 0, 2)
	if probeOutput != "" {
		sidecars = append(sidecars, evidenceSidecar{path: probeOutput, data: probeData, label: "compiler probe"})
	}
	if dependencyOutput != "" {
		sidecars = append(sidecars, evidenceSidecar{path: dependencyOutput, data: dependencyData, label: "dependency classpath snapshot"})
	}
	if err := publishEvidenceBundle(output, data, sidecars, writeNewFile); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

// CurrentFingerprint recomputes the locally available inputs for stale-artifact
// checks. It cannot attest Gradle-resolved dependencies, which remain unknown.
func CurrentFingerprint(repositoryRoot string, artifact Artifact) (string, error) {
	return currentFingerprintContext(context.Background(), repositoryRoot, artifact, nil)
}

func currentFingerprintContext(ctx context.Context, repositoryRoot string, artifact Artifact, budget *classpathReadBudget) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := artifact.Validate(); err != nil {
		return "", err
	}
	root, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	identity := sha256.Sum256([]byte(root))
	want := "sha256:" + hex.EncodeToString(identity[:])
	if artifact.Repository.Identity != want || artifact.Repository.RootMarker != want {
		return "", errors.New("build evidence belongs to a different repository root")
	}
	roots := artifact.Evidence.SourceRoots
	model := gradleModel{
		GradleVersion:            artifact.Tools.GradleVersion,
		KotlinPluginVersion:      artifact.Tools.KotlinPluginVersion,
		KotlinCompilerVersion:    artifact.Tools.KotlinCompilerVersion,
		CompileTask:              artifact.Compilation.GradleTask,
		JDKVendor:                artifact.Tools.JDKVendor,
		JDKVersion:               artifact.Tools.JDKVersion,
		GradleRuntimeJDKVendor:   artifact.Tools.GradleRuntimeJDKVendor,
		GradleRuntimeJDKVersion:  artifact.Tools.GradleRuntimeJDKVersion,
		SelectedToolchainVendor:  artifact.Tools.SelectedToolchainVendor,
		SelectedToolchainVersion: artifact.Tools.SelectedToolchainVersion,
		Project:                  artifact.Compilation.GradleProject,
		Compilation:              artifact.Compilation.KotlinCompilation,
		SourceRoots:              roots,
	}
	if artifact.Proof != nil {
		model.TaskOutcome = artifact.Proof.TaskOutcome
		model.TaskDidWork = artifact.Proof.TaskDidWork
		model.TaskSkipped = artifact.Proof.TaskSkipped
		model.SkipReasonCode = artifact.Proof.SkipReasonCode
		model.TaskInputSHA256 = artifact.Proof.TaskInputSHA256
		model.CompilerArtifacts = artifact.Proof.CompilerArtifacts
		model.OutputFiles = artifact.Proof.OutputFiles
		model.OutputRoots = artifact.Proof.OutputRoots
		model.JVMTarget = artifact.Proof.JVMTarget
		model.TaskInputsComplete = artifact.Proof.TaskInputsComplete
		model.TaskInputsStable = artifact.Proof.TaskInputsStable
		model.OutputsComplete = artifact.Proof.OutputsComplete
		model.OutputRootsClean = artifact.Proof.OutputRootsClean
		model.OutputsContained = artifact.Proof.OutputsContained
	}
	for _, dependency := range artifact.Evidence.Dependencies {
		model.Dependencies = append(model.Dependencies, dependency.Coordinate)
	}
	sort.Strings(model.Dependencies)
	current, err := repositoryFingerprintContext(ctx, root, roots, budget)
	if err != nil {
		return "", err
	}
	if artifact.Proof == nil {
		return hashFingerprint(root, model, roots, current, artifact.Evidence.Dimensions), nil
	}
	if err := validateOutputContentContext(ctx, root, *artifact.Proof, budget); err != nil {
		return "", err
	}
	return hashFingerprintV2(root, model, roots, current, artifact.Evidence.Dimensions, *artifact.Proof), nil
}

func validateOutputContent(root string, proof Proof) error {
	return validateOutputContentContext(context.Background(), root, proof, nil)
}

func validateOutputContentContext(ctx context.Context, root string, proof Proof, budget *classpathReadBudget) error {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return errors.New("repository root cannot be opened safely")
	}
	defer rootFS.Close()
	expected := make(map[string]struct{}, len(proof.OutputFiles))
	for _, output := range proof.OutputFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		expected[filepath.Clean(filepath.FromSlash(output.Path))] = struct{}{}
		abs := filepath.Join(root, filepath.FromSlash(output.Path))
		if _, inside := relativeWithin(root, abs); !inside {
			return fmt.Errorf("selected output escapes repository: %q", output.Path)
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return fmt.Errorf("build evidence is stale: selected output is unavailable: %q: %w", output.Path, err)
		}
		if _, inside := relativeWithin(root, resolved); !inside {
			return fmt.Errorf("build evidence is stale: selected output symlink escapes repository: %q", output.Path)
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil {
			return err
		}
		file, err := rootFS.Open(rel)
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		if statErr != nil {
			file.Close()
			return statErr
		}
		if budget != nil {
			if err := budget.beginFile(info.Size()); err != nil {
				file.Close()
				return err
			}
		}
		h := sha256.New()
		readLimit := info.Size()
		read, copyErr := io.Copy(h, io.LimitReader(contextReader{ctx: ctx, r: file}, readLimit))
		if budget != nil {
			budget.recordBytes(read)
		}
		after, statErr := file.Stat()
		pathAfter, pathErr := rootFS.Lstat(filepath.FromSlash(rel))
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if read != info.Size() {
			return errors.New("classpath revalidation budget exceeded or output changed while hashing")
		}
		if statErr != nil || pathErr != nil || pathAfter.Mode()&os.ModeSymlink != 0 || !pathAfter.Mode().IsRegular() || !os.SameFile(info, after) || !os.SameFile(info, pathAfter) || info.Size() != after.Size() || info.Size() != pathAfter.Size() || !info.ModTime().Equal(after.ModTime()) || !info.ModTime().Equal(pathAfter.ModTime()) {
			return errors.New("repository output changed while hashing")
		}
		if hex.EncodeToString(h.Sum(nil)) != output.SHA256 {
			return fmt.Errorf("build evidence is stale: selected output content changed: %q", output.Path)
		}
	}
	observed := make(map[string]struct{}, len(expected))
	for _, outputRoot := range proof.OutputRoots {
		absRoot := filepath.Join(root, filepath.FromSlash(outputRoot))
		resolvedRoot, err := filepath.EvalSymlinks(absRoot)
		if err != nil {
			return fmt.Errorf("build evidence is stale: selected output root is unavailable: %q: %w", outputRoot, err)
		}
		if _, inside := relativeWithin(root, resolvedRoot); !inside {
			return fmt.Errorf("build evidence is stale: selected output root symlink escapes repository: %q", outputRoot)
		}
		outputRootRel, err := filepath.Rel(root, resolvedRoot)
		if err != nil {
			return err
		}
		outputFS, err := rootFS.OpenRoot(outputRootRel)
		if err != nil {
			return fmt.Errorf("build evidence is stale: selected output root cannot be opened safely: %q", outputRoot)
		}
		err = fs.WalkDir(outputFS.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			if budget != nil {
				if err := budget.beginEntry(); err != nil {
					return err
				}
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				fullName := filepath.Join(resolvedRoot, filepath.FromSlash(name))
				target, err := filepath.EvalSymlinks(fullName)
				if err != nil {
					return fmt.Errorf("resolve selected output symlink %q: %w", fullName, err)
				}
				if _, inside := relativeWithin(root, target); !inside {
					return fmt.Errorf("build evidence is stale: selected output symlink escapes repository: %q", fullName)
				}
				info, err := rootFS.Stat(filepath.Join(outputRootRel, filepath.FromSlash(name)))
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() || !strings.HasSuffix(name, ".class") {
					return nil
				}
			} else if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".class") {
				return nil
			}
			rel, err := filepath.Rel(root, filepath.Join(resolvedRoot, filepath.FromSlash(name)))
			if err != nil {
				return err
			}
			key := filepath.Clean(rel)
			observed[key] = struct{}{}
			if _, ok := expected[key]; !ok {
				return fmt.Errorf("build evidence is stale: unrecorded class output exists: %q", filepath.ToSlash(rel))
			}
			return nil
		})
		closeErr := outputFS.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if len(observed) != len(expected) {
		for output := range expected {
			if _, ok := observed[output]; !ok {
				return fmt.Errorf("build evidence is stale: recorded class output is no longer under an output root: %q", filepath.ToSlash(output))
			}
		}
	}
	return nil
}

func proofFromModel(model gradleModel) Proof {
	inputs := append([]string{}, model.TaskInputSHA256...)
	sort.Strings(inputs)
	candidates := append([]FileIdentity{}, model.CompilerArtifacts...)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Name == candidates[j].Name {
			return candidates[i].SHA256 < candidates[j].SHA256
		}
		return candidates[i].Name < candidates[j].Name
	})
	outputs := append([]OutputFile{}, model.OutputFiles...)
	sort.Slice(outputs, func(i, j int) bool { return outputs[i].Path < outputs[j].Path })
	roots := append([]string{}, model.OutputRoots...)
	sort.Strings(roots)
	inputsWhy := "selected task's declared input files were content hashed; this is not proof of complete ABI-affecting input closure"
	if !model.TaskInputsComplete {
		inputsWhy = "selected task input inventory exceeded supported observation bounds"
	} else if !model.TaskInputsStable {
		inputsWhy = "selected task declared input files changed during task execution"
	}
	toolchainProvenance := "selected Kotlin toolchain identity is not exposed through a supported API in this producer"
	if model.SelectedToolchainVersion != "" {
		toolchainProvenance = "selected Kotlin toolchain version configuration observed; toolchain artifact identity is not proven"
	}
	return Proof{
		TaskOutcome: model.TaskOutcome, TaskDidWork: model.TaskDidWork, TaskSkipped: model.TaskSkipped,
		SkipReasonCode: model.SkipReasonCode, TaskInputSHA256: inputs,
		CompilerArtifacts: candidates, OutputFiles: outputs, OutputRoots: roots, JVMTarget: model.JVMTarget,
		TaskInputsComplete: model.TaskInputsComplete, TaskInputsStable: model.TaskInputsStable, OutputsComplete: model.OutputsComplete, OutputRootsClean: model.OutputRootsClean, OutputsContained: model.OutputsContained,
		Dimensions: ProofDimensions{
			GradleRuntimeJDK:  Dimension{State: Incomplete, Provenance: []string{"Gradle runtime vendor/version observed; runtime JDK content identity is not proven"}},
			TaskExecution:     Dimension{State: Incomplete, Provenance: []string{"Gradle TaskExecutionListener outcome observed; compiler process and loaded implementation are not observed"}},
			CompilerRuntime:   Dimension{State: Unknown, Provenance: []string{"compiler runtime identity is not exposed through a supported execution-level observation"}},
			SelectedToolchain: Dimension{State: Unknown, Provenance: []string{toolchainProvenance}},
			ABIInputs:         Dimension{State: Incomplete, Provenance: []string{inputsWhy, "generated/excluded sources, dependencies, compiler/plugin closure, toolchain, and full ABI options are not proven complete"}},
			OutputProvenance:  Dimension{State: Incomplete, Provenance: []string{outputProvenanceReason(model)}},
		},
	}
}

func outputProvenanceReason(model gradleModel) string {
	if model.TaskOutcome == "FROM_CACHE" {
		return "outputs were restored from build cache; no new compiler execution or cache provenance recovery is claimed"
	}
	if model.TaskOutcome == "UP_TO_DATE" || model.TaskOutcome == "NO_SOURCE" || model.TaskOutcome == "SKIPPED" {
		return "selected task did not report fresh work; output attribution to a new compilation is incomplete"
	}
	if !model.OutputsContained {
		return "one or more selected task outputs were outside the repository-relative observation boundary"
	}
	if !model.OutputsComplete {
		return "selected task output inventory exceeded supported observation bounds"
	}
	if !model.OutputRootsClean {
		return "selected task output roots were not empty before the task; existing user outputs were preserved"
	}
	return "clean selected-task output roots and declared class hashes were observed, but complete ABI input closure and compiler execution provenance were not established"
}

// ValidateCurrentInRepository recomputes local inputs from the artifact and
// refuses a stale fingerprint without invoking Gradle.
func (a Artifact) ValidateCurrentInRepository(repositoryRoot string) error {
	current, err := CurrentFingerprint(repositoryRoot, a)
	if err != nil {
		return err
	}
	return a.ValidateCurrent(current)
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("Gradle model output exceeds %d bytes", limit)
	}
	return data, nil
}

func safeToolIdentity(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._+- ()", r) {
			continue
		}
		return false
	}
	return true
}

func safeDependencyCoordinate(value string) bool {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		if strings.HasPrefix(value, "project ") {
			return validGradleProject(strings.TrimPrefix(value, "project "))
		}
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._+-", r) {
				continue
			}
			return false
		}
	}
	return true
}

func canonicalRepoRoots(root string, paths []string) ([]string, error) {
	set := make(map[string]struct{}, len(paths))
	for _, source := range paths {
		if source == "" {
			return nil, errors.New("Gradle reported an empty source root")
		}
		abs, err := filepath.Abs(source)
		if err != nil {
			return nil, err
		}
		rel, err := canonicalRepoPath(root, abs)
		if err != nil {
			return nil, fmt.Errorf("resolve Gradle source root %s: %w", source, err)
		}
		if rel == "." {
			return nil, errors.New("Gradle source root cannot be the repository root")
		}
		set[rel] = struct{}{}
	}
	roots := make([]string, 0, len(set))
	for root := range set {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots, nil
}

func canonicalRepoPath(root, abs string) (string, error) {
	rel, ok := relativeWithin(root, abs)
	if !ok {
		return "", errors.New("path escapes repository")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	current := root
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			current = filepath.Join(append([]string{current}, parts[i+1:]...)...)
			break
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			current, err = filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			if _, ok := relativeWithin(root, current); !ok {
				return "", errors.New("symlink escapes repository")
			}
		}
	}
	if info, err := os.Stat(current); err == nil && !info.IsDir() {
		return "", errors.New("source root is not a directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	rel, err := filepath.Rel(root, current)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Clean(rel)), nil
}

// ponytail: hashes the full tree in O(repository bytes); use a Gradle-proven
// input manifest if explicit freshness checks become costly.
func repositoryFingerprint(root string, sourceRoots []string) (string, error) {
	return repositoryFingerprintContext(context.Background(), root, sourceRoots, nil)
}

func repositoryFingerprintContext(ctx context.Context, root string, sourceRoots []string, budget *classpathReadBudget) (string, error) {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return "", errors.New("repository root cannot be opened safely")
	}
	defer rootFS.Close()
	h := sha256.New()
	write := func(name string, data []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(name)))
		h.Write(size[:])
		h.Write([]byte(name))
		binary.BigEndian.PutUint64(size[:], uint64(len(data)))
		h.Write(size[:])
		h.Write(data)
	}
	type observedRepositoryPath struct {
		info       fs.FileInfo
		linkTarget string
	}
	writeFile := func(name, path string, observed observedRepositoryPath) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		pathBefore, err := rootFS.Lstat(filepath.FromSlash(path))
		if err != nil || !os.SameFile(observed.info, pathBefore) || (pathBefore.Mode()&os.ModeSymlink != 0) != (observed.info.Mode()&os.ModeSymlink != 0) {
			return errors.New("repository path changed before hashing")
		}
		if observed.info.Mode()&os.ModeSymlink != 0 {
			linkBefore, err := rootFS.Readlink(filepath.FromSlash(path))
			if err != nil || linkBefore != observed.linkTarget {
				return errors.New("repository symlink changed before hashing")
			}
		}
		f, err := rootFS.Open(filepath.FromSlash(path))
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if budget != nil {
			if err := budget.beginFile(info.Size()); err != nil {
				f.Close()
				return err
			}
		}
		content := sha256.New()
		readLimit := info.Size()
		read, copyErr := io.Copy(content, io.LimitReader(contextReader{ctx: ctx, r: f}, readLimit))
		if budget != nil {
			budget.recordBytes(read)
		}
		after, statErr := f.Stat()
		pathAfter, pathErr := rootFS.Lstat(filepath.FromSlash(path))
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if read != info.Size() {
			return errors.New("classpath revalidation budget exceeded or repository file changed while hashing")
		}
		if statErr != nil || pathErr != nil || !os.SameFile(observed.info, pathAfter) || (pathAfter.Mode()&os.ModeSymlink != 0) != (observed.info.Mode()&os.ModeSymlink != 0) || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
			return errors.New("repository file changed while hashing")
		}
		if observed.info.Mode()&os.ModeSymlink != 0 {
			linkAfter, err := rootFS.Readlink(filepath.FromSlash(path))
			if err != nil || linkAfter != observed.linkTarget {
				return errors.New("repository symlink changed while hashing")
			}
		}
		pathTarget, targetErr := rootFS.Stat(filepath.FromSlash(path))
		if targetErr != nil || !os.SameFile(info, pathTarget) || info.Size() != pathTarget.Size() || !info.ModTime().Equal(pathTarget.ModTime()) {
			return errors.New("repository file path changed while hashing")
		}
		write("file:"+name, []byte(fmt.Sprintf("%o", info.Mode().Perm())))
		write("content:"+name, content.Sum(nil))
		return nil
	}
	err = fs.WalkDir(rootFS.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if budget != nil {
			if err := budget.beginEntry(); err != nil {
				return err
			}
		}
		pathInfo, err := rootFS.Lstat(filepath.FromSlash(name))
		if err != nil {
			return err
		}
		info := pathInfo
		linkTarget := ""
		linkText := ""
		if entry.Type()&os.ModeSymlink != 0 {
			fullName := filepath.Join(root, filepath.FromSlash(name))
			target, err := filepath.EvalSymlinks(fullName)
			if err != nil {
				return fmt.Errorf("resolve repository symlink %s: %w", name, err)
			}
			if _, ok := relativeWithin(root, target); !ok {
				return fmt.Errorf("repository symlink escapes repository: %s", name)
			}
			linkTarget, err = filepath.Rel(root, target)
			if err != nil {
				return err
			}
			linkText, err = rootFS.Readlink(filepath.FromSlash(name))
			if err != nil {
				return err
			}
			info, err = rootFS.Stat(filepath.FromSlash(name))
			if err != nil {
				return err
			}
			if info.IsDir() {
				return fmt.Errorf("directory symlink prevents a complete local fingerprint: %s", name)
			}
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel := filepath.ToSlash(name)
		if linkTarget != "" {
			write("symlink:"+filepath.ToSlash(rel), []byte(filepath.ToSlash(linkTarget)))
		}
		return writeFile(filepath.ToSlash(rel), filepath.ToSlash(rel), observedRepositoryPath{info: pathInfo, linkTarget: linkText})
	})
	if err != nil {
		return "", err
	}
	for _, sourceRoot := range sourceRoots {
		write("source-root:"+sourceRoot, nil)
		walkRoot := filepath.ToSlash(filepath.Clean(sourceRoot))
		err := fs.WalkDir(rootFS.FS(), walkRoot, func(name string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if errors.Is(walkErr, os.ErrNotExist) && name == walkRoot {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if budget != nil {
				if err := budget.beginEntry(); err != nil {
					return err
				}
			}
			if entry.IsDir() || !entry.Type().IsRegular() {
				return nil
			}
			pathInfo, err := rootFS.Lstat(filepath.ToSlash(name))
			if err != nil {
				return err
			}
			return writeFile("source:"+filepath.ToSlash(name), filepath.ToSlash(name), observedRepositoryPath{info: pathInfo})
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashFingerprint(root string, model gradleModel, roots []string, repositoryHash string, dimensions Dimensions) string {
	legacyModel := struct {
		GradleVersion         string   `json:"gradle_version"`
		KotlinPluginVersion   string   `json:"kotlin_plugin_version"`
		KotlinCompilerVersion string   `json:"kotlin_compiler_version,omitempty"`
		CompileTask           string   `json:"compile_task,omitempty"`
		JDKVendor             string   `json:"jdk_vendor"`
		JDKVersion            string   `json:"jdk_version"`
		Project               string   `json:"project"`
		Compilation           string   `json:"compilation"`
		SourceRoots           []string `json:"source_roots"`
		Dependencies          []string `json:"dependencies"`
	}{model.GradleVersion, model.KotlinPluginVersion, model.KotlinCompilerVersion, model.CompileTask, model.JDKVendor, model.JDKVersion, model.Project, model.Compilation, model.SourceRoots, model.Dependencies}
	data, _ := json.Marshal(struct {
		Repository string     `json:"repository"`
		Model      any        `json:"model"`
		Roots      []string   `json:"roots"`
		Inputs     string     `json:"inputs"`
		Dimensions Dimensions `json:"dimensions"`
	}{root, legacyModel, roots, repositoryHash, dimensions})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashFingerprintV2(root string, model gradleModel, roots []string, repositoryHash string, dimensions Dimensions, proof Proof) string {
	// Process IDs are ephemeral callback-correlation diagnostics, not v2
	// evidence inputs. Keep them out of the stable v2 fingerprint contract.
	model.GradleProcessID = 0
	// The opt-in classpath sidecar is independently versioned. Its fields must
	// not alter the established v2 fingerprint contract or make existing v2
	// artifacts fail read-only currentness checks.
	model.ClasspathObservation = ""
	model.ClasspathState = ""
	model.ClasspathProvenance = nil
	model.ClasspathEntries = nil
	data, _ := json.Marshal(struct {
		Repository string      `json:"repository"`
		Model      gradleModel `json:"model"`
		Roots      []string    `json:"roots"`
		Inputs     string      `json:"inputs"`
		Dimensions Dimensions  `json:"dimensions"`
		Proof      Proof       `json:"proof"`
	}{root, model, roots, repositoryHash, dimensions, proof})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func relativeWithin(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

func writeNewFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".codegraph-evidence-*")
	if err != nil {
		return fmt.Errorf("create evidence staging file: %w", err)
	}
	staging := f.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(staging)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("secure evidence staging file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync evidence staging file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close evidence staging file: %w", err)
	}
	// A hard link publishes the complete file atomically and fails if path
	// already exists. Unlike rename, it cannot replace a user-owned artifact.
	if err := os.Link(staging, path); err != nil {
		return fmt.Errorf("publish evidence artifact without replacing existing files: %w", err)
	}
	published = true
	if err := os.Remove(staging); err != nil {
		return &artifactPublishedError{err: err}
	}
	return nil
}

type artifactPublishedError struct{ err error }

func (e *artifactPublishedError) Error() string {
	return "artifact published but staging cleanup failed: " + e.err.Error()
}
func (e *artifactPublishedError) Unwrap() error { return e.err }

func publishEvidenceArtifacts(primaryPath string, primaryData []byte, probePath string, probeData []byte, publish func(string, []byte) error) error {
	if err := requireAbsent(primaryPath); err != nil {
		return fmt.Errorf("primary evidence output is unavailable: %w", err)
	}
	if probePath != "" {
		if err := requireAbsent(probePath); err != nil {
			return fmt.Errorf("compiler probe sidecar output is unavailable: %w", err)
		}
	}
	// The primary goes first so its publication failure cannot leave a new
	// diagnostic sidecar behind. The pair itself is not a two-file transaction.
	if err := publish(primaryPath, primaryData); err != nil {
		var publishedErr *artifactPublishedError
		if errors.As(err, &publishedErr) {
			return fmt.Errorf("primary evidence artifact was published at %q but staging cleanup failed; compiler probe sidecar was not published: %w", primaryPath, err)
		}
		return fmt.Errorf("publish primary evidence artifact: %w", err)
	}
	if probePath != "" {
		if err := publish(probePath, probeData); err != nil {
			var publishedErr *artifactPublishedError
			if errors.As(err, &publishedErr) {
				return fmt.Errorf("primary evidence artifact is published at %q and compiler probe sidecar was published at %q, but sidecar staging cleanup failed: %w", primaryPath, probePath, err)
			}
			return fmt.Errorf("primary evidence artifact was published at %q, but compiler probe sidecar publication failed; the primary artifact remains in place: %w", primaryPath, err)
		}
	}
	return nil
}

type evidenceSidecar struct {
	path  string
	data  []byte
	label string
}

func publishEvidenceBundle(primaryPath string, primaryData []byte, sidecars []evidenceSidecar, publish func(string, []byte) error) error {
	if err := requireAbsent(primaryPath); err != nil {
		return fmt.Errorf("primary evidence output is unavailable: %w", err)
	}
	seen := map[string]struct{}{filepath.Clean(primaryPath): {}}
	for _, sidecar := range sidecars {
		if sidecar.path == "" || sidecar.label == "" {
			return errors.New("evidence sidecar path and label are required")
		}
		if _, duplicate := seen[filepath.Clean(sidecar.path)]; duplicate {
			return fmt.Errorf("duplicate evidence sidecar output path %q", sidecar.path)
		}
		seen[filepath.Clean(sidecar.path)] = struct{}{}
		if err := requireAbsent(sidecar.path); err != nil {
			return fmt.Errorf("%s output is unavailable: %w", sidecar.label, err)
		}
	}
	if err := publish(primaryPath, primaryData); err != nil {
		var publishedErr *artifactPublishedError
		if errors.As(err, &publishedErr) {
			return fmt.Errorf("primary evidence artifact was published at %q but staging cleanup failed; sidecars were not published: %w", primaryPath, err)
		}
		return fmt.Errorf("publish primary evidence artifact: %w", err)
	}
	for _, sidecar := range sidecars {
		if err := publish(sidecar.path, sidecar.data); err != nil {
			var publishedErr *artifactPublishedError
			if errors.As(err, &publishedErr) {
				return fmt.Errorf("primary evidence artifact is published at %q and %s is published at %q, but sidecar staging cleanup failed: %w", primaryPath, sidecar.label, sidecar.path, err)
			}
			return fmt.Errorf("primary evidence artifact was published at %q, but %s publication failed; prior outputs remain in place: %w", primaryPath, sidecar.label, err)
		}
	}
	return nil
}

func requireAbsent(path string) error {
	_, err := os.Lstat(path)
	if err == nil {
		return fmt.Errorf("path already exists: %s", path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

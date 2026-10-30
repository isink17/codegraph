package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/isink17/codegraph/internal/buildevidence"
)

func runExportGradleEvidence(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("export-gradle-evidence", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("repo-root", "", "Gradle repository root")
	gradle := fs.String("gradle", "", "explicit Gradle executable")
	project := fs.String("project", "", "selected Gradle project path")
	compilation := fs.String("compilation", "", "selected Kotlin/JVM compilation")
	output := fs.String("output", "", "new artifact path outside the repository")
	probeJar := fs.String("compiler-probe-jar", "", "optional prebuilt Kotlin 2.3.20 diagnostic probe JAR (writes a separate .compiler-probe.json artifact)")
	probeStrategy := fs.String("compiler-execution-strategy", "", "optional compiler strategy for probe experiments: daemon or in-process")
	dependencySnapshot := fs.Bool("dependency-snapshot", false, "write a separate content-addressed snapshot of selected KotlinCompile.libraries")
	sourceSnapshot := fs.Bool("source-snapshot", false, "write a separate content-addressed snapshot of selected KotlinCompile.sources")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	artifact, err := buildevidence.ExportGradle(ctx, buildevidence.GradleRequest{
		RepositoryRoot:            *root,
		GradleCommand:             *gradle,
		Project:                   *project,
		Compilation:               *compilation,
		OutputPath:                *output,
		CompilerProbeJar:          *probeJar,
		CompilerExecutionStrategy: *probeStrategy,
		DependencySnapshot:        *dependencySnapshot,
		SourceSnapshot:            *sourceSnapshot,
		Stderr:                    stderr,
	})
	if err != nil {
		return err
	}
	outcome := "unknown"
	if artifact.Proof != nil {
		outcome = artifact.Proof.TaskOutcome
	}
	_, err = fmt.Fprintf(stdout, "Wrote diagnostic Kotlin Gradle evidence for %s/%s, task %s (%s; input fingerprint %s; trust remains incomplete)\n", artifact.Compilation.GradleProject, artifact.Compilation.KotlinCompilation, artifact.Compilation.GradleTask, outcome, artifact.InputFingerprint)
	if err != nil {
		return err
	}
	if *probeJar != "" {
		_, err = fmt.Fprintf(stdout, "Wrote opt-in compiler callback diagnostic to %s.compiler-probe.json\n", *output)
	}
	if err == nil && *dependencySnapshot {
		_, err = fmt.Fprintf(stdout, "Wrote opt-in Kotlin task classpath content snapshot to %s.classpath.json (Gradle selection and compiler-consumed completeness remain unknown)\n", *output)
	}
	if err == nil && *sourceSnapshot {
		_, err = fmt.Fprintf(stdout, "Wrote opt-in selected Kotlin task source snapshot to %s.sources.json (current selection and compiler consumption remain unknown)\n", *output)
	}
	return err
}

func runValidateGradleSourceSnapshot(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("validate-gradle-source-snapshot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("repo-root", "", "repository root used by the related Gradle evidence")
	evidencePath := fs.String("evidence", "", "related codegraph.gradle_kotlin_evidence/v2 artifact")
	snapshotPath := fs.String("snapshot", "", "codegraph.kotlin_task_source_snapshot/v1 sidecar")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if *root == "" || *evidencePath == "" || *snapshotPath == "" {
		return fmt.Errorf("--repo-root, --evidence, and --snapshot are required")
	}
	read := func(path string) ([]byte, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, buildevidence.MaxArtifactSize+1))
		if err != nil {
			return nil, err
		}
		if len(data) > buildevidence.MaxArtifactSize {
			return nil, fmt.Errorf("artifact exceeds %d bytes", buildevidence.MaxArtifactSize)
		}
		return data, nil
	}
	evidenceBytes, err := read(*evidencePath)
	if err != nil {
		return fmt.Errorf("read related build evidence: %w", err)
	}
	evidence, err := buildevidence.Decode(evidenceBytes)
	if err != nil {
		return err
	}
	snapshotBytes, err := read(*snapshotPath)
	if err != nil {
		return fmt.Errorf("read source snapshot: %w", err)
	}
	snapshot, err := buildevidence.DecodeSourceSnapshot(snapshotBytes)
	if err != nil {
		return err
	}
	status, err := buildevidence.ValidateSourceSnapshotCurrentContext(ctx, *root, evidence, snapshot)
	if err != nil {
		return err
	}
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, string(data)); err != nil {
		return err
	}
	if status.RecordedContent != buildevidence.Complete || status.BuildEvidence != buildevidence.Complete {
		return fmt.Errorf("recorded source bytes or related build evidence are stale or incomplete; current task selection and compiler-consumed sources remain UNKNOWN")
	}
	return nil
}

func runValidateGradleClasspath(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("validate-gradle-dependency-snapshot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("repo-root", "", "repository root used by the related Gradle evidence")
	evidencePath := fs.String("evidence", "", "related codegraph.gradle_kotlin_evidence/v2 artifact")
	snapshotPath := fs.String("snapshot", "", "codegraph.kotlin_task_classpath_snapshot/v1 sidecar")
	gradleHome := fs.String("gradle-user-home", "", "Gradle user home (defaults to GRADLE_USER_HOME or ~/.gradle)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if *root == "" || *evidencePath == "" || *snapshotPath == "" {
		return fmt.Errorf("--repo-root, --evidence, and --snapshot are required")
	}
	read := func(path string) ([]byte, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, buildevidence.MaxArtifactSize+1))
		if err != nil {
			return nil, err
		}
		if len(data) > buildevidence.MaxArtifactSize {
			return nil, fmt.Errorf("artifact exceeds %d bytes", buildevidence.MaxArtifactSize)
		}
		return data, nil
	}
	evidenceBytes, err := read(*evidencePath)
	if err != nil {
		return fmt.Errorf("read related build evidence: %w", err)
	}
	evidence, err := buildevidence.Decode(evidenceBytes)
	if err != nil {
		return err
	}
	snapshotBytes, err := read(*snapshotPath)
	if err != nil {
		return fmt.Errorf("read dependency snapshot: %w", err)
	}
	snapshot, err := buildevidence.DecodeClasspathSnapshot(snapshotBytes)
	if err != nil {
		return err
	}
	if *gradleHome == "" {
		*gradleHome = os.Getenv("GRADLE_USER_HOME")
		if *gradleHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			*gradleHome = filepath.Join(home, ".gradle")
		}
	}
	status, err := buildevidence.ValidateClasspathSnapshotCurrentContext(ctx, *root, *gradleHome, evidence, snapshot)
	if err != nil {
		return err
	}
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, string(data)); err != nil {
		return err
	}
	if status.BuildEvidence != buildevidence.Complete || status.RecordedContent != buildevidence.Complete {
		return fmt.Errorf("recorded dependency content or related build evidence is stale or incomplete; selection freshness and compiler-consumed completeness remain UNKNOWN")
	}
	return nil
}

package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

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
	return err
}

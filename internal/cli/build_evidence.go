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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	artifact, err := buildevidence.ExportGradle(ctx, buildevidence.GradleRequest{
		RepositoryRoot: *root,
		GradleCommand:  *gradle,
		Project:        *project,
		Compilation:    *compilation,
		OutputPath:     *output,
		Stderr:         stderr,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Wrote Kotlin Gradle evidence for %s/%s (%s)\n", artifact.Compilation.GradleProject, artifact.Compilation.KotlinCompilation, artifact.InputFingerprint)
	return err
}

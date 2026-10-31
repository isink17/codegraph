package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/isink17/codegraph/internal/jvmbinary"
)

func runInspectJVMClass(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("inspect-jvm-class", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "one class file to inspect (maximum 16 MiB)")
	inspectorDir := fs.String("inspector-dir", "", "trusted external inspector installDist directory")
	java := fs.String("java", "", "explicit Java 17 executable path")
	trust := fs.Bool("trust-inspector", false, "authorize executing this external inspector and its runtime dependencies")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("inspect-jvm-class accepts flags only")
	}
	result, err := jvmbinary.Inspect(ctx, jvmbinary.Options{
		InputPath:      *input,
		InspectorDir:   *inspectorDir,
		JavaPath:       *java,
		TrustInspector: *trust,
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

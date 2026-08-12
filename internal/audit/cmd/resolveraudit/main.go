// Command resolveraudit measures the current edge-resolution behaviour of
// CodeGraph against the adversarial fixture in internal/audit.
//
// It is a developer tool, deliberately not registered as a codegraph CLI or MCP
// command, and it never changes resolver behaviour.
//
// Usage:
//
//	go run ./internal/audit/cmd/resolveraudit
//	go run ./internal/audit/cmd/resolveraudit -json out.json
//	go run ./internal/audit/cmd/resolveraudit -repeats 5 -incremental
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/isink17/codegraph/internal/audit"
)

func main() {
	jsonPath := flag.String("json", "", "write the JSON report to this path (- for stdout)")
	repeats := flag.Int("repeats", audit.DefaultRepeats, "independent index runs used to detect nondeterminism")
	incremental := flag.Bool("incremental", false, "also measure the incremental update resolution path")
	quiet := flag.Bool("quiet", false, "suppress the human-readable summary")
	flag.Parse()

	ctx := context.Background()
	report, err := audit.Run(ctx, audit.Options{
		Repeats:                  *repeats,
		IncludeIncrementalParity: *incremental,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolveraudit: %v\n", err)
		os.Exit(1)
	}

	report.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	report.Command = append([]string{"go", "run", "./internal/audit/cmd/resolveraudit"}, os.Args[1:]...)
	report.Context.CommitSHA = commitSHA()

	if !*quiet {
		if err := report.WriteText(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "resolveraudit: %v\n", err)
			os.Exit(1)
		}
	}

	switch *jsonPath {
	case "":
	case "-":
		if err := report.WriteJSON(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "resolveraudit: %v\n", err)
			os.Exit(1)
		}
	default:
		file, err := os.Create(*jsonPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "resolveraudit: %v\n", err)
			os.Exit(1)
		}
		defer file.Close()
		if err := report.WriteJSON(file); err != nil {
			fmt.Fprintf(os.Stderr, "resolveraudit: %v\n", err)
			os.Exit(1)
		}
	}
}

// commitSHA records which checkout was measured. Absence is not an error.
func commitSHA() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

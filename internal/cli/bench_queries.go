package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/querybench"
)

// runBenchQueries measures the latency of the local graph queries against a
// repository that is already indexed.
//
// The command is observational in the same sense `audit` is: it opens the
// existing database read-only, never indexes, never migrates, and never writes
// synthetic rows into the user's graph. Its scenarios take their arguments from
// the repository's own graph -- the most-called symbol, the most-linked test
// file -- so two runs against the same index ask exactly the same questions. A
// scenario the graph cannot supply a target for is reported as skipped, with
// the reason; it is never replaced by an invented query.
//
// --fail-over-budget exists for CI. It changes the exit status only, and only
// after the report has been written, so the JSON is byte-identical whether the
// policy is on or off.
func runBenchQueries(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("bench_queries", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRootFlag := fs.String("repo-root", "", "repository root to benchmark (optional)")
	runs := fs.Int("runs", querybench.DefaultRuns, "measured calls per scenario")
	warmup := fs.Int("warmup", querybench.DefaultWarmup, "unmeasured warmup calls per scenario")
	budget := fs.Float64("budget-ms", querybench.DefaultBudgetMS, "p95 latency budget per scenario, in milliseconds (0 uses the default)")
	failOverBudget := fs.Bool("fail-over-budget", false, "exit non-zero when a measured scenario misses the budget")

	repoRootCandidate, err := parseOptionalRepoRootArg(fs, args, repoRootFlag, "")
	if err != nil {
		return err
	}
	// Validate arguments before touching the filesystem, so a typo never
	// depends on whether the repository happens to be indexed.
	if *runs < 1 {
		return fmt.Errorf("invalid --runs %d: want 1 or more", *runs)
	}
	if *warmup < 0 {
		return fmt.Errorf("invalid --warmup %d: want 0 or more", *warmup)
	}
	if *budget < 0 {
		return fmt.Errorf("invalid --budget-ms %v: want 0 or more", *budget)
	}

	opened, err := openIndexedRepoReadOnly(ctx, cfg, repoRootCandidate)
	if err != nil {
		return err
	}
	defer opened.Close()

	report, err := querybench.Run(ctx, opened.Store, opened.Repo.ID, opened.Root, querybench.Options{
		Runs:     *runs,
		Warmup:   *warmup,
		BudgetMS: *budget,
	})
	if err != nil {
		return err
	}
	if err := writeJSON(stdout, report); err != nil {
		return err
	}
	if *failOverBudget && report.OverBudget() {
		return fmt.Errorf("%d of %d measured scenarios exceeded the %.0fms budget (slowest: %s at %.3fms)",
			report.Summary.OverBudget, report.Summary.Measured, report.BudgetMS,
			report.Summary.Slowest, report.Summary.SlowestP95MS)
	}
	return nil
}

// Package querybench measures the latency of CodeGraph's local graph queries
// against a repository that is already indexed.
//
// It is the engine behind `codegraph bench-queries`. Everything here is
// read-only: the store handle it is given is expected to be opened with
// store.OpenReadOnly, and no code path in this package writes a row, creates a
// file, or triggers a migration.
//
// The report is intentionally scenario-shaped rather than sample-shaped. Output
// size scales with the number of scenarios, not with the number of runs, so
// raising --runs buys a better tail estimate without producing a bigger
// document to read.
package querybench

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/isink17/codegraph/internal/latency"
	"github.com/isink17/codegraph/internal/limits"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// Schema identifies the report format. Bump the version if the shape changes.
const Schema = "codegraph.query_bench/v1"

// Defaults for the benchmark run.
const (
	DefaultRuns   = 20
	DefaultWarmup = 3
	// DefaultBudgetMS is the P12 latency target for a warm, bounded local
	// graph query. It is a reporting threshold, not an enforced limit: a
	// scenario over budget is still measured and still reported.
	DefaultBudgetMS = 50.0
)

// Status values for a scenario result.
const (
	StatusMeasured = "measured"
	StatusSkipped  = "skipped"
)

// Options controls a benchmark run.
type Options struct {
	// Runs is the number of measured calls per scenario.
	Runs int
	// Warmup is the number of unmeasured calls per scenario, so the first
	// touch of a cold b-tree page is not reported as query latency. Zero means
	// no warmup -- a caller that wants none must be able to say so, which is
	// why this field alone does not treat zero as "use the default". The CLI
	// supplies DefaultWarmup as its flag default instead.
	Warmup int
	// BudgetMS is the per-scenario p95 threshold reported as budget_met. Zero
	// means DefaultBudgetMS, so a zero-value Options is a sensible run; a
	// caller that wants a deliberately tiny budget passes a small positive
	// value rather than zero.
	BudgetMS float64
}

func (o Options) normalized() (Options, error) {
	if o.Runs == 0 {
		o.Runs = DefaultRuns
	}
	if o.Warmup < 0 {
		return Options{}, fmt.Errorf("warmup must be >= 0, got %d", o.Warmup)
	}
	if o.Runs < 1 {
		return Options{}, fmt.Errorf("runs must be >= 1, got %d", o.Runs)
	}
	if o.BudgetMS == 0 {
		o.BudgetMS = DefaultBudgetMS
	}
	if o.BudgetMS < 0 {
		return Options{}, fmt.Errorf("budget must be >= 0, got %v", o.BudgetMS)
	}
	return o, nil
}

// Repository summarises the graph the scenarios ran against, so a latency
// number can be read against the size that produced it.
type Repository struct {
	Root       string `json:"root"`
	Files      int64  `json:"files"`
	Symbols    int64  `json:"symbols"`
	Edges      int64  `json:"edges"`
	References int64  `json:"references"`
}

// Result is one scenario's measurement.
type Result struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Target is the query argument selected from the graph, when the scenario
	// takes one. It is reported so a reader can reproduce the exact call.
	Target string `json:"target,omitempty"`
	// SkipReason explains a skipped scenario. A scenario is skipped when the
	// graph has no target to select, never to hide a slow result.
	SkipReason string `json:"skip_reason,omitempty"`

	Samples     int     `json:"samples"`
	P50MS       float64 `json:"p50_ms"`
	P95MS       float64 `json:"p95_ms"`
	MaxMS       float64 `json:"max_ms"`
	ResultCount int     `json:"result_count"`
	BudgetMet   bool    `json:"budget_met"`
}

// Summary is the roll-up a CI job or a human reads first.
type Summary struct {
	Measured     int     `json:"measured"`
	Skipped      int     `json:"skipped"`
	WithinBudget int     `json:"within_budget"`
	OverBudget   int     `json:"over_budget"`
	Slowest      string  `json:"slowest,omitempty"`
	SlowestP95MS float64 `json:"slowest_p95_ms"`
}

// Report is the full machine-readable result. CLI and any future MCP tool are
// expected to render this exact struct; there is no second serialisation.
type Report struct {
	Schema     string     `json:"schema"`
	Repository Repository `json:"repository"`
	Runs       int        `json:"runs"`
	Warmup     int        `json:"warmup"`
	BudgetMS   float64    `json:"budget_ms"`
	Results    []Result   `json:"results"`
	Summary    Summary    `json:"summary"`
}

// OverBudget reports whether any measured scenario missed the budget. It is the
// only thing a failure policy needs to consult, and it is derived from the
// report that was already emitted rather than recomputed from the samples.
func (r Report) OverBudget() bool { return r.Summary.OverBudget > 0 }

// scenario is one benchmarked query.
type scenario struct {
	name string
	// target is the graph-derived argument, empty when the scenario takes none.
	target string
	// skip, when non-empty, means the graph offered no target for this
	// scenario.
	skip string
	run  func(ctx context.Context) (int, error)
}

// Run benchmarks the repository's local graph queries.
//
// repoRoot is used only for reporting; every query is issued against repoID in
// the supplied store.
func Run(ctx context.Context, s *store.Store, repoID int64, repoRoot string, opts Options) (Report, error) {
	opts, err := opts.normalized()
	if err != nil {
		return Report{}, err
	}

	stats, err := s.Stats(ctx, repoID)
	if err != nil {
		return Report{}, fmt.Errorf("repository stats: %w", err)
	}
	targets, err := s.QueryBenchTargets(ctx, repoID)
	if err != nil {
		return Report{}, fmt.Errorf("select benchmark targets: %w", err)
	}

	report := Report{
		Schema: Schema,
		Repository: Repository{
			Root:       repoRoot,
			Files:      stats.Files,
			Symbols:    stats.Symbols,
			Edges:      stats.Edges,
			References: stats.References,
		},
		Runs:     opts.Runs,
		Warmup:   opts.Warmup,
		BudgetMS: opts.BudgetMS,
		Results:  []Result{},
	}

	for _, sc := range buildScenarios(s, repoID, targets) {
		result, err := measure(ctx, sc, opts)
		if err != nil {
			return Report{}, fmt.Errorf("scenario %s: %w", sc.name, err)
		}
		report.Results = append(report.Results, result)
	}
	report.Summary = summarize(report.Results)
	return report, nil
}

func buildScenarios(s *store.Store, repoID int64, t store.QueryBenchTargets) []scenario {
	const limit = 20

	// context_for_task is a service-level aggregate (semantic search, graph
	// expansion, ranking, budgeting), so it needs the query service rather than
	// the store alone. A nil embedder keeps it on the deterministic token-overlap
	// search path: no Ollama, no network, same questions on every run.
	svc := query.New(s, nil)

	needSymbol := func(name string) string {
		if name == "" {
			return "repository has no resolved edges, so no representative symbol could be selected"
		}
		return ""
	}

	return []scenario{
		{
			name: "graph_stats",
			run: func(ctx context.Context) (int, error) {
				st, err := s.Stats(ctx, repoID)
				return len(st.Languages), err
			},
		},
		{
			name: "list_files",
			run: func(ctx context.Context) (int, error) {
				out, err := s.ListFiles(ctx, repoID, "", limit, 0)
				return len(out), err
			},
		},
		{
			name:   "symbol_lookup",
			target: t.CallerTarget,
			skip:   needSymbol(t.CallerTarget),
			run: func(ctx context.Context) (int, error) {
				out, err := s.FindSymbolExact(ctx, repoID, t.CallerTarget, limit, 0)
				return len(out), err
			},
		},
		{
			name:   "symbol_search",
			target: t.SearchTerm,
			skip:   needSymbol(t.SearchTerm),
			run: func(ctx context.Context) (int, error) {
				out, err := s.SearchSymbols(ctx, repoID, t.SearchTerm, limit, 0)
				return len(out), err
			},
		},
		{
			name:   "find_callers",
			target: t.CallerTarget,
			skip:   needSymbol(t.CallerTarget),
			run: func(ctx context.Context) (int, error) {
				out, err := s.FindCallers(ctx, repoID, t.CallerTarget, 0, limit, 0)
				return len(out), err
			},
		},
		{
			name:   "find_callees",
			target: t.CalleeSource,
			skip:   needSymbol(t.CalleeSource),
			run: func(ctx context.Context) (int, error) {
				out, err := s.FindCallees(ctx, repoID, t.CalleeSource, 0, limit, 0)
				return len(out), err
			},
		},
		{
			name:   "related_tests",
			target: t.TestLinkedFile,
			skip: func() string {
				if t.TestLinkedFile == "" {
					return "repository has no resolved test links, so no target file could be selected"
				}
				return ""
			}(),
			run: func(ctx context.Context) (int, error) {
				out, err := s.RelatedTests(ctx, repoID, "", t.TestLinkedFile, limit, 0)
				return len(out), err
			},
		},
		{
			name:   "impact_radius",
			target: t.CallerTarget,
			skip:   needSymbol(t.CallerTarget),
			run: func(ctx context.Context) (int, error) {
				out, err := s.ImpactRadius(ctx, repoID, []string{t.CallerTarget}, nil, 2, limits.MaxPage, 0)
				return impactSize(out), err
			},
		},
		{
			name:   "trace_dependencies",
			target: t.CalleeSource,
			skip:   needSymbol(t.CalleeSource),
			run: func(ctx context.Context) (int, error) {
				out, _, err := s.TraceDependencies(ctx, repoID, t.CalleeSource, "downstream", 3, limits.MaxPage, 0)
				return len(out), err
			},
		},
		{
			name: "dead_code",
			run: func(ctx context.Context) (int, error) {
				out, err := s.FindDeadCode(ctx, repoID, limit, 0)
				return len(out), err
			},
		},
		{
			// The task is the graph-derived search term, not an invented sentence,
			// so the scenario asks the same question of the same index every run.
			name:   "context_for_task",
			target: t.SearchTerm,
			skip:   needSymbol(t.SearchTerm),
			run: func(ctx context.Context) (int, error) {
				out, err := svc.ContextForTask(ctx, repoID, t.SearchTerm, query.ContextForTaskOptions{
					IncludeCallers: true,
					IncludeTests:   true,
				})
				if err != nil {
					return 0, err
				}
				return out.ReturnedSymbols, nil
			},
		},
		{
			name: "architecture_overview",
			run: func(ctx context.Context) (int, error) {
				out, err := s.ArchitectureOverview(ctx, repoID)
				return len(out), err
			},
		},
	}
}

func impactSize(out map[string]any) int {
	summary, ok := out["summary"].(map[string]any)
	if !ok {
		return 0
	}
	n, _ := summary["affected_symbols"].(int)
	return n
}

func measure(ctx context.Context, sc scenario, opts Options) (Result, error) {
	if sc.skip != "" {
		return Result{
			Name:       sc.name,
			Status:     StatusSkipped,
			Target:     sc.target,
			SkipReason: sc.skip,
		}, nil
	}

	for i := 0; i < opts.Warmup; i++ {
		if _, err := sc.run(ctx); err != nil {
			return Result{}, err
		}
	}

	samples := make(latency.Samples, 0, opts.Runs)
	var count int
	for i := 0; i < opts.Runs; i++ {
		start := time.Now()
		n, err := sc.run(ctx)
		elapsed := time.Since(start)
		if err != nil {
			return Result{}, err
		}
		samples = append(samples, elapsed)
		count = n
	}

	sorted := samples.Sorted()
	p95 := latency.Millis(sorted.SortedPercentile(95))
	return Result{
		Name:        sc.name,
		Status:      StatusMeasured,
		Target:      sc.target,
		Samples:     len(sorted),
		P50MS:       latency.Millis(sorted.SortedPercentile(50)),
		P95MS:       p95,
		MaxMS:       latency.Millis(sorted.Max()),
		ResultCount: count,
		BudgetMet:   p95 <= opts.BudgetMS,
	}, nil
}

func summarize(results []Result) Summary {
	var out Summary
	for _, r := range results {
		if r.Status == StatusSkipped {
			out.Skipped++
			continue
		}
		out.Measured++
		if r.BudgetMet {
			out.WithinBudget++
		} else {
			out.OverBudget++
		}
		if r.P95MS > out.SlowestP95MS || (r.P95MS == out.SlowestP95MS && out.Slowest == "") {
			out.Slowest = r.Name
			out.SlowestP95MS = r.P95MS
		}
	}
	// Ties resolve to the alphabetically first name so the summary is stable.
	if out.Measured > 0 {
		tied := make([]string, 0, 2)
		for _, r := range results {
			if r.Status == StatusMeasured && r.P95MS == out.SlowestP95MS {
				tied = append(tied, r.Name)
			}
		}
		sort.Strings(tied)
		if len(tied) > 0 {
			out.Slowest = tied[0]
		}
	}
	return out
}

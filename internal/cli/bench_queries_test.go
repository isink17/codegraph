package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/querybench"
	"github.com/isink17/codegraph/internal/store"
)

// benchRepo builds an indexed repository with a small but real graph, so every
// scenario has a target to select.
func benchRepo(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()
	dbPath := filepath.Join(repoRoot, ".codegraph", "codegraph.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}

	sym := func(name string) graph.Symbol {
		return graph.Symbol{
			Language: "go", Kind: "function", Name: name, QualifiedName: "pkg." + name,
			Signature: "func " + name + "()", DocSummary: name + " does work",
			Range:     graph.Position{StartLine: 1, StartCol: 1, EndLine: 5, EndCol: 1},
			StableKey: "go:pkg." + name,
		}
	}
	target := graph.ParsedFile{Language: "go"}
	target.Symbols = append(target.Symbols, sym("Target"))
	caller := graph.ParsedFile{Language: "go"}
	caller.Symbols = append(caller.Symbols, sym("Caller"))
	caller.Edges = append(caller.Edges, graph.Edge{DstName: "pkg.Target", Kind: "calls", Evidence: "x", Line: 2})
	testFile := graph.ParsedFile{Language: "go"}
	testFile.Symbols = append(testFile.Symbols, sym("TestTarget"))
	testFile.TestLinks = append(testFile.TestLinks, graph.TestLink{
		TestName: "TestTarget", TargetName: "pkg.Target", Reason: "name_match", Score: 0.9,
		TestSymbolKey: "go:pkg.TestTarget", TargetStableKey: "go:pkg.Target",
	})

	if _, err := s.ReplaceFileGraphsBatch(ctx, repo.ID, 1, []store.ReplaceFileGraphInput{
		{Path: "target.go", Language: "go", SizeBytes: 10, ContentHash: "a", Parsed: target},
		{Path: "caller.go", Language: "go", SizeBytes: 10, ContentHash: "b", Parsed: caller},
		{Path: "target_test.go", Language: "go", SizeBytes: 10, ContentHash: "c", Parsed: testFile},
	}); err != nil {
		t.Fatalf("ReplaceFileGraphsBatch: %v", err)
	}
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatalf("ResolveEdges: %v", err)
	}
	if _, err := s.ResolveTestLinks(ctx, repo.ID); err != nil {
		t.Fatalf("ResolveTestLinks: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return repoRoot
}

func TestBenchQueriesHelpContract(t *testing.T) {
	quietStartup(t)

	root, _, err := runCLI(t, "--help")
	if err != nil {
		t.Fatalf("Run(--help) error = %v", err)
	}
	if !strings.Contains(root, "bench_queries") {
		t.Fatalf("root help does not list the bench_queries command:\n%s", root)
	}

	// Both the canonical name and the kebab-case alias must resolve.
	for _, invoked := range []string{"bench_queries", "bench-queries"} {
		out, _, err := runCLI(t, invoked, "--help")
		if err != nil {
			t.Fatalf("Run(%s --help) error = %v", invoked, err)
		}
		for _, want := range []string{"--repo-root", "--runs", "--warmup", "--budget-ms", "--fail-over-budget"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s --help is missing %q:\n%s", invoked, want, out)
			}
		}
	}
}

func TestBenchQueriesRejectsInvalidFlagsBeforeTouchingDisk(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot := t.TempDir() // deliberately not indexed

	for _, args := range [][]string{
		{"bench-queries", repoRoot, "--runs", "0"},
		{"bench-queries", repoRoot, "--runs", "-3"},
		{"bench-queries", repoRoot, "--warmup", "-1"},
		{"bench-queries", repoRoot, "--budget-ms", "-1"},
	} {
		if _, _, err := runCLI(t, args...); err == nil {
			t.Errorf("%v succeeded, want a validation error", args)
		}
	}
	// Argument validation must happen before the repository is consulted, so a
	// bad flag never creates anything on disk.
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected invocation created %d entries in the repo root", len(entries))
	}
}

// TestBenchQueriesRejectsUnindexedRepoWithoutCreatingOne is the read-only
// contract: benchmarking a repository that was never indexed must fail, and
// must not leave a database behind.
func TestBenchQueriesRejectsUnindexedRepoWithoutCreatingOne(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot := t.TempDir()

	out, _, err := runCLI(t, "bench-queries", repoRoot)
	if err == nil {
		t.Fatalf("bench-queries on an unindexed repo succeeded, output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not indexed") {
		t.Errorf("error = %v, want it to say the repository is not indexed", err)
	}
	entries, readErr := os.ReadDir(repoRoot)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("bench-queries created %d entries in an unindexed repo", len(entries))
	}
}

func TestBenchQueriesJSONShape(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot := benchRepo(t)

	out, _, err := runCLI(t, "bench-queries", repoRoot, "--runs", "2", "--warmup", "0")
	if err != nil {
		t.Fatalf("bench-queries error = %v\n%s", err, out)
	}
	var report querybench.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if report.Schema != querybench.Schema {
		t.Errorf("schema = %q, want %q", report.Schema, querybench.Schema)
	}
	if report.Runs != 2 || report.Warmup != 0 {
		t.Errorf("run parameters not echoed: runs=%d warmup=%d", report.Runs, report.Warmup)
	}
	if report.Repository.Root != repoRoot {
		t.Errorf("repository root = %q, want %q", report.Repository.Root, repoRoot)
	}
	if len(report.Results) == 0 {
		t.Fatal("report has no results")
	}
	if report.Summary.Measured == 0 {
		t.Fatal("no scenario was measured")
	}
	// The seeded graph supplies a target for every graph-derived scenario.
	for _, want := range []string{"find_callers", "related_tests"} {
		found := false
		for _, res := range report.Results {
			if res.Name == want && res.Status == querybench.StatusMeasured {
				found = true
			}
		}
		if !found {
			t.Errorf("scenario %q was not measured on a graph that supplies its target", want)
		}
	}
}

// TestBenchQueriesDoesNotWriteToTheDatabase is the read-only proof at the
// command level: the database file and its row counts are unchanged, and no
// migration ran.
func TestBenchQueriesDoesNotWriteToTheDatabase(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot := benchRepo(t)
	dbPath := filepath.Join(repoRoot, ".codegraph", "codegraph.sqlite")

	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, true)
	if err != nil {
		t.Fatalf("BuildSQLiteDSN: %v", err)
	}
	db, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	snapshot := func() string {
		t.Helper()
		var parts []string
		for _, table := range []string{"files", "symbols", "edges", "references_tbl", "test_links", "scans", "schema_migrations"} {
			var n int64
			if err := db.QueryRow(`SELECT COUNT(1) FROM ` + table).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			parts = append(parts, table+"="+strconv.FormatInt(n, 10))
		}
		return strings.Join(parts, ",")
	}

	before := snapshot()
	beforeStat, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	if _, _, err := runCLI(t, "bench-queries", repoRoot, "--runs", "2", "--warmup", "1"); err != nil {
		t.Fatalf("bench-queries error = %v", err)
	}

	if after := snapshot(); after != before {
		t.Fatalf("bench-queries changed the graph:\n before: %s\n after:  %s", before, after)
	}
	afterStat, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if afterStat.Size() != beforeStat.Size() {
		t.Fatalf("database size changed from %d to %d bytes", beforeStat.Size(), afterStat.Size())
	}
}

// benchReport builds a report with a known budget outcome. Constructed rather
// than measured: the contract under test is "what the policy does with a given
// report", and feeding it real timings would make the assertion depend on the
// host's clock resolution.
func benchReport(overBudget int) querybench.Report {
	r := querybench.Report{
		Schema:   querybench.Schema,
		Runs:     2,
		Warmup:   0,
		BudgetMS: querybench.DefaultBudgetMS,
		Results: []querybench.Result{
			{Name: "graph_stats", Status: querybench.StatusMeasured, Samples: 2, P50MS: 1, P95MS: 1, MaxMS: 1, BudgetMet: true},
			{Name: "related_tests", Status: querybench.StatusSkipped, SkipReason: "no test links"},
		},
	}
	r.Summary = querybench.Summary{Measured: 1, Skipped: 1, WithinBudget: 1}
	for i := 0; i < overBudget; i++ {
		r.Results = append(r.Results, querybench.Result{
			Name: fmt.Sprintf("slow_%d", i), Status: querybench.StatusMeasured,
			Samples: 2, P50MS: 900, P95MS: 999, MaxMS: 1000, BudgetMet: false,
		})
		r.Summary.Measured++
		r.Summary.OverBudget++
		r.Summary.Slowest = "slow_0"
		r.Summary.SlowestP95MS = 999
	}
	return r
}

// TestBenchQueryFailPolicyError pins the exit-status policy itself: for a given
// report, the flag is the only thing that decides whether there is an error.
func TestBenchQueryFailPolicyError(t *testing.T) {
	cases := []struct {
		name           string
		overBudget     int
		failOverBudget bool
		wantErr        bool
	}{
		{"over budget, policy off", 2, false, false},
		{"over budget, policy on", 2, true, true},
		{"within budget, policy off", 0, false, false},
		{"within budget, policy on", 0, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := benchReport(tc.overBudget)
			err := benchQueryFailPolicyError(report, tc.failOverBudget)
			if (err != nil) != tc.wantErr {
				t.Fatalf("benchQueryFailPolicyError(overBudget=%d, policy=%v) = %v, wantErr=%v",
					tc.overBudget, tc.failOverBudget, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "slow_0") {
				t.Errorf("error does not name the slowest scenario: %v", err)
			}
		})
	}
}

// TestEmitBenchQueryReportWritesBeforeFailing pins the ordering and the
// byte-identity: the same report produces the same stdout under either policy,
// and the failing case still emits it in full before returning the error.
func TestEmitBenchQueryReportWritesBeforeFailing(t *testing.T) {
	report := benchReport(3)

	var withoutPolicy bytes.Buffer
	if err := emitBenchQueryReport(&withoutPolicy, report, false); err != nil {
		t.Fatalf("policy off: unexpected error %v", err)
	}
	var withPolicy bytes.Buffer
	err := emitBenchQueryReport(&withPolicy, report, true)
	if err == nil {
		t.Fatal("policy on: over-budget report returned no error")
	}

	if withPolicy.String() != withoutPolicy.String() {
		t.Fatalf("the policy changed the emitted bytes:\n--- off ---\n%s\n--- on ---\n%s",
			withoutPolicy.String(), withPolicy.String())
	}
	// The bytes must be a complete, parseable report, not a truncated prefix
	// written before the error path bailed out.
	var parsed querybench.Report
	if jsonErr := json.Unmarshal(withPolicy.Bytes(), &parsed); jsonErr != nil {
		t.Fatalf("emitted output is not a valid report: %v\n%s", jsonErr, withPolicy.String())
	}
	if parsed.Summary.OverBudget != report.Summary.OverBudget || len(parsed.Results) != len(report.Results) {
		t.Fatalf("emitted report does not match the input: %+v", parsed.Summary)
	}
}

// TestBenchQueriesEmitsAReportWhicheverWayThePolicyGoes is the integration-level
// half of the same contract.
//
// It deliberately does not assert *whether* a one-microsecond budget is missed:
// that depends on the host clock's resolution, and on Windows a sub-millisecond
// query can measure as 0.000ms and meet the budget. Comparing two live
// benchmark runs' budget outcomes -- which an earlier version of this test did
// -- is comparing two different wall-clock measurements and is nondeterministic
// by construction. What is invariant, and is asserted, is that the command
// emits a complete report that agrees with its own exit status either way.
func TestBenchQueriesEmitsAReportWhicheverWayThePolicyGoes(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot := benchRepo(t)

	args := []string{"bench-queries", repoRoot, "--runs", "2", "--warmup", "0",
		"--budget-ms", "0.001", "--fail-over-budget"}
	out, _, err := runCLI(t, args...)

	report := mustReport(t, out)
	if report.Schema != querybench.Schema {
		t.Fatalf("schema = %q, want %q", report.Schema, querybench.Schema)
	}
	if report.Summary.Measured == 0 {
		t.Fatal("no scenario was measured")
	}
	if (err != nil) != (report.Summary.OverBudget > 0) {
		t.Fatalf("exit status and report disagree: err=%v, over_budget=%d",
			err, report.Summary.OverBudget)
	}
}

func mustReport(t *testing.T, s string) querybench.Report {
	t.Helper()
	var r querybench.Report
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return r
}

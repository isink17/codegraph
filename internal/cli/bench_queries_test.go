package cli

import (
	"context"
	"database/sql"
	"encoding/json"
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

// TestBenchQueriesFailPolicyChangesOnlyTheExitStatus pins the CI contract: the
// report is emitted first, and it is byte-identical with and without the
// policy.
func TestBenchQueriesFailPolicyChangesOnlyTheExitStatus(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot := benchRepo(t)

	// A budget of one microsecond puts every measured scenario over budget: the
	// fastest query on this graph is tens of microseconds.
	base := []string{"bench-queries", repoRoot, "--runs", "2", "--warmup", "0", "--budget-ms", "0.001"}

	plain, _, plainErr := runCLI(t, base...)
	if plainErr != nil {
		t.Fatalf("without --fail-over-budget: %v", plainErr)
	}
	failing, _, failErr := runCLI(t, append(append([]string{}, base...), "--fail-over-budget")...)
	if failErr == nil {
		t.Fatal("--fail-over-budget did not fail on an over-budget report")
	}
	if failing == "" {
		t.Fatal("--fail-over-budget suppressed the report")
	}

	// Timings differ run to run, so compare the structure rather than the bytes.
	normalize := func(s string) string {
		var r querybench.Report
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		var b strings.Builder
		b.WriteString(r.Schema)
		for _, res := range r.Results {
			b.WriteString("|" + res.Name + ":" + res.Status + ":" + res.Target + ":" + strconv.FormatBool(res.BudgetMet))
		}
		b.WriteString("|over=" + strconv.Itoa(r.Summary.OverBudget))
		return b.String()
	}
	if normalize(plain) != normalize(failing) {
		t.Fatalf("report differs with the failure policy:\n%s\n%s", normalize(plain), normalize(failing))
	}
	if r := mustReport(t, failing); r.Summary.OverBudget == 0 {
		t.Fatalf("a one-microsecond budget produced no over-budget scenario: %+v", r.Summary)
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

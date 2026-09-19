package querybench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

// seededRepo builds a small but genuinely shaped graph: one hot target with
// several callers, one high fan-out source, and (optionally) a test link, so
// every scenario has a real target to select.
func seededRepo(t *testing.T, withTestLinks bool) (*store.Store, int64, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, dir)
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	if err := s.EnsureCanonicalRepositoryPaths(ctx, repo.ID, true); err != nil {
		t.Fatalf("EnsureCanonicalRepositoryPaths: %v", err)
	}

	sym := func(name string, line int) graph.Symbol {
		return graph.Symbol{
			Language: "go", Kind: "function", Name: name, QualifiedName: "pkg." + name,
			Signature: "func " + name + "()", DocSummary: name + " does work",
			Range:     graph.Position{StartLine: line, StartCol: 1, EndLine: line + 5, EndCol: 1},
			StableKey: "go:pkg." + name,
		}
	}

	target := graph.ParsedFile{Language: "go"}
	target.Symbols = append(target.Symbols, sym("Target", 1), sym("Helper", 21))
	inputs := []store.ReplaceFileGraphInput{
		{Path: "target.go", Language: "go", SizeBytes: 100, ContentHash: "t", Parsed: target},
	}
	for i := 0; i < 4; i++ {
		pf := graph.ParsedFile{Language: "go"}
		pf.Symbols = append(pf.Symbols, sym(fmt.Sprintf("Caller%d", i), 1))
		pf.Edges = append(pf.Edges, graph.Edge{DstName: "pkg.Target", Kind: "calls", Evidence: "x", Line: 2})
		inputs = append(inputs, store.ReplaceFileGraphInput{
			Path: fmt.Sprintf("caller%d.go", i), Language: "go", SizeBytes: 100,
			ContentHash: fmt.Sprintf("c%d", i), Parsed: pf,
		})
	}
	fanout := graph.ParsedFile{Language: "go"}
	fanout.Symbols = append(fanout.Symbols, sym("Fanout", 1))
	for i := 0; i < 4; i++ {
		fanout.Edges = append(fanout.Edges, graph.Edge{
			DstName: fmt.Sprintf("pkg.Caller%d", i), Kind: "calls", Evidence: "x", Line: 2,
		})
	}
	inputs = append(inputs, store.ReplaceFileGraphInput{
		Path: "fanout.go", Language: "go", SizeBytes: 100, ContentHash: "f", Parsed: fanout,
	})

	if withTestLinks {
		tf := graph.ParsedFile{Language: "go"}
		tf.Symbols = append(tf.Symbols, sym("TestTarget", 1))
		tf.TestLinks = append(tf.TestLinks, graph.TestLink{
			TestName: "TestTarget", TargetName: "pkg.Target", Reason: "name_match", Score: 0.9,
			TestSymbolKey: "go:pkg.TestTarget", TargetStableKey: "go:pkg.Target",
		})
		inputs = append(inputs, store.ReplaceFileGraphInput{
			Path: "target_test.go", Language: "go", SizeBytes: 100, ContentHash: "tt", Parsed: tf,
		})
	}

	if _, err := s.ReplaceFileGraphsBatch(ctx, repo.ID, 1, inputs); err != nil {
		t.Fatalf("ReplaceFileGraphsBatch: %v", err)
	}
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatalf("ResolveEdges: %v", err)
	}
	if _, err := s.ResolveTestLinks(ctx, repo.ID); err != nil {
		t.Fatalf("ResolveTestLinks: %v", err)
	}
	return s, repo.ID, dir
}

func resultByName(t *testing.T, r Report, name string) Result {
	t.Helper()
	for _, res := range r.Results {
		if res.Name == name {
			return res
		}
	}
	t.Fatalf("no scenario named %q in report", name)
	return Result{}
}

func TestReportShapeAndBudgetAccounting(t *testing.T) {
	ctx := context.Background()
	s, repoID, root := seededRepo(t, true)

	report, err := Run(ctx, s, repoID, root, Options{Runs: 5, Warmup: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Schema != Schema {
		t.Errorf("schema = %q, want %q", report.Schema, Schema)
	}
	if report.Runs != 5 || report.Warmup != 1 || report.BudgetMS != DefaultBudgetMS {
		t.Errorf("run parameters not echoed: %+v", report)
	}
	if report.Repository.Symbols == 0 || report.Repository.Edges == 0 {
		t.Errorf("repository summary is empty: %+v", report.Repository)
	}

	if report.Summary.Measured+report.Summary.Skipped != len(report.Results) {
		t.Errorf("summary counts %d+%d do not cover %d results",
			report.Summary.Measured, report.Summary.Skipped, len(report.Results))
	}
	if report.Summary.WithinBudget+report.Summary.OverBudget != report.Summary.Measured {
		t.Errorf("budget counts %d+%d != measured %d",
			report.Summary.WithinBudget, report.Summary.OverBudget, report.Summary.Measured)
	}

	for _, res := range report.Results {
		if res.Status != StatusMeasured {
			continue
		}
		if res.Samples != 5 {
			t.Errorf("%s samples = %d, want 5", res.Name, res.Samples)
		}
		if res.P50MS > res.P95MS || res.P95MS > res.MaxMS {
			t.Errorf("%s percentiles out of order: p50=%v p95=%v max=%v",
				res.Name, res.P50MS, res.P95MS, res.MaxMS)
		}
		if res.BudgetMet != (res.P95MS <= report.BudgetMS) {
			t.Errorf("%s budget_met=%v disagrees with p95=%v budget=%v",
				res.Name, res.BudgetMet, res.P95MS, report.BudgetMS)
		}
	}

	// Every scenario that takes a graph-derived argument must report it.
	for _, name := range []string{"symbol_lookup", "find_callers", "find_callees", "related_tests"} {
		if res := resultByName(t, report, name); res.Target == "" {
			t.Errorf("%s has no reported target", name)
		}
	}
}

// TestOutputSizeIsIndependentOfRuns pins the report contract: raising --runs
// must buy a better tail estimate, not a bigger document.
func TestOutputSizeIsIndependentOfRuns(t *testing.T) {
	ctx := context.Background()
	s, repoID, root := seededRepo(t, true)

	small, err := Run(ctx, s, repoID, root, Options{Runs: 2, Warmup: 0})
	if err != nil {
		t.Fatalf("Run(2): %v", err)
	}
	large, err := Run(ctx, s, repoID, root, Options{Runs: 40, Warmup: 0})
	if err != nil {
		t.Fatalf("Run(40): %v", err)
	}
	if len(small.Results) != len(large.Results) {
		t.Fatalf("scenario count changed with runs: %d vs %d", len(small.Results), len(large.Results))
	}
	smallJSON, _ := json.Marshal(small)
	largeJSON, _ := json.Marshal(large)
	// Allow for differing digit counts in the timings, but nothing structural.
	if len(largeJSON) > len(smallJSON)+len(large.Results)*40 {
		t.Fatalf("report grew with runs: %d bytes at runs=2, %d bytes at runs=40",
			len(smallJSON), len(largeJSON))
	}
}

// TestScenarioSelectionIsStable pins that two runs against the same index ask
// the same questions. Without this, a latency delta could come from a different
// target rather than a different implementation.
func TestScenarioSelectionIsStable(t *testing.T) {
	ctx := context.Background()
	s, repoID, root := seededRepo(t, true)

	names := func(r Report) string {
		var b strings.Builder
		for _, res := range r.Results {
			fmt.Fprintf(&b, "%s=%s/%s;", res.Name, res.Status, res.Target)
		}
		return b.String()
	}
	first, err := Run(ctx, s, repoID, root, Options{Runs: 1, Warmup: 0})
	if err != nil {
		t.Fatalf("Run(first): %v", err)
	}
	second, err := Run(ctx, s, repoID, root, Options{Runs: 1, Warmup: 0})
	if err != nil {
		t.Fatalf("Run(second): %v", err)
	}
	if names(first) != names(second) {
		t.Fatalf("scenario selection changed between runs:\n%s\n%s", names(first), names(second))
	}
}

// TestSkippedScenarioIsExplainedNotFabricated pins that a graph with nothing to
// select produces a skip with a reason, not an invented query.
func TestSkippedScenarioIsExplainedNotFabricated(t *testing.T) {
	ctx := context.Background()
	s, repoID, root := seededRepo(t, false)

	report, err := Run(ctx, s, repoID, root, Options{Runs: 1, Warmup: 0})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := resultByName(t, report, "related_tests")
	if res.Status != StatusSkipped {
		t.Fatalf("related_tests status = %q, want %q", res.Status, StatusSkipped)
	}
	if res.SkipReason == "" {
		t.Fatal("skipped scenario has no reason")
	}
	if res.Samples != 0 {
		t.Fatalf("skipped scenario reported %d samples", res.Samples)
	}
	if report.Summary.Skipped == 0 {
		t.Fatal("summary does not count the skipped scenario")
	}
}

func TestEmptyGraphSkipsSymbolScenariosWithoutError(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, dir)
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	if err := s.EnsureCanonicalRepositoryPaths(ctx, repo.ID, true); err != nil {
		t.Fatalf("EnsureCanonicalRepositoryPaths: %v", err)
	}

	report, err := Run(ctx, s, repo.ID, dir, Options{Runs: 1, Warmup: 0})
	if err != nil {
		t.Fatalf("Run on empty graph: %v", err)
	}
	for _, name := range []string{"symbol_lookup", "find_callers", "find_callees", "related_tests"} {
		if res := resultByName(t, report, name); res.Status != StatusSkipped {
			t.Errorf("%s on an empty graph = %q, want skipped", name, res.Status)
		}
	}
	if resultByName(t, report, "graph_stats").Status != StatusMeasured {
		t.Error("graph_stats should still be measurable on an empty graph")
	}
}

func TestInvalidOptionsAreRejected(t *testing.T) {
	ctx := context.Background()
	s, repoID, root := seededRepo(t, true)
	for _, opts := range []Options{
		{Runs: -1},
		{Runs: 1, Warmup: -1},
		{Runs: 1, BudgetMS: -5},
	} {
		if _, err := Run(ctx, s, repoID, root, opts); err == nil {
			t.Errorf("Run(%+v) = nil error, want a validation error", opts)
		}
	}
	// A zero Runs or BudgetMS means "use the default"; a zero Warmup means
	// "no warmup", because a caller must be able to ask for none.
	report, err := Run(ctx, s, repoID, root, Options{})
	if err != nil {
		t.Fatalf("Run(zero options): %v", err)
	}
	if report.Runs != DefaultRuns || report.BudgetMS != DefaultBudgetMS {
		t.Fatalf("zero options did not fall back to defaults: %+v", report)
	}
	if report.Warmup != 0 {
		t.Fatalf("zero Warmup = %d, want 0 (explicit no-warmup)", report.Warmup)
	}
}

// TestRunDoesNotMutateTheDatabase is the read-only proof at the engine level:
// the graph tables and the database file must be untouched by a benchmark.
func TestRunDoesNotMutateTheDatabase(t *testing.T) {
	ctx := context.Background()
	s, repoID, root := seededRepo(t, true)

	dsn, err := store.BuildSQLiteDSN(filepath.Join(root, "graph.sqlite"), store.OpenOptions{}, false, true)
	if err != nil {
		t.Fatalf("BuildSQLiteDSN: %v", err)
	}
	db, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	counts := func() []int64 {
		t.Helper()
		var out []int64
		for _, table := range []string{"files", "symbols", "edges", "references_tbl", "test_links", "scans", "dirty_files"} {
			var n int64
			if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM `+table).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			out = append(out, n)
		}
		return out
	}
	before := counts()
	if _, err := Run(ctx, s, repoID, root, Options{Runs: 3, Warmup: 1}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	after := counts()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("benchmark changed row counts: %v -> %v", before, after)
	}
}

// TestRunAgainstReadOnlyStore proves the engine works on a handle the driver
// itself refuses writes on, which is how the CLI opens it.
func TestRunAgainstReadOnlyStore(t *testing.T) {
	ctx := context.Background()
	s, _, root := seededRepo(t, true)
	dbPath := filepath.Join(root, "graph.sqlite")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("stat db: %v", err)
	}

	ro, err := store.OpenReadOnly(dbPath, store.OpenOptions{})
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	repo, found, err := ro.FindRepo(ctx, root)
	if err != nil || !found {
		t.Fatalf("FindRepo: found=%v err=%v", found, err)
	}
	report, err := Run(ctx, ro, repo.ID, root, Options{Runs: 2, Warmup: 1})
	if err != nil {
		t.Fatalf("Run on read-only store: %v", err)
	}
	if report.Summary.Measured == 0 {
		t.Fatal("no scenario was measured against the read-only store")
	}
}

// snapshotPathState captures every byte the path-format gate must leave
// alone: files.path, dirty_files.path, the settings table, and row counts.
func snapshotPathState(t *testing.T, dbPath string) string {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, hex(path), is_deleted FROM files ORDER BY id`,
		`SELECT hex(path), reason, queued_at FROM dirty_files ORDER BY path`,
		`SELECT key, value FROM settings ORDER BY key`,
		`SELECT (SELECT COUNT(*) FROM files), (SELECT COUNT(*) FROM dirty_files), (SELECT COUNT(*) FROM symbols), (SELECT COUNT(*) FROM edges), (SELECT COUNT(*) FROM test_links)`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				if bs, ok := v.([]byte); ok {
					b.Write(bs)
				} else {
					fmt.Fprint(&b, v)
				}
				b.WriteByte('|')
			}
			b.WriteByte('\n')
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

// relocateRepo moves every seeded file under a `src`/`pkg` directory spelled
// with sep, adds a dirty row in the same spelling, and removes the current
// path-format marker when legacy is set. With `\` and legacy=true the result
// is a populated pre-P23 index whose related_tests target would be a
// native-spelled path; with `/` and legacy=false it is the same graph in the
// supported shape.
func relocateRepo(t *testing.T, root string, repoID int64, sep string, legacy bool) {
	t.Helper()
	dsn, err := store.BuildSQLiteDSN(filepath.Join(root, "graph.sqlite"), store.OpenOptions{}, false, false)
	if err != nil {
		t.Fatalf("BuildSQLiteDSN: %v", err)
	}
	db, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	prefix := "src" + sep + "pkg" + sep
	if _, err := db.Exec(`UPDATE files SET path = ? || path WHERE repo_id = ?`, prefix, repoID); err != nil {
		t.Fatalf("relocate files.path: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO dirty_files(repo_id, path, reason, queued_at) VALUES(?, ?, 'legacy', '2026-01-01T00:00:00Z')`, repoID, prefix+"file.go"); err != nil {
		t.Fatalf("insert dirty row: %v", err)
	}
	if legacy {
		if _, err := db.Exec(`DELETE FROM settings WHERE value = 'logical-slash-v1'`); err != nil {
			t.Fatalf("remove marker: %v", err)
		}
	}
}

// TestLegacyPathFormatFailsClosedBeforeAnyRead proves Run owns the P23
// precondition for every caller: on a populated repository without the
// current path-format marker it returns store.ErrRepositoryPathFormatRebuild
// with no report, no legacy-spelled target, and every byte of the database
// untouched.
//
// The sentinel must come back bare. An ungated Run would still end in the
// sentinel, because Store.ListFiles (and RelatedTests) gate themselves -- but
// only after graph_stats was measured and the benchmark targets, including the
// legacy-spelled test file, were read, and wrapped as "scenario list_files:
// ...". The bare error is what proves Run refused before reading anything.
//
// The same graph under the marker benchmarks normally and selects the
// relocated test file, so the gate keys on the marker alone.
func TestLegacyPathFormatFailsClosedBeforeAnyRead(t *testing.T) {
	ctx := context.Background()
	s, repoID, root := seededRepo(t, true)
	relocateRepo(t, root, repoID, `\`, true)
	dbPath := filepath.Join(root, "graph.sqlite")
	before := snapshotPathState(t, dbPath)
	if !strings.Contains(before, "7372635C") { // hex(`src\`)
		t.Fatalf("fixture did not produce a native-spelled files.path:\n%s", before)
	}
	if strings.Contains(before, "logical-slash-v1") {
		t.Fatalf("fixture still carries the current marker:\n%s", before)
	}

	report, err := Run(ctx, s, repoID, root, Options{Runs: 1, Warmup: 0})
	if !errors.Is(err, store.ErrRepositoryPathFormatRebuild) {
		t.Fatalf("Run() on legacy index: err = %v, want ErrRepositoryPathFormatRebuild", err)
	}
	if err.Error() != store.ErrRepositoryPathFormatRebuild.Error() {
		t.Fatalf("Run() refused only after reading the graph (error is wrapped by a scenario): %v", err)
	}
	if len(report.Results) != 0 || report.Summary.Measured != 0 || report.Schema != "" {
		t.Fatalf("Run() on legacy index returned a partial report: %+v", report)
	}
	if after := snapshotPathState(t, dbPath); after != before {
		t.Fatalf("Run() mutated the legacy database:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// Positive control: same graph, supported spelling, marker present.
	cur, curRepo, curRoot := seededRepo(t, true)
	relocateRepo(t, curRoot, curRepo, "/", false)
	report, err = Run(ctx, cur, curRepo, curRoot, Options{Runs: 1, Warmup: 0})
	if err != nil {
		t.Fatalf("Run() on marked repository: %v", err)
	}
	if got := resultByName(t, report, "related_tests"); got.Status != StatusMeasured || !strings.HasPrefix(got.Target, "src/pkg/") {
		t.Fatalf("related_tests on marked repository = %+v, want measured against a relocated src/pkg/ file", got)
	}
}

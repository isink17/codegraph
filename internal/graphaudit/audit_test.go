package graphaudit_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graphaudit"
	"github.com/isink17/codegraph/internal/store"
)

// fixture builds a database, lets the caller write graph rows into it through a
// plain sql.DB, and returns a read-only store plus the repo id.
//
// The rows are written with raw SQL on purpose. Most of the states this package
// reports are unreachable through the public API -- that is what makes them
// worth auditing -- and the audit is opened read-only, so the fixture and the
// subject under test cannot be the same handle.
func fixture(t *testing.T, write func(t *testing.T, db *sql.DB, repoID int64)) (*store.Store, int64) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")

	rw, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	repo, err := rw.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if write != nil {
		dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, false)
		if err != nil {
			t.Fatalf("BuildSQLiteDSN() error = %v", err)
		}
		db, err := sql.Open(store.SQLiteDriverName(), dsn)
		if err != nil {
			t.Fatalf("sql.Open() error = %v", err)
		}
		write(t, db, repo.ID)
		if err := db.Close(); err != nil {
			t.Fatalf("fixture db Close() error = %v", err)
		}
	}

	ro, err := store.OpenReadOnly(dbPath, store.OpenOptions{})
	if err != nil {
		t.Fatalf("OpenReadOnly() error = %v", err)
	}
	t.Cleanup(func() { ro.Close() })
	return ro, repo.ID
}

// seedCallEdge writes one source file, one source symbol, one destination
// symbol, and one edge between them, and returns the edge id. It is the
// smallest graph any check can fire on.
func seedCallEdge(t *testing.T, db *sql.DB, repoID int64) (edgeID, dstSymbolID int64) {
	t.Helper()
	fileRes, err := db.Exec(`INSERT INTO files(repo_id, path, language, indexed_at) VALUES(?, 'a.go', 'go', '')`, repoID)
	if err != nil {
		t.Fatalf("insert file: %v", err)
	}
	fileID, _ := fileRes.LastInsertId()

	insertSymbol := func(name, qualified string) int64 {
		res, err := db.Exec(`
			INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name,
			                    start_line, start_col, end_line, end_col, stable_key)
			VALUES(?, ?, 'go', 'function', ?, ?, '', 1, 1, 1, 1, ?)
		`, repoID, fileID, name, qualified, qualified+"|go")
		if err != nil {
			t.Fatalf("insert symbol %s: %v", name, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	src := insertSymbol("Caller", "pkg/a.Caller")
	dst := insertSymbol("Helper", "pkg/a.Helper")

	edgeRes, err := db.Exec(`
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, NULL, 'Helper', 'call', '', ?, 1)
	`, repoID, src, fileID)
	if err != nil {
		t.Fatalf("insert edge: %v", err)
	}
	id, _ := edgeRes.LastInsertId()
	return id, dst
}

func run(t *testing.T, s *store.Store, repoID int64, opts graphaudit.Options) *graphaudit.Report {
	t.Helper()
	opts.RepoID = repoID
	report, err := graphaudit.Run(context.Background(), s, opts)
	if err != nil {
		t.Fatalf("graphaudit.Run() error = %v", err)
	}
	return report
}

func findingFor(report *graphaudit.Report, code string) *graphaudit.Finding {
	for i := range report.Findings {
		if report.Findings[i].Code == code {
			return &report.Findings[i]
		}
	}
	return nil
}

// TestCleanGraphReportsOK is the baseline the other tests are read against.
func TestCleanGraphReportsOK(t *testing.T) {
	s, repoID := fixture(t, func(t *testing.T, db *sql.DB, repoID int64) {
		edgeID, dst := seedCallEdge(t, db, repoID)
		if _, err := db.Exec(`
			UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ? WHERE id = ?
		`, dst, store.ResolutionStrategyExactName, store.ResolutionConfidenceHigh, edgeID); err != nil {
			t.Fatalf("bind edge: %v", err)
		}
	})

	report := run(t, s, repoID, graphaudit.Options{})
	if report.Status != graphaudit.StatusOK {
		t.Fatalf("status = %q, want ok (findings: %+v)", report.Status, report.Findings)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", report.Findings)
	}
	if len(report.SkippedChecks) != 0 {
		t.Fatalf("skipped = %+v, want none on a current schema", report.SkippedChecks)
	}
	if report.Schema != graphaudit.SchemaID {
		t.Errorf("schema = %q, want %q", report.Schema, graphaudit.SchemaID)
	}
	if report.Graph.Edges != 1 || report.Graph.ResolvedEdges != 1 || report.Graph.UnresolvedEdges != 0 {
		t.Errorf("graph summary = %+v, want 1 edge, resolved", report.Graph)
	}
	// The four classification keys are always present, even at zero.
	for _, key := range []string{"builtin", "stdlib", "external", "unknown"} {
		if _, ok := report.UnresolvedClassification[key]; !ok {
			t.Errorf("unresolved_classification is missing the %q key: %+v", key, report.UnresolvedClassification)
		}
	}
}

// TestDanglingTargetProducesErrorStatus covers the error path end to end:
// finding code, severity, count, examples, and the derived status.
func TestDanglingTargetProducesErrorStatus(t *testing.T) {
	s, repoID := fixture(t, func(t *testing.T, db *sql.DB, repoID int64) {
		edgeID, _ := seedCallEdge(t, db, repoID)
		if _, err := db.Exec(`
			UPDATE edges SET dst_symbol_id = 999999, resolution_strategy = ?, resolution_confidence = ? WHERE id = ?
		`, store.ResolutionStrategyExactName, store.ResolutionConfidenceHigh, edgeID); err != nil {
			t.Fatalf("bind edge to a missing symbol: %v", err)
		}
	})

	report := run(t, s, repoID, graphaudit.Options{})
	if report.Status != graphaudit.StatusError {
		t.Fatalf("status = %q, want error", report.Status)
	}
	f := findingFor(report, graphaudit.CodeDanglingEdgeTarget)
	if f == nil {
		t.Fatalf("no %s finding in %+v", graphaudit.CodeDanglingEdgeTarget, report.Findings)
	}
	if f.Severity != graphaudit.SeverityError || f.Count != 1 || len(f.Examples) != 1 {
		t.Errorf("finding = %+v, want one error-severity violation with one example", f)
	}
}

// TestLegacyMetadataIsAWarningNotAnError separates the two severities: a
// pre-019 row degrades trust but is not corruption, so the command must not
// treat it as a broken graph.
func TestLegacyMetadataIsAWarningNotAnError(t *testing.T) {
	s, repoID := fixture(t, func(t *testing.T, db *sql.DB, repoID int64) {
		edgeID, dst := seedCallEdge(t, db, repoID)
		if _, err := db.Exec(`UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, dst, edgeID); err != nil {
			t.Fatalf("bind edge without provenance: %v", err)
		}
	})

	report := run(t, s, repoID, graphaudit.Options{})
	if report.Status != graphaudit.StatusWarning {
		t.Fatalf("status = %q, want warning (findings: %+v)", report.Status, report.Findings)
	}
	f := findingFor(report, graphaudit.CodeResolvedMissingMetadata)
	if f == nil || f.Severity != graphaudit.SeverityWarning || f.Count != 1 {
		t.Fatalf("finding = %+v, want one warning-severity violation", f)
	}
	if findingFor(report, graphaudit.CodeInvalidResolutionMetadata) != nil {
		t.Error("a legacy row was also reported as invalid metadata")
	}
}

// TestFindingsAreOrderedErrorsFirst pins the list order, including the
// test_links finding, which is produced outside the edge catalogue.
func TestFindingsAreOrderedErrorsFirst(t *testing.T) {
	s, repoID := fixture(t, func(t *testing.T, db *sql.DB, repoID int64) {
		// A warning: bound with no provenance.
		edgeID, dst := seedCallEdge(t, db, repoID)
		if _, err := db.Exec(`UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, dst, edgeID); err != nil {
			t.Fatalf("bind edge: %v", err)
		}
		// An error, in a different table: a test link to nothing.
		var testFileID int64
		if err := db.QueryRow(`SELECT id FROM files WHERE repo_id = ?`, repoID).Scan(&testFileID); err != nil {
			t.Fatalf("read file id: %v", err)
		}
		if _, err := db.Exec(`
			INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score)
			VALUES(?, ?, NULL, NULL, 888888, 'name_match', 0.5)
		`, repoID, testFileID); err != nil {
			t.Fatalf("insert dangling test_link: %v", err)
		}
	})

	report := run(t, s, repoID, graphaudit.Options{})
	if report.Status != graphaudit.StatusError {
		t.Fatalf("status = %q, want error", report.Status)
	}
	seenWarning := false
	for _, f := range report.Findings {
		if f.Severity == graphaudit.SeverityWarning {
			seenWarning = true
			continue
		}
		if f.Severity == graphaudit.SeverityError && seenWarning {
			t.Fatalf("error finding %q appears after a warning: %+v", f.Code, report.Findings)
		}
	}
	link := findingFor(report, graphaudit.CodeDanglingTestLinkReference)
	if link == nil || len(link.TestLinkExamples) != 1 || len(link.Examples) != 0 {
		t.Fatalf("test link finding = %+v, want test-link examples only", link)
	}
}

// TestExampleLimitIsHonoured checks the three meanings of the option: default,
// explicit cap, and counts-only -- and that the count never changes with it.
func TestExampleLimitIsHonoured(t *testing.T) {
	const total = 20
	s, repoID := fixture(t, func(t *testing.T, db *sql.DB, repoID int64) {
		edgeID, _ := seedCallEdge(t, db, repoID)
		var srcID, fileID int64
		if err := db.QueryRow(`SELECT src_symbol_id, file_id FROM edges WHERE id = ?`, edgeID).Scan(&srcID, &fileID); err != nil {
			t.Fatalf("read edge: %v", err)
		}
		if _, err := db.Exec(`UPDATE edges SET dst_symbol_id = 999999 WHERE id = ?`, edgeID); err != nil {
			t.Fatalf("bind first edge: %v", err)
		}
		for i := 1; i < total; i++ {
			if _, err := db.Exec(`
				INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
				VALUES(?, ?, 999999, 'Helper', 'call', '', ?, 1)
			`, repoID, srcID, fileID); err != nil {
				t.Fatalf("insert edge %d: %v", i, err)
			}
		}
	})

	byDefault := run(t, s, repoID, graphaudit.Options{})
	f := findingFor(byDefault, graphaudit.CodeDanglingEdgeTarget)
	if f == nil || f.Count != total {
		t.Fatalf("finding = %+v, want count %d", f, total)
	}
	if len(f.Examples) != graphaudit.DefaultExampleLimit {
		t.Errorf("default examples = %d, want %d", len(f.Examples), graphaudit.DefaultExampleLimit)
	}
	if byDefault.ExampleLimit != graphaudit.DefaultExampleLimit {
		t.Errorf("report example_limit = %d, want %d", byDefault.ExampleLimit, graphaudit.DefaultExampleLimit)
	}

	capped := run(t, s, repoID, graphaudit.Options{ExampleLimit: 2})
	f = findingFor(capped, graphaudit.CodeDanglingEdgeTarget)
	if f.Count != total || len(f.Examples) != 2 {
		t.Errorf("capped finding = count %d with %d examples, want count %d with 2", f.Count, len(f.Examples), total)
	}

	countsOnly := run(t, s, repoID, graphaudit.Options{ExampleLimit: -1})
	f = findingFor(countsOnly, graphaudit.CodeDanglingEdgeTarget)
	if f.Count != total || len(f.Examples) != 0 {
		t.Errorf("counts-only finding = count %d with %d examples, want count %d with 0", f.Count, len(f.Examples), total)
	}
}

// TestReportSizeScalesWithFindingTypesNotEdgeCount is the output-efficiency
// guarantee: multiplying the violating population by 25 must not grow the JSON.
func TestReportSizeScalesWithFindingTypesNotEdgeCount(t *testing.T) {
	sizeFor := func(t *testing.T, edges int) int {
		s, repoID := fixture(t, func(t *testing.T, db *sql.DB, repoID int64) {
			edgeID, _ := seedCallEdge(t, db, repoID)
			var srcID, fileID int64
			if err := db.QueryRow(`SELECT src_symbol_id, file_id FROM edges WHERE id = ?`, edgeID).Scan(&srcID, &fileID); err != nil {
				t.Fatalf("read edge: %v", err)
			}
			if _, err := db.Exec(`UPDATE edges SET dst_symbol_id = 999999 WHERE id = ?`, edgeID); err != nil {
				t.Fatalf("bind edge: %v", err)
			}
			for i := 1; i < edges; i++ {
				if _, err := db.Exec(`
					INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
					VALUES(?, ?, 999999, 'Helper', 'call', '', ?, 1)
				`, repoID, srcID, fileID); err != nil {
					t.Fatalf("insert edge %d: %v", i, err)
				}
			}
		})
		payload, err := json.Marshal(run(t, s, repoID, graphaudit.Options{}))
		if err != nil {
			t.Fatalf("marshal report: %v", err)
		}
		return len(payload)
	}

	small := sizeFor(t, 20)
	large := sizeFor(t, 500)
	// The only difference between the two reports is the count field's digits.
	if delta := large - small; delta > 32 {
		t.Fatalf("report grew by %d bytes for 25x the violations (%d -> %d); output is not bounded", delta, small, large)
	}
}

// TestFailOnSemantics covers the policy matrix and, critically, that the report
// itself does not depend on the policy.
func TestFailOnSemantics(t *testing.T) {
	cases := []struct {
		policy graphaudit.FailOn
		status graphaudit.Status
		want   bool
	}{
		{graphaudit.FailOnNone, graphaudit.StatusOK, false},
		{graphaudit.FailOnNone, graphaudit.StatusWarning, false},
		{graphaudit.FailOnNone, graphaudit.StatusError, false},
		{graphaudit.FailOnError, graphaudit.StatusOK, false},
		{graphaudit.FailOnError, graphaudit.StatusWarning, false},
		{graphaudit.FailOnError, graphaudit.StatusError, true},
		{graphaudit.FailOnWarning, graphaudit.StatusOK, false},
		{graphaudit.FailOnWarning, graphaudit.StatusWarning, true},
		{graphaudit.FailOnWarning, graphaudit.StatusError, true},
	}
	for _, tc := range cases {
		if got := tc.policy.ShouldFail(tc.status); got != tc.want {
			t.Errorf("FailOn(%q).ShouldFail(%q) = %v, want %v", tc.policy, tc.status, got, tc.want)
		}
	}

	if _, err := graphaudit.ParseFailOn("sometimes"); err == nil {
		t.Error("ParseFailOn(\"sometimes\") accepted an invalid policy")
	}
	for _, valid := range []string{"none", "error", "warning"} {
		if _, err := graphaudit.ParseFailOn(valid); err != nil {
			t.Errorf("ParseFailOn(%q) error = %v", valid, err)
		}
	}
}

// TestAuditDoesNotMutateTheDatabase is the read-only guarantee, asserted
// against the actual row population rather than against the absence of write
// calls in the source.
func TestAuditDoesNotMutateTheDatabase(t *testing.T) {
	s, repoID := fixture(t, func(t *testing.T, db *sql.DB, repoID int64) {
		edgeID, dst := seedCallEdge(t, db, repoID)
		if _, err := db.Exec(`UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, dst, edgeID); err != nil {
			t.Fatalf("bind edge: %v", err)
		}
	})

	before := run(t, s, repoID, graphaudit.Options{})
	after := run(t, s, repoID, graphaudit.Options{})

	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("a second audit produced a different report; the first one mutated state\nfirst:  %s\nsecond: %s", beforeJSON, afterJSON)
	}
	// The warning must still be a warning: nothing was backfilled.
	if after.Status != graphaudit.StatusWarning {
		t.Fatalf("status after a second run = %q, want warning; audit appears to have repaired the row", after.Status)
	}
}

// TestPreP4SchemaReportsSkippedChecks covers the legacy path at report level:
// on a database predating migration 019 the metadata checks must appear as
// skipped, with a reason, and must NOT be silently absent (which would read as
// "checked, nothing found") or fail the run.
func TestPreP4SchemaReportsSkippedChecks(t *testing.T) {
	s, repoID := fixture(t, func(t *testing.T, db *sql.DB, repoID int64) {
		seedCallEdge(t, db, repoID)
		// Drop the migration 019 columns to reproduce a pre-P4 database.
		// SQLite supports DROP COLUMN from 3.35, and the columns carry no index.
		for _, column := range []string{"resolution_strategy", "resolution_confidence"} {
			if _, err := db.Exec(`ALTER TABLE edges DROP COLUMN ` + column); err != nil {
				t.Skipf("this SQLite build cannot DROP COLUMN (%v); the pre-019 path is covered in internal/store", err)
			}
		}
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version >= 19`); err != nil {
			t.Fatalf("roll back the recorded migration version: %v", err)
		}
	})

	report := run(t, s, repoID, graphaudit.Options{})
	if report.SchemaVersion >= 19 {
		t.Fatalf("schema_version = %d, want < 19 for a pre-P4 database", report.SchemaVersion)
	}
	skipped := map[string]string{}
	for _, sc := range report.SkippedChecks {
		skipped[sc.Code] = sc.Reason
	}
	for _, code := range []string{
		graphaudit.CodeInvalidResolutionMetadata,
		graphaudit.CodeResolvedMissingMetadata,
		graphaudit.CodeLowConfidenceResolution,
		graphaudit.CodeCrossLanguageEdgeImplicitStrategy,
	} {
		reason, ok := skipped[code]
		if !ok {
			t.Errorf("check %s is neither reported nor skipped on a pre-019 database", code)
			continue
		}
		if reason == "" {
			t.Errorf("check %s was skipped with no reason", code)
		}
	}
	// The schema-independent checks still ran, so the graph summary and the
	// classification are populated as usual.
	if report.Graph.Edges != 1 {
		t.Errorf("graph summary = %+v, want 1 edge", report.Graph)
	}
	if report.UnresolvedClassification == nil {
		t.Error("unresolved classification is nil on a pre-019 database")
	}
	if report.ResolutionStrategy == nil || len(report.ResolutionStrategy) != 0 {
		t.Errorf("resolution_strategy = %+v, want an empty (not nil) map", report.ResolutionStrategy)
	}
}

// TestReportJSONUsesStableFieldNames pins the wire contract that CI and agents
// read. Renaming any of these is a breaking change and should fail here first.
func TestReportJSONUsesStableFieldNames(t *testing.T) {
	s, repoID := fixture(t, nil)
	payload, err := json.Marshal(run(t, s, repoID, graphaudit.Options{}))
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	for _, field := range []string{
		`"schema"`, `"repository"`, `"graph"`, `"status"`, `"schema_version"`,
		`"example_limit"`, `"findings"`, `"unresolved_classification"`,
		`"resolution_strategy"`, `"resolution_confidence"`,
	} {
		if !strings.Contains(string(payload), field) {
			t.Errorf("report JSON is missing the %s field: %s", field, payload)
		}
	}
}

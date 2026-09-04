package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/store"
)

// quietStartup silences the version check so command output is only the
// command's own.
func quietStartup(t *testing.T) {
	t.Helper()
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })
}

// indexedRepo creates a repository directory holding a real graph database with
// one repos row, and returns the repo root and the database path.
//
// It does not run the indexer: `audit` must work against whatever database is
// on disk, and building one directly keeps the test about the command rather
// than about indexing.
func indexedRepo(t *testing.T, seed func(t *testing.T, db *sql.DB, repoID int64)) (string, string) {
	t.Helper()
	repoRoot := t.TempDir()
	dbPath := filepath.Join(repoRoot, config.RepoArtifactsDir, store.RepoDatabaseFileName)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	repo, err := s.UpsertRepo(context.Background(), repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if seed != nil {
		dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, false)
		if err != nil {
			t.Fatalf("BuildSQLiteDSN: %v", err)
		}
		db, err := sql.Open(store.SQLiteDriverName(), dsn)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		seed(t, db, repo.ID)
		db.Close()
	}
	return repoRoot, dbPath
}

// seedDanglingEdge writes one edge bound to a symbol id that does not exist,
// which is the smallest error-severity graph.
func seedDanglingEdge(t *testing.T, db *sql.DB, repoID int64) {
	t.Helper()
	fileRes, err := db.Exec(`INSERT INTO files(repo_id, path, language, indexed_at) VALUES(?, 'a.go', 'go', '')`, repoID)
	if err != nil {
		t.Fatalf("insert file: %v", err)
	}
	fileID, _ := fileRes.LastInsertId()
	symRes, err := db.Exec(`
		INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name,
		                    start_line, start_col, end_line, end_col, stable_key)
		VALUES(?, ?, 'go', 'function', 'Caller', 'pkg/a.Caller', '', 1, 1, 1, 1, 'pkg/a.Caller|go')
	`, repoID, fileID)
	if err != nil {
		t.Fatalf("insert symbol: %v", err)
	}
	srcID, _ := symRes.LastInsertId()
	if _, err := db.Exec(`
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
		                  resolution_strategy, resolution_confidence)
		VALUES(?, ?, 999999, 'Helper', 'call', '', ?, 1, ?, ?)
	`, repoID, srcID, fileID, store.ResolutionStrategyExactName, store.ResolutionConfidenceHigh); err != nil {
		t.Fatalf("insert dangling edge: %v", err)
	}
}

func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := Run(context.Background(), args, &out, &errOut)
	return out.String(), errOut.String(), err
}

// TestAuditHelpContract pins the documented surface: the command is listed, and
// its own help names every flag.
func TestAuditHelpContract(t *testing.T) {
	quietStartup(t)

	root, _, err := runCLI(t, "--help")
	if err != nil {
		t.Fatalf("Run(--help) error = %v", err)
	}
	if !strings.Contains(root, "audit") {
		t.Fatalf("root help does not list the audit command:\n%s", root)
	}

	out, _, err := runCLI(t, "audit", "--help")
	if err != nil {
		t.Fatalf("Run(audit --help) error = %v", err)
	}
	for _, want := range []string{"audit [PATH]", "--examples", "--fail-on", "--repo-root"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit --help is missing %q:\n%s", want, out)
		}
	}
}

// TestAuditRejectsUnindexedRepoWithoutCreatingOne is the contract that keeps
// audit from silently auditing a database it just made.
func TestAuditRejectsUnindexedRepoWithoutCreatingOne(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot := t.TempDir()

	out, _, err := runCLI(t, "audit", repoRoot)
	if err == nil {
		t.Fatalf("audit on an unindexed repo succeeded, output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not indexed") {
		t.Errorf("error = %q, want it to say the repository is not indexed", err)
	}
	if !strings.Contains(err.Error(), "index") {
		t.Errorf("error = %q, want it to name the command that fixes this", err)
	}
	entries, readErr := os.ReadDir(repoRoot)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("audit created %d entries in an unindexed repo: %v", len(entries), entries)
	}
}

// TestAuditRejectsInvalidFlags covers argument validation before any database
// is touched.
func TestAuditRejectsInvalidFlags(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot, _ := indexedRepo(t, nil)

	if _, _, err := runCLI(t, "audit", repoRoot, "--fail-on", "sometimes"); err == nil {
		t.Error("audit accepted --fail-on sometimes")
	}
	if _, _, err := runCLI(t, "audit", repoRoot, "--examples", "-3"); err == nil {
		t.Error("audit accepted a negative --examples")
	}
}

// TestAuditOutputsReportJSON checks the command's primary contract on a real
// database.
func TestAuditOutputsReportJSON(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot, dbPath := indexedRepo(t, nil)

	out, _, err := runCLI(t, "audit", repoRoot)
	if err != nil {
		t.Fatalf("Run(audit) error = %v", err)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("audit output is not JSON: %v\n%s", err, out)
	}
	if report["schema"] != "codegraph.graph_audit/v1" {
		t.Errorf("schema = %v, want codegraph.graph_audit/v1", report["schema"])
	}
	if report["status"] != "ok" {
		t.Errorf("status = %v, want ok on an empty graph", report["status"])
	}
	repository, _ := report["repository"].(map[string]any)
	if repository["db_path"] != dbPath {
		t.Errorf("repository.db_path = %v, want %v", repository["db_path"], dbPath)
	}
}

// TestAuditFailOnControlsOnlyTheExitCode is the CI contract: the same report is
// printed under every policy, and only the returned error differs.
func TestAuditFailOnControlsOnlyTheExitCode(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot, _ := indexedRepo(t, seedDanglingEdge)

	byDefault, _, err := runCLI(t, "audit", repoRoot)
	if err != nil {
		t.Fatalf("audit with the default policy returned an error for a graph finding: %v", err)
	}
	if !strings.Contains(byDefault, `"status": "error"`) {
		t.Fatalf("report does not carry the error status:\n%s", byDefault)
	}

	failed, _, err := runCLI(t, "audit", repoRoot, "--fail-on", "error")
	if err == nil {
		t.Fatal("audit --fail-on error returned no error for an error-status graph")
	}
	if failed != byDefault {
		t.Errorf("--fail-on changed the report:\n--- default ---\n%s\n--- fail-on error ---\n%s", byDefault, failed)
	}

	// A warning policy must also fail on an error status.
	if _, _, err := runCLI(t, "audit", repoRoot, "--fail-on", "warning"); err == nil {
		t.Error("audit --fail-on warning returned no error for an error-status graph")
	}
	// An explicit none matches the default.
	if _, _, err := runCLI(t, "audit", repoRoot, "--fail-on", "none"); err != nil {
		t.Errorf("audit --fail-on none returned an error: %v", err)
	}
}

// TestAuditExamplesFlagIsApplied checks the flag reaches the report.
func TestAuditExamplesFlagIsApplied(t *testing.T) {
	quietStartup(t)
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "home"))
	repoRoot, _ := indexedRepo(t, seedDanglingEdge)

	out, _, err := runCLI(t, "audit", repoRoot, "--examples", "0")
	if err != nil {
		t.Fatalf("Run(audit --examples 0) error = %v", err)
	}
	if strings.Contains(out, `"examples"`) {
		t.Errorf("--examples 0 still emitted examples:\n%s", out)
	}
	if !strings.Contains(out, `"count": 1`) {
		t.Errorf("--examples 0 lost the count:\n%s", out)
	}
}

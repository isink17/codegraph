package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/store"
)

func snapshotRepoDBPathState(t *testing.T, dbPath string) string {
	t.Helper()
	raw, err := sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, hex(path), is_deleted FROM files ORDER BY id`,
		`SELECT hex(path), reason, queued_at FROM dirty_files ORDER BY path`,
		`SELECT key, value FROM settings ORDER BY key`,
		`SELECT (SELECT COUNT(*) FROM files), (SELECT COUNT(*) FROM dirty_files), (SELECT COUNT(*) FROM scans), (SELECT COUNT(*) FROM symbols)`,
	} {
		rows, err := raw.Query(q)
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

// TestRunIndexLegacyPathFormatRequiresRebuildThenRecovers is the end-to-end
// recovery proof through the real CLI: a populated pre-P23-shaped database is
// refused by `index` and `update_graph` without being touched, and the
// documented recovery, `index --rebuild`, removes the database file and
// produces a current-format index with logical repo-relative paths.
func TestRunIndexLegacyPathFormatRequiresRebuildThenRecovers(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repoRoot, "src", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "src", "pkg", "file.go"), []byte("package pkg\n\nfunc Thing() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), []string{"index", repoRoot}, &out, &errOut); err != nil {
		t.Fatalf("Run(index) error = %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := store.CanonicalRepoPath(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	dbPath, err := dbPathForRepo(cfg, repoRoot, canonical)
	if err != nil {
		t.Fatal(err)
	}

	// Degrade the current index into a pre-P23 shape: marker gone, native
	// path spelling, and a dirty row in the same spelling.
	raw, err := sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var repoID int64
	if err := raw.QueryRow(`SELECT id FROM repos WHERE canonical_path = ?`, canonical).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DELETE FROM settings WHERE value = 'logical-slash-v1'`,
		`UPDATE files SET path = replace(path, '/', '\')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO dirty_files(repo_id, path, reason, queued_at) VALUES(?, ?, 'legacy', '2026-01-01T00:00:00Z')`, repoID, `src\pkg\file.go`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotRepoDBPathState(t, dbPath)
	if !strings.Contains(before, "7372635C") { // hex(`src\`)
		t.Fatalf("fixture did not produce native-spelled files.path:\n%s", before)
	}

	for _, args := range [][]string{
		{"index", repoRoot},
		{"index", repoRoot, "--force"},
		{"update_graph", repoRoot},
	} {
		out.Reset()
		err := Run(context.Background(), args, &out, &errOut)
		if !errors.Is(err, store.ErrRepositoryPathFormatRebuild) {
			t.Fatalf("Run(%v) on legacy database: err = %v, want ErrRepositoryPathFormatRebuild", args, err)
		}
		if strings.Contains(out.String(), `"summary"`) {
			t.Fatalf("Run(%v) printed a summary despite refusing: %s", args, out.String())
		}
		if after := snapshotRepoDBPathState(t, dbPath); after != before {
			t.Fatalf("Run(%v) mutated the legacy database:\nbefore:\n%s\nafter:\n%s", args, before, after)
		}
	}

	// Read surfaces (the F4B1 probe, now permanent): every query command must
	// fail closed on the legacy database and emit no legacy-spelled path.
	for _, args := range [][]string{
		{"find-symbol", repoRoot, "Thing", "--exact"},
		{"find-symbol", repoRoot, "Thing"},
		{"search", repoRoot, "Thing"},
		{"find_callers", repoRoot, "Thing"},
		{"find_callees", repoRoot, "Thing"},
		{"get_impact_radius", repoRoot, "--symbols", "Thing"},
		{"find_related_tests", repoRoot, "--symbol", "Thing"},
		{"stats", repoRoot},
		{"graph", "export", repoRoot},
		// audit reads the store through graphaudit.Run, not query.Service;
		// the gate it inherits is graphaudit's own.
		{"audit", repoRoot},
		// bench-queries reads the store through querybench.Run, not
		// query.Service; the gate it inherits is querybench's own.
		{"bench-queries", repoRoot, "--runs", "1", "--warmup", "0"},
	} {
		out.Reset()
		err := Run(context.Background(), args, &out, &errOut)
		if !errors.Is(err, store.ErrRepositoryPathFormatRebuild) {
			t.Fatalf("Run(%v) on legacy database: err = %v, output = %s; want ErrRepositoryPathFormatRebuild", args, err, out.String())
		}
		if strings.Contains(out.String(), `src\\pkg`) || strings.Contains(out.String(), `"count"`) {
			t.Fatalf("Run(%v) emitted repository data from a legacy database: %s", args, out.String())
		}
		if after := snapshotRepoDBPathState(t, dbPath); after != before {
			t.Fatalf("Run(%v) mutated the legacy database:\nbefore:\n%s\nafter:\n%s", args, before, after)
		}
	}

	// bench-queries must refuse before it selects a target or measures a
	// scenario: the bare sentinel, not a per-scenario error. A downstream
	// Store gate (list_files, related_tests) also yields the sentinel, but
	// only after the legacy targets were read and wrapped as
	// "scenario <name>: ...", so errors.Is alone would not prove the
	// querybench gate is what refused.
	out.Reset()
	err = Run(context.Background(), []string{"bench-queries", repoRoot, "--runs", "1", "--warmup", "0"}, &out, &errOut)
	if err == nil || err.Error() != store.ErrRepositoryPathFormatRebuild.Error() {
		t.Fatalf("Run(bench-queries) on legacy database: err = %v, want the bare rebuild sentinel", err)
	}

	// Supported recovery: remove the database and re-index.
	out.Reset()
	if err := Run(context.Background(), []string{"index", repoRoot, "--rebuild"}, &out, &errOut); err != nil {
		t.Fatalf("Run(index --rebuild) error = %v", err)
	}
	var rebuilt map[string]any
	if err := json.Unmarshal(out.Bytes(), &rebuilt); err != nil {
		t.Fatalf("rebuild output json parse error = %v: %s", err, out.String())
	}
	summary, _ := rebuilt["summary"].(map[string]any)
	if removed, _ := summary["removed_db_files"].([]any); len(removed) == 0 {
		t.Fatalf("rebuild did not report removed database files: %s", out.String())
	}

	raw, err = sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var paths []string
	rows, err := raw.Query(`SELECT path FROM files WHERE is_deleted = 0 ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	rows.Close()
	if len(paths) != 1 || paths[0] != "src/pkg/file.go" {
		t.Fatalf("files.path after rebuild = %q, want [src/pkg/file.go]", paths)
	}
	var markers, dirty int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM settings WHERE value = 'logical-slash-v1'`).Scan(&markers); err != nil || markers != 1 {
		t.Fatalf("marker rows after rebuild = %d, %v; want 1", markers, err)
	}
	var rebuiltRepoID int64
	if err := raw.QueryRow(`SELECT id FROM repos WHERE canonical_path = ?`, canonical).Scan(&rebuiltRepoID); err != nil {
		t.Fatal(err)
	}
	var markerValue string
	if err := raw.QueryRow(`SELECT value FROM settings WHERE key = 'format.canonical_repository_paths.v1.' || ?`, rebuiltRepoID).Scan(&markerValue); err != nil || markerValue != "logical-slash-v1" {
		t.Fatalf("marker under literal per-repo key after rebuild = %q, %v", markerValue, err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM dirty_files`).Scan(&dirty); err != nil || dirty != 0 {
		t.Fatalf("dirty_files after rebuild = %d, %v; want 0 (legacy queue discarded with the database)", dirty, err)
	}
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(context.Background(), canonical)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequireCanonicalRepositoryPaths(context.Background(), repo.ID); err != nil {
		t.Fatalf("RequireCanonicalRepositoryPaths after rebuild: %v", err)
	}
	if count := queryCount(t, repoRoot, "Thing"); count != 1 {
		t.Fatalf("find-symbol Thing after rebuild count = %d, want 1", count)
	}
	out.Reset()
	if err := Run(context.Background(), []string{"find-symbol", repoRoot, "Thing", "--exact"}, &out, &errOut); err != nil {
		t.Fatalf("Run(find-symbol) after rebuild: %v", err)
	}
	if !strings.Contains(out.String(), `"file": "src/pkg/file.go"`) || strings.Contains(out.String(), `src\\pkg`) {
		t.Fatalf("find-symbol after rebuild does not report the logical path: %s", out.String())
	}
	out.Reset()
	if err := Run(context.Background(), []string{"audit", repoRoot}, &out, &errOut); err != nil {
		t.Fatalf("Run(audit) after rebuild: %v", err)
	}
	if !strings.Contains(out.String(), `"schema": "codegraph.graph_audit/v1"`) {
		t.Fatalf("audit after rebuild did not print a report: %s", out.String())
	}
	out.Reset()
	if err := Run(context.Background(), []string{"bench-queries", repoRoot, "--runs", "1", "--warmup", "0"}, &out, &errOut); err != nil {
		t.Fatalf("Run(bench-queries) after rebuild: %v", err)
	}
	if !strings.Contains(out.String(), `"schema": "codegraph.query_bench/v1"`) {
		t.Fatalf("bench-queries after rebuild did not print a report: %s", out.String())
	}
}

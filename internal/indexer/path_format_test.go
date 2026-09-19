package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// snapshotLegacyPathState dumps the bytes the path-format contract protects
// plus the scans count, which proves a rejected run never reached BeginScan.
func snapshotLegacyPathState(t *testing.T, raw *sql.DB) string {
	t.Helper()
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

// makeLegacyPathFormat turns a freshly indexed current-format database into
// the shape a pre-P23 index presents: no format marker, files.path in native
// Windows spelling, and a queued dirty row in the same spelling.
func makeLegacyPathFormat(t *testing.T, raw *sql.DB, repoID int64) {
	t.Helper()
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
}

// TestIndexRejectsLegacyRepositoryPathFormatWithoutMutation proves the
// indexer's write boundary: every run kind against a legacy database fails
// with ErrRepositoryPathFormatRebuild before BeginScan, and leaves files.path,
// dirty_files.path, settings and row counts byte-identical.
func TestIndexRejectsLegacyRepositoryPathFormatWithoutMutation(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "src", "pkg", "file.go"), "package pkg\n\nfunc Thing() {}\n")
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	makeLegacyPathFormat(t, raw, repo.ID)
	before := snapshotLegacyPathState(t, raw)
	if !strings.Contains(before, "737263"+"5C") { // hex("src\")
		t.Fatalf("fixture did not produce a native-spelled files.path:\n%s", before)
	}

	runs := []struct {
		name string
		opts Options
		run  func(context.Context, Options) (store.ScanSummary, error)
	}{
		{"Index", Options{RepoRoot: repoRoot}, idx.Index},
		{"Index --force", Options{RepoRoot: repoRoot, Force: true}, idx.Index},
		{"Update", Options{RepoRoot: repoRoot}, idx.Update},
		{"Update path-scoped", Options{RepoRoot: repoRoot, Paths: []string{"src/pkg/file.go"}}, idx.Update},
	}
	for _, r := range runs {
		_, err := r.run(ctx, r.opts)
		if !errors.Is(err, store.ErrRepositoryPathFormatRebuild) {
			t.Fatalf("%s on legacy database: err = %v, want ErrRepositoryPathFormatRebuild", r.name, err)
		}
		if after := snapshotLegacyPathState(t, raw); after != before {
			t.Fatalf("%s mutated the legacy database:\nbefore:\n%s\nafter:\n%s", r.name, before, after)
		}
	}
	if err := s.RequireCanonicalRepositoryPaths(ctx, repo.ID); !errors.Is(err, store.ErrRepositoryPathFormatRebuild) {
		t.Fatalf("marker adopted by rejected runs: %v", err)
	}
}

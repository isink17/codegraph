package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCanonicalRepositoryPathFormatMarker(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequireCanonicalRepositoryPaths(ctx, repo.ID); err != nil {
		t.Fatalf("missing marker on empty repo: %v", err)
	}
	if err := s.EnsureCanonicalRepositoryPaths(ctx, repo.ID, false); !errors.Is(err, ErrRepositoryPathFormatRebuild) {
		t.Fatalf("non-full initialize: %v", err)
	}
	if err := s.EnsureCanonicalRepositoryPaths(ctx, repo.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireCanonicalRepositoryPaths(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalRepositoryPathFormatConcurrentInitialize(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.EnsureCanonicalRepositoryPaths(ctx, repo.ID, true) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// pathFormatMarker reads the raw per-repo marker row. The second result is
// false when no row exists at all.
func pathFormatMarker(t *testing.T, s *Store, repoID int64) (string, bool) {
	t.Helper()
	var value string
	err := s.db.QueryRowContext(context.Background(), `SELECT value FROM settings WHERE key=?`, canonicalRepositoryPathsKey(repoID)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return value, true
}

func setPathFormatMarker(t *testing.T, s *Store, repoID int64, value string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, canonicalRepositoryPathsKey(repoID), value); err != nil {
		t.Fatalf("set marker: %v", err)
	}
}

func insertTestDirtyFile(t *testing.T, s *Store, repoID int64, path string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO dirty_files(repo_id, path, reason, queued_at) VALUES(?, ?, 'fixture', '2026-01-01T00:00:00Z')`, repoID, path); err != nil {
		t.Fatalf("insert dirty file %q: %v", path, err)
	}
}

// snapshotPathState captures every byte the P23 format contract protects:
// files.path and dirty_files.path (hex, so a separator swap cannot hide behind
// display normalisation), row counts, and the whole settings table (so a
// silently adopted marker under any key shows up).
func snapshotPathState(t *testing.T, s *Store, repoID int64) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	dump := func(query string, args ...any) {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("snapshot %q: %v", query, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("snapshot scan: %v", err)
			}
			for _, v := range vals {
				switch x := v.(type) {
				case []byte:
					b.Write(x)
				default:
					fmt.Fprint(&b, x)
				}
				b.WriteByte('|')
			}
			b.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("snapshot rows: %v", err)
		}
		b.WriteString("--\n")
	}
	dump(`SELECT id, hex(path), is_deleted FROM files WHERE repo_id=? ORDER BY id`, repoID)
	dump(`SELECT hex(path), reason, queued_at FROM dirty_files WHERE repo_id=? ORDER BY path`, repoID)
	dump(`SELECT key, value FROM settings ORDER BY key`)
	dump(`SELECT (SELECT COUNT(*) FROM files WHERE repo_id=?), (SELECT COUNT(*) FROM dirty_files WHERE repo_id=?), (SELECT COUNT(*) FROM scans WHERE repo_id=?)`, repoID, repoID, repoID)
	return b.String()
}

func requireRebuild(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrRepositoryPathFormatRebuild) {
		t.Fatalf("%s: err = %v, want ErrRepositoryPathFormatRebuild", what, err)
	}
}

// openUnmarkedRepo opens a fresh database and creates a repo row without the
// path-format marker: the shape a pre-P23 database presents on first open.
func openUnmarkedRepo(t *testing.T) (*Store, int64) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s, repo.ID
}

// TestCanonicalRepositoryPathFormatMatrix pins the P23 R2 format contract
// state by state. Marker value "logical-slash-v1" is the only accepted
// format; every other populated state must fail closed with
// ErrRepositoryPathFormatRebuild and leave the database untouched.
func TestCanonicalRepositoryPathFormatMatrix(t *testing.T) {
	ctx := context.Background()

	t.Run("A_new_empty_repo", func(t *testing.T) {
		s, repoID := openUnmarkedRepo(t)
		if err := s.RequireCanonicalRepositoryPaths(ctx, repoID); err != nil {
			t.Fatalf("Require on empty unmarked repo: %v", err)
		}
		before := snapshotPathState(t, s, repoID)
		files, err := s.ListFiles(ctx, repoID, "", 10, 0)
		if err != nil || len(files) != 0 {
			t.Fatalf("ListFiles on empty unmarked repo = %v, %v", files, err)
		}
		if after := snapshotPathState(t, s, repoID); after != before {
			t.Fatalf("read-only empty-repo gate mutated the database:\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if _, ok := pathFormatMarker(t, s, repoID); ok {
			t.Fatal("read-only empty-repo operation created a marker")
		}
		before = snapshotPathState(t, s, repoID)
		requireRebuild(t, "QueueDirtyFile on empty unmarked repo", s.QueueDirtyFile(ctx, repoID, "src/pkg/file.go", "update"))
		if after := snapshotPathState(t, s, repoID); after != before {
			t.Fatalf("rejected write mutated the database:\nbefore:\n%s\nafter:\n%s", before, after)
		}
		requireRebuild(t, "Ensure(incremental) on empty unmarked repo", s.EnsureCanonicalRepositoryPaths(ctx, repoID, false))
		if _, ok := pathFormatMarker(t, s, repoID); ok {
			t.Fatal("incremental Ensure created a marker on an empty repo")
		}
		// The supported initialisation path: a full index of an empty repo.
		if err := s.EnsureCanonicalRepositoryPaths(ctx, repoID, true); err != nil {
			t.Fatalf("Ensure(full) on empty repo: %v", err)
		}
		if got, ok := pathFormatMarker(t, s, repoID); !ok || got != "logical-slash-v1" {
			t.Fatalf("marker after full initialise = %q,%v; want logical-slash-v1", got, ok)
		}
		// Pin the literal per-repo key, independent of the production helper.
		var literal string
		if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'format.canonical_repository_paths.v1.' || ?`, repoID).Scan(&literal); err != nil || literal != "logical-slash-v1" {
			t.Fatalf("marker under literal key = %q, %v; want logical-slash-v1", literal, err)
		}
		if err := s.RequireCanonicalRepositoryPaths(ctx, repoID); err != nil {
			t.Fatalf("Require after initialise: %v", err)
		}
		if files, err := s.ListFiles(ctx, repoID, "", 10, 0); err != nil || len(files) != 0 {
			t.Fatalf("ListFiles on fresh repo = %v, %v", files, err)
		}
	})

	t.Run("B_current_format", func(t *testing.T) {
		s, repoID := openUnmarkedRepo(t)
		if err := s.EnsureCanonicalRepositoryPaths(ctx, repoID, true); err != nil {
			t.Fatal(err)
		}
		if _, err := insertTestFile(ctx, s, repoID, "src/pkg/file.go"); err != nil {
			t.Fatal(err)
		}
		if err := s.RequireCanonicalRepositoryPaths(ctx, repoID); err != nil {
			t.Fatalf("Require: %v", err)
		}
		if err := s.EnsureCanonicalRepositoryPaths(ctx, repoID, false); err != nil {
			t.Fatalf("Ensure(incremental) on current populated repo: %v", err)
		}
		files, err := s.ListFiles(ctx, repoID, "src/", 10, 0)
		if err != nil || len(files) != 1 || files[0]["path"] != "src/pkg/file.go" {
			t.Fatalf("ListFiles = %v, %v", files, err)
		}
		if _, err := s.RelatedTests(ctx, repoID, "", "src/pkg/file.go", 10, 0); err != nil {
			t.Fatalf("RelatedTests: %v", err)
		}
		if err := s.QueueDirtyFile(ctx, repoID, "src/pkg/file.go", "test"); err != nil {
			t.Fatalf("QueueDirtyFile: %v", err)
		}
		var dirty string
		if err := s.db.QueryRowContext(ctx, `SELECT path FROM dirty_files WHERE repo_id=?`, repoID).Scan(&dirty); err != nil || dirty != "src/pkg/file.go" {
			t.Fatalf("dirty_files.path = %q, %v; want logical spelling", dirty, err)
		}
		if got, _ := pathFormatMarker(t, s, repoID); got != "logical-slash-v1" {
			t.Fatalf("marker drifted to %q", got)
		}
	})

	t.Run("C_populated_marker_missing", func(t *testing.T) {
		s, repoID := openUnmarkedRepo(t)
		if _, err := insertTestFile(ctx, s, repoID, "src/pkg/file.go"); err != nil {
			t.Fatal(err)
		}
		before := snapshotPathState(t, s, repoID)
		requireRebuild(t, "Require", s.RequireCanonicalRepositoryPaths(ctx, repoID))
		requireRebuild(t, "Ensure(full)", s.EnsureCanonicalRepositoryPaths(ctx, repoID, true))
		requireRebuild(t, "Ensure(incremental)", s.EnsureCanonicalRepositoryPaths(ctx, repoID, false))
		if _, ok := pathFormatMarker(t, s, repoID); ok {
			t.Fatal("marker was adopted on a populated unmarked repo")
		}
		if after := snapshotPathState(t, s, repoID); after != before {
			t.Fatalf("database mutated by rejected calls:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("D_populated_wrong_marker", func(t *testing.T) {
		s, repoID := openUnmarkedRepo(t)
		if _, err := insertTestFile(ctx, s, repoID, "src/pkg/file.go"); err != nil {
			t.Fatal(err)
		}
		setPathFormatMarker(t, s, repoID, "native-v0")
		before := snapshotPathState(t, s, repoID)
		requireRebuild(t, "Require", s.RequireCanonicalRepositoryPaths(ctx, repoID))
		requireRebuild(t, "Ensure(full)", s.EnsureCanonicalRepositoryPaths(ctx, repoID, true))
		requireRebuild(t, "Ensure(incremental)", s.EnsureCanonicalRepositoryPaths(ctx, repoID, false))
		if got, ok := pathFormatMarker(t, s, repoID); !ok || got != "native-v0" {
			t.Fatalf("marker = %q,%v; want the legacy value left untouched", got, ok)
		}
		if after := snapshotPathState(t, s, repoID); after != before {
			t.Fatalf("database mutated by rejected calls:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("D_empty_wrong_marker", func(t *testing.T) {
		s, repoID := openUnmarkedRepo(t)
		setPathFormatMarker(t, s, repoID, "native-v0")
		before := snapshotPathState(t, s, repoID)
		requireRebuild(t, "Require", s.RequireCanonicalRepositoryPaths(ctx, repoID))
		if _, err := s.ListFiles(ctx, repoID, "", 10, 0); !errors.Is(err, ErrRepositoryPathFormatRebuild) {
			t.Fatalf("ListFiles = %v, want rebuild error", err)
		}
		requireRebuild(t, "Ensure(full)", s.EnsureCanonicalRepositoryPaths(ctx, repoID, true))
		if got, ok := pathFormatMarker(t, s, repoID); !ok || got != "native-v0" {
			t.Fatalf("marker = %q,%v; want native-v0 left untouched", got, ok)
		}
		if after := snapshotPathState(t, s, repoID); after != before {
			t.Fatalf("database mutated:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	// F: no rows, but a foreign marker. Full initialise may only create a
	// marker, never overwrite one, so this must still fail closed.
	t.Run("F_empty_repo_wrong_marker", func(t *testing.T) {
		s, repoID := openUnmarkedRepo(t)
		setPathFormatMarker(t, s, repoID, "native-v0")
		before := snapshotPathState(t, s, repoID)
		requireRebuild(t, "Require", s.RequireCanonicalRepositoryPaths(ctx, repoID))
		requireRebuild(t, "Ensure(full) over foreign marker", s.EnsureCanonicalRepositoryPaths(ctx, repoID, true))
		if got, ok := pathFormatMarker(t, s, repoID); !ok || got != "native-v0" {
			t.Fatalf("marker = %q,%v; want native-v0 left untouched", got, ok)
		}
		if after := snapshotPathState(t, s, repoID); after != before {
			t.Fatalf("database mutated:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	// E: rows spelled the way a pre-P23 Windows index could have written them.
	// The gate rejects the repository before any query looks at the bytes, so
	// the test never asserts anything about how such a row would be read: it
	// is not a valid current files.path and no code path is allowed to treat
	// it as one.
	t.Run("E_legacy_native_path_rows", func(t *testing.T) {
		s, repoID := openUnmarkedRepo(t)
		if _, err := insertTestFile(ctx, s, repoID, `src\pkg\file.go`); err != nil {
			t.Fatal(err)
		}
		before := snapshotPathState(t, s, repoID)
		requireRebuild(t, "Require", s.RequireCanonicalRepositoryPaths(ctx, repoID))
		if _, err := s.ListFiles(ctx, repoID, "src/", 10, 0); !errors.Is(err, ErrRepositoryPathFormatRebuild) {
			t.Fatalf("ListFiles on legacy rows = %v, want rebuild error", err)
		}
		if _, err := s.ListFiles(ctx, repoID, `src\`, 10, 0); !errors.Is(err, ErrRepositoryPathFormatRebuild) {
			t.Fatalf("ListFiles with native filter on legacy rows = %v, want rebuild error", err)
		}
		if after := snapshotPathState(t, s, repoID); after != before {
			t.Fatalf("database mutated by rejected calls:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("E_dirty_files_alone_count_as_populated", func(t *testing.T) {
		s, repoID := openUnmarkedRepo(t)
		insertTestDirtyFile(t, s, repoID, `src\pkg\file.go`)
		before := snapshotPathState(t, s, repoID)
		requireRebuild(t, "Ensure(full) with legacy dirty rows", s.EnsureCanonicalRepositoryPaths(ctx, repoID, true))
		if _, ok := pathFormatMarker(t, s, repoID); ok {
			t.Fatal("full initialise adopted a marker over legacy dirty_files rows")
		}
		if after := snapshotPathState(t, s, repoID); after != before {
			t.Fatalf("database mutated:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
}

// TestLegacyRepositoryPathFormatFailsWithoutMutation drives every store-owned
// guard entrypoint (read and write) against one legacy database and proves
// the only outcome is the rebuild error: no path byte, dirty row, marker or
// row count changes.
func TestLegacyRepositoryPathFormatFailsWithoutMutation(t *testing.T) {
	ctx := context.Background()
	s, repoID := openUnmarkedRepo(t)
	for _, p := range []string{`src\pkg\file.go`, "src/pkg/other.go"} {
		if _, err := insertTestFile(ctx, s, repoID, p); err != nil {
			t.Fatal(err)
		}
	}
	insertTestDirtyFile(t, s, repoID, `src\pkg\file.go`)
	before := snapshotPathState(t, s, repoID)

	attempts := []struct {
		name string
		call func() error
	}{
		{"RequireCanonicalRepositoryPaths", func() error { return s.RequireCanonicalRepositoryPaths(ctx, repoID) }},
		{"EnsureCanonicalRepositoryPaths(full)", func() error { return s.EnsureCanonicalRepositoryPaths(ctx, repoID, true) }},
		{"EnsureCanonicalRepositoryPaths(incremental)", func() error { return s.EnsureCanonicalRepositoryPaths(ctx, repoID, false) }},
		{"ListFiles", func() error { _, err := s.ListFiles(ctx, repoID, "", 10, 0); return err }},
		{"RelatedTests", func() error { _, err := s.RelatedTests(ctx, repoID, "", "src/pkg/other.go", 10, 0); return err }},
		{"QueueDirtyFile", func() error { return s.QueueDirtyFile(ctx, repoID, "src/pkg/new.go", "test") }},
	}
	for _, a := range attempts {
		requireRebuild(t, a.name, a.call())
		if after := snapshotPathState(t, s, repoID); after != before {
			t.Fatalf("%s mutated the legacy database:\nbefore:\n%s\nafter:\n%s", a.name, before, after)
		}
	}
	if _, ok := pathFormatMarker(t, s, repoID); ok {
		t.Fatal("marker adopted on legacy database")
	}
	var native int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE repo_id=? AND path = ?`, repoID, `src\pkg\file.go`).Scan(&native); err != nil || native != 1 {
		t.Fatalf("legacy row count = %d, %v; want the original row, unrewritten", native, err)
	}
	var slashCopy int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE repo_id=? AND path = ?`, repoID, "src/pkg/file.go").Scan(&slashCopy); err != nil || slashCopy != 0 {
		t.Fatalf("slash copy count = %d, %v; want no migrated duplicate", slashCopy, err)
	}
}

// TestRepositoryPathFormatIsRepoScoped pins that the marker is keyed per
// repository: a legacy repo in the same database neither blocks nor rewrites a
// current one, and detecting it leaves both repos byte-identical.
func TestRepositoryPathFormatIsRepoScoped(t *testing.T) {
	ctx := context.Background()
	s, repoA := openUnmarkedRepo(t)
	repoB, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureCanonicalRepositoryPaths(ctx, repoA, true); err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestFile(ctx, s, repoA, "src/a.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestFile(ctx, s, repoB.ID, `src\b.go`); err != nil {
		t.Fatal(err)
	}
	beforeA := snapshotPathState(t, s, repoA)
	beforeB := snapshotPathState(t, s, repoB.ID)

	if err := s.RequireCanonicalRepositoryPaths(ctx, repoA); err != nil {
		t.Fatalf("repo A: %v", err)
	}
	requireRebuild(t, "repo B Require", s.RequireCanonicalRepositoryPaths(ctx, repoB.ID))
	requireRebuild(t, "repo B Ensure(full)", s.EnsureCanonicalRepositoryPaths(ctx, repoB.ID, true))
	requireRebuild(t, "repo B ListFiles", func() error { _, err := s.ListFiles(ctx, repoB.ID, "", 10, 0); return err }())

	files, err := s.ListFiles(ctx, repoA, "", 10, 0)
	if err != nil || len(files) != 1 || files[0]["path"] != "src/a.go" {
		t.Fatalf("repo A ListFiles after B rejection = %v, %v", files, err)
	}
	if afterA := snapshotPathState(t, s, repoA); afterA != beforeA {
		t.Fatalf("repo A mutated by B's rejection:\nbefore:\n%s\nafter:\n%s", beforeA, afterA)
	}
	// A stays writable, and A's write does not touch B.
	if err := s.QueueDirtyFile(ctx, repoA, "src/a.go", "test"); err != nil {
		t.Fatalf("repo A QueueDirtyFile after B rejection: %v", err)
	}
	var dirtyA string
	if err := s.db.QueryRowContext(ctx, `SELECT path FROM dirty_files WHERE repo_id=?`, repoA).Scan(&dirtyA); err != nil || dirtyA != "src/a.go" {
		t.Fatalf("repo A dirty row = %q, %v", dirtyA, err)
	}
	if afterB := snapshotPathState(t, s, repoB.ID); afterB != beforeB {
		t.Fatalf("repo B mutated:\nbefore:\n%s\nafter:\n%s", beforeB, afterB)
	}
	if got, ok := pathFormatMarker(t, s, repoA); !ok || got != "logical-slash-v1" {
		t.Fatalf("repo A marker = %q,%v", got, ok)
	}
	if _, ok := pathFormatMarker(t, s, repoB.ID); ok {
		t.Fatal("repo B marker adopted")
	}
	var markers int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key LIKE ?`, canonicalRepositoryPathsSettingKey+".%").Scan(&markers); err != nil || markers != 1 {
		t.Fatalf("marker rows = %d, %v; want exactly repo A's", markers, err)
	}
}

// TestRepositoryPathFormatErrorContract pins how callers tell "rebuild
// required" apart from ordinary failures, and that the message never claims
// an in-place migration happened.
func TestRepositoryPathFormatErrorContract(t *testing.T) {
	ctx := context.Background()
	s, repoID := openUnmarkedRepo(t)
	if _, err := insertTestFile(ctx, s, repoID, "src/pkg/file.go"); err != nil {
		t.Fatal(err)
	}
	err := s.RequireCanonicalRepositoryPaths(ctx, repoID)
	if !errors.Is(err, ErrRepositoryPathFormatRebuild) {
		t.Fatalf("Require returned %v, want errors.Is(ErrRepositoryPathFormatRebuild)", err)
	}
	if _, lerr := s.ListFiles(ctx, repoID, "", 10, 0); !errors.Is(lerr, ErrRepositoryPathFormatRebuild) {
		t.Fatalf("ListFiles wraps the sentinel away: %v", lerr)
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "re-index") && !strings.Contains(msg, "rebuild") {
		t.Fatalf("message does not name the recovery action: %q", err.Error())
	}
	if strings.Contains(msg, "migrat") {
		t.Fatalf("message claims a migration: %q", err.Error())
	}
	if errors.Is(ErrSymbolNotFound, ErrRepositoryPathFormatRebuild) {
		t.Fatal("rebuild sentinel is confusable with symbol-not-found")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if cerr := s.RequireCanonicalRepositoryPaths(ctx, repoID); cerr == nil || errors.Is(cerr, ErrRepositoryPathFormatRebuild) {
		t.Fatalf("closed database reported as %v; want an ordinary error distinct from rebuild-required", cerr)
	}
}

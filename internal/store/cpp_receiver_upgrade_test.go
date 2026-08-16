package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestCppReceiverUpgradeMarkScope pins the three properties the P22.11 upgrade
// mark has to have to be safe on a shared database: it touches only C/C++ rows,
// only the repository it was asked about, and only once.
func TestCppReceiverUpgradeMarkScope(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	repoA, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo(A) error = %v", err)
	}
	repoB, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo(B) error = %v", err)
	}

	seed := func(repoID int64, path, language string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO files(repo_id, path, language, size_bytes, mtime_unix_ns, content_sha256, is_deleted)
			VALUES(?, ?, ?, 10, 4242, 'abc', 0)`, repoID, path, language); err != nil {
			t.Fatalf("seed %s error = %v", path, err)
		}
	}
	seed(repoA.ID, "a.cpp", "cpp")
	seed(repoA.ID, "a.go", "go")
	seed(repoB.ID, "b.cpp", "cpp")

	meta := func(repoID int64, path string) (int64, string) {
		t.Helper()
		var mtime int64
		var hash string
		if err := s.db.QueryRowContext(ctx,
			`SELECT mtime_unix_ns, content_sha256 FROM files WHERE repo_id = ? AND path = ?`,
			repoID, path).Scan(&mtime, &hash); err != nil {
			t.Fatalf("meta %s error = %v", path, err)
		}
		return mtime, hash
	}

	pending, err := s.CppReceiverUpgradePending(ctx, repoA.ID)
	if err != nil || !pending {
		t.Fatalf("CppReceiverUpgradePending(A) = %v, %v; want true, nil", pending, err)
	}
	if err := s.MarkCppFilesForReparseOnce(ctx, repoA.ID); err != nil {
		t.Fatalf("MarkCppFilesForReparseOnce(A) error = %v", err)
	}

	if mtime, hash := meta(repoA.ID, "a.cpp"); mtime != -1 || hash != "" {
		t.Fatalf("A/a.cpp metadata = (%d, %q), want (-1, \"\")", mtime, hash)
	}
	if mtime, hash := meta(repoA.ID, "a.go"); mtime != 4242 || hash != "abc" {
		t.Fatalf("A/a.go metadata = (%d, %q), want it untouched", mtime, hash)
	}
	if mtime, hash := meta(repoB.ID, "b.cpp"); mtime != 4242 || hash != "abc" {
		t.Fatalf("B/b.cpp metadata = (%d, %q), want it untouched (other repository)", mtime, hash)
	}

	// Idempotent: a second call after the indexer has rewritten the metadata
	// must not clear it again, or every run would reparse the whole tree.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE files SET mtime_unix_ns = 99, content_sha256 = 'xyz' WHERE repo_id = ?`, repoA.ID); err != nil {
		t.Fatalf("rewrite metadata error = %v", err)
	}
	pending, err = s.CppReceiverUpgradePending(ctx, repoA.ID)
	if err != nil || pending {
		t.Fatalf("CppReceiverUpgradePending(A) after mark = %v, %v; want false, nil", pending, err)
	}
	if err := s.MarkCppFilesForReparseOnce(ctx, repoA.ID); err != nil {
		t.Fatalf("second MarkCppFilesForReparseOnce(A) error = %v", err)
	}
	if mtime, hash := meta(repoA.ID, "a.cpp"); mtime != 99 || hash != "xyz" {
		t.Fatalf("A/a.cpp metadata = (%d, %q) after second mark, want it untouched", mtime, hash)
	}

	// The other repository still owes its own upgrade.
	pending, err = s.CppReceiverUpgradePending(ctx, repoB.ID)
	if err != nil || !pending {
		t.Fatalf("CppReceiverUpgradePending(B) = %v, %v; want true, nil", pending, err)
	}
}

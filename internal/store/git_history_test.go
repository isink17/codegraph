package store

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/githistory"
)

// A database written before the history migration reads as not computed and
// gains the tables when a writable binary opens it.
func TestGitHistoryMigrationOnOlderDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE git_history_state; DROP TABLE git_file_history; DROP TABLE git_worktree_changes; DROP TABLE git_symbol_history; DROP TABLE git_filtered_paths;
		DELETE FROM schema_migrations WHERE version IN (2, 3)`); err != nil {
		t.Fatal(err)
	}
	got, err := s.GitHistory(ctx, 1, GitHistoryQuery{})
	if err != nil || got.History.Status != githistory.StatusAbsent || got.History.AbsentReason != githistory.ReasonNotComputed {
		t.Fatalf("pre-migration read = %+v, %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := githistory.State{Status: githistory.StatusOK, Watermark: "abc", WindowLimit: 250, WindowCommits: 1, Algorithm: githistory.Algorithm}
	if err := s.ReplaceGitHistory(ctx, 1, state, []githistory.FileStats{{Path: "a.go", Commits: 1, FirstSHA: "abc", LastSHA: "abc", Authors: 1, TopAuthor: "a@x", TopAuthorCommits: 1}}, []string{"b.go"}, GitSymbolUpdate{}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GitHistory(ctx, 1, GitHistoryQuery{})
	if err != nil || got.Total != 2 || got.Files[0].Path != "a.go" || !got.Files[1].WorktreeDiffers {
		t.Fatalf("after migration = %+v, %v", got, err)
	}
}

// A database from before symbol history (migration 2 only) keeps its stored
// history when opened, gains the tables, and records migration 3. A database
// from a newer binary (a migration this build lacks) is refused without a byte changing, which is what an
// older binary does with migration 5.
func TestGitSymbolHistoryMigrationUpgradeAndNewerRefusal(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	state := githistory.State{Status: githistory.StatusOK, Watermark: "abc", WindowLimit: 250, WindowCommits: 1, Algorithm: "file-v1"}
	if err := s.ReplaceGitHistory(ctx, 1, state, []githistory.FileStats{{Path: "a.go", Commits: 1, FirstSHA: "abc", LastSHA: "abc", Authors: 1, TopAuthor: "a@x", TopAuthorCommits: 1}}, nil, GitSymbolUpdate{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE git_symbol_history; DROP TABLE git_filtered_paths; DELETE FROM schema_migrations WHERE version = 3`); err != nil {
		t.Fatal(err)
	}
	before, err := s.GitHistory(ctx, 1, GitHistoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	after, err := s.GitHistory(ctx, 1, GitHistoryQuery{})
	if err != nil || !reflect.DeepEqual(before, after) || after.History.Algorithm != "file-v1" {
		t.Fatalf("upgrade changed stored history:\n%+v\n%+v (%v)", before, after, err)
	}
	var tables, applied int
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sqlite_master WHERE name IN ('git_symbol_history', 'git_filtered_paths')),
		(SELECT COUNT(*) FROM schema_migrations WHERE version = 3)`).Scan(&tables, &applied); err != nil || tables != 2 || applied != 1 {
		t.Fatalf("after upgrade: tables=%d migration3=%d %v", tables, applied, err)
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (5, '2026-10-04T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot := func() [32]byte {
		var sum [32]byte
		for _, suffix := range []string{"", "-wal"} {
			data, err := os.ReadFile(path + suffix)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			part := sha256.Sum256(data)
			for k := range sum {
				sum[k] ^= part[k]
			}
		}
		return sum
	}
	was := snapshot()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("database with a newer migration opened")
	}
	if snapshot() != was {
		t.Fatal("refused open mutated the database")
	}
}

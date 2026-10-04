package store

import (
	"context"
	"path/filepath"
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
	if _, err := s.db.ExecContext(ctx, `DROP TABLE git_history_state; DROP TABLE git_file_history; DROP TABLE git_worktree_changes; DROP TABLE git_symbol_history;
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

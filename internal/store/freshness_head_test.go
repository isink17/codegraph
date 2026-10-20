package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/isink17/codegraph/internal/githistory"
	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/graph"
)

// headScan records a completed scan of the given scope at head the way the
// indexer does: the row's HEADs and the Git history watermark, which every
// scan advances, path-scoped ones included.
func headScan(t *testing.T, s *Store, repoID int64, scope, head string) {
	t.Helper()
	id, started := scopedScan(t, s, repoID, scope, head)
	if err := s.ReplaceGitHistory(context.Background(), repoID, githistory.State{Status: githistory.StatusOK, Watermark: head}, []githistory.FileStats{}, nil, GitSymbolUpdate{}); err != nil {
		t.Fatal(err)
	}
	completeScan(t, s, id, started, "completed", head)
}

func expectHead(t *testing.T, s *Store, repoID int64, step, atIndex, source string, moved bool) graph.Freshness {
	t.Helper()
	f := freshness(t, s, repoID)
	wantChanged, wantVerdict := "no", graph.FreshnessVerdictConsistentWithLastScan
	if moved {
		wantChanged, wantVerdict = "yes", graph.FreshnessVerdictStaleKnown
	}
	if f.Worktree.HeadAtIndex != atIndex || f.Worktree.HeadAtIndexSource != source || f.Worktree.HeadChanged != wantChanged ||
		f.Verdict != wantVerdict || slices.Contains(f.Reasons, "head_moved") != moved {
		t.Fatalf("%s: worktree %+v verdict %s reasons %v", step, f.Worktree, f.Verdict, f.Reasons)
	}
	return f
}

// A path-scoped scan at a new HEAD advances the history watermark but not the
// last full scan, so HEAD still reads as moved until a full scan covers it.
func TestFreshnessHeadMovedAfterPathScopedScan(t *testing.T) {
	r := gittest.Init(t)
	r.Write("a.go", "package a\n\nfunc A() {}\n")
	r.Write("b.go", "package a\n\nfunc B() {}\n")
	first := r.Commit("", "one")
	s, repoID := freshnessStore(t, r.Dir)
	headScan(t, s, repoID, ScanScopeFull, first)
	expectHead(t, s, repoID, "full at first", first, "last_full_scan", false)

	headScan(t, s, repoID, ScanScopePaths, first)
	expectHead(t, s, repoID, "path scan, HEAD unchanged", first, "last_full_scan", false)

	r.Write("a.go", "package a\n\nfunc A() { B() }\n")
	r.Write("b.go", "package a\n\nfunc B() {}\n\nfunc C() {}\n")
	second := r.Commit("", "two")
	expectHead(t, s, repoID, "commit", first, "last_full_scan", true)

	headScan(t, s, repoID, ScanScopePaths, second)
	f := expectHead(t, s, repoID, "path scan at new HEAD", first, "last_full_scan", true)
	if f.Worktree.HeadNow != second || f.LatestScan.HeadAtFinish != second || f.Coverage.LastFullScan.HeadAtFinish != first {
		t.Fatalf("path scan at new HEAD: %+v / %+v", f.Worktree, f.Coverage.LastFullScan)
	}

	// Detached at the newer commit: the same HEAD id, still moved.
	r.Git("checkout", "-q", "--detach", second)
	expectHead(t, s, repoID, "detached at new HEAD", first, "last_full_scan", true)

	// Back at the commit the full scan saw: HEAD is compared by id.
	r.Git("checkout", "-q", "--detach", first)
	expectHead(t, s, repoID, "reverted to full scan HEAD", first, "last_full_scan", false)

	r.Git("checkout", "-q", "--detach", second)
	headScan(t, s, repoID, ScanScopeFull, second)
	expectHead(t, s, repoID, "full scan at new HEAD", second, "last_full_scan", false)
}

// A commit followed directly by a full scan clears the moved HEAD.
func TestFreshnessHeadMovedThenFullScan(t *testing.T) {
	r := gittest.Init(t)
	first := r.Commit("", "one")
	s, repoID := freshnessStore(t, r.Dir)
	headScan(t, s, repoID, ScanScopeFull, first)
	second := r.Commit("", "two")
	expectHead(t, s, repoID, "commit", first, "last_full_scan", true)
	headScan(t, s, repoID, ScanScopeFull, second)
	expectHead(t, s, repoID, "full scan", second, "last_full_scan", false)
}

// Without a full scan that recorded its HEAD, the history watermark stays the
// anchor: rows from before coverage recording, or a full scan whose HEAD
// could not be read.
func TestFreshnessHeadFallsBackToWatermark(t *testing.T) {
	r := gittest.Init(t)
	first := r.Commit("", "one")
	s, repoID := freshnessStore(t, r.Dir)
	headScan(t, s, repoID, ScanScopePaths, first)
	expectHead(t, s, repoID, "path scan only", first, "git_history_watermark", false)
	headScan(t, s, repoID, ScanScopeFull, "")
	if err := s.ReplaceGitHistory(context.Background(), repoID, githistory.State{Status: githistory.StatusOK, Watermark: first}, []githistory.FileStats{}, nil, GitSymbolUpdate{}); err != nil {
		t.Fatal(err)
	}
	expectHead(t, s, repoID, "full scan without HEAD", first, "git_history_watermark", false)
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return [32]byte{}
	}
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// Reading freshness writes no database file and recreates no removed scan
// lock, through a writable or a read-only handle.
func TestFreshnessStatusIsReadOnly(t *testing.T) {
	r := gittest.Init(t)
	first := r.Commit("", "one")
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(context.Background(), r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	headScan(t, s, repo.ID, ScanScopeFull, first)
	if err := s.MarkReconcilePending(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(scanLockPath(path)); err != nil {
		t.Fatal(err)
	}
	files := []string{path, path + "-wal"}
	sums := func() [][32]byte {
		out := make([][32]byte, len(files))
		for i, f := range files {
			out[i] = fileSum(t, f)
		}
		return out
	}
	before := sums()
	freshness(t, s, repo.ID)
	ro, err := OpenReadOnly(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.FreshnessStatus(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	_ = ro.Close()
	if after := sums(); !slices.Equal(before, after) {
		t.Fatal("freshness read changed a database file")
	}
	if _, err := os.Stat(scanLockPath(path)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("scan lock recreated: %v", err)
	}
}

package indexer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func coverageFixture(t *testing.T) (*gittest.Repo, string, *store.Store, string) {
	t.Helper()
	r := gittest.Init(t)
	r.Write("a.go", "package a\n\nfunc A() {}\n")
	head := r.Commit("", "one")
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return r, head, s, dbPath
}

func coverageOf(t *testing.T, s *store.Store, root string) graph.Freshness {
	t.Helper()
	ctx := context.Background()
	repo, found, err := s.FindRepo(ctx, root)
	if err != nil || !found {
		t.Fatalf("FindRepo = %v, %v", found, err)
	}
	f, err := s.FreshnessStatus(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Coverage.Recorded {
		t.Fatalf("coverage not recorded on a migrated database: %+v", f.Coverage)
	}
	return f
}

// A full scan records scope full and HEAD at both ends. A later path-scoped
// update at a new HEAD advances the Git history watermark, so head_changed
// reads "no", but it never becomes the last full scan.
func TestScanCoverageFullThenPathScopedAtNewHead(t *testing.T) {
	ctx := context.Background()
	r, first, s, _ := coverageFixture(t)
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: r.Dir}); err != nil {
		t.Fatal(err)
	}
	f := coverageOf(t, s, r.Dir)
	full := f.Coverage.LastFullScan
	if full == nil || full.Scope != store.ScanScopeFull || full.HeadAtStart != first || full.HeadAtFinish != first || full.Overlap != "none" {
		t.Fatalf("last full scan = %+v", full)
	}
	if f.Coverage.FailedAfterLastFull != 0 {
		t.Fatalf("failed after full = %d", f.Coverage.FailedAfterLastFull)
	}

	r.Write("b.go", "package a\n\nfunc B() { A() }\n")
	second := r.Commit("", "two")
	if _, err := idx.Update(ctx, Options{RepoRoot: r.Dir, Paths: []string{"b.go"}}); err != nil {
		t.Fatal(err)
	}
	f = coverageOf(t, s, r.Dir)
	if f.LatestScan.Scope != store.ScanScopePaths || f.LatestScan.HeadAtStart != second || f.LatestScan.HeadAtFinish != second {
		t.Fatalf("latest = %+v", f.LatestScan)
	}
	if f.Worktree.HeadAtIndex != second || f.Worktree.HeadChanged != "no" {
		t.Fatalf("watermark should follow the path-scoped scan: %+v", f.Worktree)
	}
	if f.Coverage.LastFullScan == nil || f.Coverage.LastFullScan.ID != full.ID || f.Coverage.LastFullScan.HeadAtFinish != first {
		t.Fatalf("path-scoped scan proved full coverage: %+v", f.Coverage.LastFullScan)
	}

	// An update without paths is a full walk.
	if _, err := idx.Update(ctx, Options{RepoRoot: r.Dir}); err != nil {
		t.Fatal(err)
	}
	f = coverageOf(t, s, r.Dir)
	if f.Coverage.LastFullScan.ID == full.ID || f.Coverage.LastFullScan.HeadAtFinish != second || f.Coverage.LastFullScan.Kind != "update" {
		t.Fatalf("full update not recorded: %+v", f.Coverage.LastFullScan)
	}
}

// A walk under a language override that differs from the repository's
// configuration is filtered, not full; an override equal to it is full.
func TestScanCoverageFilteredWalkIsNotFull(t *testing.T) {
	ctx := context.Background()
	r, _, s, _ := coverageFixture(t)
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: r.Dir, Languages: []string{"go"}}); err != nil {
		t.Fatal(err)
	}
	f := coverageOf(t, s, r.Dir)
	if f.LatestScan.Scope != store.ScanScopeFiltered || f.Coverage.LastFullScan != nil {
		t.Fatalf("latest = %+v, last full = %+v", f.LatestScan, f.Coverage.LastFullScan)
	}

	r.Write(".codegraph/config.json", `{"languages": ["go"]}`)
	if _, err := idx.Index(ctx, Options{RepoRoot: r.Dir, Languages: []string{"go"}}); err != nil {
		t.Fatal(err)
	}
	f = coverageOf(t, s, r.Dir)
	if f.LatestScan.Scope != store.ScanScopeFull || f.Coverage.LastFullScan == nil {
		t.Fatalf("override equal to repo config: latest = %+v", f.LatestScan)
	}
}

// Uncommitted edits are indexed but never examined for freshness: the full
// scan still records HEAD, and nothing reads as fresh or as checked.
func TestScanCoverageDirtyWorktreeIsNotChecked(t *testing.T) {
	ctx := context.Background()
	r, head, s, _ := coverageFixture(t)
	r.Write("a.go", "package a\n\nfunc A() {}\n\nfunc Dirty() {}\n")
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: r.Dir}); err != nil {
		t.Fatal(err)
	}
	r.Write("a.go", "package a\n\nfunc A() {}\n\nfunc Dirtier() {}\n")
	f := coverageOf(t, s, r.Dir)
	if f.Coverage.LastFullScan == nil || f.Coverage.LastFullScan.HeadAtFinish != head {
		t.Fatalf("last full = %+v", f.Coverage.LastFullScan)
	}
	if f.State == "fresh" || f.Filesystem != "not_checked" || f.Watcher != "not_in_this_process" {
		t.Fatalf("dirty worktree claimed checked: %+v", f)
	}
}

// A cancelled full scan and a full scan whose last write fails are recorded
// failed with their scope; neither becomes the last full scan and each counts
// as a failure after it. Neither records a finishing HEAD.
func TestScanCoverageFailedFullScans(t *testing.T) {
	ctx := context.Background()
	r, head, s, dbPath := coverageFixture(t)
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: r.Dir}); err != nil {
		t.Fatal(err)
	}
	full := coverageOf(t, s, r.Dir).Coverage.LastFullScan

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.Write("a.go", "package a\n\nfunc A() {}\n\nfunc C() {}\n")
	cancelling := New(s, parser.NewRegistry(cancelingGoAdapter{Adapter: goparser.New(), cancel: cancel}), nil)
	if _, err := cancelling.Index(cctx, Options{RepoRoot: r.Dir}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Index() error = %v; want context.Canceled", err)
	}
	f := coverageOf(t, s, r.Dir)
	if f.LatestScan.Status != "failed" || f.LatestScan.Scope != store.ScanScopeFull || f.LatestScan.HeadAtStart != head || f.LatestScan.HeadAtFinish != "" {
		t.Fatalf("cancelled scan = %+v", f.LatestScan)
	}
	if f.Coverage.LastFullScan.ID != full.ID || f.Coverage.FailedAfterLastFull != 1 {
		t.Fatalf("coverage after cancel = %+v", f.Coverage)
	}

	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, event := range []string{"INSERT", "UPDATE"} {
		if _, err := raw.Exec(`CREATE TRIGGER fail_swiftpm_` + event + ` BEFORE ` + event + ` ON settings
			WHEN NEW.key LIKE 'scope.swiftpm_manifest_fingerprint%'
			BEGIN SELECT RAISE(ABORT, 'injected fingerprint failure'); END`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := idx.Update(ctx, Options{RepoRoot: r.Dir}); err == nil || !strings.Contains(err.Error(), "injected fingerprint failure") {
		t.Fatalf("Update() error = %v", err)
	}
	f = coverageOf(t, s, r.Dir)
	if f.LatestScan.Status != "failed" || f.LatestScan.Scope != store.ScanScopeFull || f.LatestScan.HeadAtFinish != "" {
		t.Fatalf("failed update = %+v", f.LatestScan)
	}
	if f.Coverage.LastFullScan.ID != full.ID || f.Coverage.FailedAfterLastFull != 2 {
		t.Fatalf("coverage after failed write = %+v", f.Coverage)
	}
}

// Each linked worktree records its own HEAD: moving another worktree's HEAD
// changes neither this worktree's recorded heads nor its current HEAD.
func TestScanCoverageWorktreeDivergence(t *testing.T) {
	ctx := context.Background()
	r, head, s, _ := coverageFixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	r.Git("worktree", "add", "-q", "-b", "side", linked)
	other := &gittest.Repo{T: t, Dir: linked}
	other.Write("c.go", "package a\n\nfunc C() {}\n")
	sideHead := other.Commit("", "side")

	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: r.Dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Index(ctx, Options{RepoRoot: linked}); err != nil {
		t.Fatal(err)
	}
	main := coverageOf(t, s, r.Dir)
	side := coverageOf(t, s, linked)
	if main.Coverage.LastFullScan.HeadAtFinish != head || main.Worktree.HeadNow != head {
		t.Fatalf("main worktree = %+v / %+v", main.Coverage.LastFullScan, main.Worktree)
	}
	if side.Coverage.LastFullScan.HeadAtFinish != sideHead || side.Worktree.HeadNow != sideHead {
		t.Fatalf("linked worktree = %+v / %+v", side.Coverage.LastFullScan, side.Worktree)
	}
}

package indexer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// cancelingGoAdapter cancels the scan's context from inside a parse, which is
// a caller cancellation arriving mid-scan.
type cancelingGoAdapter struct {
	*goparser.Adapter
	cancel context.CancelFunc
}

func (a cancelingGoAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	a.cancel()
	return a.Adapter.Parse(ctx, path, content)
}

func latestScan(t *testing.T, s *store.Store, repoRoot string) store.ScanRecord {
	t.Helper()
	ctx := context.Background()
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	scans, err := s.ListScans(ctx, repo.ID, 1, 0)
	if err != nil || len(scans) != 1 {
		t.Fatalf("ListScans() = %v, %v; want one scan", scans, err)
	}
	return scans[0]
}

func TestCancelledScanIsRecordedFailed(t *testing.T) {
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "a.go"), "package a\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(repoRoot, "b.go"), "package a\n\nfunc B() { A() }\n")
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idx := New(s, parser.NewRegistry(cancelingGoAdapter{Adapter: goparser.New(), cancel: cancel}), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Index() error = %v; want context.Canceled", err)
	}

	scan := latestScan(t, s, repoRoot)
	if scan.Status != "failed" || !strings.Contains(scan.ErrorText, "context canceled") || scan.FinishedAt == "" {
		t.Fatalf("scan = status %q, error %q, finished %q; want failed with \"context canceled\"", scan.Status, scan.ErrorText, scan.FinishedAt)
	}
}

func TestScanFailingLastWriteIsRecordedFailed(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "a.go"), "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	// Fail the SwiftPM fingerprint write, the scan's last write before it is
	// closed, on the following update.
	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, false)
	if err != nil {
		t.Fatalf("BuildSQLiteDSN() error = %v", err)
	}
	raw, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer raw.Close()
	for _, event := range []string{"INSERT", "UPDATE"} {
		if _, err := raw.Exec(`CREATE TRIGGER fail_swiftpm_` + event + ` BEFORE ` + event + ` ON settings
			WHEN NEW.key LIKE 'scope.swiftpm_manifest_fingerprint%'
			BEGIN SELECT RAISE(ABORT, 'injected fingerprint failure'); END`); err != nil {
			t.Fatalf("create trigger: %v", err)
		}
	}

	writeFile(t, filepath.Join(repoRoot, "a.go"), "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: repoRoot}); err == nil || !strings.Contains(err.Error(), "injected fingerprint failure") {
		t.Fatalf("Update() error = %v; want injected fingerprint failure", err)
	}

	scan := latestScan(t, s, repoRoot)
	if scan.Status != "failed" || !strings.Contains(scan.ErrorText, "injected fingerprint failure") {
		t.Fatalf("scan = status %q, error %q; want failed with the injected error", scan.Status, scan.ErrorText)
	}
}

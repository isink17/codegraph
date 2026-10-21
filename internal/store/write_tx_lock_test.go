package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/graph"
)

// Every read-write open of an existing database briefly holds the write lock
// (the locked validation gate and Migrate use BEGIN IMMEDIATE), so a status
// read can overlap a running scan. A scan transaction that read before writing
// could not upgrade while that lock was held: SQLite returns SQLITE_BUSY at
// once instead of running the busy handler, and the scan failed.
func TestScanWriteTransactionWaitsForConcurrentOpenerLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scanID, _, err := s.BeginScan(ctx, repo.ID, "full")
	if err != nil {
		t.Fatal(err)
	}

	dsn, err := buildSQLiteSafeExistingDSN(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	opener, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer opener.Close()
	gate, err := opener.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	if _, err := gate.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.ReplaceFileGraph(ctx, repo.ID, scanID, "a.go", "go", 1, 1, "h", graph.ParsedFile{
			Symbols: []graph.Symbol{{Name: "A", QualifiedName: "a.A", Kind: "function"}},
		})
	}()
	select {
	case err := <-done:
		_, _ = gate.ExecContext(ctx, `ROLLBACK`)
		t.Fatalf("scan write finished while another connection held the write lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := gate.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("scan write after the opener released its lock: %v", err)
	}
}

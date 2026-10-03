//go:build !sqlite_cgo

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	sqlite "modernc.org/sqlite"
)

// betweenStatementsCancel is a context whose cancellation is observed only by
// Done channels requested after it fires. A statement already running keeps
// the never-closing channel it was handed, so the driver never interrupts it:
// the cancellation lands deterministically between two statements of a
// transaction, which is exactly where a real cancellation can land.
type betweenStatementsCancel struct {
	context.Context
	fired  atomic.Bool
	open   chan struct{}
	closed chan struct{}
}

func newBetweenStatementsCancel() *betweenStatementsCancel {
	c := &betweenStatementsCancel{Context: context.Background(), open: make(chan struct{}), closed: make(chan struct{})}
	close(c.closed)
	return c
}

func (c *betweenStatementsCancel) Done() <-chan struct{} {
	if c.fired.Load() {
		return c.closed
	}
	return c.open
}

func (c *betweenStatementsCancel) Err() error {
	if c.fired.Load() {
		return context.Canceled
	}
	return nil
}

var (
	cancelFromSQLOnce sync.Once
	cancelFromSQL     atomic.Pointer[betweenStatementsCancel]
)

// registerCancelFromSQL installs a SQL function that fires the armed context.
// It must run before the store opens its connection.
func registerCancelFromSQL(t *testing.T) {
	t.Helper()
	cancelFromSQLOnce.Do(func() {
		sqlite.MustRegisterScalarFunction("codegraph_test_cancel", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
			if c := cancelFromSQL.Swap(nil); c != nil {
				c.fired.Store(true)
			}
			return nil, nil
		})
	})
}

// A claim cancelled after BEGIN IMMEDIATE must roll back on the connection it
// hands back to the pool. Otherwise the pooled connection stays inside the
// write transaction: later autocommit writes join it and are never committed,
// and every other connection is locked out.
func TestClaimDirtyFilesCancelledMidTransactionReleasesConnection(t *testing.T) {
	registerCancelFromSQL(t)
	bg := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(bg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureCanonicalRepositoryPaths(bg, repo.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDirtyFile(bg, repo.ID, "a.go", "event"); err != nil {
		t.Fatal(err)
	}
	// TEMP: bound to the store's single pooled connection, not the schema.
	if _, err := s.db.ExecContext(bg, `CREATE TEMP TRIGGER cancel_claim AFTER UPDATE ON dirty_files BEGIN SELECT codegraph_test_cancel(); END`); err != nil {
		t.Fatal(err)
	}

	ctx := newBetweenStatementsCancel()
	cancelFromSQL.Store(ctx)
	if _, err := s.ClaimDirtyFiles(ctx, repo.ID, "2026-01-01T00:00:00Z", "watch_inflight"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ClaimDirtyFiles err = %v, want context.Canceled", err)
	}

	if err := s.QueueDirtyFile(bg, repo.ID, "b.go", "event"); err != nil {
		t.Fatalf("QueueDirtyFile after cancelled claim: %v", err)
	}
	dsn, err := BuildSQLiteDSN(dbPath, OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	rows, err := other.QueryContext(bg, `SELECT path || ':' || reason FROM dirty_files WHERE repo_id = ? ORDER BY path`, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.go:event", "b.go:event"}; !slices.Equal(got, want) {
		t.Fatalf("committed dirty rows seen by another connection = %q, want %q (claim not rolled back, later write not committed)", got, want)
	}
	paths, err := s.ClaimDirtyFiles(bg, repo.ID, "2026-01-02T00:00:00Z", "watch_inflight")
	if err != nil || !slices.Equal(paths, []string{"a.go", "b.go"}) {
		t.Fatalf("claim after recovery = %q, %v", paths, err)
	}
}

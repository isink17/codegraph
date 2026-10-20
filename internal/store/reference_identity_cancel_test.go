package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/graph"
)

// A pass cancelled at any point leaves either every identity it would derive
// or none, and returns its connection to the pool outside a transaction.
func TestReferenceIdentityReconcileCancelledRollsBack(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	for i, name := range []string{"a.go", "b.go"} {
		parsed := graph.ParsedFile{
			Symbols:    []graph.Symbol{{Language: "go", Kind: "function", Name: "f", QualifiedName: "f" + name, StableKey: "f" + name, Range: graph.Position{StartLine: 1, EndLine: 3}}},
			References: []graph.Reference{{Kind: "call", Name: "f", Range: graph.Position{StartLine: 2, StartCol: 2, EndLine: 2, EndCol: 3}}},
			Edges:      []graph.Edge{{DstName: "f", Kind: "calls", Line: 2, Col: 2}},
		}
		if err := s.ReplaceFileGraph(ctx, repoID, int64(i+1), name, "go", 1, 1, "go", parsed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = src_symbol_id WHERE repo_id = ?`, repoID); err != nil {
		t.Fatal(err)
	}
	identities := func() string {
		var out string
		if err := s.db.QueryRowContext(ctx, `SELECT group_concat(id || ':' || COALESCE(symbol_id, '-') || ':' || COALESCE(context_symbol_id, '-'), ',') FROM (SELECT * FROM references_tbl WHERE repo_id = ? ORDER BY id)`, repoID).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	clearIdentities := func() {
		if _, err := s.db.ExecContext(ctx, `UPDATE references_tbl SET symbol_id = NULL, context_symbol_id = NULL WHERE repo_id = ?`, repoID); err != nil {
			t.Fatal(err)
		}
	}
	clearIdentities()
	before := identities()
	if err := s.ReconcileReferenceIdentities(ctx, repoID); err != nil {
		t.Fatal(err)
	}
	after := identities()
	if before == after {
		t.Fatalf("fixture derives nothing: %s", after)
	}
	s.db.SetMaxOpenConns(1)
	cancelled := 0
	for i := range 200 {
		clearIdentities()
		runCtx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(time.Duration(i%40)*25*time.Microsecond, cancel)
		err := s.ReconcileReferenceIdentities(runCtx, repoID)
		timer.Stop()
		cancel()
		if err != nil {
			cancelled++
		}
		if got := identities(); got != before && got != after {
			t.Fatalf("run %d (err %v) left %s, want %s or %s", i, err, got, before, after)
		}
		if err != nil && identities() != before {
			t.Fatalf("run %d failed with %v but kept its writes", i, err)
		}
		// The pool has one connection, the one the run used: returned inside
		// a transaction it could not begin another.
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			t.Fatalf("run %d: connection returned inside a transaction: %v", i, err)
		}
		if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
	if cancelled == 0 {
		t.Fatal("no run was cancelled")
	}
	if err := s.ReconcileReferenceIdentities(ctx, repoID); err != nil || identities() != after {
		t.Fatalf("pass after cancellations: %v, %s", err, identities())
	}
}

// beginErrConn reports a cancellation for a BEGIN that took effect, the race
// a cancelled context can lose.
type beginErrConn struct{ driver.Conn }

func (c beginErrConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	res, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err == nil && query == `BEGIN IMMEDIATE` {
		return nil, context.Canceled
	}
	return res, err
}

type beginErrConnector struct {
	drv driver.Driver
	dsn string
}

func (c beginErrConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	return beginErrConn{conn}, err
}

func (c beginErrConnector) Driver() driver.Driver { return c.drv }

// A BEGIN that took effect but reported an error is rolled back before its
// connection returns to the pool.
func TestReferenceIdentityRollsBackBeginReportedFailed(t *testing.T) {
	ctx := context.Background()
	probe, err := sql.Open(sqliteDriverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	drv := probe.Driver()
	_ = probe.Close()
	db := sql.OpenDB(beginErrConnector{drv: drv, dsn: filepath.Join(t.TempDir(), "graph.sqlite")})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s := &Store{db: db}
	if err := s.ReconcileReferenceIdentities(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("connection returned inside a transaction: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
}

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// execHookConn passes every Exec and Query through hook, which runs the
// statement with exec and may cancel or fail it around that point.
type execHookConn struct {
	driver.Conn
	hook func(ctx context.Context, query string, exec func() error) error
}

func (c execHookConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	var res driver.Result
	err := c.hook(ctx, query, func() (err error) {
		res, err = c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
		return err
	})
	return res, err
}

func (c execHookConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	var rows driver.Rows
	err := c.hook(ctx, query, func() (err error) {
		rows, err = c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
		return err
	})
	if err != nil && rows != nil {
		_ = rows.Close()
		rows = nil
	}
	return rows, err
}

type execHookConnector struct {
	drv  driver.Driver
	dsn  string
	hook func(ctx context.Context, query string, exec func() error) error
}

func (c execHookConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return execHookConn{Conn: conn, hook: c.hook}, nil
}

func (c execHookConnector) Driver() driver.Driver { return c.drv }

// hookedStore is a migrated store whose database runs every Exec through hook.
func hookedStore(t *testing.T, hook func(ctx context.Context, query string, exec func() error) error) (*Store, int64) {
	t.Helper()
	s, repoID := newQueryTestStore(t)
	dsn, err := BuildSQLiteDSN(s.path, OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	orig := s.db
	s.db = sql.OpenDB(execHookConnector{drv: orig.Driver(), dsn: dsn, hook: hook})
	t.Cleanup(func() { _ = orig.Close() })
	return s, repoID
}

// cancelAfter runs the armed statement, then cancels its context and reports
// what a driver observing that cancellation would: the race in which a
// statement takes effect but its caller is told it was cancelled.
type cancelAfter struct {
	query  string
	armed  atomic.Bool
	cancel context.CancelFunc
}

func (c *cancelAfter) hook(ctx context.Context, query string, exec func() error) error {
	if !strings.Contains(query, c.query) || !c.armed.CompareAndSwap(true, false) {
		return exec()
	}
	if err := exec(); err != nil {
		return err
	}
	c.cancel()
	return ctx.Err()
}

// requireFreeConnection fails unless the store's only pooled connection can be
// taken and can start a write transaction.
func requireFreeConnection(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pooled connection not returned: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("connection returned inside a transaction: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
}

func dirtyRows(t *testing.T, s *Store, repoID int64) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), `SELECT path || ':' || reason FROM dirty_files WHERE repo_id = ? ORDER BY path`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return out
}

// withoutReturning rejects DELETE ... RETURNING the way SQLite builds older
// than 3.35 do, so DrainDirtyFiles takes its fallback path.
func withoutReturning(next func(context.Context, string, func() error) error) func(context.Context, string, func() error) error {
	return func(ctx context.Context, query string, exec func() error) error {
		if strings.Contains(query, "RETURNING path") {
			return errors.New(`near "RETURNING": syntax error`)
		}
		return next(ctx, query, exec)
	}
}

func drainDirtyFiles(ctx context.Context, s *Store, repoID int64) ([]string, error) {
	return s.DrainDirtyFiles(ctx, repoID)
}

var dirtyQueueTransactions = []struct {
	name string
	run  func(context.Context, *Store, int64) ([]string, error)
	// noReturning runs the store without DELETE ... RETURNING support.
	noReturning bool
	// lastWrite is a substring of the last statement before COMMIT.
	lastWrite string
	// committed is dirty_files once the transaction took effect.
	committed []string
}{
	{"claim", func(ctx context.Context, s *Store, repoID int64) ([]string, error) {
		return s.ClaimDirtyFiles(ctx, repoID, "2026-01-01T00:00:00Z", "watch_inflight")
	}, false, "UPDATE dirty_files", []string{"a.go:watch_inflight"}},
	{"drain", drainDirtyFiles, false, "DELETE FROM dirty_files", nil},
	{"drain_without_returning", drainDirtyFiles, true, "DELETE FROM dirty_files", nil},
}

func dirtyQueueStore(t *testing.T, noReturning bool, hook func(context.Context, string, func() error) error) (*Store, int64) {
	t.Helper()
	if noReturning {
		hook = withoutReturning(hook)
	}
	s, repoID := hookedStore(t, hook)
	if err := s.QueueDirtyFile(context.Background(), repoID, "a.go", "event"); err != nil {
		t.Fatal(err)
	}
	s.db.SetMaxOpenConns(1)
	return s, repoID
}

// A cancellation that lands as COMMIT takes effect must not be reported as a
// failure: the caller would drop work the queue already gave up.
func TestDirtyQueueCommitRacingCancelReportsCommit(t *testing.T) {
	for _, tc := range dirtyQueueTransactions {
		t.Run(tc.name, func(t *testing.T) {
			bg := context.Background()
			fault := &cancelAfter{query: `COMMIT`}
			s, repoID := dirtyQueueStore(t, tc.noReturning, fault.hook)
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			fault.cancel = cancel
			fault.armed.Store(true)
			paths, err := tc.run(ctx, s, repoID)
			if got := dirtyRows(t, s, repoID); !slices.Equal(got, tc.committed) {
				t.Fatalf("dirty rows = %q, want the committed %q", got, tc.committed)
			}
			if err != nil || !slices.Equal(paths, []string{"a.go"}) {
				t.Fatalf("committed transaction reported %q, %v", paths, err)
			}
			requireFreeConnection(t, s.db)
		})
	}
}

// A BEGIN IMMEDIATE that took effect but reported a cancellation is rolled
// back before its connection returns to the pool.
func TestDirtyQueueBeginReportedFailedRollsBack(t *testing.T) {
	for _, tc := range dirtyQueueTransactions {
		t.Run(tc.name, func(t *testing.T) {
			bg := context.Background()
			fault := &cancelAfter{query: `BEGIN IMMEDIATE`}
			s, repoID := dirtyQueueStore(t, tc.noReturning, fault.hook)
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			fault.cancel = cancel
			fault.armed.Store(true)
			if _, err := tc.run(ctx, s, repoID); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			requireFreeConnection(t, s.db)
			if got := dirtyRows(t, s, repoID); !slices.Equal(got, []string{"a.go:event"}) {
				t.Fatalf("dirty rows = %q, want untouched", got)
			}
		})
	}
}

// A statement failure inside the binder's transaction, including creating its
// TEMP table, rolls it back, so the transaction's connection returns to the pool.
func TestResolveEdgeTargetsStatementFailureReleasesConnection(t *testing.T) {
	for _, stmt := range []string{
		`CREATE TEMP TABLE IF NOT EXISTS tmp_kotlin_scope_veto(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`,
		`DELETE FROM tmp_kotlin_scope_veto`,
	} {
		t.Run(strings.Fields(stmt)[0], func(t *testing.T) {
			resolveEdgeTargetsFailingAt(t, stmt)
		})
	}
}

func resolveEdgeTargetsFailingAt(t *testing.T, stmt string) {
	ctx := context.Background()
	injected := errors.New("injected statement failure")
	var armed atomic.Bool
	s, repoID := hookedStore(t, func(_ context.Context, query string, exec func() error) error {
		if query == stmt && armed.CompareAndSwap(true, false) {
			return injected
		}
		return exec()
	})
	file, err := insertTestFileLang(ctx, s, repoID, "src/a.py", "python")
	if err != nil {
		t.Fatal(err)
	}
	caller, err := insertTestSymbolLang(ctx, s, repoID, file, "caller", "pymod.caller", "python")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, file_id, line)
		VALUES(?, ?, 'helper', ?, ?, 1)`, repoID, caller, EdgeKindCalls, file)
	if err != nil {
		t.Fatal(err)
	}
	edgeID, _ := res.LastInsertId()
	targets := []edgeTarget{{edgeID: edgeID, srcLanguage: "python", srcFileID: file, dstName: "helper", edgeKind: EdgeKindCalls}}
	armed.Store(true)
	if _, err := s.resolveEdgeTargets(ctx, repoID, targets, map[int64]struct{}{}, newImportScopeCache(s, repoID)); !errors.Is(err, injected) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if armed.Load() {
		t.Fatal("fault was never injected")
	}
	s.db.SetMaxOpenConns(1)
	requireFreeConnection(t, s.db)
}

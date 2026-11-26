//go:build !sqlite_cgo

package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"modernc.org/libc"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	cancelMidRowMu  sync.Mutex
	cancelMidRow    context.CancelFunc
	cancelMidRowDB  uintptr
	cancelMidRowHit bool
)

func init() {
	// Cancels the running query's context while SQLite computes its row and
	// returns once the driver has interrupted the connection, so the query
	// hands that row to a context that has already ended.
	sqlite.MustRegisterScalarFunction("codegraph_test_cancel_mid_row", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		cancelMidRowMu.Lock()
		cancel, db := cancelMidRow, cancelMidRowDB
		cancelMidRowMu.Unlock()
		cancel()
		tls := libc.NewTLS()
		defer tls.Close()
		for deadline := time.Now().Add(5 * time.Second); sqlite3.Xsqlite3_is_interrupted(tls, db) == 0; {
			if time.Now().After(deadline) {
				return nil, errors.New("the driver never interrupted the cancelled query")
			}
			time.Sleep(time.Millisecond)
		}
		cancelMidRowMu.Lock()
		cancelMidRowHit = true
		cancelMidRowMu.Unlock()
		return int64(1), nil
	})
}

// A query cancelled after SQLite produced its first row must not leave the
// native statement behind. With one left, the pool later drops the
// connection, sqlite3_close_v2 reports success but keeps it as a zombie, and
// the process holds the database, -wal and -shm open until it exits; on
// Windows a stopped watcher's database then cannot be removed.
func TestCancelledQueryFinalizesItsStatement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	drv := s.db.Driver()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dsn, err := BuildSQLiteDSN(path, OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := drv.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	db := uintptr(reflect.ValueOf(c).Elem().FieldByName("db").Uint())
	tls := libc.NewTLS()
	defer tls.Close()
	if sqlite3.Xsqlite3_next_stmt(tls, db, 0) != 0 {
		t.Fatal("a statement is unfinalized before the query runs")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelMidRowMu.Lock()
	cancelMidRow, cancelMidRowDB, cancelMidRowHit = cancel, db, false
	cancelMidRowMu.Unlock()
	rows, qerr := c.(driver.QueryerContext).QueryContext(ctx, "SELECT codegraph_test_cancel_mid_row()", nil)
	if rows != nil {
		rows.Close()
	}
	cancelMidRowMu.Lock()
	hit := cancelMidRowHit
	cancelMidRowMu.Unlock()
	// Without the interrupt the row never reaches a cancelled context, and the
	// test would prove nothing.
	if !hit || ctx.Err() == nil {
		t.Fatalf("the function never saw the driver interrupt the query: %v", qerr)
	}
	if sqlite3.Xsqlite3_next_stmt(tls, db, 0) != 0 {
		t.Fatalf("cancelled query returned %v and left its statement unfinalized", qerr)
	}
}

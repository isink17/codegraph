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
	cancelMidRowMu sync.Mutex
	cancelMidRow   context.CancelFunc
	cancelMidRowDB uintptr
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
	if dc, ok := c.(*diagConn); ok { // diagnosis branch: trace wrapper
		c = dc.inner
	}
	db := uintptr(reflect.ValueOf(c).Elem().FieldByName("db").Uint())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelMidRowMu.Lock()
	cancelMidRow, cancelMidRowDB = cancel, db
	cancelMidRowMu.Unlock()
	rows, qerr := c.(driver.QueryerContext).QueryContext(ctx, "SELECT codegraph_test_cancel_mid_row()", nil)
	if rows != nil {
		rows.Close()
	}
	if ctx.Err() == nil {
		t.Fatal("the query did not run the cancelling function")
	}
	tls := libc.NewTLS()
	defer tls.Close()
	if sqlite3.Xsqlite3_next_stmt(tls, db, 0) != 0 {
		t.Fatalf("cancelled query returned %v and left its statement unfinalized", qerr)
	}
}

// Package diagsqlite reproduces a modernc.org/sqlite statement leak on
// context cancellation. Diagnosis branch only, not for merge.
package diagsqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"modernc.org/libc"
	_ "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func nativeDB(c driver.Conn) uintptr {
	return uintptr(reflect.ValueOf(c).Elem().FieldByName("db").Uint())
}

func outstanding(db uintptr) int {
	tls := libc.NewTLS()
	defer tls.Close()
	n := 0
	for s := sqlite3.Xsqlite3_next_stmt(tls, db, 0); s != 0; s = sqlite3.Xsqlite3_next_stmt(tls, db, s) {
		n++
	}
	return n
}

// A cancelled QueryContext must not leave a native statement behind: one
// left behind turns the later close into a zombie that keeps the database,
// -wal and -shm files open.
func TestCancelledQueryLeavesNoStatement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.sqlite")
	setup, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE t(v); INSERT INTO t VALUES (1),(2),(3)`); err != nil {
		t.Fatal(err)
	}
	drv := setup.Driver()
	defer setup.Close()

	leaks, cancelled := 0, 0
	for i := 0; i < 2000 && leaks == 0; i++ {
		c, err := drv.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		db := nativeDB(c)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(time.Duration(i%50)*time.Microsecond, cancel)
		rows, err := c.(driver.QueryerContext).QueryContext(ctx, "SELECT v FROM t", nil)
		if rows != nil {
			rows.Close()
		}
		if err != nil {
			cancelled++
		}
		n := outstanding(db)
		closeErr := c.Close()
		if n > 0 {
			leaks++
			t.Logf("iteration %d: QueryContext err=%v rows=%v, %d native statement(s) outstanding, Close err=%v", i, err, rows != nil, n, closeErr)
		}
		cancel()
	}
	t.Logf("cancelled queries: %d", cancelled)
	if leaks > 0 {
		t.Errorf("%d cancelled QueryContext call(s) left a native statement unfinalized", leaks)
	}
}

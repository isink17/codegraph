//go:build !sqlite_cgo

package store

// Diagnosis branch only, not for merge: a pass-through wrapper around the
// modernc driver that gives every native connection an id and records, at
// Close, how many native statements were still unfinalized. sqlite3_close_v2
// returns OK for such a connection but keeps its files open (a zombie).

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

const diagDriverName = "sqlite-diag"

type diagRecord struct {
	ID          int64
	DSN         string
	OpenStack   string
	Closed      bool
	CloseErr    error
	Outstanding int
	OutSQL      []string
	CloseStack  string
	CancelLeaks []string
}

var (
	diagMu      sync.Mutex
	diagRecords []*diagRecord
	diagNextID  atomic.Int64
)

func init() {
	db, err := sql.Open("sqlite", "")
	if err != nil {
		panic(err)
	}
	sql.Register(diagDriverName, &diagDriver{inner: db.Driver()})
}

func diagStack(skip int) string {
	pcs := make([]uintptr, 40)
	n := runtime.Callers(skip, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	for {
		f, more := frames.Next()
		if strings.Contains(f.Function, "isink17/codegraph/") {
			fmt.Fprintf(&b, " <- %s:%d", f.Function[strings.LastIndex(f.Function, "/")+1:], f.Line)
		}
		if !more {
			break
		}
	}
	return b.String()
}

func diagNative(c driver.Conn) uintptr {
	return uintptr(reflect.ValueOf(c).Elem().FieldByName("db").Uint())
}

// diagStatements lists the SQL of every unfinalized statement on db that
// belongs to the caller, not to a virtual table (FTS5 shadow-table statements,
// which sqlite3_close finalizes before its busy check).
func diagStatements(db uintptr) []string {
	if db == 0 {
		return nil
	}
	tls := libc.NewTLS()
	defer tls.Close()
	var out []string
	for s := sqlite3.Xsqlite3_next_stmt(tls, db, 0); s != 0; s = sqlite3.Xsqlite3_next_stmt(tls, db, s) {
		q := libc.GoString(sqlite3.Xsqlite3_sql(tls, s))
		if strings.Contains(q, "'main'.'") || strings.HasPrefix(q, "PRAGMA 'main'.") {
			continue
		}
		if len(q) > 120 {
			q = q[:120]
		}
		out = append(out, fmt.Sprintf("busy=%d %q", sqlite3.Xsqlite3_stmt_busy(tls, s), q))
	}
	return out
}

type diagDriver struct{ inner driver.Driver }

func (d *diagDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	rec := &diagRecord{ID: diagNextID.Add(1), DSN: name, OpenStack: diagStack(3)}
	diagMu.Lock()
	diagRecords = append(diagRecords, rec)
	diagMu.Unlock()
	return &diagConn{inner: c, rec: rec}, nil
}

type diagConn struct {
	inner driver.Conn
	rec   *diagRecord
}

func (c *diagConn) Prepare(q string) (driver.Stmt, error) { return c.inner.Prepare(q) }
func (c *diagConn) Begin() (driver.Tx, error) {
	return c.inner.Begin() //nolint:staticcheck // pass-through
}

func (c *diagConn) Close() error {
	stmts := diagStatements(diagNative(c.inner))
	err := c.inner.Close()
	diagMu.Lock()
	c.rec.Closed, c.rec.CloseErr, c.rec.Outstanding, c.rec.OutSQL = true, err, len(stmts), stmts
	if len(stmts) > 0 {
		c.rec.CloseStack = diagStack(3)
	}
	diagMu.Unlock()
	return err
}

func (c *diagConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.inner.(driver.ConnBeginTx).BeginTx(ctx, o)
}

func (c *diagConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return c.inner.(driver.ConnPrepareContext).PrepareContext(ctx, q)
}

func (c *diagConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.inner.(driver.ExecerContext).ExecContext(ctx, q, a)
}

func (c *diagConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	r, err := c.inner.(driver.QueryerContext).QueryContext(ctx, q, a)
	if err != nil && ctx.Err() != nil {
		if stmts := diagStatements(diagNative(c.inner)); len(stmts) > 0 {
			diagMu.Lock()
			c.rec.CancelLeaks = append(c.rec.CancelLeaks, fmt.Sprintf("err=%v outstanding=%v at%s", err, stmts, diagStack(3)))
			diagMu.Unlock()
		}
	}
	return r, err
}

func (c *diagConn) Ping(ctx context.Context) error { return c.inner.(driver.Pinger).Ping(ctx) }
func (c *diagConn) ResetSession(ctx context.Context) error {
	return c.inner.(driver.SessionResetter).ResetSession(ctx)
}
func (c *diagConn) IsValid() bool { return c.inner.(driver.Validator).IsValid() }

// DiagSQLiteReport summarizes every native connection whose DSN contains
// key: how many were opened, how many are still open, and every zombie
// (closed with unfinalized statements) with its statements and stacks.
func DiagSQLiteReport(key string) (opened, open, zombies int, details []string) {
	diagMu.Lock()
	defer diagMu.Unlock()
	for _, r := range diagRecords {
		if !strings.Contains(r.DSN, key) {
			continue
		}
		opened++
		if !r.Closed {
			open++
			details = append(details, fmt.Sprintf("conn %d NEVER CLOSED opened%s", r.ID, r.OpenStack))
		}
		if r.Closed && r.Outstanding > 0 {
			zombies++
			details = append(details, fmt.Sprintf("conn %d ZOMBIE close_err=%v outstanding=%v opened%s closed%s", r.ID, r.CloseErr, r.OutSQL, r.OpenStack, r.CloseStack))
		}
		for _, l := range r.CancelLeaks {
			details = append(details, fmt.Sprintf("conn %d CANCEL-LEAK %s", r.ID, l))
		}
	}
	return opened, open, zombies, details
}

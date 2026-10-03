package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A WAL read transaction that tries to write after another connection
// committed fails with the extended code SQLITE_BUSY_SNAPSHOT. Both drivers
// must classify every extended BUSY/LOCKED variant as busy, not only the
// primary codes.
func TestIsSQLiteBusyClassifiesExtendedBusySnapshot(t *testing.T) {
	ctx := context.Background()
	dsn, err := BuildSQLiteDSN(filepath.Join(t.TempDir(), "busy.sqlite"), OpenOptions{}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE t(v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	reader, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := reader.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = reader.ExecContext(ctx, `ROLLBACK`) }()
	var n int
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, `INSERT INTO t VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	_, err = reader.ExecContext(ctx, `INSERT INTO t VALUES(2)`)
	if err == nil {
		t.Fatal("stale-snapshot write succeeded; want SQLITE_BUSY_SNAPSHOT")
	}
	if !isSQLiteBusy(err) {
		t.Fatalf("isSQLiteBusy(%v) = false, want true", err)
	}
}

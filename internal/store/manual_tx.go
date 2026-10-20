package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
)

// rollbackManualTx ends a transaction begun with an explicit BEGIN on conn.
// It is detached from ctx: a cancelled ctx would skip the ROLLBACK and hand the
// pooled connection back still inside the transaction. It runs even when BEGIN
// reported an error, since a cancel can race a BEGIN that took effect. Any
// failure other than "no transaction is active" leaves the transaction state
// unknown, so the connection is discarded instead of returned to the pool.
func rollbackManualTx(ctx context.Context, conn *sql.Conn) {
	_, err := conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
	if err == nil || strings.Contains(err.Error(), "no transaction is active") {
		return
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

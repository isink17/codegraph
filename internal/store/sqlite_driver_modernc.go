//go:build !sqlite_cgo

package store

import (
	_ "modernc.org/sqlite"
)

const sqliteDriverName = "sqlite"

// A deferred transaction that reads before it writes cannot wait for another
// connection's write lock: SQLite fails the upgrade with SQLITE_BUSY without
// calling the busy handler. Starting write transactions IMMEDIATE takes the
// lock up front, under busy_timeout. modernc keeps ReadOnly transactions
// deferred.
const sqliteWriteTxLock = "immediate"

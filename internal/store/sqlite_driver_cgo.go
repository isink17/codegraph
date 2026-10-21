//go:build sqlite_cgo && cgo

package store

import (
	_ "github.com/mattn/go-sqlite3"
)

const sqliteDriverName = "sqlite3"

// mattn applies _txlock to ReadOnly transactions as well, which would make
// every read transaction take the write lock, so keep its default.
const sqliteWriteTxLock = "deferred"

//go:build !sqlite_cgo

package store

import (
	_ "modernc.org/sqlite"
)

const sqliteDriverName = "sqlite"

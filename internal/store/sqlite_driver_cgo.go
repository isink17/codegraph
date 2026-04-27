//go:build sqlite_cgo && cgo

package store

import (
	_ "github.com/mattn/go-sqlite3"
)

const sqliteDriverName = "sqlite3"

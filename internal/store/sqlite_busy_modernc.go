//go:build !sqlite_cgo

package store

import (
	"errors"
	"strings"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		code := se.Code()
		const sqliteLockedSharedcache = sqlite3.SQLITE_LOCKED | (1 << 8)
		return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED || code == sqliteLockedSharedcache
	}
	s := err.Error()
	return strings.Contains(s, "database is locked") || strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "SQLITE_LOCKED")
}

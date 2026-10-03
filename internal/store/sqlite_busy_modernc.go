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
		// The driver enables extended result codes, so a busy error may be
		// SQLITE_BUSY_RECOVERY/_SNAPSHOT/_TIMEOUT or SQLITE_LOCKED_*: the
		// primary code is the low byte.
		code := se.Code() & 0xff
		return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
	}
	s := err.Error()
	return strings.Contains(s, "database is locked") || strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "SQLITE_LOCKED")
}

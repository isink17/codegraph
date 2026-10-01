//go:build windows

package store

import (
	"errors"

	"golang.org/x/sys/windows"
)

func isTransientSQLiteInspectionLock(err error) bool {
	if isSQLiteBusy(err) {
		return true
	}
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

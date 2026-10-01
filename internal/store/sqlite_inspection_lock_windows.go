//go:build windows

package store

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func isTransientSQLiteInspectionLock(err error) bool {
	if isSQLiteBusy(err) {
		return true
	}
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true
	}
	var pathErr *os.PathError
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) &&
		errors.As(err, &pathErr) && pathErr.Op == "open" && strings.HasSuffix(pathErr.Path, "-journal")
}

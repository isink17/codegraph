//go:build !windows

package store

import (
	"errors"
	"syscall"
	"testing"
)

func TestSQLiteInspectionLockClassifierLeavesUnixErrorsAlone(t *testing.T) {
	if isTransientSQLiteInspectionLock(syscall.Errno(32)) {
		t.Fatal("Unix errno 32 must not be classified as a Windows sharing violation")
	}
	if isTransientSQLiteInspectionLock(errors.New("database is locked")) {
		t.Fatal("SQLite lock retry must not change non-Windows inspection behavior")
	}
}

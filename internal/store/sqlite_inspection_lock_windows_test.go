//go:build windows

package store

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSQLiteInspectionLockClassifierRecognizesWindowsSharingErrors(t *testing.T) {
	if !isTransientSQLiteInspectionLock(errors.New("database is locked")) {
		t.Fatal("SQLite busy error was not classified as transient")
	}
	for _, errno := range []windows.Errno{windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION} {
		err := &os.PathError{Op: "open", Path: "db-journal", Err: errno}
		if !isTransientSQLiteInspectionLock(err) {
			t.Errorf("%v was not classified as transient", errno)
		}
	}
}

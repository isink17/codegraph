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

func TestSQLiteInspectionLockClassifierScopesAccessDeniedToJournalOpen(t *testing.T) {
	journal := &os.PathError{Op: "open", Path: "db.sqlite-journal", Err: windows.ERROR_ACCESS_DENIED}
	if !isTransientSQLiteInspectionLock(journal) {
		t.Fatal("journal open access denied was not classified as transient")
	}

	main := &os.PathError{Op: "open", Path: "db.sqlite", Err: windows.ERROR_ACCESS_DENIED}
	if isTransientSQLiteInspectionLock(main) {
		t.Fatal("main database access denied was classified as transient")
	}

	stat := &os.PathError{Op: "stat", Path: "db.sqlite-journal", Err: windows.ERROR_ACCESS_DENIED}
	if isTransientSQLiteInspectionLock(stat) {
		t.Fatal("journal stat access denied was classified as transient")
	}
}

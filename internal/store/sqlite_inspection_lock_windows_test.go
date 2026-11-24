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

func TestSQLiteInspectionLockClassifierScopesAccessDeniedToJournalAndWALOpen(t *testing.T) {
	for _, path := range []string{"db.sqlite-journal", "db.sqlite-wal"} {
		err := &os.PathError{Op: "open", Path: path, Err: windows.ERROR_ACCESS_DENIED}
		if !isTransientSQLiteInspectionLock(err) {
			t.Fatalf("%s open access denied was not classified as transient", path)
		}
	}

	main := &os.PathError{Op: "open", Path: "db.sqlite", Err: windows.ERROR_ACCESS_DENIED}
	if isTransientSQLiteInspectionLock(main) {
		t.Fatal("main database access denied was classified as transient")
	}

	for _, path := range []string{"db.sqlite-journal", "db.sqlite-wal"} {
		stat := &os.PathError{Op: "stat", Path: path, Err: windows.ERROR_ACCESS_DENIED}
		if isTransientSQLiteInspectionLock(stat) {
			t.Fatalf("%s stat access denied was classified as transient", path)
		}
	}

	shm := &os.PathError{Op: "open", Path: "db.sqlite-shm", Err: windows.ERROR_ACCESS_DENIED}
	if isTransientSQLiteInspectionLock(shm) {
		t.Fatal("WAL-index open access denied was classified as transient")
	}
}

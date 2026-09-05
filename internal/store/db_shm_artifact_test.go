package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestSQLiteInspectionIgnoresWALIndex pins the classification: the WAL-index is
// never part of the fingerprinted state, so its presence, absence or content
// cannot make a logically unchanged database look changed.
func TestSQLiteInspectionIgnoresWALIndex(t *testing.T) {
	path, reader, writer, tx := v2PinnedWALFixture(t)
	defer tx.Rollback()
	defer reader.Close()
	defer writer.Close()
	if _, err := writer.Exec(`UPDATE schema_migrations SET applied_at = 'wal' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + sqliteTransientArtifactSuffix); err != nil {
		t.Fatalf("fixture has no WAL-index: %v", err)
	}
	artifacts, err := readSQLiteInspectionArtifacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := artifacts[path+sqliteTransientArtifactSuffix]; ok {
		t.Fatal("WAL-index was fingerprinted as persistent state")
	}
	if _, ok := artifacts[path+"-wal"]; !ok {
		t.Fatal("WAL was not fingerprinted")
	}
}

// TestValidationNeverOpensWALIndex proves the source WAL-index is not consulted,
// without depending on a platform-specific lock. Replacing it with a directory
// makes any attempt to open and stream it fail on every OS -- non-regular files
// are rejected by streamSQLiteInspectionArtifact -- while the database and WAL
// stay perfectly readable. This is the portable stand-in for the Windows case,
// where the live WAL-index is memory-mapped and byte-range locked and reading it
// raises a sharing violation.
func TestValidationNeverOpensWALIndex(t *testing.T) {
	path := copyWALFixtureWithoutConnections(t)
	// Nothing holds the database open, so the only WAL-index that could be read
	// is this unreadable stand-in.
	if err := os.Mkdir(path+sqliteTransientArtifactSuffix, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateExistingDatabase(path); err != nil {
		t.Fatalf("validation consulted the WAL-index: %v", err)
	}
	snapshotPath, cleanup, err := createSQLiteValidationSnapshot(path)
	if err != nil {
		t.Fatalf("snapshot consulted the WAL-index: %v", err)
	}
	defer cleanup()
	if info, err := os.Stat(path + sqliteTransientArtifactSuffix); err != nil || !info.IsDir() {
		t.Fatalf("source WAL-index replaced: info=%v err=%v", info, err)
	}
	assertSnapshotSeesWALCommit(t, snapshotPath, "only-in-wal")
}

// TestValidationSnapshotRebuildsWALIndexPrivately proves the copied database and
// WAL alone reproduce the committed state: no WAL-index is copied, SQLite
// rebuilds its own inside the temporary directory, and a value that exists only
// in the source WAL is visible in the snapshot. The source WAL-index is left
// exactly as it was.
func TestValidationSnapshotRebuildsWALIndexPrivately(t *testing.T) {
	path, reader, writer, tx := v2PinnedWALFixture(t)
	defer tx.Rollback()
	defer reader.Close()
	defer writer.Close()
	if _, err := writer.Exec(`UPDATE schema_migrations SET applied_at = 'only-in-wal' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	// The pinned read transaction blocks checkpointing, so the committed value
	// lives in the WAL and not in the main database file.
	assertMainDatabaseLacksValue(t, path, "only-in-wal")

	sourceIndex := statOrNil(t, path+sqliteTransientArtifactSuffix)
	if sourceIndex == nil {
		t.Fatal("fixture has no WAL-index")
	}
	snapshotPath, cleanup, err := createSQLiteValidationSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	assertSnapshotSeesWALCommit(t, snapshotPath, "only-in-wal")

	afterIndex := statOrNil(t, path+sqliteTransientArtifactSuffix)
	if afterIndex == nil || !os.SameFile(sourceIndex, afterIndex) || sourceIndex.Size() != afterIndex.Size() {
		t.Fatal("source WAL-index was replaced or resized by snapshotting")
	}
}

// copyWALFixtureWithoutConnections yields a database whose committed state lives
// in an uncheckpointed WAL, with no connection open against it. Copying is the
// only way to keep the WAL: the last connection to close checkpoints it away.
func copyWALFixtureWithoutConnections(t *testing.T) string {
	t.Helper()
	source, reader, writer, tx := v2PinnedWALFixture(t)
	if _, err := writer.Exec(`UPDATE schema_migrations SET applied_at = 'only-in-wal' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	assertMainDatabaseLacksValue(t, source, "only-in-wal")
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(source + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertSnapshotSeesWALCommit(t *testing.T, snapshotPath, want string) {
	t.Helper()
	dsn, err := buildSQLiteValidationDSN(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("snapshot applied_at = %q, want %q", got, want)
	}
}

func assertMainDatabaseLacksValue(t *testing.T, path, value string) {
	t.Helper()
	dsn, err := buildSQLiteImmutableDSN(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == value {
		t.Fatalf("value %q reached the main database file; it must exist only in the WAL", value)
	}
}

func statOrNil(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return nil
	}
	return info
}

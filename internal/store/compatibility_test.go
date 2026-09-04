package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpen_DatabaseCompatibilityMatrix(t *testing.T) {
	t.Run("new database", func(t *testing.T) {
		s, err := Open(filepath.Join(t.TempDir(), "new.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	})

	t.Run("migration 018", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v1.sqlite")
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		var max int
		if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&max); err != nil {
			t.Fatal(err)
		}
		if max != maxSupportedMigration {
			t.Fatalf("max migration = %d, want %d", max, maxSupportedMigration)
		}
	})

	for _, version := range []int{19, 32} {
		t.Run(fmt.Sprintf("migration %d", version), func(t *testing.T) {
			path := makeV1Database(t)
			addMigrationRow(t, path, version)
			assertNewerDatabase(t, path)
		})
	}

	t.Run("future marker", func(t *testing.T) {
		path := makeV1Database(t)
		setUserVersion(t, path, 1)
		assertNewerDatabase(t, path)
	})

	t.Run("future marker with unfamiliar schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "future.sqlite")
		db := openRawDatabase(t, path)
		if _, err := db.Exec(`PRAGMA user_version = 7; CREATE TABLE future_only (value TEXT);`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		assertNewerDatabase(t, path)
	})

	t.Run("corrupt compatibility metadata", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "corrupt.sqlite")
		db := openRawDatabase(t, path)
		if _, err := db.Exec(`CREATE TABLE not_schema_migrations (version INTEGER);`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		var err error
		_, err = Open(path)
		if !errors.Is(err, ErrDatabaseUnsupported) {
			t.Fatalf("Open() error = %v, want ErrDatabaseUnsupported", err)
		}
	})
}

func TestOpen_NewerDatabaseIsByteForByteUntouched(t *testing.T) {
	path := makeV1Database(t)
	setUserVersion(t, path, 1)
	dir := filepath.Dir(path)
	before := snapshotDirectory(t, dir)

	assertNewerDatabase(t, path)
	after := snapshotDirectory(t, dir)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("directory changed:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestCheckDatabaseCompatibility_OriginalHasNoSideEffects(t *testing.T) {
	path := makeV1Database(t)
	before := snapshotDirectory(t, filepath.Dir(path))
	if err := CheckDatabaseCompatibility(path, OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if after := snapshotDirectory(t, filepath.Dir(path)); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("source directory changed:\nbefore=%v\nafter=%v", before, after)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("sidecar %s exists after compatibility inspection: %v", suffix, err)
		}
	}
}

func TestBuildSQLiteDSN_ReadOnlyDoesNotRequestJournalMode(t *testing.T) {
	dsn, err := BuildSQLiteDSN(filepath.Join(t.TempDir(), "graph.sqlite"), OpenOptions{}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, pragma := range u.Query()["_pragma"] {
		if strings.HasPrefix(pragma, "journal_mode(") {
			t.Fatalf("read-only DSN requests journal mode: %q", pragma)
		}
	}
}

func TestCheckDatabaseCompatibility_WALCopyReadsSidecarState(t *testing.T) {
	path := makeV1Database(t)
	db := openRawDatabase(t, path)
	defer db.Close()
	if _, err := db.Exec(`PRAGMA wal_autocheckpoint = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(19, '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("WAL sidecar missing: %v", err)
	}
	before := snapshotDirectory(t, filepath.Dir(path))
	if err := CheckDatabaseCompatibility(path, OpenOptions{}); !errors.Is(err, ErrDatabaseNewer) {
		t.Fatalf("CheckDatabaseCompatibility() error = %v, want ErrDatabaseNewer", err)
	}
	if after := snapshotDirectory(t, filepath.Dir(path)); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("source directory changed:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestCheckDatabaseCompatibility_RollbackJournalCopyRecovers(t *testing.T) {
	path := makeV1Database(t)
	db := openRawDatabase(t, path)
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode = DELETE`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(19, '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-journal"); err != nil {
		t.Fatalf("rollback journal missing: %v", err)
	}
	before := snapshotDirectory(t, filepath.Dir(path))
	if err := CheckDatabaseCompatibility(path, OpenOptions{}); err != nil {
		t.Fatalf("CheckDatabaseCompatibility() error = %v", err)
	}
	if after := snapshotDirectory(t, filepath.Dir(path)); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("source directory changed:\nbefore=%v\nafter=%v", before, after)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

type fileSnapshot struct {
	name  string
	size  int64
	hash  [sha256.Size]byte
	mode  os.FileMode
	mtime time.Time
}

func snapshotDirectory(t *testing.T, dir string) []fileSnapshot {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]fileSnapshot, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fileSnapshot{name: entry.Name(), size: info.Size(), hash: sha256.Sum256(data), mode: info.Mode(), mtime: info.ModTime()})
	}
	return out
}

func makeV1Database(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func openRawDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn, err := BuildSQLiteDSN(path, OpenOptions{}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func addMigrationRow(t *testing.T, path string, version int) {
	t.Helper()
	db := openRawDatabase(t, path)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, '')`, version); err != nil {
		t.Fatal(err)
	}
}

func setUserVersion(t *testing.T, path string, version int) {
	t.Helper()
	db := openRawDatabase(t, path)
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		t.Fatal(err)
	}
}

func assertNewerDatabase(t *testing.T, path string) {
	t.Helper()
	_, err := OpenWithOptions(path, OpenOptions{PerformanceProfile: "fast"})
	if !errors.Is(err, ErrDatabaseNewer) {
		t.Fatalf("OpenWithOptions() error = %v, want %q", err, ErrDatabaseNewer)
	}
	if err.Error() != ErrDatabaseNewer.Error() {
		t.Fatalf("error = %q, want exact %q", err, ErrDatabaseNewer)
	}
}

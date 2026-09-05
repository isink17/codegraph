package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSQLiteArtifactMemoryBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.sqlite")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(16 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	first, err := readSQLiteInspectionArtifacts(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := readSQLiteInspectionArtifacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if !sqliteInspectionArtifactsEqual(first, second) {
		t.Fatal("unchanged artifacts differ")
	}
	snapshot, cleanup, err := vacuumSQLiteSnapshot(path, first)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	copyState, err := streamSQLiteInspectionArtifact(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if copyState.digest != first[path].digest || copyState.info.Size() != first[path].info.Size() {
		t.Fatal("streamed snapshot differs from source")
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Fatalf("artifact fingerprints and copy allocated %d bytes for a 16 MiB file; limit 2 MiB", allocated)
	}
}

func TestSQLiteArtifactFingerprintDetectsChanges(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-journal"} {
		t.Run("artifact="+suffix, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.sqlite")
			for _, name := range []string{path, path + suffix} {
				if err := os.WriteFile(name, []byte("before"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Coarse/restored timestamps cannot hide a same-size content change.
			stamp := time.Unix(1700000000, 0)
			if err := os.Chtimes(path+suffix, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			before, err := readSQLiteInspectionArtifacts(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+suffix, []byte("after!"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path+suffix, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			after, err := readSQLiteInspectionArtifacts(path)
			if err != nil {
				t.Fatal(err)
			}
			if !sqliteInspectionFileInfoEqual(before[path+suffix].info, after[path+suffix].info) {
				t.Fatal("fixture must preserve size, identity and timestamp to isolate digest comparison")
			}
			if sqliteInspectionArtifactsEqual(before, after) {
				t.Fatal("same-size rewrite with restored timestamp was missed")
			}
			if err := os.Rename(path+suffix, path+suffix+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+suffix, []byte("after!"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path+suffix, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			replaced, err := readSQLiteInspectionArtifacts(path)
			if err != nil {
				t.Fatal(err)
			}
			if sqliteInspectionArtifactsEqual(after, replaced) {
				t.Fatal("same-content file replacement was missed")
			}
			if err := os.Remove(path + suffix); err != nil {
				t.Fatal(err)
			}
			missing, err := readSQLiteInspectionArtifacts(path)
			if suffix == "" {
				if err == nil {
					t.Fatal("missing main database accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if sqliteInspectionArtifactsEqual(after, missing) {
					t.Fatal("disappearing sidecar was missed")
				}
				if sqliteInspectionArtifactsEqual(missing, after) {
					t.Fatal("appearing sidecar was missed")
				}
			}
		})
	}
}

type sqliteArtifactWriterFunc func([]byte) (int, error)

func (f sqliteArtifactWriterFunc) Write(p []byte) (int, error) { return f(p) }

func TestSQLiteArtifactStreamRejectsChangesAndIOFailure(t *testing.T) {
	for _, change := range []string{"shrink", "grow", "replace", "remove", "write-error", "short-write"} {
		t.Run(change, func(t *testing.T) {
			if runtime.GOOS == "windows" && (change == "replace" || change == "remove") {
				t.Skip("Windows denies deletion/rename while os.Open holds a read handle; replacement between reads is tested separately")
			}
			path := filepath.Join(t.TempDir(), "source.sqlite")
			data := make([]byte, 64<<10)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected write failure")
			changed := false
			_, err := streamSQLiteInspectionArtifact(path, sqliteArtifactWriterFunc(func(p []byte) (int, error) {
				if changed {
					return len(p), nil
				}
				changed = true
				switch change {
				case "shrink":
					if err := os.Truncate(path, 1); err != nil {
						t.Fatal(err)
					}
				case "grow":
					if err := os.Truncate(path, int64(len(data)+1)); err != nil {
						t.Fatal(err)
					}
				case "replace":
					if err := os.Rename(path, path+".old"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, data, 0o600); err != nil {
						t.Fatal(err)
					}
				case "remove":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "write-error":
					return 0, injected
				case "short-write":
					return 0, nil
				}
				return len(p), nil
			}))
			if err == nil {
				t.Fatal("unstable or incomplete stream accepted")
			}
			if change == "write-error" && !errors.Is(err, injected) {
				t.Fatalf("lost write error: %v", err)
			}
			if change == "short-write" && !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("lost short write: %v", err)
			}
		})
	}
}

func TestSQLiteSnapshotCopyFailureCleansTempDirectory(t *testing.T) {
	privateTemp := t.TempDir()
	for _, variable := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(variable, privateTemp)
	}
	for _, failure := range []string{"changed-content", "missing-sidecar", "invalid-database"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.sqlite")
			for _, name := range []string{path, path + "-wal"} {
				if err := os.WriteFile(name, []byte("before"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			artifacts, err := readSQLiteInspectionArtifacts(path)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "changed-content":
				if err := os.WriteFile(path+"-wal", []byte("after!"), 0o600); err != nil {
					t.Fatal(err)
				}
				stamp := artifacts[path+"-wal"].info.ModTime()
				if err := os.Chtimes(path+"-wal", stamp, stamp); err != nil {
					t.Fatal(err)
				}
			case "missing-sidecar":
				if err := os.Remove(path + "-wal"); err != nil {
					t.Fatal(err)
				}
			}
			before := validationTempDirCount(t)
			_, cleanup, err := vacuumSQLiteSnapshot(path, artifacts)
			if err == nil {
				cleanup()
				t.Fatal("invalid snapshot accepted")
			}
			if failure == "changed-content" && !strings.Contains(err.Error(), "changed while copying") {
				t.Fatalf("changed copied bytes must be rejected before SQLite recovery: %v", err)
			}
			if got := validationTempDirCount(t); got != before {
				t.Fatalf("failed copy/recovery leaked temp directory: before=%d after=%d", before, got)
			}
		})
	}
}

func TestSQLiteSnapshotRecoversHotJournalWithoutSourceWrites(t *testing.T) {
	path := createReadOnlyProbeDatabase(t)
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE; PRAGMA cache_size=1;
		CREATE TABLE journal_probe(value INTEGER, padding BLOB);
		WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<64)
		INSERT INTO journal_probe SELECT 1, zeroblob(4096) FROM n`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// More dirty pages than the cache forces a spill with a recoverable journal.
	if _, err := tx.Exec(`UPDATE journal_probe SET value=2`); err != nil {
		t.Fatal(err)
	}
	before := snapshotSQLiteArtifacts(t, path)
	reader, err := OpenReadOnly(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var value int
	if err := reader.db.QueryRow(`SELECT max(value) FROM journal_probe`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 1 {
		t.Fatalf("snapshot value=%d, want committed value=1", value)
	}
	assertSQLiteArtifactsEqual(t, before, snapshotSQLiteArtifacts(t, path))
}

func TestV2DatabaseIdentityAndCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pragmas, err := s.DBPragmas(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if pragmas.UserVersion != DatabaseFormatUserVersion {
		t.Fatalf("user_version = %d, want %d", pragmas.UserVersion, DatabaseFormatUserVersion)
	}
	ceiling, err := MigrationCeiling()
	if err != nil {
		t.Fatal(err)
	}
	// The v2 generation boundary exists so migration 033 and up can be v2-only.
	// user_version stays 2 while the ceiling moves.
	if ceiling != 34 {
		t.Fatalf("migration ceiling = %d, want 34", ceiling)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reopened.Close()
}

func TestV2ZeroUserVersionRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	setUserVersion(t, path, 0)
	s, err = Open(path)
	if err != nil {
		t.Fatalf("compatible user_version=0 database rejected: %v", err)
	}
	defer s.Close()
	pragmas, err := s.DBPragmas(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if pragmas.UserVersion != DatabaseFormatUserVersion {
		t.Fatalf("recovered user_version = %d", pragmas.UserVersion)
	}
}

func TestV2EmptyBootstrapShellAllowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	createSQLiteShell(t, path, "")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("empty bootstrap shell rejected: %v", err)
	}
	s.Close()
}

func TestV2MissingMigrationMetadataWithUserTableRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	createSQLiteShell(t, path, "CREATE TABLE foreign_table (id INTEGER)")
	if _, err := Open(path); err == nil {
		t.Fatal("database with user table and no migration metadata opened")
	}
}

func TestV2LockedFormatValidationRejectsFutureMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	setUserVersion(t, path, DatabaseFormatUserVersion+1)
	dsn, err := BuildSQLiteDSN(path, OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if err := validateDatabaseFormat(context.Background(), conn, path); err == nil {
		t.Fatal("locked validation accepted future marker")
	}
	if _, err := conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
}

func TestV2FutureUserVersionDoesNotMutate(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	setUserVersion(t, path, DatabaseFormatUserVersion+1)
	want := snapshotSQLiteArtifacts(t, path)
	if _, err := Open(path); err == nil {
		t.Fatal("future database opened")
	}
	got := snapshotSQLiteArtifacts(t, path)
	assertSQLiteArtifactsEqual(t, want, got)
}

func TestV2FutureMigrationDoesNotOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	dsn, err := BuildSQLiteDSN(path, OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(999, '')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	want := snapshotSQLiteArtifacts(t, path)
	if _, err := Open(path); err == nil {
		t.Fatal("future migration database opened")
	}
	assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))
}

func TestV2ValidationWALVisibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	dsn, err := BuildSQLiteDSN(path, OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.Exec(`PRAGMA wal_autocheckpoint = 0`); err != nil {
		t.Fatal(err)
	}
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var initial int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&initial); err != nil {
		t.Fatal(err)
	}
	if initial != DatabaseFormatUserVersion {
		t.Fatalf("initial user_version = %d, want %d", initial, DatabaseFormatUserVersion)
	}

	writer, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`PRAGMA wal_autocheckpoint = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(999, '')`); err != nil {
		t.Fatal(err)
	}

	observe := func(name, validationDSN string) [2]int {
		t.Helper()
		db, err := sql.Open(sqliteDriverName, validationDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var userVersion, futureMigrations int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
			t.Fatalf("%s user_version: %v", name, err)
		}
		if err := db.QueryRow(`SELECT COUNT(1) FROM schema_migrations WHERE version = 999`).Scan(&futureMigrations); err != nil {
			t.Fatalf("%s migration: %v", name, err)
		}
		return [2]int{userVersion, futureMigrations}
	}

	immutableDSN, err := BuildSQLiteDSN(path, OpenOptions{}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	immutableURL, err := url.Parse(immutableDSN)
	if err != nil {
		t.Fatal(err)
	}
	query := immutableURL.Query()
	query.Set("immutable", "1")
	immutableURL.RawQuery = query.Encode()
	immutableDSN = immutableURL.String()
	if got := observe("immutable", immutableDSN); got != [2]int{2, 0} {
		t.Fatalf("immutable observes user_version=%d future_migrations=%d, want stale 2/0", got[0], got[1])
	}
	readOnlyDSN, err := BuildSQLiteDSN(path, OpenOptions{}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := observe("read-only", readOnlyDSN); got != [2]int{3, 1} {
		t.Fatalf("read-only observes user_version=%d future_migrations=%d, want WAL 3/1", got[0], got[1])
	}
	if got := observe("writable", dsn); got != [2]int{3, 1} {
		t.Fatalf("writable observes user_version=%d future_migrations=%d, want WAL 3/1", got[0], got[1])
	}
}

func TestV2WALFutureMetadataRejectedWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write string
	}{
		{name: "user_version", write: `PRAGMA user_version = 3`},
		{name: "migration", write: `INSERT INTO schema_migrations(version, applied_at) VALUES(999, '')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, reader, writer, tx := v2PinnedWALFixture(t)
			defer tx.Rollback()
			defer reader.Close()
			defer writer.Close()
			if _, err := writer.Exec(tc.write); err != nil {
				t.Fatal(err)
			}
			want := snapshotSQLiteArtifacts(t, path)
			if _, err := Open(path); err == nil {
				t.Fatal("future WAL metadata accepted")
			}
			assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))
		})
	}
}

func TestV2WALCurrentMetadataAccepted(t *testing.T) {
	path, reader, writer, tx := v2PinnedWALFixture(t)
	defer tx.Rollback()
	defer reader.Close()
	defer writer.Close()
	if _, err := writer.Exec(`UPDATE schema_migrations SET applied_at = 'wal' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	want := snapshotSQLiteArtifacts(t, path)
	beforeSnapshots := validationSnapshotCalls.Load()
	if err := validateExistingDatabase(path); err != nil {
		t.Fatalf("current WAL metadata rejected: %v", err)
	}
	if got := validationSnapshotCalls.Load() - beforeSnapshots; got != 1 {
		t.Fatalf("snapshot calls = %d, want 1", got)
	}
	assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))
}

func TestOpenReadOnlyUsesAcceptedSnapshot(t *testing.T) {
	path, reader, writer, tx := v2PinnedWALFixture(t)
	defer tx.Rollback()
	defer reader.Close()
	defer writer.Close()
	beforeSnapshots := validationSnapshotCalls.Load()
	beforeTemp := validationTempDirCount(t)
	want := snapshotSQLiteArtifacts(t, path)
	ro, err := OpenReadOnly(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))
	if _, err := writer.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	var userVersion int
	if err := ro.db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if userVersion != DatabaseFormatUserVersion {
		t.Fatalf("read-only snapshot user_version = %d, want %d", userVersion, DatabaseFormatUserVersion)
	}
	if got := validationSnapshotCalls.Load() - beforeSnapshots; got != 1 {
		t.Fatalf("snapshot calls = %d, want 1", got)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if got := validationTempDirCount(t); got != beforeTemp {
		t.Fatalf("validation temp directories before=%d after=%d", beforeTemp, got)
	}
}

func TestOpenReadOnlyFailedSnapshotCleansTempDirectory(t *testing.T) {
	path, reader, writer, tx := v2PinnedWALFixture(t)
	defer tx.Rollback()
	defer reader.Close()
	defer writer.Close()
	if _, err := writer.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	before := validationTempDirCount(t)
	if _, err := OpenReadOnly(path, OpenOptions{}); err == nil {
		t.Fatal("future WAL database opened read-only")
	}
	if got := validationTempDirCount(t); got != before {
		t.Fatalf("validation temp directories before=%d after=%d", before, got)
	}
}

func TestOpenReadOnlySnapshotStableAcrossSourceWALChanges(t *testing.T) {
	path := createReadOnlyProbeDatabase(t)
	ro, err := OpenReadOnly(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	dsn, err := BuildSQLiteDSN(path, OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`UPDATE readonly_probe SET value = 'new'`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(999, '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := ro.db.QueryRow(`SELECT value FROM readonly_probe`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "old" {
		t.Fatalf("read-only value = %q, want old snapshot", value)
	}
	var userVersion, futureMigrations int
	if err := ro.db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if err := ro.db.QueryRow(`SELECT COUNT(1) FROM schema_migrations WHERE version = 999`).Scan(&futureMigrations); err != nil {
		t.Fatal(err)
	}
	if userVersion != DatabaseFormatUserVersion || futureMigrations != 0 {
		t.Fatalf("read-only metadata = user_version %d, future migrations %d; want 2, 0", userVersion, futureMigrations)
	}
}

func TestOpenReadOnlySnapshotStableAcrossRollbackJournalWrite(t *testing.T) {
	path := createReadOnlyProbeDatabase(t)
	ro, err := OpenReadOnly(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	writer, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatal(err)
	}
	tx, err := writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE readonly_probe SET value = 'rollback-new'`); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := ro.db.QueryRow(`SELECT value FROM readonly_probe`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "old" {
		t.Fatalf("read-only value = %q, want old snapshot", value)
	}
}

func TestV2NoSidecarValidationDoesNotCreateSidecars(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	want := snapshotSQLiteArtifacts(t, path)
	beforeSnapshots := validationSnapshotCalls.Load()
	if _, ok := want[path+"-wal"]; ok {
		t.Fatal("fixture WAL still exists")
	}
	if _, ok := want[path+"-shm"]; ok {
		t.Fatal("fixture SHM still exists")
	}
	if err := validateExistingDatabase(path); err != nil {
		t.Fatal(err)
	}
	if got := validationSnapshotCalls.Load() - beforeSnapshots; got != 0 {
		t.Fatalf("validation snapshot calls = %d, want 0", got)
	}
	beforeSnapshots = validationSnapshotCalls.Load()
	ro, err := OpenReadOnly(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if got := validationSnapshotCalls.Load() - beforeSnapshots; got != 1 {
		t.Fatalf("read-only snapshot calls = %d, want 1", got)
	}
	assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))
}

func createReadOnlyProbeDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE readonly_probe(value TEXT)`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO readonly_probe(value) VALUES('old')`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStoreCleanupRunsOnceAfterDatabaseClose(t *testing.T) {
	db, err := sql.Open(sqliteDriverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("cleanup failed")
	var calls atomic.Int32
	s := &Store{
		db: db,
		cleanup: func() error {
			calls.Add(1)
			if err := db.Ping(); err == nil {
				t.Error("cleanup ran before database close")
			}
			return cleanupErr
		},
	}
	if err := s.Close(); !errors.Is(err, cleanupErr) {
		t.Fatalf("Close() error = %v, want cleanup error", err)
	}
	if err := s.Close(); !errors.Is(err, cleanupErr) {
		t.Fatalf("second Close() error = %v, want cleanup error", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want 1", got)
	}
}

func TestV2RollbackJournalValidationDoesNotMutateSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE schema_migrations SET applied_at = 'uncommitted' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	want := snapshotSQLiteArtifacts(t, path)
	if _, ok := want[path+"-journal"]; !ok {
		t.Fatal("fixture rollback journal missing")
	}
	if err := validateExistingDatabase(path); err != nil {
		t.Fatalf("rollback-journal validation: %v", err)
	}
	assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))
}

func validationTempDirCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "codegraph-validation-") {
			count++
		}
	}
	return count
}

func v2PinnedWALFixture(t *testing.T) (string, *sql.DB, *sql.DB, *sql.Tx) {
	t.Helper()
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dsn, err := BuildSQLiteDSN(path, OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := reader.Begin()
	if err != nil {
		reader.Close()
		t.Fatal(err)
	}
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		tx.Rollback()
		reader.Close()
		t.Fatal(err)
	}
	writer, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		tx.Rollback()
		reader.Close()
		t.Fatal(err)
	}
	return path, reader, writer, tx
}

func TestV2MalformedMigrationMetadataRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	dsn, err := BuildSQLiteDSN(path, OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE schema_migrations`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	want := snapshotSQLiteArtifacts(t, path)
	if _, err := Open(path); err == nil {
		t.Fatal("malformed migration metadata opened")
	}
	assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))
}

func setUserVersion(t *testing.T, path string, version int) {
	t.Helper()
	dsn, err := BuildSQLiteDSN(path, OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatal(err)
	}
}

func createSQLiteShell(t *testing.T, path, statement string) {
	t.Helper()
	dsn, err := BuildSQLiteDSN(path, OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if statement != "" {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

type sqliteArtifact struct {
	exists bool
	data   []byte
}

func snapshotSQLiteArtifacts(t *testing.T, path string) map[string]sqliteArtifact {
	t.Helper()
	out := map[string]sqliteArtifact{}
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		data, err := os.ReadFile(candidate)
		if err == nil {
			out[candidate] = sqliteArtifact{exists: true, data: data}
		} else if !os.IsNotExist(err) {
			t.Fatalf("read artifact %s: %v", candidate, err)
		}
	}
	return out
}

func assertSQLiteArtifactsEqual(t *testing.T, want, got map[string]sqliteArtifact) {
	t.Helper()
	for path, before := range want {
		after := got[path]
		if before.exists != after.exists || !bytes.Equal(before.data, after.data) {
			t.Fatalf("artifact mutated: %s", path)
		}
	}
	for path, after := range got {
		if _, ok := want[path]; !ok && after.exists {
			t.Fatalf("new artifact created: %s", path)
		}
	}
}

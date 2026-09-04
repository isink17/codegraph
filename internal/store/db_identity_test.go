package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

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
	if ceiling != 32 {
		t.Fatalf("migration ceiling = %d, want 32", ceiling)
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

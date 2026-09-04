package store

import (
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
	for name, before := range want {
		if string(got[name]) != string(before) {
			t.Fatalf("artifact mutated: %s", name)
		}
	}
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
	if _, err := Open(path); err == nil {
		t.Fatal("future migration database opened")
	}
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
	if _, err := Open(path); err == nil {
		t.Fatal("malformed migration metadata opened")
	}
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

func snapshotSQLiteArtifacts(t *testing.T, path string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		data, err := os.ReadFile(candidate)
		if err == nil {
			out[candidate] = data
		}
	}
	return out
}

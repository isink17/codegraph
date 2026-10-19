package store

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// indexedDatabase creates a closed database that records root, with migration
// 11 rolled back so the next writable open has a migration to apply.
func indexedDatabase(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertRepo(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE go_file_identity_evidence`,
		`DROP TABLE go_module_evidence`,
		`DELETE FROM schema_migrations WHERE version = 11`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func hasMigrationVersion(t *testing.T, s *Store, version int) bool {
	t.Helper()
	ok, err := hasMigration(s.db, version)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestOpenExistingMissingCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	path := filepath.Join(dir, RepoDatabaseFileName)
	_, err := OpenExistingWithOptions(path, OpenOptions{}, t.TempDir())
	if !errors.Is(err, ErrRepoNotIndexed) {
		t.Fatalf("err = %v, want ErrRepoNotIndexed", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("parent directory created: %v", err)
	}
}

// The connection, not the preceding stat, refuses a database deleted after it
// was looked up: sql.Open is lazy, so the file vanishes before SQLite opens it.
func TestOpenExistingConnectionRefusesVanishedDatabase(t *testing.T) {
	root := t.TempDir()
	path := indexedDatabase(t, root)
	dsn, err := buildSQLiteSafeExistingDSN(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := validateExistingDatabaseLocked(t.Context(), db, path, nil); err == nil {
		t.Fatal("vanished database validated")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("database recreated: %v", err)
	}
}

func TestOpenExistingRefusesEmptyAndUnindexedDatabases(t *testing.T) {
	empty := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	createSQLiteShell(t, shell, "")
	noRepo := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(noRepo)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	for name, path := range map[string]string{"empty": empty, "shell": shell, "no repo": noRepo} {
		want := snapshotSQLiteArtifacts(t, path)
		if _, err := OpenExistingWithOptions(path, OpenOptions{}, t.TempDir()); !errors.Is(err, ErrRepoNotIndexed) {
			t.Errorf("%s: err = %v, want ErrRepoNotIndexed", name, err)
		}
		assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))
	}
}

func TestOpenExistingRefusesInvalidDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	if err := os.WriteFile(path, []byte("not a database, not empty either"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := snapshotSQLiteArtifacts(t, path)
	if _, err := OpenExistingWithOptions(path, OpenOptions{}, t.TempDir()); err == nil || errors.Is(err, ErrRepoNotIndexed) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))
}

func TestOpenExistingRejectsForeignRootBeforeMigration(t *testing.T) {
	owner := t.TempDir()
	path := indexedDatabase(t, owner)
	want := snapshotSQLiteArtifacts(t, path)

	_, err := OpenExistingWithOptions(path, OpenOptions{}, t.TempDir())
	var mismatch *RepoMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want *RepoMismatchError", err)
	}
	ownerCanonical, _ := CanonicalRepoPath(owner)
	if mismatch.Recorded != ownerCanonical {
		t.Errorf("recorded = %q, want %q", mismatch.Recorded, ownerCanonical)
	}
	assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))

	ro, err := OpenReadOnly(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if hasMigrationVersion(t, ro, 11) {
		t.Error("migration applied to a foreign database")
	}
	repos, err := ro.ListRepos(t.Context(), 10, 0)
	if err != nil || len(repos) != 1 || repos[0].CanonicalPath != ownerCanonical {
		t.Errorf("repos = %v (%v), want only the owner", repos, err)
	}
}

func TestOpenExistingMigratesMatchingRoot(t *testing.T) {
	owner := t.TempDir()
	path := indexedDatabase(t, owner)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(owner, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	s, err := OpenExistingWithOptions(path, OpenOptions{}, link)
	if err != nil {
		t.Fatalf("open via symlink: %v", err)
	}
	defer s.Close()
	if !hasMigrationVersion(t, s, 11) {
		t.Error("pending migration not applied")
	}
	if _, found, err := s.FindRepo(t.Context(), owner); err != nil || !found {
		t.Errorf("FindRepo = %v, %v", found, err)
	}
}

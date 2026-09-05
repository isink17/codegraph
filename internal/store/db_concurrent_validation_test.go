package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestValidateExistingDatabaseUnstableSourceIsNotCorrupt pins the distinction
// the preflight must draw: a source that keeps changing has produced no
// verdict, so it must not be reported as an unsupported or corrupt database.
func TestValidateExistingDatabaseUnstableSourceIsNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, RepoDatabaseFileName)
	// Swap between two valid, compatible databases. Every inspection therefore
	// sees either a complete supported database or a replaced one -- never a
	// torn or unsupported file -- so the only verdicts available are "accepted"
	// and "unstable", and an unstable one can only come from the replacement.
	variants := make([]string, 2)
	for i := range variants {
		variants[i] = filepath.Join(dir, fmt.Sprintf("variant-%d.sqlite", i))
		s, err := Open(variants[i])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`CREATE TABLE padding(value BLOB)`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO padding(value) VALUES(zeroblob(?))`, (i+1)*64<<10); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	swap := func(i int) {
		t.Helper()
		staged := filepath.Join(dir, "staged.sqlite")
		data, err := os.ReadFile(variants[i%len(variants)])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(staged, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(staged, path); err != nil {
			t.Fatal(err)
		}
	}
	swap(0)

	var stop atomic.Bool
	var swapped atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; !stop.Load(); i++ {
			// Replacing the file atomically changes its identity, which every
			// inspection must notice.
			source, err := os.ReadFile(variants[i%len(variants)])
			if err != nil {
				return
			}
			staged := filepath.Join(dir, "swap.sqlite")
			if os.WriteFile(staged, source, 0o600) != nil || os.Rename(staged, path) != nil {
				return
			}
			swapped.Add(1)
		}
	}()
	defer func() {
		stop.Store(true)
		wg.Wait()
	}()

	// A run may still catch a quiescent moment and legitimately accept the
	// database; only a wrong classification is a failure.
	for attempt := 0; attempt < 200; attempt++ {
		err := validateExistingDatabase(path)
		if err == nil {
			continue
		}
		if !errors.Is(err, errSQLiteValidationUnstable) {
			t.Fatalf("validateExistingDatabase() error = %v, want %v", err, errSQLiteValidationUnstable)
		}
		return
	}
	t.Skipf("never observed an unstable inspection after %d file replacements", swapped.Load())
}

// TestOpenSucceedsWhileWriterChangesDatabase covers the CI regression: a
// legitimate concurrent writer makes the source non-quiescent, and openers must
// still succeed rather than exhaust preflight retries.
func TestOpenSucceedsWhileWriterChangesDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	seed, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()

	var stop atomic.Bool
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; !stop.Load(); i++ {
			if _, err := seed.db.Exec(`UPDATE schema_migrations SET applied_at = ? WHERE version = 1`, fmt.Sprint(i)); err != nil {
				return
			}
		}
	}()

	const openers = 8
	errs := make(chan error, openers)
	var wg sync.WaitGroup
	wg.Add(openers)
	for i := 0; i < openers; i++ {
		go func() {
			defer wg.Done()
			s, err := Open(path)
			if err == nil {
				err = s.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	stop.Store(true)
	writer.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Open() during concurrent writes error = %v", err)
		}
	}
}

func TestOpen_ConcurrentBootstrapRepeated(t *testing.T) {
	for round := 0; round < 25; round++ {
		path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
		const workers = 16
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		wg.Add(workers)
		for i := 0; i < workers; i++ {
			go func() {
				defer wg.Done()
				s, err := Open(path)
				if err == nil {
					err = s.Close()
				}
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: concurrent Open() error = %v", round, err)
			}
		}
		s, err := Open(path)
		if err != nil {
			t.Fatalf("round %d: reopen error = %v", round, err)
		}
		var userVersion int
		if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if userVersion != DatabaseFormatUserVersion {
			t.Fatalf("round %d: user_version = %d, want %d", round, userVersion, DatabaseFormatUserVersion)
		}
	}
}

// TestSafeExistingOpenRejectsUnsupportedWithoutWrites exercises the phase-1
// path an unstable preflight falls through to. The preflight is bypassed on
// purpose: the point is that the connection pragmas and the locked check alone
// reject an unsupported database without touching it or creating a sidecar.
func TestSafeExistingOpenRejectsUnsupportedWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(t *testing.T, path string)
	}{
		{
			name:    "future user_version",
			corrupt: func(t *testing.T, path string) { setUserVersion(t, path, DatabaseFormatUserVersion+1) },
		},
		{
			name: "future migration",
			corrupt: func(t *testing.T, path string) {
				execOnDatabase(t, path, `INSERT INTO schema_migrations(version, applied_at) VALUES(999, '')`)
			},
		},
		{
			name: "malformed migration metadata",
			corrupt: func(t *testing.T, path string) {
				execOnDatabase(t, path, `INSERT INTO schema_migrations(version, applied_at) VALUES(-1, '')`)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			tc.corrupt(t, path)
			// Leave the database in rollback-journal mode so that applying
			// journal_mode(WAL) before validation would rewrite the source
			// header, making any such write visible as a mutated artifact.
			execOnDatabase(t, path, `PRAGMA journal_mode = DELETE`)

			want := snapshotSQLiteArtifacts(t, path)
			dsn, err := buildSQLiteSafeExistingDSN(path, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open(sqliteDriverName, dsn)
			if err != nil {
				t.Fatal(err)
			}
			validationErr := validateExistingDatabaseLocked(t.Context(), db, path)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if validationErr == nil {
				t.Fatal("unsupported database accepted by locked validation")
			}
			assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))

			// The full open path must reject it too, still without writing.
			if _, err := Open(path); err == nil {
				t.Fatal("unsupported database opened")
			}
			assertSQLiteArtifactsEqual(t, want, snapshotSQLiteArtifacts(t, path))
		})
	}
}

// TestSafeExistingDSNOmitsPersistentPragmas guards the pragma audit: the
// pre-validation DSN must carry no setting that rewrites the source file.
func TestSafeExistingDSNOmitsPersistentPragmas(t *testing.T) {
	dsn, err := buildSQLiteSafeExistingDSN(filepath.Join(t.TempDir(), RepoDatabaseFileName), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("mode"); got != "rw" {
		t.Fatalf("mode = %q, want rw", got)
	}
	for _, pragma := range u.Query()["_pragma"] {
		for _, persistent := range []string{"journal_mode(", "auto_vacuum(", "page_size("} {
			if strings.HasPrefix(pragma, persistent) {
				t.Fatalf("pre-validation DSN carries persistent pragma %q", pragma)
			}
		}
	}
}

func execOnDatabase(t *testing.T, path, statement string) {
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
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

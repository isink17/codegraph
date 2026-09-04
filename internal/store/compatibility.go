package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const maxSupportedMigration = 18

var (
	// ErrDatabaseNewer means the database format belongs to a newer CodeGraph.
	ErrDatabaseNewer = errors.New("database created by a newer CodeGraph version; upgrade CodeGraph before opening it")
	// ErrDatabaseUnsupported means an existing file is not a readable v1 database.
	ErrDatabaseUnsupported = errors.New("unsupported or corrupt CodeGraph database")
)

// CheckDatabaseCompatibility inspects an existing database without creating
// it or changing its parent directory. Missing and zero-byte paths are valid
// inputs for the normal v1 creation flow.
func CheckDatabaseCompatibility(path string, opts OpenOptions) error {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && st.Size() == 0) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDatabaseUnsupported, err)
	}
	return checkDatabaseCompatibility(path, opts)
}

func checkDatabaseCompatibility(path string, opts OpenOptions) error {
	inspectPath, cleanup, err := compatibilityInspectionPath(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDatabaseUnsupported, err)
	}
	defer cleanup()
	dsn, err := BuildSQLiteDSN(inspectPath, opts, false, true)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDatabaseUnsupported, err)
	}
	// immutable prevents SQLite from creating WAL/SHM state while this
	// preflight is deciding whether the writable opener may run.
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDatabaseUnsupported, err)
	}
	query := u.Query()
	if inspectPath == path {
		query.Set("immutable", "1")
	}
	u.RawQuery = query.Encode()
	dsn = u.String()
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDatabaseUnsupported, err)
	}
	defer db.Close()

	ctx := context.Background()
	var userVersion int64
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		return fmt.Errorf("%w: %v", ErrDatabaseUnsupported, err)
	}
	if userVersion != 0 {
		return ErrDatabaseNewer
	}

	var tableType, tableSQL string
	err = db.QueryRowContext(ctx, `SELECT type FROM sqlite_master WHERE name = 'schema_migrations'`).Scan(&tableType)
	if errors.Is(err, sql.ErrNoRows) || tableType != "table" {
		return fmt.Errorf("%w: missing schema_migrations", ErrDatabaseUnsupported)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDatabaseUnsupported, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name = 'schema_migrations'`).Scan(&tableSQL); err != nil || tableSQL == "" {
		return fmt.Errorf("%w: invalid schema_migrations", ErrDatabaseUnsupported)
	}

	var count, invalidRows, minVersion, maxVersion int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(1), SUM(version < 1 OR applied_at IS NULL), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0)
		FROM schema_migrations
	`).Scan(&count, &invalidRows, &minVersion, &maxVersion); err != nil {
		return fmt.Errorf("%w: invalid schema_migrations: %v", ErrDatabaseUnsupported, err)
	}
	if count == 0 || invalidRows != 0 || minVersion < 1 {
		return fmt.Errorf("%w: invalid schema_migrations", ErrDatabaseUnsupported)
	}
	if maxVersion > maxSupportedMigration {
		return ErrDatabaseNewer
	}
	return nil
}

func compatibilityInspectionPath(path string) (string, func(), error) {
	sidecars := []string{path + "-wal", path + "-shm", path + "-journal"}
	for _, sidecar := range sidecars {
		if _, err := os.Stat(sidecar); err == nil {
			tmpDir, err := os.MkdirTemp("", "codegraph-compat-")
			if err != nil {
				return "", func() {}, err
			}
			cleanup := func() { _ = os.RemoveAll(tmpDir) }
			copyFile := func(src, dst string) error {
				data, err := os.ReadFile(src)
				if err != nil {
					return err
				}
				return os.WriteFile(dst, data, 0o600)
			}
			copyPath := filepath.Join(tmpDir, "database.sqlite")
			if err := copyFile(path, copyPath); err != nil {
				cleanup()
				return "", func() {}, err
			}
			for _, sidecar := range sidecars {
				if _, err := os.Stat(sidecar); err == nil {
					if err := copyFile(sidecar, copyPath+strings.TrimPrefix(sidecar, path)); err != nil {
						cleanup()
						return "", func() {}, err
					}
				} else if !errors.Is(err, fs.ErrNotExist) {
					cleanup()
					return "", func() {}, err
				}
			}
			return copyPath, cleanup, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", func() {}, err
		}
	}
	return path, func() {}, nil
}

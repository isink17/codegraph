package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/isink17/codegraph/internal/graph"
)

// Read-only access to an already-indexed database.
//
// Every other entry point into this package is allowed to create what it does
// not find: OpenWithOptions runs os.MkdirAll, treats a missing or zero-byte
// file as a new database, and runs Migrate before returning. That is correct
// for indexing and for the query commands that index on demand, but it is
// exactly wrong for a reader whose contract is "report on what is already
// there". Such a reader must not be able to answer questions about a database
// it just created itself.
//
// OpenReadOnly closes that off in two independent ways rather than one:
//
//  1. ErrRepoNotIndexed, from an explicit os.Stat before the driver is touched,
//     so a missing or empty database is a named error a caller can act on
//     rather than an empty result set.
//  2. `mode=ro` in the DSN, so even if the stat were to race a deletion, SQLite
//     itself refuses to create the file, and no statement issued on the handle
//     can write. Migrate is deliberately not called: on an up-to-date database
//     it would be a no-op, but "no-op" is a property of the current schema
//     rather than of this function, and a reader should not depend on it.
//
// The returned *Store is a normal Store whose read methods all work; only the
// write paths will fail, and they fail at the driver.

// ErrRepoNotIndexed reports that the repository has no graph database yet, or
// has one that was created but never written. Callers should surface it as an
// actionable "run codegraph index" message rather than as a zeroed report.
var ErrRepoNotIndexed = errors.New("store: repository is not indexed")

// OpenReadOnly opens an existing graph database for reading only.
//
// It returns ErrRepoNotIndexed (wrapped, so errors.Is matches) when dbPath does
// not exist or is a zero-byte placeholder. It never creates a database, never
// creates its parent directory, and never migrates.
func OpenReadOnly(dbPath string, opts OpenOptions) (*Store, error) {
	st, err := os.Stat(dbPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: no graph database at %s", ErrRepoNotIndexed, dbPath)
		}
		return nil, err
	}
	if st.Size() == 0 {
		return nil, fmt.Errorf("%w: graph database at %s is empty", ErrRepoNotIndexed, dbPath)
	}
	if err := validateExistingDatabase(dbPath); err != nil {
		return nil, err
	}
	// isNewDB=false keeps the one-time creation pragmas out of the DSN;
	// readOnly=true adds mode=ro.
	dsn, err := BuildSQLiteDSN(dbPath, opts, false, true)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		return nil, err
	}
	// sql.Open is lazy: it validates the DSN but opens no connection, so a
	// database the driver cannot actually read (a WAL file in a directory we
	// may not write, a corrupt header) would first fail somewhere inside the
	// caller's own queries, reported as a bare driver error with no path. Force
	// one statement here so the failure is attributable to the file.
	var probe int
	if err := db.QueryRow(`SELECT 1`).Scan(&probe); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s read-only: %w", dbPath, err)
	}
	return &Store{db: db}, nil
}

// FindRepo looks up a repository by root path without creating it.
//
// It is the read-only counterpart of UpsertRepo, which inserts a repos row as a
// side effect of being asked for one. Callers that must not mutate the database
// use this and treat found=false as "this database exists but does not know
// this repository".
func (s *Store) FindRepo(ctx context.Context, rootPath string) (graph.Repo, bool, error) {
	canonical, err := CanonicalRepoPath(rootPath)
	if err != nil {
		return graph.Repo{}, false, err
	}
	var repo graph.Repo
	err = s.db.QueryRowContext(ctx, `
		SELECT id, root_path, canonical_path
		FROM repos
		WHERE canonical_path = ?
	`, canonical).Scan(&repo.ID, &repo.RootPath, &repo.CanonicalPath)
	if errors.Is(err, sql.ErrNoRows) {
		return graph.Repo{}, false, nil
	}
	if err != nil {
		return graph.Repo{}, false, err
	}
	return repo, true, nil
}

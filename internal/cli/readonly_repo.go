package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/isink17/codegraph/internal/appname"
	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

// readOnlyRepo is an already-indexed graph database opened for reading only,
// together with the repository it holds.
type readOnlyRepo struct {
	Store  *store.Store
	Repo   graph.Repo
	Root   string
	DBPath string
}

// Close releases the database handle.
func (r *readOnlyRepo) Close() { _ = r.Store.Close() }

// openIndexedRepoReadOnly resolves a repository root, finds its existing graph
// database, and opens it read-only.
//
// This is the lifecycle every observational command needs and the one thing it
// must not get wrong: `repoDBPathsForRepo` (not `dbPathForRepo`) so no artifacts
// directory is created, an explicit stat so a missing database is an actionable
// "not indexed" error rather than an empty report, `store.OpenReadOnly` so the
// driver itself refuses writes and no migration runs, and `FindRepo` (not
// `UpsertRepo`) so asking about an unknown repository does not insert a row for
// it. A reader must not be able to answer questions about a database it created
// itself.
func openIndexedRepoReadOnly(ctx context.Context, cfg config.Config, repoRootCandidate string) (*readOnlyRepo, error) {
	repoRoot, err := config.ResolveRepoRoot(repoRootCandidate, "")
	if err != nil {
		return nil, err
	}
	canonical, err := store.CanonicalRepoPath(repoRoot)
	if err != nil {
		return nil, err
	}
	candidates, err := repoDBPathsForRepo(cfg, repoRoot, canonical)
	if err != nil {
		return nil, err
	}
	dbPath := ""
	for _, path := range candidates {
		st, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			// A database we cannot stat is not an unindexed repository. Telling
			// the user to re-index a permission-denied or I/O-failing database
			// would be wrong advice, and following it would rewrite a database
			// that may have been fine -- so surface the real error, the way
			// dbPathForRepo already does.
			return nil, fmt.Errorf("stat %s: %w", path, statErr)
		}
		if st.Size() > 0 {
			dbPath = path
			break
		}
	}
	if dbPath == "" {
		return nil, fmt.Errorf("%s is not indexed: no graph database found (run %s index %s)",
			repoRoot, appname.BinaryName, repoRoot)
	}

	s, err := store.OpenReadOnly(dbPath, store.OpenOptions{PerformanceProfile: cfg.DBPerformanceProfile})
	if err != nil {
		return nil, err
	}
	repo, found, err := s.FindRepo(ctx, repoRoot)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	if !found {
		_ = s.Close()
		return nil, fmt.Errorf("%s is not indexed: %s holds no graph for it (run %s index %s)",
			repoRoot, dbPath, appname.BinaryName, repoRoot)
	}
	return &readOnlyRepo{Store: s, Repo: repo, Root: repoRoot, DBPath: dbPath}, nil
}

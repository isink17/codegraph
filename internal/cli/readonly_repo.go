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
	dbPath, err := existingRepoDBPath(cfg, repoRoot, canonical)
	if err != nil {
		return nil, err
	}
	if dbPath == "" {
		return nil, notIndexedError(fmt.Sprintf("%s is not indexed: no graph database found (run %s index %s)",
			repoRoot, appname.BinaryName, repoRoot))
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
		err := repoRowMissingError(ctx, s, repoRoot, dbPath)
		_ = s.Close()
		return nil, err
	}
	return &readOnlyRepo{Store: s, Repo: repo, Root: repoRoot, DBPath: dbPath}, nil
}

// existingRepoDBPath returns the repository's non-empty graph database, or ""
// when there is none. It never creates anything, which is what lets a caller
// decide whether creating one is its job.
func existingRepoDBPath(cfg config.Config, repoRoot, canonical string) (string, error) {
	candidates, err := repoDBPathsForRepo(cfg, repoRoot, canonical)
	if err != nil {
		return "", err
	}
	for _, path := range candidates {
		st, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			// A database we cannot stat is not an unindexed repository. Telling
			// the user to re-index a permission-denied or I/O-failing database
			// would be wrong advice, and following it would rewrite a database
			// that may have been fine -- so surface the real error.
			return "", fmt.Errorf("stat %s: %w", path, statErr)
		}
		if st.Size() > 0 {
			return path, nil
		}
	}
	return "", nil
}

// repoRowMissingError explains a database that exists but holds no row for
// repoRoot. A database built for another root (a moved repository, a copied
// .codegraph directory) is reported as such and never rebound to this root.
// An empty database stays a plain "not indexed", wrapping ErrRepoNotIndexed.
func repoRowMissingError(ctx context.Context, s *store.Store, repoRoot, dbPath string) error {
	other, found, err := s.PrimaryRepo(ctx)
	if err != nil {
		return err
	}
	if found {
		return fmt.Errorf("%s holds the graph of %s, not %s; it is not reused for another root (run %s index %s --rebuild to replace it)",
			dbPath, other.CanonicalPath, repoRoot, appname.BinaryName, repoRoot)
	}
	return notIndexedError(fmt.Sprintf("%s is not indexed: %s holds no graph for it (run %s index %s)",
		repoRoot, dbPath, appname.BinaryName, repoRoot))
}

// notIndexedError keeps the established message while letting callers that
// report "not indexed" as a status, rather than a failure, recognise it with
// errors.Is(err, store.ErrRepoNotIndexed).
type notIndexedError string

func (e notIndexedError) Error() string { return string(e) }
func (e notIndexedError) Unwrap() error { return store.ErrRepoNotIndexed }

// openExistingApp opens a repository's existing graph database for the query
// commands, or reports "not indexed". It never creates a database or a repo
// row, but unlike openIndexedRepoReadOnly it opens the file in place: a query
// must keep working while watch or serve writes, must not copy the database on
// every call, and must still apply pending migrations after an upgrade.
func openExistingApp(ctx context.Context, cfg config.Config, repoRootCandidate string) (*App, graphRepo, int64, error) {
	repoRoot, err := config.ResolveRepoRoot(repoRootCandidate, "")
	if err != nil {
		return nil, graphRepo{}, 0, err
	}
	canonical, err := store.CanonicalRepoPath(repoRoot)
	if err != nil {
		return nil, graphRepo{}, 0, err
	}
	dbPath, err := existingRepoDBPath(cfg, repoRoot, canonical)
	if err != nil {
		return nil, graphRepo{}, 0, err
	}
	if dbPath == "" {
		return nil, graphRepo{}, 0, notIndexedError(fmt.Sprintf("%s is not indexed: no graph database found (run %s index %s)",
			repoRoot, appname.BinaryName, repoRoot))
	}
	app, repo, err := openIndexedApp(ctx, cfg, repoRoot, dbPath)
	if err != nil {
		return nil, graphRepo{}, 0, err
	}
	return app, graphRepo{ID: repo.ID, RootPath: repo.RootPath}, repo.ID, nil
}

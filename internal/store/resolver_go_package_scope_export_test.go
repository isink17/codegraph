package store

import "context"

// ClearEdgeResolutionsForTest unbinds every edge in a repository.
//
// It exists so one indexed tree can be handed to each resolver entrypoint from
// the same starting state, which is the only way to compare what the repo-wide,
// path-scoped and name-targeted resolvers decide about the same graph. It uses
// resolverClearResolutionSQL, so a test can never leave an edge carrying the
// provenance of a binding it no longer has.
func (s *Store) ClearEdgeResolutionsForTest(ctx context.Context, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE edges SET `+resolverClearResolutionSQL+` WHERE repo_id = ?`, repoID)
	return err
}

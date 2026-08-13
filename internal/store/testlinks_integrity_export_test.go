package store

import "context"

// TestLinkRow is a test-only projection of a test_links row, used by the
// external store_test integration tests that drive the real indexer.
type TestLinkRow struct {
	TestFile       string
	TargetFile     string
	TargetSymbolID *int64
	Reason         string
	Score          float64
}

// CountDanglingTestLinkRefsForTest reports how many test_links rows in the repo
// carry an id column that no longer matches an existing row. The P6 invariant
// is that this is always 0.
//
// It covers every id column, not only target_symbol_id: test_symbol_id is
// currently same-file only (assigned from the inserting file's own symbols) and
// target_file_id/test_file_id only move on purge, but a future cross-file
// resolution of those columns would reintroduce exactly this bug class.
//
// Test-only helper: the production API deliberately does not grow another
// exported diagnostic method for this.
func (s *Store) CountDanglingTestLinkRefsForTest(ctx context.Context, repoID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(1)
		FROM test_links t
		WHERE t.repo_id = ?
		  AND (
		       (t.target_symbol_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM symbols s WHERE s.id = t.target_symbol_id))
		    OR (t.test_symbol_id   IS NOT NULL AND NOT EXISTS (SELECT 1 FROM symbols s WHERE s.id = t.test_symbol_id))
		    OR (t.target_file_id   IS NOT NULL AND NOT EXISTS (SELECT 1 FROM files   f WHERE f.id = t.target_file_id))
		    OR NOT EXISTS (SELECT 1 FROM files f WHERE f.id = t.test_file_id)
		  )
	`, repoID).Scan(&n)
	return n, err
}

// TestLinksForTest returns every test_links row in the repo, joined to file
// paths, ordered for stable assertions.
func (s *Store) TestLinksForTest(ctx context.Context, repoID int64) ([]TestLinkRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tf.path, COALESCE(gf.path, ''), t.target_symbol_id, t.reason, t.score
		FROM test_links t
		JOIN files tf ON tf.id = t.test_file_id
		LEFT JOIN files gf ON gf.id = t.target_file_id
		WHERE t.repo_id = ?
		ORDER BY tf.path, COALESCE(gf.path, ''), t.reason
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TestLinkRow
	for rows.Next() {
		var r TestLinkRow
		if err := rows.Scan(&r.TestFile, &r.TargetFile, &r.TargetSymbolID, &r.Reason, &r.Score); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

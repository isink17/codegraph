package store

import "context"

// LiveFilePathsUnder returns the indexed (not deleted) paths strictly under
// the logical directory dir, in path order, through a range scan of the path
// index.
func (s *Store) LiveFilePathsUnder(ctx context.Context, repoID int64, dir string) ([]string, error) {
	// "0" is the byte after "/", so the range is exactly the dir+"/" prefix.
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM files WHERE repo_id=? AND is_deleted=0 AND path >= ? AND path < ? ORDER BY path`, repoID, dir+"/", dir+"0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

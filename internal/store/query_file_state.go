package store

import (
	"context"
	"strings"

	"github.com/isink17/codegraph/internal/platform"
)

// fileStateChunkSize bounds one IN-list of paths.
const fileStateChunkSize = 500

// FileSourceState is what the indexer recorded about a file's bytes. It is the
// cheap half of freshness: comparing it against a stat of the file on disk
// detects the common case -- the file changed after it was indexed -- without
// re-hashing content on every source-rendering request.
type FileSourceState struct {
	SizeBytes     int64
	MtimeUnixNs   int64
	ContentSHA256 string
}

// FileSourceStates returns the indexed size and modification time for each of
// the given logical repository paths. Paths the repository does not know are
// simply absent from the result.
func (s *Store) FileSourceStates(ctx context.Context, repoID int64, paths []string) (map[string]FileSourceState, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	wanted := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, raw := range paths {
		if raw == "" {
			continue
		}
		logical, err := platform.LogicalRepositoryPath(raw)
		if err != nil {
			return nil, err
		}
		if seen[logical] {
			continue
		}
		seen[logical] = true
		wanted = append(wanted, logical)
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	out := make(map[string]FileSourceState, len(wanted))
	for start := 0; start < len(wanted); start += fileStateChunkSize {
		end := min(start+fileStateChunkSize, len(wanted))
		chunk := wanted[start:end]

		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		for _, path := range chunk {
			args = append(args, path)
		}

		rows, err := s.db.QueryContext(ctx, `
			SELECT path, size_bytes, mtime_unix_ns, content_sha256
			FROM files
			WHERE repo_id = ? AND path IN (`+placeholders+`)
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var path string
			var state FileSourceState
			if err := rows.Scan(&path, &state.SizeBytes, &state.MtimeUnixNs, &state.ContentSHA256); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[path] = state
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}

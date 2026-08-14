package store

import (
	"context"
	gopath "path"
	"path/filepath"
	"strings"
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
// the given repository-relative paths. Paths the repository does not know are
// simply absent from the result.
func (s *Store) FileSourceStates(ctx context.Context, repoID int64, paths []string) (map[string]FileSourceState, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	wanted := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, raw := range paths {
		// Two forms are in play. The keys of the returned map are canonical
		// (slash-separated), because that is the form every caller holds -- a
		// symbol's FilePath, a related test's file. `files.path` itself is stored
		// in the indexing host's native form (the indexer derives it with
		// filepath.Rel/filepath.Clean), so the IN list below binds both forms;
		// binding only the canonical one returned no rows at all on Windows, which
		// made source-drift detection silently answer "not drifted".
		normalized := gopath.Clean(filepath.ToSlash(strings.TrimSpace(raw)))
		if normalized == "" || normalized == "." || seen[normalized] {
			continue
		}
		seen[normalized] = true
		wanted = append(wanted, normalized)
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	out := make(map[string]FileSourceState, len(wanted))
	for start := 0; start < len(wanted); start += fileStateChunkSize {
		end := min(start+fileStateChunkSize, len(wanted))
		chunk := wanted[start:end]

		bound := make([]string, 0, len(chunk)*2)
		for _, path := range chunk {
			bound = append(bound, storedPathVariants(path)...)
		}
		placeholders := strings.TrimRight(strings.Repeat("?,", len(bound)), ",")
		args := make([]any, 0, len(bound)+1)
		args = append(args, repoID)
		for _, path := range bound {
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
			out[filepath.ToSlash(path)] = state
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}

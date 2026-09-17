package store

import (
	"context"
	"testing"
)

func TestDeletedPathsInScanPreservesStoredPathIdentity(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	paths := []string{"pkg/a.go", `pkg/weird\name.go`}
	for _, path := range paths {
		if _, err := insertTestFile(ctx, s, repoID, path); err != nil {
			t.Fatalf("insertTestFile(%q) error = %v", path, err)
		}
	}

	const scanID = 7
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET is_deleted = 1, last_scan_id = ? WHERE repo_id = ?`, scanID, repoID); err != nil {
		t.Fatalf("mark files deleted error = %v", err)
	}
	got, err := s.DeletedPathsInScan(ctx, repoID, scanID)
	if err != nil {
		t.Fatalf("DeletedPathsInScan() error = %v", err)
	}
	if len(got) != len(paths) {
		t.Fatalf("DeletedPathsInScan() = %q, want %q", got, paths)
	}
	seen := make(map[string]bool, len(got))
	for _, path := range got {
		seen[path] = true
	}
	for _, path := range paths {
		if !seen[path] {
			t.Fatalf("DeletedPathsInScan() missing exact path %q: %q", path, got)
		}
	}
	if seen["pkg/weird/name.go"] {
		t.Fatalf("DeletedPathsInScan() created a slash alias: %q", got)
	}
}

package store

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestDirtyQueuePathFormatMatrix(t *testing.T) {
	ctx := context.Background()
	const originalAt = "2026-01-01T00:00:00Z"
	const claimAt = "2026-01-02T00:00:00Z"
	states := []struct {
		name, marker string
		files, dirty bool
	}{
		{name: "A_fresh_empty_unmarked"},
		{name: "B_current_empty", marker: "logical-slash-v1"},
		{name: "C_current_dirty", marker: "logical-slash-v1", dirty: true},
		{name: "D_populated_unmarked", files: true, dirty: true},
		{name: "E_dirty_only_unmarked", dirty: true},
		{name: "F_empty_foreign", marker: "native-v0"},
		{name: "G_populated_foreign", marker: "native-v0", files: true, dirty: true},
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			for _, op := range []string{"QueueDirtyFile", "QueueDirtyFiles", "HasDirtyFiles", "ClaimDirtyFiles", "DeleteClaimedDirtyFiles", "DrainDirtyFiles"} {
				t.Run(op, func(t *testing.T) {
					s, repoID := openUnmarkedRepo(t)
					current := state.marker == "logical-slash-v1"
					path := `src\pkg\file.go`
					if current {
						path = "src/pkg/file.go"
					}
					if state.marker != "" {
						setPathFormatMarker(t, s, repoID, state.marker)
					}
					if state.files {
						if _, err := insertTestFile(ctx, s, repoID, path); err != nil {
							t.Fatal(err)
						}
					}
					if state.dirty {
						insertTestDirtyFile(t, s, repoID, path)
					}
					before := snapshotPathState(t, s, repoID)
					var err error
					var hasDirty bool
					var paths []string
					switch op {
					case "QueueDirtyFile":
						err = s.QueueDirtyFile(ctx, repoID, path, "event")
					case "QueueDirtyFiles":
						err = s.QueueDirtyFiles(ctx, repoID, []string{path, "src/pkg/new.go"}, "event")
					case "HasDirtyFiles":
						hasDirty, err = s.HasDirtyFiles(ctx, repoID)
					case "ClaimDirtyFiles":
						paths, err = s.ClaimDirtyFiles(ctx, repoID, claimAt, "watch_inflight")
					case "DeleteClaimedDirtyFiles":
						err = s.DeleteClaimedDirtyFiles(ctx, repoID, []string{path}, originalAt)
					case "DrainDirtyFiles":
						paths, err = s.DrainDirtyFiles(ctx, repoID)
					}
					read := op == "HasDirtyFiles"
					allowed := current || (read && state.marker == "" && !state.files && !state.dirty)
					// Snapshot before checking the error: a missing gate must expose
					// metadata mutation as well as the missing sentinel.
					if !allowed || read {
						if after := snapshotPathState(t, s, repoID); after != before {
							t.Errorf("dirty state mutated (err=%v):\nbefore:\n%s\nafter:\n%s", err, before, after)
						}
					}
					if !allowed {
						if !errors.Is(err, ErrRepositoryPathFormatRebuild) {
							t.Errorf("err = %v, want ErrRepositoryPathFormatRebuild", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if read && hasDirty != state.dirty {
						t.Errorf("HasDirtyFiles = %v, want %v", hasDirty, state.dirty)
					}
					if op == "ClaimDirtyFiles" || op == "DrainDirtyFiles" {
						var want []string
						if state.dirty {
							want = []string{path}
						}
						if !slices.Equal(paths, want) {
							t.Errorf("paths = %q, want %q", paths, want)
						}
					}
					wantCount := 0
					if state.dirty {
						wantCount = 1
					}
					switch op {
					case "QueueDirtyFile":
						wantCount = 1
					case "QueueDirtyFiles":
						wantCount = 2
					case "DeleteClaimedDirtyFiles", "DrainDirtyFiles":
						wantCount = 0
					}
					var count int
					if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dirty_files WHERE repo_id=?`, repoID).Scan(&count); err != nil || count != wantCount {
						t.Errorf("dirty count = %d, %v; want %d", count, err, wantCount)
					}
					if op == "ClaimDirtyFiles" && state.dirty {
						var at, reason string
						if err := s.db.QueryRowContext(ctx, `SELECT queued_at, reason FROM dirty_files WHERE repo_id=? AND path=?`, repoID, path).Scan(&at, &reason); err != nil || at != claimAt || reason != "watch_inflight" {
							t.Errorf("claim metadata = %q, %q, %v", at, reason, err)
						}
					}
					if marker, exists := pathFormatMarker(t, s, repoID); marker != state.marker || exists != (state.marker != "") {
						t.Errorf("marker changed: %q, %v", marker, exists)
					}
				})
			}
		})
	}
}

func TestDirtyQueueNoOpAndValidationBeforeDatabase(t *testing.T) {
	// No database at all: strict no-ops and malformed timestamps must return
	// before any format lookup, independent of repository state.
	s := &Store{}
	ctx := context.Background()
	for _, paths := range [][]string{nil, {}} {
		if err := s.QueueDirtyFiles(ctx, 1, paths, "event"); err != nil {
			t.Fatal(err)
		}
		for _, at := range []string{"", "claim"} {
			if err := s.DeleteClaimedDirtyFiles(ctx, 1, paths, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.ClaimDirtyFiles(ctx, 1, "", "event"); err == nil || err.Error() != "claimAt must be non-empty" {
		t.Fatalf("claim validation = %v", err)
	}
	if err := s.DeleteClaimedDirtyFiles(ctx, 1, []string{"a.go"}, ""); err == nil || err.Error() != "claimedAt must be non-empty" {
		t.Fatalf("delete validation = %v", err)
	}
}

func TestDirtyQueueExactPathsAndRequeue(t *testing.T) {
	ctx := context.Background()
	s, repoID := openUnmarkedRepo(t)
	if err := s.EnsureCanonicalRepositoryPaths(ctx, repoID, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"src/pkg/file.go", `src/x\y.go`}
	if err := s.QueueDirtyFile(ctx, repoID, want[1], "single"); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDirtyFiles(ctx, repoID, want, "batch"); err != nil {
		t.Fatal(err)
	}
	const claimAt = "2026-01-01T00:00:00Z"
	paths, err := s.ClaimDirtyFiles(ctx, repoID, claimAt, "watch_inflight")
	slices.Sort(paths)
	if err != nil || !slices.Equal(paths, want) {
		t.Fatalf("claimed paths = %q, %v; want exact %q", paths, err, want)
	}
	if err := s.QueueDirtyFile(ctx, repoID, want[1], "requeued"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteClaimedDirtyFiles(ctx, repoID, paths, claimAt); err != nil {
		t.Fatal(err)
	}
	paths, err = s.DrainDirtyFiles(ctx, repoID)
	if err != nil || !slices.Equal(paths, want[1:]) {
		t.Fatalf("requeued paths = %q, %v; want %q", paths, err, want[1:])
	}
	if dirty, err := s.HasDirtyFiles(ctx, repoID); err != nil || dirty {
		t.Fatalf("after drain = %v, %v", dirty, err)
	}
}

func TestDirtyQueueDrainOrdering(t *testing.T) {
	ctx := context.Background()
	s, repoID := openUnmarkedRepo(t)
	setPathFormatMarker(t, s, repoID, "logical-slash-v1")
	// Path order and insertion order differ from queue order.
	for _, path := range []string{"a.go", "z.go"} {
		insertTestDirtyFile(t, s, repoID, path)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE dirty_files SET queued_at='2026-01-02T00:00:00Z' WHERE repo_id=? AND path='a.go'`, repoID); err != nil {
		t.Fatal(err)
	}
	paths, err := s.DrainDirtyFiles(ctx, repoID)
	if err != nil || !slices.Equal(paths, []string{"z.go", "a.go"}) {
		t.Fatalf("drain order = %q, %v", paths, err)
	}
	if dirty, err := s.HasDirtyFiles(ctx, repoID); err != nil || dirty {
		t.Fatalf("after drain = %v, %v", dirty, err)
	}
}

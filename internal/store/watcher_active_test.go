package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Lock files a watcher left behind are free and do not count; a lock that
// cannot be probed counts as held, so a rebuild refuses rather than guess.
func TestWatcherActiveTreatsUnprobeableLockAsHeld(t *testing.T) {
	db := filepath.Join(t.TempDir(), "codegraph.v2.sqlite")
	for _, p := range []string{watchLockPath(db, 1), watchLivePath(db, 1)} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if live, err := WatcherActive(db); err != nil || live {
		t.Fatalf("free lock files: live=%v err=%v", live, err)
	}
	probeLockFile = func(*os.File, bool) (bool, error) { return false, errors.New("lock call failed") }
	t.Cleanup(func() { probeLockFile = lockFile })
	if live, err := WatcherActive(db); err != nil || !live {
		t.Fatalf("unprobeable lock: live=%v err=%v", live, err)
	}
}

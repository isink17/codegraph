//go:build windows

package watcher

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/isink17/codegraph/internal/config"
)

// A config read that meets a rename still in flight fails with a sharing
// violation on Windows. The watcher retries it and then applies the new
// config, instead of stopping.
func TestConfigReloadRetriesSharingViolation(t *testing.T) {
	shortRetries(t)
	var busy atomic.Int32
	old := loadRepoConfig
	loadRepoConfig = func(root string) (config.RepoConfig, error) {
		if busy.Add(1) == 1 {
			return config.RepoConfig{}, &fs.PathError{Op: "open", Path: filepath.Join(root, ".codegraph", "config.json"), Err: windows.ERROR_SHARING_VIOLATION}
		}
		return old(root)
	}
	t.Cleanup(func() { loadRepoConfig = old })
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n", "gen/g.go": goFile})
	cfgDir := filepath.Join(r.root, ".codegraph")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cfgDir, "config.json"), `{"exclude": ["gen/**"]}`)
	r.waitLive(t, "excluded file retired after the retried reload", func(p []string) bool {
		return !slices.Contains(p, "gen/g.go")
	})
	if st := r.w.Stats(); st.LastTransient != "config_busy" || busy.Load() < 2 {
		t.Fatalf("the sharing violation was not retried: %+v", st)
	}
	r.waitState(t, StateWatching)
}

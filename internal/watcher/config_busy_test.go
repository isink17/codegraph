package watcher

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/isink17/codegraph/internal/config"
)

// Errno 32 is ERROR_SHARING_VIOLATION on Windows but EPIPE elsewhere, which
// must stay permanent. Only a sharing violation on the repository config is
// retried; one on any other file is not.
func TestConfigBusyIsWindowsConfigOnly(t *testing.T) {
	onConfig := &fs.PathError{Op: "open", Path: filepath.Join("repo", ".codegraph", "config.json"), Err: syscall.Errno(32)}
	onSource := &fs.PathError{Op: "open", Path: filepath.Join("repo", "main.go"), Err: syscall.Errno(32)}
	kind, ok := configLoadTransient(onConfig)
	if runtime.GOOS == "windows" {
		if !ok || kind != "config_busy" {
			t.Fatalf("config sharing violation: kind %q transient %v", kind, ok)
		}
	} else if ok {
		t.Fatalf("EPIPE on the config classified as transient %q", kind)
	}
	if kind, ok := configLoadTransient(onSource); ok {
		t.Fatalf("errno 32 on a source file classified as transient %q", kind)
	}
}

var errTestConfigBusy = errors.New("config busy (test)")

// Consecutive busy reads, on the reload and then on the recoveries it
// schedules, are each retried; the config is applied once a read succeeds.
// One rename raises at most a few reload events, so five busy reads reach the
// recovery path too.
func TestConfigReloadRetriesConsecutiveBusyReads(t *testing.T) {
	shortRetries(t)
	oldBusy, oldLoad := isSharingViolation, loadRepoConfig
	isSharingViolation = func(err error) bool { return errors.Is(err, errTestConfigBusy) }
	var loads atomic.Int32
	loadRepoConfig = func(root string) (config.RepoConfig, error) {
		if loads.Add(1) <= 5 {
			return config.RepoConfig{}, &fs.PathError{Op: "open", Path: filepath.Join(root, ".codegraph", "config.json"), Err: errTestConfigBusy}
		}
		return oldLoad(root)
	}
	t.Cleanup(func() { isSharingViolation, loadRepoConfig = oldBusy, oldLoad })
	r := startLiveRepo(t, map[string]string{"main.go": "package main\n", "gen/g.go": goFile})
	writeConfigAtomically(t, r.root, `{"exclude": ["gen/**"]}`)
	r.waitLive(t, "excluded file retired after five busy reads", func(p []string) bool {
		return !slices.Contains(p, "gen/g.go")
	})
	if n := loads.Load(); n < 6 {
		t.Fatalf("config read %d times, want the five busy reads retried", n)
	}
	r.waitState(t, StateWatching)
}

// writeConfigAtomically renames a complete config into place, so no reader
// sees it half-written.
func writeConfigAtomically(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".codegraph")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
}

package watcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// diagProbeConfig records, for a failed config reload, when it failed relative
// to the test's rename, how long the file stayed unreadable, and who held it.
// Diagnosis branch only.
func diagProbeConfig(t *testing.T, path string, loadErr error, renameStart, renameEnd int64) {
	failAt := time.Now().UnixNano()
	okAt, tries := int64(-1), 0
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); tries++ {
		if _, err := os.ReadFile(path); err == nil {
			okAt = time.Now().UnixNano()
			break
		}
		time.Sleep(100 * time.Microsecond)
	}
	// handle64 takes far longer than a rename: only a holder that is still
	// there after the probe can show up in it.
	var handles []byte
	if exe := os.Getenv("DIAG_HANDLE_EXE"); exe != "" && (okAt < 0 || okAt-failAt > int64(50*time.Millisecond)) {
		handles, _ = exec.Command(exe, "-accepteula", "-nobanner", "-a", filepath.Dir(path)).CombinedOutput()
	}
	rel := func(ns int64) int64 {
		if ns <= 0 {
			return -1
		}
		return (ns - renameStart) / 1000
	}
	t.Logf("DIAG t2 err=%v rename_start=0us rename_end=%dus fail=%dus readable_again=%dus tries=%d handles:\n%s",
		loadErr, rel(renameEnd), rel(failAt), rel(okAt), tries, handles)
}

package watcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/store"
)

// diagCloseStore closes s and measures how long graph.sqlite stays
// unrenamable afterwards. On Windows a file with an open handle cannot be
// renamed or deleted, so a nonzero release time means something still held
// the database when the store closed. Diagnosis branch only.
func diagCloseStore(t *testing.T, s *store.Store, dbPath string) {
	before := s.DiagDBStats()
	closeErr := s.Close()
	after := s.DiagDBStats()
	start := time.Now()
	releasedMS := int64(-1)
	var firstErr, probeErr error
	var stacks string
	for time.Since(start) < 5*time.Second {
		probeErr = os.Rename(dbPath, dbPath+".probe")
		if probeErr == nil {
			releasedMS = 0
			if firstErr != nil {
				releasedMS = max(1, time.Since(start).Milliseconds())
			}
			if err := os.Rename(dbPath+".probe", dbPath); err != nil {
				t.Logf("DIAG rename back failed: %v", err)
			}
			break
		}
		if firstErr == nil {
			firstErr = probeErr
			buf := make([]byte, 1<<20)
			stacks = string(buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
	key := filepath.Base(filepath.Dir(filepath.Dir(dbPath)))
	opened, open, zombies, details := store.DiagSQLiteReport(key)
	t.Logf("DIAG sqlite test=%s opened=%d open=%d zombies=%d locked=%t fds_after=%d", t.Name(), opened, open, zombies, firstErr != nil, diagOpenFDs(dbPath))
	for _, d := range details {
		t.Logf("DIAG sqlite-detail test=%s %s", t.Name(), d)
	}
	t.Logf("DIAG close test=%s inuse_before=%d open_before=%d inuse_after=%d open_after=%d close_err=%v release_ms=%d first_probe_err=%v final_probe_err=%v",
		t.Name(), before.InUse, before.OpenConnections, after.InUse, after.OpenConnections, closeErr, releasedMS, firstErr, probeErr)
	if firstErr == nil {
		if entries, err := os.ReadDir(filepath.Dir(dbPath)); err == nil {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Logf("DIAG okdir test=%s entries=%s", t.Name(), strings.Join(names, ","))
		}
	}
	if firstErr != nil {
		if exe := os.Getenv("DIAG_HANDLE_EXE"); exe != "" {
			out, err := exec.Command(exe, "-accepteula", "-nobanner", "-a", filepath.Dir(dbPath)).CombinedOutput()
			t.Logf("DIAG handles test=%s self_pid=%d err=%v\n%s", t.Name(), os.Getpid(), err, out)
		}
		var names []string
		if entries, err := os.ReadDir(filepath.Dir(dbPath)); err == nil {
			for _, e := range entries {
				names = append(names, e.Name())
			}
		}
		t.Logf("DIAG dir test=%s entries=%s", t.Name(), strings.Join(names, ","))
		t.Logf("DIAG goroutines test=%s at first failed probe:\n%s", t.Name(), stacks)
	}
}

// diagOpenFDs counts this process's descriptors on the database and its
// -wal/-shm files (Linux only; -1 elsewhere).
func diagOpenFDs(dbPath string) int {
	if runtime.GOOS != "linux" {
		return -1
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err == nil && (target == dbPath || target == dbPath+"-wal" || target == dbPath+"-shm") {
			n++
		}
	}
	return n
}

package watcher

import (
	"os"
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
	t.Logf("DIAG close test=%s inuse_before=%d open_before=%d inuse_after=%d open_after=%d close_err=%v release_ms=%d first_probe_err=%v final_probe_err=%v",
		t.Name(), before.InUse, before.OpenConnections, after.InUse, after.OpenConnections, closeErr, releasedMS, firstErr, probeErr)
	if firstErr != nil {
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

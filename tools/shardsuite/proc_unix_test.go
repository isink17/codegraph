//go:build !windows

package main

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func assertGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("test binary %d still running", pid)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

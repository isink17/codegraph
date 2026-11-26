package watcher

import (
	"runtime"
	"syscall"
	"testing"
)

// Errno 32 is ERROR_SHARING_VIOLATION on Windows but EPIPE elsewhere, which
// must stay permanent.
func TestConfigBusyIsWindowsOnly(t *testing.T) {
	kind, ok := configLoadTransient(syscall.Errno(32))
	if runtime.GOOS == "windows" {
		if !ok || kind != "config_busy" {
			t.Fatalf("sharing violation: kind %q transient %v", kind, ok)
		}
		return
	}
	if ok {
		t.Fatalf("EPIPE classified as transient %q", kind)
	}
}

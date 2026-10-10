//go:build cgo

package indexer

import (
	"strings"
	"testing"
)

// TestGoExternalImportPolicyUpgradeWithdrawsStaleEdge models a graph written
// by Go resolver policy 1, which bound stderrors.Is to the local errors.Is
// through exact_qualified. An ordinary update with no source change must
// withdraw it, and the result must equal a fresh index.
func TestGoExternalImportPolicyUpgradeWithdrawsStaleEdge(t *testing.T) {
	r := newRegisteredPolicyRepo(t, goExternalImportTree())
	for _, call := range []string{"errors.Is", "errors.As", "errors.Unwrap"} {
		r.bind(t, "go113.go", call, call)
	}
	r.exec(t, `UPDATE settings SET value='1' WHERE key=?`, r.markerKey("go"))
	if got := r.edgeState(t, "go113.go", "errors.Is"); !strings.Contains(got, "errors.Is(") {
		t.Fatalf("setup: stale edge not written: %s", got)
	}

	summary := r.update(t)
	if summary.FilesIndexed != 0 || !strings.Contains(strings.Join(summary.ResolverPolicyLanguages, ","), "go") {
		t.Fatalf("policy upgrade: indexed=%d languages=%v", summary.FilesIndexed, summary.ResolverPolicyLanguages)
	}
	for _, call := range []string{"errors.Is", "errors.As", "errors.Unwrap"} {
		if got := r.edgeState(t, "go113.go", call); strings.Contains(got, "go113.go") {
			t.Errorf("stale %s survived the policy upgrade: %s", call, got)
		}
		if got := r.refTarget(t, "go113.go", call); got != "" {
			t.Errorf("stale %s reference identity survived: %q", call, got)
		}
	}
	r.assertPolicyParity(t, "after Go policy upgrade")

	// The upgrade is recorded once: a reopened, unchanged graph does not
	// redecide Go again.
	if again := r.update(t); len(again.ResolverPolicyLanguages) != 0 {
		t.Fatalf("second update redecided %v", again.ResolverPolicyLanguages)
	}
	r.assertPolicyParity(t, "after no-op")
}

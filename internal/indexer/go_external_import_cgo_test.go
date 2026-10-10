//go:build cgo

package indexer

import (
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
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

// TestGoModuleRootImportPolicyUpgradeBindsOnUnchangedSource models a graph
// written by Go resolver policy 2, which left an import of the root module's
// own path unresolved. An ordinary update with no source change must bind it,
// survive a reopen, and the result must equal a fresh index.
func TestGoModuleRootImportPolicyUpgradeBindsOnUnchangedSource(t *testing.T) {
	r := newRegisteredPolicyRepo(t, goModuleRootTree())
	const want = "m.go:m.Foo(function) [module_import/high]"
	if got := r.edgeState(t, "m_ext_test.go", "example.com/m.Foo"); !strings.HasSuffix(got, want) {
		t.Fatalf("fresh: %s", got)
	}
	r.bind(t, "m_ext_test.go", "example.com/m.Foo", "")
	r.exec(t, `UPDATE settings SET value='2' WHERE key=?`, r.markerKey("go"))
	if got := r.edgeState(t, "m_ext_test.go", "example.com/m.Foo"); strings.Contains(got, "m.go:") {
		t.Fatalf("setup: epoch-2 state not written: %s", got)
	}

	summary := r.update(t)
	if summary.FilesIndexed != 0 || !strings.Contains(strings.Join(summary.ResolverPolicyLanguages, ","), "go") {
		t.Fatalf("policy upgrade: indexed=%d languages=%v", summary.FilesIndexed, summary.ResolverPolicyLanguages)
	}
	if got := r.edgeState(t, "m_ext_test.go", "example.com/m.Foo"); !strings.HasSuffix(got, want) {
		t.Fatalf("after upgrade: %s", got)
	}
	if got := r.refTarget(t, "m_ext_test.go", "example.com/m.Foo"); got != "m.Foo" {
		t.Fatalf("reference identity = %q", got)
	}
	r.assertPolicyParity(t, "after Go policy upgrade")

	// Reopen the database: the marker is durable, so nothing is redecided.
	if err := r.store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r.store, r.idx = s, New(s, policyRegistry(), nil)
	if again := r.update(t); len(again.ResolverPolicyLanguages) != 0 {
		t.Fatalf("update after reopen redecided %v", again.ResolverPolicyLanguages)
	}
	if got := r.edgeState(t, "m_ext_test.go", "example.com/m.Foo"); !strings.HasSuffix(got, want) {
		t.Fatalf("after reopen: %s", got)
	}
	r.assertPolicyParity(t, "after reopen no-op")
}

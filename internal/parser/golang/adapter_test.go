package golang

import (
	"context"
	"testing"
)

// TestLinkTests_SkipsTestMain: TestMain is the harness hook, not a test of a
// function called Main -- linking it would let a repo's exported Main steal a
// wrong test edge (P22.2).
func TestLinkTests_SkipsTestMain(t *testing.T) {
	src := `package pkg

import "testing"

func TestMain(m *testing.M) {}

func TestHelper(t *testing.T) {}
`
	pf, err := New().Parse(context.Background(), "pkg_test.go", []byte(src))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(pf.TestLinks) != 1 {
		t.Fatalf("TestLinks = %+v, want only TestHelper's link", pf.TestLinks)
	}
	link := pf.TestLinks[0]
	if link.TargetStableKey != "func:pkg::Helper" {
		t.Fatalf("TargetStableKey = %q, want func:pkg::Helper", link.TargetStableKey)
	}
	if link.Reason != "test_name_match" {
		t.Fatalf("Reason = %q, want test_name_match", link.Reason)
	}
}

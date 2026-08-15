//go:build cgo

package treesitter

import (
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func testLinkTargets(pf *graph.ParsedFile) []string {
	out := make([]string, 0, len(pf.TestLinks))
	for _, l := range pf.TestLinks {
		out = append(out, l.TargetStableKey)
	}
	return out
}

// TestLinkTestsGo_SkipsTestMain mirrors the pure-Go adapter's rule: the
// harness hook links nothing (P22.2).
func TestLinkTestsGo_SkipsTestMain(t *testing.T) {
	pf := &graph.ParsedFile{Symbols: []graph.Symbol{
		{Kind: "function", Name: "TestMain", QualifiedName: "pkg.TestMain", StableKey: "func:pkg::TestMain"},
		{Kind: "function", Name: "TestHelper", QualifiedName: "pkg.TestHelper", StableKey: "func:pkg::TestHelper"},
	}}
	linkTestsGo("pkg", pf)
	got := testLinkTargets(pf)
	if len(got) != 1 || got[0] != "func:pkg::Helper" {
		t.Fatalf("test link targets = %v, want [func:pkg::Helper]", got)
	}
}

// TestLinkTestsPython_TargetModuleStripsTestAffix: Python symbol stable keys
// scope to the defining file's module, so the target key must name the
// production module, not the test module (P22.2).
func TestLinkTestsPython_TargetModuleStripsTestAffix(t *testing.T) {
	cases := []struct {
		module string
		want   string
	}{
		{module: "test_utils", want: "func:utils::parse"},
		{module: "utils_test", want: "func:utils::parse"},
		{module: "utils", want: "func:utils::parse"}, // no affix: intra-file guess
	}
	for _, tc := range cases {
		pf := &graph.ParsedFile{Symbols: []graph.Symbol{
			{Kind: "function", Name: "test_parse", QualifiedName: tc.module + ".test_parse", StableKey: "func:" + tc.module + "::test_parse"},
		}}
		linkTestsPython(tc.module, pf)
		got := testLinkTargets(pf)
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("module %q: test link targets = %v, want [%s]", tc.module, got, tc.want)
		}
	}
}

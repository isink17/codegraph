//go:build cgo

package treesitter

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestLinkTestsGenericUsesAdapterKeyAndBoundaries(t *testing.T) {
	pf := &graph.ParsedFile{Symbols: []graph.Symbol{
		{Kind: "function", Name: "test_process", QualifiedName: "test_process", StableKey: "func:typescript:widget:test_process"},
		{Kind: "function", Name: "testProcess", QualifiedName: "testProcess", StableKey: "func:typescript:widget:testProcess"},
		{Kind: "function", Name: "TestProcess", QualifiedName: "TestProcess", StableKey: "func:typescript:widget:TestProcess"},
		{Kind: "function", Name: "testimonials", QualifiedName: "testimonials", StableKey: "func:typescript:widget:testimonials"},
		{Kind: "function", Name: "testingSomething", QualifiedName: "testingSomething", StableKey: "func:typescript:widget:testingSomething"},
		{Kind: "function", Name: "test", QualifiedName: "test", StableKey: "func:typescript:widget:test"},
		{Kind: "function", Name: "Test", QualifiedName: "Test", StableKey: "func:typescript:widget:Test"},
	}}
	linkTestsGeneric("widget", pf, func(target string) string {
		return "func:typescript:widget:" + target
	})

	got := testLinkTargets(pf)
	want := []string{
		"func:typescript:widget:process",
		"func:typescript:widget:Process",
		"func:typescript:widget:Process",
	}
	if len(got) != len(want) {
		t.Fatalf("generic test targets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("generic test targets = %v, want %v", got, want)
		}
	}
}

func TestTypeScriptGenericTestTargetMatchesProductionGrammar(t *testing.T) {
	pf, err := NewTypeScript().Parse(context.Background(), "widget.test.ts", []byte("function Process() {}\nfunction testProcess() {}\nfunction testimonials() {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := testLinkTargets(&pf); len(got) != 1 || got[0] != "func:typescript:widget:Process" {
		t.Fatalf("TypeScript test targets = %v, want [func:typescript:widget:Process]", got)
	}
}

func TestTestTargetModuleStripsSupportedAffixes(t *testing.T) {
	for _, tc := range []struct {
		module string
		suffix string
		want   string
	}{
		{"WidgetTest", "Test", "Widget"},
		{"widget.test", ".test", "widget"},
		{"widget_test", "_test", "widget"},
		{"testimonials", "Test", "testimonials"},
	} {
		if got := testTargetModule(tc.module, tc.suffix); got != tc.want {
			t.Errorf("testTargetModule(%q, %q) = %q, want %q", tc.module, tc.suffix, got, tc.want)
		}
	}
}

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

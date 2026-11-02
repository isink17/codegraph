package python

import (
	"context"
	"slices"
	"testing"
)

func TestAdapterRoutesPythonWindowsScriptExtension(t *testing.T) {
	adapter := New()
	if !adapter.Supports("scripts/tool.pyw") || !slices.Contains(adapter.Extensions(), ".pyw") {
		t.Fatal("Python adapter does not declare .pyw support")
	}
	pf, err := adapter.Parse(context.Background(), "scripts/tool.pyw", []byte("def run():\n    pass\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Symbols) != 1 || pf.Symbols[0].Name != "run" || pf.Symbols[0].Range.StartLine != 1 {
		t.Fatalf(".pyw symbols = %+v", pf.Symbols)
	}
}

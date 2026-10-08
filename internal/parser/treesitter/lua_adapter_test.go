//go:build cgo

package treesitter

import (
	"context"
	"testing"
)

func TestLuaAdapterDeclarationsImportsAndCallReferences(t *testing.T) {
	const src = `local function localFn() end
function globalFn() localFn(); module.run(); obj:method(); unknown() end
function M.tableFn() end
function M:method() end
require("./provider")
require(moduleName)
`
	p, err := NewLua().Parse(context.Background(), "main.lua", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range p.Symbols {
		got[s.StableKey] = s.Kind
	}
	for key, kind := range map[string]string{
		"func:lua:local:localFn":    "function",
		"func:lua:global:globalFn":  "function",
		"func:lua:global:M.tableFn": "function",
		"func:lua:global:M.method":  "method",
	} {
		if got[key] != kind {
			t.Errorf("symbol %q = %q", key, got[key])
		}
	}
	if len(p.Edges) != 0 {
		t.Fatalf("unproven calls produced edges: %+v", p.Edges)
	}
	references := map[string]bool{}
	for _, r := range p.References {
		references[r.Name] = true
	}
	for _, name := range []string{"localFn", "module.run", "obj:method", "unknown"} {
		if !references[name] {
			t.Errorf("missing call reference %q", name)
		}
	}
	if len(p.Imports) != 1 || p.Imports[0] != "./provider" {
		t.Fatalf("imports = %v", p.Imports)
	}
}

func TestLuaAdapterRejectsNonLuaExtension(t *testing.T) {
	if NewLua().Supports("main.luau") || !NewLua().Supports("main.lua") {
		t.Fatal("unexpected extension support")
	}
}

func TestLuaAdapterSyntaxErrorDoesNotInventDeclarations(t *testing.T) {
	p, err := NewLua().Parse(context.Background(), "broken.lua", []byte("function broken(\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Symbols) != 0 || len(p.Edges) != 0 {
		t.Fatalf("syntax recovery invented graph facts: %+v", p)
	}
}

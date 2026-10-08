//go:build cgo

package treesitter

import (
	"context"
	"testing"
)

func TestLuaScopeAndResolutionBoundaries(t *testing.T) {
	const source = `local function outer()
  local function inner() end
  do local function shadowed() end; shadowed() end
  inner()
end
function duplicate() end
function duplicate() end
function T.dot() end
function T:colon() end
T.dot()
value:colon()
require("pkg.static")
require(moduleName)
unknown = function() end
unknown()
local callback = function() end
callback()
`
	parsed, err := NewLua().Parse(context.Background(), "scope.lua", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Symbols) != 7 {
		t.Fatalf("symbols = %d, want seven named declarations", len(parsed.Symbols))
	}
	if len(parsed.Edges) != 0 {
		t.Fatalf("unproven calls produced edges: %+v", parsed.Edges)
	}
	if len(parsed.Imports) != 1 || parsed.Imports[0] != "pkg.static" {
		t.Fatalf("static imports = %v", parsed.Imports)
	}
	if len(parsed.References) != 6 {
		t.Fatalf("call references = %d, want six", len(parsed.References))
	}
}

func TestLuaDeclarationIdentitySeparatesLexicalDuplicates(t *testing.T) {
	const source = `local function same() end
local function outer()
  local function same() end
  do local function same() end end
end
`
	parsed, err := NewLua().Parse(context.Background(), "same.lua", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Symbols) != 4 {
		t.Fatalf("symbols = %d, want outer and three same declarations", len(parsed.Symbols))
	}
	keys := map[string]bool{}
	for _, sym := range parsed.Symbols {
		if keys[sym.StableKey] {
			t.Errorf("duplicate declaration identity %q", sym.StableKey)
		}
		keys[sym.StableKey] = true
	}
}

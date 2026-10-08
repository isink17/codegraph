//go:build cgo

package treesitter

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
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
	if got := luaEdgeTargets(parsed.Edges); !slices.Equal(got, []string{"shadowed@3:37->3:6", "inner@4:3->2:3"}) {
		t.Fatalf("edges = %v, want only the two local function calls", got)
	}
	if len(parsed.Imports) != 1 || parsed.Imports[0] != "pkg.static" {
		t.Fatalf("static imports = %v", parsed.Imports)
	}
	if len(parsed.References) != 8 {
		t.Fatalf("call references = %d, want eight", len(parsed.References))
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

func luaEdgeTargets(edges []graph.Edge) []string {
	out := []string{}
	for _, e := range edges {
		out = append(out, fmt.Sprintf("%s@%d:%d->%s", e.DstName, e.Line, e.Col, strings.TrimPrefix(e.Evidence, graph.LuaLocalFunctionEvidence)))
	}
	return out
}

func TestLuaLocalFunctionCallResolution(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         []string
	}{
		{"nested duplicate names bind innermost", `local function same() end
local function outer()
  local function same() end
  same()
  do local function same() end same() end
  same()
end
same()
`, []string{"same@4:3->3:3", "same@5:32->5:6", "same@6:3->3:3", "same@8:1->1:1"}},
		{"local variable shadows local function", `local function f() end
local function g(f) f() end
local function h()
  local f = 1
  f()
end
local function k() for f = 1, 2 do f() end end
local function m() for _, f in ipairs(t) do f() end end
`, []string{}},
		{"two calls on one line", `local function a() end
local function b() end
local function run() a() b() a() end
`, []string{"a@3:22->1:1", "b@3:26->2:1", "a@3:30->1:1"}},
		{"one resolved and one unresolved on one line", `local function a() end
local function run() a() missing() end
`, []string{"a@2:22->1:1"}},
		{"reassignment in a closure refuses every call", `local function f() end
local function g() f() end
local function h() local cb = function() f = nil end end
`, []string{}},
		{"non-local function statement reassigns the local", `local function f() end
local function g() f() end
function f() end
`, []string{}},
		{"reassignment of a shadowing local leaves the outer binding", `local function f() end
local function g()
  local function f() end
  f = nil
end
local function h() f() end
`, []string{"f@6:20->1:1"}},
		{"initialiser does not see its own local", `local function f() end
local function g()
  local f = function() f() end
end
`, []string{"f@3:24->1:1"}},
		{"recursion sees its own local function", `local function f() f() end
`, []string{"f@1:20->1:1"}},
		{"declaration after the call is not visible", `local function g() f() end
local function f() end
`, []string{}},
		{"global functions fields and methods stay unresolved", `function f() end
local M = {}
function M.f() end
local function g() f() M.f() M:f() end
`, []string{}},
		{"if branches scope their locals", `local function f() end
local function g()
  if c then local function f() end f() else f() end
end
`, []string{"f@3:36->3:13", "f@3:45->1:1"}},
		{"debug access refuses the file", `local function f() end
local function g() f() end
debug.setlocal(1, 1, nil)
`, []string{}},
		{"parse error refuses the file", `local function f() end
local function g() f() end
goto skip
::skip::
local x <const> = 1
`, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := NewLua().Parse(context.Background(), "calls.lua", []byte(tc.source))
			if err != nil {
				t.Fatal(err)
			}
			if got := luaEdgeTargets(parsed.Edges); !slices.Equal(got, tc.want) {
				t.Fatalf("edges = %v, want %v", got, tc.want)
			}
			for _, e := range parsed.Edges {
				found := false
				for _, r := range parsed.References {
					found = found || (r.Name == e.DstName && r.Range.StartLine == e.Line && r.Range.StartCol == e.Col)
				}
				if !found {
					t.Errorf("edge %s@%d:%d has no reference at the same occurrence", e.DstName, e.Line, e.Col)
				}
			}
		})
	}
}

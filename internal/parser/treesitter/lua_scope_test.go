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
	// A computed require can load the debug library, so this file proves no
	// call; the same file without it proves exactly the two local calls.
	if len(parsed.Edges) != 0 {
		t.Fatalf("edges = %v, want none beside a computed require", luaEdgeTargets(parsed.Edges))
	}
	static, err := NewLua().Parse(context.Background(), "scope.lua", []byte(strings.Replace(source, "require(moduleName)\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := luaEdgeTargets(static.Edges); !slices.Equal(got, []string{"shadowed@3:37->3:6", "inner@4:3->2:3"}) {
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
		{"debug through require refuses the file", `local function f() end
local function g() f() end
local d = require("debug")
`, []string{}},
		{"parse error refuses the file", `local function f() end
local function g() f() end
local x = = 1
`, []string{}},
		{"goto labels and attributes bind no name", `local function f() end
local function g()
  local limit <const> = 1
  local r <close> = nil
  for i = 1, limit do
    if i then goto continue end
    f()
    ::continue::
  end
end
`, []string{"f@7:5->1:1"}},
		{"attributed local shadows the function", `local function f() end
local function g()
  local f <const> = 1
  f()
end
`, []string{}},
		{"repeat condition sees the body's locals", `local function r() end
local function g()
  repeat local r = 1 until r()
  r()
end
`, []string{"r@4:3->1:1"}},
		{"generic for header is outside the loop scope", `local function f() end
local function g()
  for f in f() do f() end
end
`, []string{"f@3:12->1:1"}},
		{"numeric for bounds are outside the loop scope", `local function f() end
local function g()
  for f = f(), f() do f() end
end
`, []string{"f@3:11->1:1", "f@3:16->1:1"}},
		{"calls in assignment targets are walked", `local function f() end
local function g(t)
  t[f()] = 1
end
`, []string{"f@3:5->1:1"}},
		{"multiple assignment reassigns every name", `local function f() end
local function g() f() end
local a
a, f = 1, 2
`, []string{}},
		{"string and table call forms are bare calls", `local function f() end
local function g()
  f "x"
  f { 1 }
  f
  (
    1
  )
end
`, []string{"f@3:3->1:1", "f@4:3->1:1", "f@5:3->1:1"}},
		{"parenthesized indexed and chained callees stay unresolved", `local function f() return f end
local t = { f = f }
local function g()
  (f)()
  t.f()
  t["f"]()
  f()()
end
`, []string{"f@7:3->1:1"}},
		{"self in a method is a parameter", `local function self() end
local M = {}
function M:m() self() end
function M.n() self() end
`, []string{"self@4:16->1:1"}},
		{"columns count UTF-8 bytes", "local function f() end\nlocal s = \"\u00fc\U0001F600\" local function g() f() end\n",
			[]string{"f@2:39->1:1"}},
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

// LuaDebugFree is the file's half of the repository-wide debug proof: it holds
// exactly when the file's own code cannot reach the debug library, and a file
// with a parse error earns it only by never spelling a hazard name.
func TestLuaDebugFreeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         bool
	}{
		{"plain module", "local function f() end\nlocal function g() f() end\nreturn {g = g}\n", true},
		{"literal require and debug.traceback", "local j = require(\"json\")\nlocal t = debug.traceback\n", true},
		{"debug as data", "log.debug(\"debug\")\nlocal t = {debug = true}\n", true},
		{"debug.setupvalue", "debug.setupvalue(f, 1, nil)\n", false},
		{"debug aliased", "local dbg = debug\n", false},
		{"debug required", "local d = require(\"debug\")\n", false},
		{"computed require", "local x = require(name)\n", false},
		{"parse error without a hazard name", "local x = = 1\n", true},
		{"parse error spelling debug in a comment", "local x = = 1 -- debug\n", false},
		{"parse error spelling require", "local x = = require\n", false},
		{"parse error with a longer identifier", "local x = = debugger + my_G\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := NewLua().Parse(context.Background(), "m.lua", []byte(tc.source))
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Scope.LuaDebugFree != tc.want {
				t.Fatalf("LuaDebugFree = %v, want %v", parsed.Scope.LuaDebugFree, tc.want)
			}
		})
	}
}

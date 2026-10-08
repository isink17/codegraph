//go:build cgo

package indexer

import (
	"fmt"
	"slices"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// luaProven is a file whose one call, g's call to f on line 2, is bound by
// lexical scope to the `local function f` on line 1. Each case appends a
// trailer and states whether that proof survives.
const luaProven = "local function f() end\nlocal function g() f() end\n"

// luaRefusalCases pin when the Lua resolver keeps or withdraws a lexical
// binding, asserted on the indexed graph so they hold across parser changes.
//
// overBroad marks a case whose empty answer is a deliberate conservative
// refusal, not a semantic necessity: the binding is sound, but the current
// rule cannot tell or has no declaration symbol to bind to. A precision change is expected to flip exactly
// these cases, and must do so on purpose. knownGap marks the opposite: a
// binding the current rule keeps although a run-time path it does not see can
// rewrite the local; a soundness fix is expected to flip those.
var luaRefusalCases = []struct {
	name      string
	files     tree // m.lua is the file under test
	want      []string
	overBroad bool
	knownGap  bool
}{
	// -- the debug library, which can rewrite locals and upvalues at run time
	{name: "debug only in a line comment", files: tree{"m.lua": luaProven + "-- debug.setlocal\n"}, want: []string{"2->1"}},
	{name: "debug only in a block comment", files: tree{"m.lua": luaProven + "--[[ debug.setupvalue ]]\n"}, want: []string{"2->1"}},
	{name: "debug inside a longer string", files: tree{"m.lua": luaProven + "local s = \"debug mode\"\n"}, want: []string{"2->1"}},
	{name: "debug as a whole string literal", files: tree{"m.lua": luaProven + "local level = \"debug\"\n"}, overBroad: true},
	{name: "debug as a whole long string", files: tree{"m.lua": luaProven + "local level = [[debug]]\n"}, overBroad: true},
	{name: "debug as a field name", files: tree{"m.lua": luaProven + "local function h() log.debug(\"x\") end\n"}, overBroad: true},
	{name: "debug as a method name", files: tree{"m.lua": luaProven + "local function h() log:debug(\"x\") end\n"}, overBroad: true},
	{name: "debug as a table key", files: tree{"m.lua": luaProven + "local opts = {debug = true}\n"}, overBroad: true},
	{name: "locally shadowed debug", files: tree{"m.lua": luaProven + "local debug = {}\nlocal function h() debug.x() end\n"}, overBroad: true},
	{name: "debug.traceback only", files: tree{"m.lua": luaProven + "local function h() return xpcall(g, debug.traceback) end\n"}, overBroad: true},
	{name: "debug.getinfo only", files: tree{"m.lua": luaProven + "local function h() return debug.getinfo(1) end\n"}, overBroad: true},
	{name: "debug.setlocal", files: tree{"m.lua": luaProven + "local function h() debug.setlocal(2, 1, nil) end\n"}},
	{name: "debug.sethook", files: tree{"m.lua": luaProven + "debug.sethook(function() end, \"c\")\n"}},
	{name: "debug.setupvalue", files: tree{"m.lua": luaProven + "debug.setupvalue(g, 1, nil)\n"}},
	{name: "debug.upvaluejoin", files: tree{"m.lua": luaProven + "debug.upvaluejoin(g, 1, g, 1)\n"}},
	{name: "debug aliased through a local", files: tree{"m.lua": luaProven + "local d = debug\nd.setupvalue(g, 1, nil)\n"}},
	{name: "debug through _G field", files: tree{"m.lua": luaProven + "_G.debug.setupvalue(g, 1, nil)\n"}},
	{name: "debug through _G index", files: tree{"m.lua": luaProven + "_G[\"debug\"].setupvalue(g, 1, nil)\n"}},
	{name: "debug through require", files: tree{"m.lua": luaProven + "require(\"debug\").setupvalue(g, 1, nil)\n"}},
	{name: "debug through a computed name", files: tree{"m.lua": luaProven + "_G[\"de\" .. \"bug\"].setupvalue(g, 1, nil)\n"}, want: []string{"2->1"}, knownGap: true},
	{name: "debug used on this file from another file", files: tree{
		"m.lua":     luaProven + "return {g = g}\n",
		"other.lua": "local m = require(\"m\")\ndebug.setupvalue(m.g, 1, nil)\n",
	}, want: []string{"2->1"}, knownGap: true},

	// -- environment and global-table hazards reach globals, never locals
	{name: "local _ENV", files: tree{"m.lua": luaProven + "local _ENV = {}\n"}, want: []string{"2->1"}},
	{name: "setfenv", files: tree{"m.lua": luaProven + "setfenv(1, {})\n"}, want: []string{"2->1"}},
	{name: "load assigning the name", files: tree{"m.lua": luaProven + "load(\"f = nil\")()\n"}, want: []string{"2->1"}},
	{name: "loadstring assigning the name", files: tree{"m.lua": luaProven + "loadstring(\"f = nil\")()\n"}, want: []string{"2->1"}},
	{name: "rawset on _G", files: tree{"m.lua": luaProven + "rawset(_G, \"f\", nil)\n"}, want: []string{"2->1"}},
	{name: "_G field assignment", files: tree{"m.lua": luaProven + "_G.f = nil\n"}, want: []string{"2->1"}},

	// -- parse errors
	// A chunk with any syntax error never loads, so there is no run-time
	// binding to prove; withholding the edge is the honest answer.
	{name: "parse error in an independent region", files: tree{"m.lua": luaProven + "local function h()\n  local x = = 1\nend\n"}},
	{name: "parse error in the call region", files: tree{"m.lua": "local function f() end\nlocal function g() f( end\n"}},
	{name: "parse error that may hide a reassignment", files: tree{"m.lua": luaProven + "f = = nil\n"}},
	{name: "parse error in another file", files: tree{"m.lua": luaProven, "bad.lua": "local x = = 1\n"}, want: []string{"2->1"}},

	// -- Lua 5.2+ goto and Lua 5.4 attributes. The grammar parses them, and
	// neither a label nor an attribute binds or rebinds a name, so they leave
	// the proof intact.
	{name: "goto and label", files: tree{"m.lua": luaProven + "local function h()\n  goto done\n  ::done::\nend\n"}, want: []string{"2->1"}},
	{name: "const attribute elsewhere", files: tree{"m.lua": luaProven + "local limit <const> = 10\n"}, want: []string{"2->1"}},
	{name: "close attribute elsewhere", files: tree{"m.lua": luaProven + "local function h()\n  local r <close> = nil\nend\n"}, want: []string{"2->1"}},
	{name: "const local shadowing the function", files: tree{"m.lua": "local function f() end\nlocal f <const> = 1\nlocal function g() f() end\n"}},

	// -- proven closure and local-call cases
	{name: "nested local function calls an enclosing local function", files: tree{"m.lua": "local function helper() end\nlocal function outer()\n  local function inner() helper() end\n  inner()\nend\n"}, want: []string{"3->1", "4->3"}},
	{name: "recursion through local function", files: tree{"m.lua": "local function fact(n)\n  if n <= 1 then return 1 end\n  return n * fact(n - 1)\nend\n"}, want: []string{"3->1"}},
	{name: "forward reference is a global", files: tree{"m.lua": "local function a() b() end\nlocal function b() a() end\n"}, want: []string{"2->1"}},
	{name: "local declared then assigned a function", files: tree{"m.lua": "local f\nf = function() end\nlocal function g() f() end\n"}},
	{name: "local initialised with a function expression", files: tree{"m.lua": "local f = function() end\nlocal function g() f() end\n"}, overBroad: true},
	{name: "local function later reassigned", files: tree{"m.lua": luaProven + "f = function() end\n"}},

	// -- calls that no lexical proof reaches
	{name: "unknown global", files: tree{"m.lua": "local function g() missing() end\n", "other.lua": "function missing() end\n"}},
	{name: "field call on a table holding the local", files: tree{"m.lua": "local function f() end\nlocal t = {f = f}\nlocal function g() t.f() end\n"}},
	{name: "method call", files: tree{"m.lua": "local function f() end\nlocal function g(o) o:f() end\n"}},
	{name: "metatable __index", files: tree{"m.lua": "local function f() end\nlocal t = setmetatable({}, {__index = function() return f end})\nlocal function g() t.f() end\n"}},
	{name: "metatable __call", files: tree{"m.lua": "local function f() end\nlocal c = setmetatable({}, {__call = f})\nlocal function g() c() end\n"}},
	{name: "required module field", files: tree{"m.lua": "local m = require(\"other\")\nlocal function g() m.f() end\n", "other.lua": "local M = {}\nfunction M.f() end\nreturn M\n"}},
	{name: "required module called directly", files: tree{"m.lua": "local function g() require(\"other\").f() end\n", "other.lua": "local M = {}\nfunction M.f() end\nreturn M\n"}},
}

func TestLuaRefusalPrecision(t *testing.T) {
	for _, tc := range luaRefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			got := luaBoundCalls(t, newLifecycleRepo(t, tc.files), "m.lua")
			want := tc.want
			if want == nil {
				want = []string{}
			}
			if slices.Equal(got, want) {
				return
			}
			note := ""
			switch {
			case tc.overBroad:
				note = " (case marked overBroad: a precision change flips it; update the case deliberately)"
			case tc.knownGap:
				note = " (case marked knownGap: a soundness fix flips it; update the case deliberately)"
			}
			t.Fatalf("bound calls = %v, want %v%s", got, want, note)
		})
	}
}

// luaBoundCalls renders every call edge of path as "callLine->declLine", and
// fails on any call bound by a strategy other than the lexical one or to a
// declaration in another file.
func luaBoundCalls(t *testing.T, r *lifecycleRepo, path string) []string {
	t.Helper()
	symbols, err := r.store.ExportSymbolsPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	declLine := map[int64]int{}
	declFile := map[int64]string{}
	for _, s := range symbols {
		declLine[s.ID] = s.Range.StartLine
		declFile[s.ID] = s.FilePath
	}
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range edges {
		if e.Kind != store.EdgeKindCalls || e.DstSymbolID == nil {
			continue
		}
		if e.ResolutionStrategy != store.ResolutionStrategyLuaLocalFunction || declFile[*e.DstSymbolID] != e.FilePath {
			t.Fatalf("%s:%d %q bound by %q to %s", e.FilePath, e.Line, e.DstName, e.ResolutionStrategy, declFile[*e.DstSymbolID])
		}
		if e.FilePath == path {
			out = append(out, fmt.Sprintf("%d->%d", e.Line, declLine[*e.DstSymbolID]))
		}
	}
	slices.Sort(out)
	return out
}

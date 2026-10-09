//go:build cgo

package indexer

import (
	"fmt"
	"slices"
	"strings"
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
	// String data, field and method names, table keys and a lexically bound
	// debug never reach the library; only a free reference, a loader or a
	// global-table lookup does. traceback and getinfo read the stack but
	// cannot rebind a local or upvalue.
	{name: "debug as a whole string literal", files: tree{"m.lua": luaProven + "local level = \"debug\"\n"}, want: []string{"2->1"}},
	{name: "debug as a whole long string", files: tree{"m.lua": luaProven + "local level = [[debug]]\n"}, want: []string{"2->1"}},
	{name: "debug as a field name", files: tree{"m.lua": luaProven + "local function h() log.debug(\"x\") end\n"}, want: []string{"2->1"}},
	{name: "debug as a method name", files: tree{"m.lua": luaProven + "local function h() log:debug(\"x\") end\n"}, want: []string{"2->1"}},
	{name: "debug as a table key", files: tree{"m.lua": luaProven + "local opts = {debug = true}\n"}, want: []string{"2->1"}},
	{name: "debug as a numeric table key", files: tree{"m.lua": luaProven + "local t = {debug = 1}\n"}, want: []string{"2->1"}},
	{name: "debug as a method declaration", files: tree{"m.lua": luaProven + "local M = {}\nfunction M:debug() end\nfunction M.debug() end\n"}, want: []string{"2->1"}},
	{name: "debug as a label", files: tree{"m.lua": luaProven + "local function h()\n  goto debug\n  ::debug::\nend\n"}, want: []string{"2->1"}},
	{name: "locally shadowed debug", files: tree{"m.lua": luaProven + "local debug = {}\nlocal function h() debug.x() end\n"}, want: []string{"2->1"}},
	{name: "debug as a parameter", files: tree{"m.lua": luaProven + "local function h(debug) debug.setlocal(1, 1, nil) end\n"}, want: []string{"2->1"}},
	{name: "debug.traceback only", files: tree{"m.lua": luaProven + "local function h() return xpcall(g, debug.traceback) end\n"}, want: []string{"2->1"}},
	{name: "debug.getinfo only", files: tree{"m.lua": luaProven + "local function h() return debug.getinfo(1) end\n"}, want: []string{"2->1"}},
	{name: "string debug passed to a function", files: tree{"m.lua": luaProven + "log(\"debug\", 1)\n"}, want: []string{"2->1"}},
	{name: "_G literal field and key", files: tree{"m.lua": luaProven + "local p = _G.print\nlocal q = _G[\"print\"]\nlocal r = rawget(_G, \"print\")\n"}, want: []string{"2->1"}},
	{name: "package path and loaded module", files: tree{"m.lua": luaProven + "package.path = \"x\"\nlocal j = package.loaded.json\nlocal k = package.loaded[\"json\"]\n"}, want: []string{"2->1"}},
	{name: "load of a hazard-free literal chunk", files: tree{"m.lua": luaProven + "load(\"return 1\")()\n"}, want: []string{"2->1"}},
	{name: "require of a literal module", files: tree{"m.lua": luaProven + "local j = require(\"json\")\nlocal k = require \"lpeg\"\n"}, want: []string{"2->1"}},
	{name: "debug.setlocal", files: tree{"m.lua": luaProven + "local function h() debug.setlocal(2, 1, nil) end\n"}},
	{name: "debug.sethook", files: tree{"m.lua": luaProven + "debug.sethook(function() end, \"c\")\n"}},
	{name: "debug.setupvalue", files: tree{"m.lua": luaProven + "debug.setupvalue(g, 1, nil)\n"}},
	{name: "debug.upvaluejoin", files: tree{"m.lua": luaProven + "debug.upvaluejoin(g, 1, g, 1)\n"}},
	{name: "debug aliased through a local", files: tree{"m.lua": luaProven + "local d = debug\nd.setupvalue(g, 1, nil)\n"}},
	{name: "debug through _G field", files: tree{"m.lua": luaProven + "_G.debug.setupvalue(g, 1, nil)\n"}},
	{name: "debug through _G index", files: tree{"m.lua": luaProven + "_G[\"debug\"].setupvalue(g, 1, nil)\n"}},
	{name: "debug through require", files: tree{"m.lua": luaProven + "require(\"debug\").setupvalue(g, 1, nil)\n"}},
	{name: "debug through require of _G", files: tree{"m.lua": luaProven + "require(\"_G\").debug.setupvalue(g, 1, nil)\n"}},
	{name: "debug through require of package", files: tree{"m.lua": luaProven + "require \"package\".loaded.debug.setupvalue(g, 1, nil)\n"}},
	{name: "_G required into a local", files: tree{"m.lua": luaProven + "local G = require '_G'\nG.debug.setlocal(1, 1, nil)\n"}},
	{name: "_G required inside a literal chunk", files: tree{"m.lua": luaProven + "load(\"return require'_G'\")().debug.setlocal(1, 1, nil)\n"}},
	{name: "LuaJIT ffi required", files: tree{"m.lua": luaProven + "local ffi = require(\"ffi\")\n"}},
	{name: "LuaJIT ffi through package.loaded", files: tree{"m.lua": luaProven + "local x = package.loaded.ffi\n"}},
	{name: "LuaJIT ffi through _G", files: tree{"m.lua": luaProven + "local x = _G[\"ffi\"]\n"}},
	// A computed name can spell debug, so every computed loader argument or
	// global-table key is a hazard.
	{name: "debug through a computed name", files: tree{"m.lua": luaProven + "_G[\"de\" .. \"bug\"].setupvalue(g, 1, nil)\n"}},
	{name: "_G indexed by a variable", files: tree{"m.lua": luaProven + "local k = \"x\"\nlocal v = _G[k]\n"}},
	{name: "_ENV indexed by a computed key", files: tree{"m.lua": luaProven + "local v = _ENV[\"de\" .. \"bug\"]\n"}},
	{name: "_G string key with an escape", files: tree{"m.lua": luaProven + "local v = _G[\"de\\98ug\"]\n"}},
	{name: "_G long string key with a leading newline", files: tree{"m.lua": luaProven + "local v = _G[ [[\ndebug]] ]\n"}},
	{name: "rawget on _G with a computed key", files: tree{"m.lua": luaProven + "local k = \"x\"\nlocal v = rawget(_G, k)\n"}},
	{name: "rawget on _G for debug", files: tree{"m.lua": luaProven + "local v = rawget(_G, \"debug\")\n"}},
	{name: "_G passed as a value", files: tree{"m.lua": luaProven + "local env = _G\n"}},
	{name: "_G._G chain", files: tree{"m.lua": luaProven + "local v = _G._G\n"}},
	{name: "getfenv", files: tree{"m.lua": luaProven + "local env = getfenv(1)\n"}},
	{name: "package.loaded debug", files: tree{"m.lua": luaProven + "local d = package.loaded.debug\n"}},
	{name: "package.loaded computed key", files: tree{"m.lua": luaProven + "local k = \"x\"\nlocal d = package.loaded[k]\n"}},
	{name: "package.loaded as a value", files: tree{"m.lua": luaProven + "local loaded = package.loaded\n"}},
	{name: "require of a computed module", files: tree{"m.lua": luaProven + "local name = \"debug\"\nlocal d = require(name)\n"}},
	{name: "require passed as a value", files: tree{"m.lua": luaProven + "local ok, d = pcall(require, \"json\")\n"}},
	{name: "require aliased through a local", files: tree{"m.lua": luaProven + "local r = require\n"}},
	{name: "load of a computed chunk", files: tree{"m.lua": luaProven + "local src = \"x\"\nload(src)()\n"}},
	{name: "load of a literal chunk naming debug", files: tree{"m.lua": luaProven + "load(\"debug.setlocal(2, 1, nil)\")()\n"}},
	{name: "loadstring of a literal chunk computing debug", files: tree{"m.lua": luaProven + "loadstring([[return _G['de'..'bug'] ]])()\n"}},
	{name: "dofile of a computed path", files: tree{"m.lua": luaProven + "local p = \"x\"\ndofile(p)\n"}},
	{name: "debug member other than traceback and getinfo", files: tree{"m.lua": luaProven + "local reg = debug.getregistry()\n"}},
	{name: "debug indexed by brackets", files: tree{"m.lua": luaProven + "local tb = debug[\"traceback\"]\n"}},
	{name: "debug declared as a global function", files: tree{"m.lua": luaProven + "function debug.trace() end\n"}},
	{name: "debug used as a method receiver", files: tree{"m.lua": luaProven + "debug:x()\n"}},
	{name: "debug in a bracket table key", files: tree{"m.lua": luaProven + "local t = {[debug] = 1}\n"}},
	{name: "parenthesised _G", files: tree{"m.lua": luaProven + "local v = (_G).print\n"}},
	// Shadowing is lexical: a local debug in one scope does not cover a free
	// debug in another, nor its own initialiser.
	{name: "shadowed debug then free debug in another scope", files: tree{"m.lua": luaProven + "local function h()\n  local debug = {}\n  debug.x()\nend\nlocal function k() debug.setlocal(1, 1, nil) end\n"}},
	{name: "local debug aliasing the library", files: tree{"m.lua": luaProven + "local debug = debug\n"}},
	{name: "free debug before a later local debug", files: tree{"m.lua": luaProven + "local function h() debug.setlocal(1, 1, nil) end\nlocal debug = {}\n"}},

	// -- other indexed files. The debug library acts on the whole Lua state:
	// setupvalue and upvaluejoin on a closure m.lua exports, or setlocal from
	// a hook or a callback, rebind m.lua's locals although m.lua never names
	// debug. Any indexed file that can reach it withdraws every proof.
	{name: "debug used on this file from another file", files: tree{
		"m.lua":     luaProven + "return {g = g}\n",
		"other.lua": "local m = require(\"m\")\ndebug.setupvalue(m.g, 1, nil)\n",
	}},
	{name: "debug.upvaluejoin from another file", files: tree{
		"m.lua":     luaProven + "return {g = g}\n",
		"other.lua": "local m = require(\"m\")\nlocal function evil() end\ndebug.upvaluejoin(m.g, 1, function() return evil end, 1)\n",
	}},
	{name: "debug.sethook in an unrelated file", files: tree{"m.lua": luaProven, "other.lua": "debug.sethook(function() debug.setlocal(2, 1, nil) end, \"c\")\n"}},
	{name: "debug required in another file", files: tree{"m.lua": luaProven, "other.lua": "local d = require(\"debug\")\n"}},
	{name: "debug aliased in another file", files: tree{"m.lua": luaProven, "other.lua": "local dbg = debug\n"}},
	{name: "computed require in another file", files: tree{"m.lua": luaProven, "other.lua": "local name = ...\nlocal x = require(name)\n"}},
	{name: "pcall require in another file", files: tree{"m.lua": luaProven, "other.lua": "local ok, x = pcall(require, \"json\")\n"}},
	{name: "load of a computed chunk in another file", files: tree{"m.lua": luaProven, "other.lua": "local src = ...\nload(src)()\n"}},
	{name: "dofile of a computed path in another file", files: tree{"m.lua": luaProven, "other.lua": "local p = ...\ndofile(p)\n"}},
	{name: "_ENV indexed by a computed key in another file", files: tree{"m.lua": luaProven, "other.lua": "local k = ...\nlocal v = _ENV[k]\n"}},
	{name: "LuaJIT ffi in another file", files: tree{"m.lua": luaProven, "other.lua": "local ffi = require(\"ffi\")\n"}},
	{name: "parse error in another file spelling debug", files: tree{"m.lua": luaProven, "bad.lua": "local x = = debug\n"}},
	{name: "hazard in a nested directory", files: tree{"m.lua": luaProven, "lib/deep/other.lua": "local d = debug\n"}},
	// Neither table mutation nor the environment reaches a local: a call to
	// the local function f does not read M.f, _G.f or the chunk environment.
	{name: "exported field replaced from another file", files: tree{
		"m.lua":     luaProven + "return {f = f, g = g}\n",
		"other.lua": "local m = require(\"m\")\nm.f = function() end\nm.g = nil\n",
	}, want: []string{"2->1"}},
	{name: "global of the same name assigned in another file", files: tree{"m.lua": luaProven, "other.lua": "f = function() end\nfunction g() end\n"}, want: []string{"2->1"}},
	{name: "setfenv in another file", files: tree{"m.lua": luaProven + "return {g = g}\n", "other.lua": "local m = require(\"m\")\nsetfenv(m.g, {})\n"}, want: []string{"2->1"}},
	{name: "local _ENV in another file", files: tree{"m.lua": luaProven, "other.lua": "local _ENV = {}\n"}, want: []string{"2->1"}},
	{name: "local closure escapes and is called from another file", files: tree{"m.lua": luaProven + "return g\n", "other.lua": "local g = require(\"m\")\ng()\n"}, want: []string{"2->1"}},
	{name: "debug.traceback and getinfo in another file", files: tree{"m.lua": luaProven, "other.lua": "local function h() return debug.getinfo(1), debug.traceback() end\n"}, want: []string{"2->1"}},
	{name: "debug as a field or string in another file", files: tree{"m.lua": luaProven, "other.lua": "log.debug(\"debug\")\nlocal t = {debug = true}\n"}, want: []string{"2->1"}},
	// C code is outside the Lua proof: lua_setupvalue or lua_setlocal through
	// the C API rebinds an upvalue as debug.setupvalue does, undetected.
	{name: "C API lua_setupvalue in an indexed C++ file", files: tree{"m.lua": luaProven, "host.cpp": "struct lua_State;\nextern \"C\" const char *lua_setupvalue(lua_State *L, int f, int n);\nvoid rebind(lua_State *L) { lua_setupvalue(L, -2, 1); }\n"}, want: []string{"2->1"}, knownGap: true},

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

// Another file's debug access withdraws m.lua's proof although m.lua never
// changes, and restoring that file restores it: every update re-decides the
// whole repository's Lua calls, so the incremental graph matches a fresh
// index at every step, deletes and mixed-language updates included.
func TestLuaCrossFileDebugHazardLifecycleParity(t *testing.T) {
	const caller = "local function f() end\nlocal function g() f() end\nreturn {g = g}\n"
	r := newLifecycleRepo(t, tree{
		"m.lua":     caller,
		"other.lua": "local m = require(\"m\")\n",
		"main.go":   "package main\n\nfunc main() {}\n",
	})
	bound := ` => m.lua:f(function) [lua_local_function/high]`
	step := func(name, want string) {
		t.Helper()
		if got := r.edgeState(t, "m.lua", "f"); (want == bound) != (got == bound) {
			t.Fatalf("%s: f edge = %q, want bound=%v", name, got, want == bound)
		}
		r.assertFreshParity(t, name)
	}
	step("fresh", bound)

	r.write(t, "other.lua", "local m = require(\"m\")\ndebug.setupvalue(m.g, 1, nil)\n")
	r.update(t, "other.lua")
	step("other reaches debug", "")

	r.write(t, "other.lua", "local m = require(\"m\")\n")
	r.update(t, "other.lua")
	step("other stops reaching debug", bound)

	r.write(t, "hook.lua", "debug.sethook(function() end, \"c\")\n")
	r.update(t, "hook.lua")
	step("hazard file added", "")

	// A delete and an unrelated Go edit in one update: the deleted file's
	// tombstone keeps Lua in the update's language scope.
	r.remove(t, "hook.lua")
	r.write(t, "main.go", "package main\n\nfunc main() { helper() }\n\nfunc helper() {}\n")
	r.update(t, "hook.lua", "main.go")
	step("hazard file deleted with a Go edit", bound)

	r.write(t, "bad.lua", "local x = = debug\n")
	r.update(t, "bad.lua")
	step("parse error spelling debug", "")

	r.write(t, "bad.lua", "local x = = 1\n")
	r.update(t, "bad.lua")
	step("parse error without a hazard name", bound)

	// A Lua file the indexer never scans proves nothing about itself.
	writeRepoConfig(t, r.root, 256, "")
	r.write(t, "big.lua", "-- "+strings.Repeat("x", 300)+"\n")
	r.update(t, "big.lua")
	if got := r.edgeState(t, "m.lua", "f"); got == bound {
		t.Fatalf("oversize Lua file: f edge = %q, want unbound", got)
	}
	r.remove(t, "big.lua")
	r.update(t, "big.lua")
	if got := r.edgeState(t, "m.lua", "f"); got != bound {
		t.Fatalf("oversize Lua file removed: f edge = %q, want %q", got, bound)
	}

	r.remove(t, "m.lua")
	r.update(t, "m.lua")
	r.assertFreshParity(t, "caller deleted")
}

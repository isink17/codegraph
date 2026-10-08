# Lua grammar provenance

The CGO Lua adapter parses with
[`tree-sitter-grammars/tree-sitter-lua`](https://github.com/tree-sitter-grammars/tree-sitter-lua)
`v0.3.0`, commit `534c461d2b75b0887ec968ef9635f4460b0878b7`, MIT licensed
("Copyright (c) 2021 Munif Tanjim"). The generated `parser.c`, `scanner.c`
and headers are vendored unmodified in `internal/parser/treesitter/luagrammar`
with the upstream `LICENSE.md` verbatim; `PROVENANCE.md` there records the
file hashes and the refresh steps. The parser is ABI 14 and links against the
`github.com/smacker/go-tree-sitter` runtime like that module's own grammars.
Binary distributions must carry the MIT notice in `LICENSE.md`.

The earlier pin, the `lua` package of `github.com/smacker/go-tree-sitter`
(`tjdevries/tree-sitter-lua` revision `acb3f366`, no license file), is no
longer imported. Its external scanner kept state in C globals, so files parsed
at the same time could change each other's trees; the vendored scanner keeps
its state per parser. The grammar is standard Lua 5.4, not Luau; Lua 5.5
`global` declarations do not parse.

The adapter extracts function declarations, literal `require` specifier
strings, and call references. It emits a call edge only for a bare `name(...)`
call whose innermost lexical binding is a `local function` statement of the
same file that is never assigned to anywhere in its scope, closures included;
the edge's evidence names that declaration's position, and the resolver binds
the symbol at exactly that position or nothing. Parameters, loop variables,
other locals, globals, fields, methods, modules, dynamic requires, table
dispatch, metatables and receiver identity remain unresolved. A file with any
parse error, or whose own code can reach the `debug` library (whose
`setlocal`, `setupvalue`, `upvaluejoin` and `sethook` rewrite locals at run
time), produces no call edges. Reaching it means a free (not lexically bound)
reference to `debug`, `_G`, `_ENV`, `package`, `getfenv`, `require`, `load`,
`loadstring`, `dofile` or `loadfile`, except: `debug.traceback` and
`debug.getinfo` (they read the stack, never rebind it); `_G.x`, `_G["x"]`,
`rawget`/`rawset(_G, "x", ...)`, the same through `_ENV`, and
`package.loaded.x` for a literal `x` that is none of those names;
`package.path`, `package.cpath` and `package.config`; `require`, `dofile` and
`loadfile` of a literal other than `"debug"`; and `load`/`loadstring` of a
literal chunk that passes the same check. A computed key or module name
(`_G["de".."bug"]`, `_G[k]`, `require(name)`), a string with an escape, and
any of those names passed as a value (`pcall(require, m)`, `local env = _G`)
count as reaching it. Field and method names, table-constructor keys, labels,
parameters and locals named `debug`, and plain string data do not.

The proof covers this file's code only. Another module that obtains a
function this file exports and calls `debug.setupvalue` on it, or C code using
`lua_setupvalue`, can rebind an upvalue the edge relies on; that is not
detected. Nor is a literal `require` of another module that itself returns
the debug library: what a module returns is outside this file. A literal
`require` of a hazard name (`"_G"` and `"package"` are preloaded and hold
the library) or of LuaJIT's `"ffi"` does count as reaching it. Calls at file top level
have no source symbol and are not persisted as edges.

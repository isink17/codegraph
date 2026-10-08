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
parse error, or that names `debug` as an identifier or string
(whose `setlocal`/`setupvalue` rewrite locals at run time), produces no call
edges. `debug` reached without spelling it (`_G["de".."bug"]`) is not detected. Calls at file top level
have no source symbol and are not persisted as edges.

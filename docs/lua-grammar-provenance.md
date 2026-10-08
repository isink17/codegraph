# Lua grammar provenance

The CGO Lua adapter uses the ABI 14 parser already distributed by the pinned
`github.com/smacker/go-tree-sitter` dependency (`v0.0.0-20240827094217-dd81d9e9be82`).
That dependency's `_automation/grammars.json` records the source as
[`tjdevries/tree-sitter-lua`](https://github.com/tjdevries/tree-sitter-lua),
revision `acb3f3666383bd8cba9c9d9dde00ec4b16b055d6` (2024-03-14).

At that exact revision, `package.json` declares `"license": "ISC"`. The
revision has no standalone license file, no `tree-sitter.json`, and no
copyright holder declaration. The generated `parser.c` reports
`LANGUAGE_VERSION 14`; generated `parser.c` and `scanner.c` are distributed by
the Go dependency with dependency-side changes, including a parser-header
include rewrite. CodeGraph imports the dependency package and does not copy or
modify those generated C sources.

The ISC declaration is the upstream package metadata; this note does not infer
an author or add a fabricated copyright notice. The grammar is standard Lua,
not the distinct Luau grammar.

The adapter currently extracts function declarations, literal `require`
specifier strings, and call references. Calls are not resolved to edges because
this grammar and the current persisted evidence model do not prove lexical
ownership, runtime global state, or reassignment. Dynamic requires, table
dispatch, metatables, and receiver identity remain unresolved.

A local smoke sample was parsed from `lua/lua` commit
`0b29f408433e92953cc72b1d3e06c7ac8139e439`: `testes/all.lua`,
`main.lua`, `closure.lua`, `calls.lua`, and `locals.lua` produced 14 symbols,
51 call references, and zero call edges. Those upstream files are not copied
into this repository because the checkout has no standalone license file.

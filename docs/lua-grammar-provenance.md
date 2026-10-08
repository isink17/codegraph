# Lua grammar provenance

The CGO Lua adapter uses the ABI 14 parser already distributed by the pinned
`github.com/smacker/go-tree-sitter` dependency (`v0.0.0-20240827094217-dd81d9e9be82`).
That dependency's `_automation/grammars.json` records the source as
[`tjdevries/tree-sitter-lua`](https://github.com/tjdevries/tree-sitter-lua),
revision `acb3f3666383bd8cba9c9d9dde00ec4b16b055d6` (2024-03-14).

At that exact revision, `package.json` declares `"license": "ISC"`. The
revision has no standalone license file, no `tree-sitter.json`, and no
copyright holder declaration. The scanner's history credits earlier
Azganoth and MunifTanjim tree-sitter-lua sources, so the package metadata alone
does not settle the scanner's full provenance. The pinned package identifies
the generated parser/scanner paths, and the parser reports `LANGUAGE_VERSION
14`; byte-level provenance and generation-history differences have not been
fully reconciled. The Go module's own MIT license names Maxim Sukharev, but
does not establish the grammar's holder or grant.

CodeGraph imports the dependency package and does not copy or modify those
generated C sources. This evidence is not final redistribution clearance. The
grammar notice/provenance obligation must be closed, or a compatible grammar
with a clear license selected, before public v2.0 redistribution.

The ISC declaration is the upstream package metadata; this note does not infer
an author or add a fabricated copyright notice. The grammar is standard Lua,
not the distinct Luau grammar.

The adapter extracts function declarations, literal `require` specifier
strings, and call references. It emits a call edge only for a bare `name(...)`
call whose innermost lexical binding is a `local function` statement of the
same file that is never assigned to anywhere in its scope, closures included;
the edge's evidence names that declaration's position, and the resolver binds
the symbol at exactly that position or nothing. Parameters, loop variables,
other locals, globals, fields, methods, modules, dynamic requires, table
dispatch, metatables and receiver identity remain unresolved. A file with any
parse error (the pinned grammar does not parse `goto`, labels, or `<const>` /
`<close>` attributes) or that names `debug` as an identifier or string
(whose `setlocal`/`setupvalue` rewrite locals at run time) produces no call
edges. `debug` reached without spelling it (`_G["de".."bug"]`) is not detected. Calls at file top level
have no source symbol and are not persisted as edges.

The pinned grammar folds the newline before a statement that starts at column
zero into that statement's first token; the adapter reports positions and
names from the first non-space byte.

A local smoke sample was parsed from `lua/lua` commit
`0b29f408433e92953cc72b1d3e06c7ac8139e439`: `testes/all.lua`,
`main.lua`, `closure.lua`, `calls.lua`, and `locals.lua` produced 14 symbols,
51 call references, and zero call edges before local call resolution existed;
that count was not re-measured for this change. Those upstream files are not copied
into this repository because the checkout has no standalone license file.

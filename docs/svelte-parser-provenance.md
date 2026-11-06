# Bounded Svelte component parsing

`treesitter.NewSvelte()` is a CGO-only adapter for `.svelte` (case-insensitive).
It uses the existing vendored JavaScript and TypeScript grammars through
`NewTypeScript().Parse`; no Svelte grammar, compiler, preprocessor or runtime is
required. Its profile is `treesitter:svelte:v1`, semantic epoch 1.

## Supported boundary

The adapter reads only the initial whitespace/comment-separated script preamble.
It accepts one instance script and one module script (`context="module"` or
the Svelte 5 boolean `module` attribute), in either order. JavaScript is the
default; literal `lang="js"` and `lang="ts"` select the existing grammars.
Single-quoted attributes are accepted too. All other attributes (including
`src`, preprocessors and generics), duplicate attributes/scopes, and incomplete
script boundaries cause the entire preamble to produce no semantic evidence.
Scripts after template markup or style elements are outside this first version's
boundary. Malformed trailing markup cannot extend the parsed script region.
Closing scripts follow the literal HTML raw-text delimiter `</script>`; other
closing-tag spellings are conservatively unsupported.

Every block is parsed separately with all other bytes replaced by spaces except
CR/LF. Byte lengths, original script bytes, and line/column positions are retained
without offset rebasing, including tabs, Unicode and CRLF. Original source is
never rewritten. Module and instance declarations have distinct stable keys and
qualified names using `#module` and `#instance` suffixes on the logical file path.
The same source always produces the same identities, independent of block order.

Symbols, literal imports and script call references reuse TS/JS extraction.
Call edges, scope/import binding evidence, re-exports and test links are withheld:
this adapter has no Svelte resolver contract. In particular lifecycle functions
are ordinary script call references, not inferred runtime relationships.
Templates, directives, event handlers, actions, bindings, styles and generated
component exports contribute no symbols or references. No name matching connects
template expressions to script declarations. File token weights still cover the
whole file, as they do for other adapters.

## Production registration and capabilities

The CGO default registry registers `tsparser.NewSvelte()` for `.svelte` files.
It reports symbols available, calls false, and no-CGO parser/calls false,
with resolver dimensions unknown. CLI doctor lists Svelte among symbols-only
languages; MCP `supported_languages` exposes the same effective registry and
its versioned profile. Indexing persists `treesitter:svelte:v1`, semantic epoch 1,
and `parser_call_edges=false` on Svelte files.

The no-CGO build has no Svelte adapter or profile and omits Svelte from its
supported language, extension and MCP capability lists. The shared no-CGO
availability table explicitly records parser/calls false; it does not advertise
a fallback parser. `.svelte` is not claimed by the TS/JS adapter. The standard
registry collision rule fails closed when another language claims `.svelte`.

## Upstream fixture

`internal/parser/treesitter/svelte/fixtures/Tick.svelte` is an unmodified copy of
`documentation/examples/06-lifecycle/03-tick/App.svelte` from
[sveltejs/svelte](https://github.com/sveltejs/svelte/blob/663a3ca550b2051c8e32cb3b1fae4b1fadd9659e/documentation/examples/06-lifecycle/03-tick/App.svelte),
commit `663a3ca550b2051c8e32cb3b1fae4b1fadd9659e` (tag `svelte@5.0.0`).
SHA-256: `484627c57f92e2ffc0d1b54799bef8ff8f456de9c1ac661171da04e6b59e5807`.
Its MIT license is retained in `svelte/LICENSE.svelte.md`. Tests run offline.

Tests cover TS/module and JS/instance declarations, imports, lifecycle references,
original source coordinates, unsupported/ambiguous input, opaque malformed
templates, extension ownership, profiles, and real fresh-index versus incremental
and no-op graph parity (including replacement, ambiguity, recovery and deletion).

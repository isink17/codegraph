# Architecture

Core engine:

- config and install paths
- SQLite-backed graph store
- parser registry and adapters
- incremental indexer
- query and export layer
- MCP stdio server

Client-specific integration:

- `AGENTS.md`
- `docs/ai-assistant-guidance.md`
- `examples/*.json`

The package map is in [`AGENTS.md`](../AGENTS.md) and the
[detailed reference](README-detailed.md#architecture).

## Tree-sitter binding

CGO builds parse through `github.com/smacker/go-tree-sitter`, which bundles the
grammars CodeGraph uses. The official `github.com/tree-sitter/go-tree-sitter`
binding (v0.25.0) was evaluated and not adopted: its grammar modules do not
include Kotlin, Swift or C#, it makes parser and tree lifetimes explicit
(`Close`), so the shared extraction code would need a lifetime refactor, and
its Go grammar produced a different tree shape. Neither binding offers a
parser without CGO. A migration would need a grammar source and license plan,
parse and extraction parity for every shipped language, and native validation
on all six release platforms.

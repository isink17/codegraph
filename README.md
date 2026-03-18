# codegraph

`codegraph` is a local-first code context engine and MCP server for source repositories.

It builds and maintains a persistent local graph of files, symbols, references, and call edges, then exposes high-signal tools for coding agents and local developer workflows.

## Goals

- Local persistent repository graph in SQLite
- Incremental updates with content hashing
- MCP over stdio for Codex, Gemini CLI, and compatible clients
- One-binary install and operation where practical
- Clean local-first architecture without cloud dependencies

## Current milestone

This first substantial milestone includes:

- `codegraph install`
- `codegraph index <path>`
- `codegraph update <path>`
- `codegraph stats <path>`
- `codegraph serve`
- `codegraph graph export <path>`
- `codegraph doctor`
- `codegraph watch <path>`
- SQLite-backed repository metadata and graph storage
- Incremental file hashing and file-local graph replacement
- Working MCP tools including `graph_stats`
- Initial Go parser adapter using the standard Go AST behind a parser abstraction that is ready for Tree-sitter adapters

## Install

Prerequisite: install Go (1.26+ recommended) and ensure `go` is on your `PATH`.

Install Go examples:

```bash
# macOS (Homebrew)
brew install go

# Ubuntu/Debian
sudo apt-get update && sudo apt-get install -y golang-go
```

```powershell
# Windows (winget)
winget install -e --id GoLang.Go
```

```bash
go install github.com/isink17/codegraph/cmd/codegraph@latest
codegraph install
```

If you see `go: command not found`:

- Check installation: `go version`
- Restart your terminal after installing Go
- Ensure Go bin is on `PATH`:
- macOS/Linux common: `/usr/local/go/bin`
- Windows common: `C:\Program Files\Go\bin`

Then index a repository:

```bash
codegraph index .
codegraph stats .
codegraph serve --repo-root .
```

## Core commands

- `codegraph install`
- `codegraph index <path>`
- `codegraph update <path>`
- `codegraph serve --repo-root <path>`
- `codegraph stats <path>`
- `codegraph doctor`
- `codegraph graph export <path> --format json`
- `codegraph watch <path>`

## Design notes

- Core engine lives under `internal/`
- Client-specific guidance lives in `AGENTS.md`, `GEMINI.md`, and `examples/`
- Storage is SQLite with explicit migrations
- The parser layer is isolated so Tree-sitter adapters can be added without redesigning the indexer or query engine

## Roadmap highlights

- Tree-sitter adapters for more languages
- Better symbol resolution across files and packages
- Stronger semantic search ranking
- Richer related-test heuristics
- More export formats and tighter visualization flows

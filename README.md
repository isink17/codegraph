# codegraph

`codegraph` is a local-first code context engine and MCP server for source repositories.

It builds and maintains a persistent local graph of files, symbols, references, and call edges, then exposes high-signal tools for coding agents and local developer workflows.

## What You Get

- Local persistent repository graph in SQLite
- Incremental updates with content hashing
- MCP over stdio for Codex, Gemini CLI, and compatible clients
- One-binary workflow with minimal runtime dependencies
- Automatic ignore of hidden directories (`.*`) and common generated output directories (for example `build/`, `dist/`, `target/`, `out/`, `bin/`)
- Client-agnostic core architecture (client guidance stays in docs/examples)

## Current Milestone

This milestone includes:

- `codegraph install`
- `codegraph index <path>`
- `codegraph update <path>`
- `codegraph stats <path>`
- `codegraph find-symbol <path> <query>`
- `codegraph search <path> <query>`
- `codegraph callers <path> --symbol <name>`
- `codegraph callees <path> --symbol <name>`
- `codegraph impact <path> --symbol <name>`
- `codegraph serve --repo-root <path>`
- `codegraph graph export <path>`
- `codegraph doctor`
- `codegraph watch <path>`
- SQLite-backed repository metadata and graph storage
- Incremental file hashing and file-local graph replacement
- MCP tools including `graph_stats`
- Initial Go parser adapter behind parser abstractions

## Quick Start

### 1. Install Go

Go 1.26+ is recommended.

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

Verify:

```bash
go version
```

### 2. Install `codegraph`

```bash
go install github.com/isink17/codegraph/cmd/codegraph@latest
```

If you installed Go or changed PATH just now, restart your terminal before running:

```bash
codegraph install
```

### 3. Index a repository

```bash
codegraph index .
codegraph stats .
```

### 4. Start MCP server for tools/agents

```bash
codegraph serve --repo-root .
```

Important: `serve` is a long-running stdio server. It will look idle in a terminal until an MCP client sends requests.

## Common Setup Issue: `codegraph` Command Not Found

If `go install ...` succeeds but `codegraph` is not found, your Go bin directory is not on PATH.

Check where Go installs binaries:

```bash
go env GOBIN
go env GOPATH
```

Typical binary paths:

- Linux/macOS: `$HOME/go/bin` (or `$GOBIN`)
- Windows: `%USERPROFILE%\go\bin` (or `%GOBIN%`)

Add it to PATH, then open a new terminal.

Verify:

```bash
# Linux/macOS
command -v codegraph
```

```powershell
# Windows
where.exe codegraph
```

If you do not want to depend on PATH yet, run via Go directly:

```bash
go run ./cmd/codegraph install
go run ./cmd/codegraph index .
go run ./cmd/codegraph serve --repo-root .
```

## Using With Codex CLI (MCP)

Add `codegraph` as an MCP server:

```bash
codex mcp add codegraph -- codegraph serve --repo-root /absolute/path/to/repo
codex mcp list
```

Windows example:

```powershell
codex mcp add codegraph -- codegraph serve --repo-root D:\path\to\repo
codex mcp list
```

If `codegraph` is not on PATH, use:

```bash
codex mcp add codegraph -- go run ./cmd/codegraph serve --repo-root /absolute/path/to/repo
```

Manual config example (matches `examples/codex-mcp.json`):

```json
{
  "mcpServers": {
    "codegraph": {
      "command": "codegraph",
      "args": [
        "serve",
        "--repo-root",
        "/absolute/path/to/repo"
      ]
    }
  }
}
```

## Using With Other MCP Clients

Examples are in:

- `examples/codex-mcp.json`
- `examples/gemini-mcp.json`
- `examples/claude-desktop-mcp.json`

All follow the same `codegraph serve --repo-root <repo>` command pattern.

## Core Commands

- `codegraph install`
- `codegraph index <path>`
- `codegraph update <path>`
- `codegraph index <path> --jsonl` and `codegraph update <path> --jsonl` for line-delimited machine-readable output
- `codegraph serve --repo-root <path>`
- `codegraph stats <path>`
- `codegraph find-symbol <path> <query>`
- `codegraph search <path> <query>`
- `codegraph callers <path> --symbol <name>`
- `codegraph callees <path> --symbol <name>`
- `codegraph impact <path> [--symbol <name>] [--file <path>]`
- `codegraph doctor`
- `codegraph graph export <path> --format json|dot`
- `codegraph watch <path>`
- `codegraph watch <path> --jsonl` for line-delimited watch lifecycle events and final watch stats

## MCP Tools Exposed By `serve`

- `index_repo`
- `update_graph`
- `find_symbol`
- `find_callers`
- `find_callees`
- `get_impact_radius`
- `find_related_tests`
- `search_symbols`
- `search_semantic`
- `graph_stats`
- `supported_languages`

## Verification

Use this checklist after setup:

```bash
go test ./...
go build ./cmd/codegraph
```

Functional smoke test:

```bash
codegraph index .
codegraph update .
codegraph stats .
```

For MCP integration, confirm your client can call `tools/list` and `tools/call` against `codegraph serve`.

`codegraph doctor` now also reports whether `codegraph` is available on your shell PATH (`codegraph_on_path`), the resolved binary path (`codegraph_path`), and actionable PATH setup tips (`recommendations`) when it is missing.

## Architecture Notes

- Core engine code lives under `internal/`
- Storage is SQLite with explicit migrations
- Parser layer is isolated so additional language adapters can be added without redesigning indexer/query
- Keep core packages client-agnostic; client-specific instructions belong in docs/examples

## Roadmap Highlights

- Tree-sitter adapters for more languages
- Better cross-file/package symbol resolution
- Stronger semantic search ranking
- Richer related-test heuristics
- More export formats and visualization workflows


## License

This project is licensed under the Functional Source License, Version 1.1, MIT Future License (`FSL-1.1-MIT`).

On the second anniversary of each version’s release, that version converts to the MIT License.
See [`LICENSE`](./LICENSE) for details.

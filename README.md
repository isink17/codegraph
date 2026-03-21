# codegraph

`codegraph` is a local-first code context engine and MCP server for source repositories.

It builds a persistent repository graph in SQLite, keeps it incrementally up to date, and exposes that graph through both a CLI and an MCP stdio server for tools like Codex, Gemini CLI, and Claude Desktop.

Latest release: `v1.0.5` (March 21, 2026).

## What It Does

- Indexes files, symbols, references, and call edges into a local SQLite database
- Stores the database per repository as `codegraph.sqlite`
- Supports repeatable local workflows with no cloud dependency
- Exposes high-signal MCP tools for indexing, querying, impact analysis, semantic search, and stats
- Keeps the core engine client-agnostic; client-specific setup lives in docs and examples

## Supported Languages

Current parsing support includes:

- Go
- Python
- Java
- Kotlin
- C#
- TypeScript and JavaScript
- Rust
- Ruby
- Swift
- PHP
- C and C++

Go currently has the strongest parser support. Other languages use lighter heuristic adapters where noted by the codebase.

## Install

### Prerequisite

Go `1.26+` is recommended.

```bash
go version
```

### Install the binary

```bash
go install github.com/isink17/codegraph/cmd/codegraph@latest
```

Then run:

```bash
codegraph install
```

`codegraph install` creates the default config if missing and prints MCP setup snippets for supported clients.

If `codegraph` is not on your `PATH`, either add your Go bin directory to `PATH` or run from source:

```bash
go run ./cmd/codegraph install
go run ./cmd/codegraph index .
go run ./cmd/codegraph serve --repo-root .
```

On Windows, verify the binary with:

```powershell
where.exe codegraph
```

## Quick Start

From the repository you want to index:

```bash
codegraph index .
codegraph stats .
```

This creates `codegraph.sqlite` in the repository root.

Add this to your repo `.gitignore` if you do not want to commit the local database:

```gitignore
codegraph.sqlite
codegraph.sqlite-shm
codegraph.sqlite-wal
```

To start the MCP server for the current repo:

```bash
codegraph serve --repo-root .
```

`serve` is a long-running stdio server. When started in a terminal it will usually appear idle until an MCP client connects and sends requests.

## Database And Config

By default, `codegraph` stores graph data in the repository root as `codegraph.sqlite`.

Global config lives in the standard OS config directory:

- Windows: `%AppData%\codegraph\config.json`
- macOS: `~/Library/Application Support/codegraph/config.json`
- Linux: `${XDG_CONFIG_HOME:-~/.config}/codegraph/config.json`

Useful config commands:

- `codegraph config show`
- `codegraph config edit-path`
- `codegraph config validate`
- `codegraph config init --repo .`

`codegraph config init --repo .` creates a repo-local `.codegraph/config.json` with include, exclude, language, debounce, and parser settings for that repository.

## Core CLI Workflows

### Index and update

```bash
codegraph index .
codegraph update .
codegraph stats .
```

- `index` performs a scan and writes both scan summary and resulting repo stats
- `update` uses the same indexing pipeline but reports the scan kind as `update`
- `stats` returns the current persisted graph stats for the repo

For machine-readable streaming output:

```bash
codegraph index . --jsonl
codegraph update . --jsonl
```

### Query the graph

```bash
codegraph find-symbol . HelloWorld
codegraph search . http
codegraph callers . --symbol MyFunc
codegraph callees . --symbol MyFunc
codegraph impact . --symbol MyFunc --depth 2
```

These commands return concise JSON intended to be stable enough for local tooling.

### Watch for changes

```bash
codegraph watch .
```

For streaming watch lifecycle events:

```bash
codegraph watch . --jsonl
```

### Export the graph

```bash
codegraph graph export . --format json
codegraph graph export . --format dot
```

For focused graph exports:

```bash
codegraph graph export . --format dot --symbol MyTypeOrFunction
```

For line-delimited graph export:

```bash
codegraph graph export . --jsonl
```

### Maintenance

```bash
codegraph doctor
codegraph doctor --fix
codegraph clean .
codegraph clean . --vacuum
codegraph benchmark
```

- `doctor` checks basic installation and environment state
- `doctor --fix` applies non-destructive setup fixes
- `clean` inspects or vacuums the repo database
- `benchmark` runs the repository benchmark workflow

## MCP Setup

### Codex

Use `config.toml`:

```toml
[mcp_servers.codegraph]
command = "codegraph"
args = ["serve", "--repo-root", "/absolute/path/to/repo"]
startup_timeout_sec = 60
```

Windows example:

```toml
[mcp_servers.codegraph]
command = "codegraph"
args = ["serve", "--repo-root", "D:\\path\\to\\repo"]
startup_timeout_sec = 60
```

If `codegraph` is not on `PATH`, point the command at Go from the repository checkout:

```toml
[mcp_servers.codegraph]
command = "go"
args = ["run", "./cmd/codegraph", "serve", "--repo-root", "/absolute/path/to/repo"]
startup_timeout_sec = 60
```

### Gemini CLI and Claude Desktop

Examples:

- `examples/codex-mcp.toml`
- `examples/gemini-mcp.json`
- `examples/claude-desktop-mcp.json`

Codex uses TOML config. Gemini CLI and Claude Desktop use JSON examples in this repo.
See `examples/README.md` for client-specific notes and path replacement guidance.

All clients use the same server command pattern:

```text
codegraph serve --repo-root <repo>
```

For Codex specifically, keep `startup_timeout_sec = 60` so the server has enough time to open or migrate the SQLite database on startup.

## MCP Tools

`codegraph serve` currently exposes these MCP tools:

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
- `list_repos`

## Graphviz Preview

You can render a DOT export with Graphviz:

```bash
codegraph graph export . --format dot > graph.dot
dot -Tsvg graph.dot -o graph.svg
```

For PNG output:

```bash
dot -Tpng graph.dot -o graph.png
```

## Build And Verify

Build:

```bash
go build ./cmd/codegraph
```

Test:

```bash
go test ./...
```

Functional smoke test:

```bash
codegraph index .
codegraph update .
codegraph stats .
```

MCP smoke test:

- Start `codegraph serve --repo-root .`
- Confirm your MCP client can complete `initialize`, `tools/list`, and `tools/call`

## Architecture

High-level package map:

- `cmd/codegraph`: CLI entrypoint
- `internal/config`: config loading and path resolution
- `internal/store`: SQLite storage and migrations
- `internal/indexer`: repository scan and incremental updates
- `internal/parser`: parser interfaces and language adapters
- `internal/query`: symbol, caller, callee, impact, and stats queries
- `internal/mcp`: stdio MCP server and tool routing
- `internal/export`: JSON and DOT export
- `internal/watcher`: file watch and debounced updates

See `docs/architecture.md` for the short architecture note.

## License

This project is licensed under the Functional Source License, Version 1.1, MIT Future License (`FSL-1.1-MIT`).

On the second anniversary of each version's release, that version converts to the MIT License. See `LICENSE` for details.

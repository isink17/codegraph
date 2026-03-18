# AGENTS.md

## Project Scope

`codegraph` is a local-first code context engine and MCP server. Keep the core engine client-agnostic. Put Codex, Gemini, and other client guidance in docs or examples rather than hard-coding client behavior into the binary.

## Architecture Map

- `cmd/codegraph`: main entrypoint and command bootstrap
- `internal/appname`: centralized product naming for easy rename
- `internal/config`: config loading, path resolution, install defaults
- `internal/cli`: command handlers and JSON/text output
- `internal/store`: SQLite connection, migrations, persistence
- `internal/indexer`: repository scan, hashing, incremental updates
- `internal/parser`: parser interfaces and language adapters
- `internal/query`: symbol, caller, callee, impact, and stats queries
- `internal/search`: lightweight local semantic ranking
- `internal/mcp`: stdio MCP server and tool routing
- `internal/export`: JSON and DOT export
- `internal/watcher`: file watch and debounced updates

## Working Rules

- Preserve the local-first architecture
- Prefer one-binary workflows and minimal runtime dependencies
- Keep public interfaces narrow and explicit
- Use SQLite migrations for schema changes
- Keep JSON responses concise and predictable
- Avoid adding cloud services, heavy UI, or client-specific behavior in core packages

## Verification

- Run `go test ./...` when tests exist
- Run `go build ./cmd/codegraph`
- For indexing changes, verify `codegraph index`, `codegraph update`, and `codegraph stats`
- For MCP changes, verify `codegraph serve` still answers `tools/list` and `tools/call`

## Release Hygiene

- Keep `README.md` and `CHANGELOG.md` aligned with shipped behavior for each tag.
- Add a new changelog section for each release (for example `v1.0.2 -> v1.0.3`) before tagging.
- Tag releases with semantic version tags (`vX.Y.Z`) after tests/build pass.

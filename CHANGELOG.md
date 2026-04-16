# Changelog

# Release v1.0.9 - 16-04-2026

Improved Node.js repo indexing stability by hard-skipping common generated/tooling directories (for example node_modules and .next), refining default excludes, and clarifying ignore override behavior.

## Changes

### Fixes
- **Indexer:** Established a strict skip policy for common Node.js generated directories (e.g., `node_modules`, `.next`, `.nuxt`). These are now hardcoded and enforced early during filesystem traversal.
- **Indexer:** Clarified ignore override behavior; hardcoded skips are now non-overridable via negation patterns in `.codegraphignore` to ensure predictable indexer performance.
- **Config:** Centralized default exclude patterns to maintain consistency across the CLI and indexer.
- **SQL Hardening:** Added explicit bounds and safety checks for all path-filtering SQL queries.

## Upgrade Notes
- No required migrations or configuration changes.

## v1.0.7 - 27-03-2026

### Fixed

- Restored release cross-compilation by splitting tree-sitter adapters behind `//go:build cgo` and using heuristic parsers in `CGO_ENABLED=0` builds.

## v1.0.6 - 26-03-2026

### Fixed

- Stopped sending JSON-RPC responses to MCP notifications (`notifications/initialized` and other `notifications/*` methods), which violated the protocol and caused strict clients to fail on connect.
- Changed tool schema `"required": null` to omit the field when empty, fixing JSON Schema validation failures in strict MCP clients.
- Removed non-standard `structuredContent` field from tool call responses to conform to the MCP spec.
- Routed unhandled-method logging through the configured stderr writer instead of Go's default logger.

### Changed

- `NewServer` now accepts an `io.Writer` for error output, giving callers control over diagnostic logging.

### Docs

- Added Claude Code MCP setup section to README with `.mcp.json` examples.
- Added missing `list_scans` and `latest_scan_errors` to the MCP tools list in README.
- Added one-line descriptions to all 14 MCP tools in README.

## v1.0.5 - 21-03-2026

### Fixed

- Switched the default database location to per-repository `codegraph.sqlite` and excluded `codegraph.sqlite*` from indexing so repo-local DB mode works on repeated `index` and `serve` runs.
- Treated the previous global `db_dir` default as a legacy value so existing installs fall forward to repo-local DB behavior without manual config edits.
- Updated Codex MCP setup guidance and examples to use `config.toml` with `startup_timeout_sec = 60`.

## v1.0.3 - 18-03-2026

### Changed

- Simplified core code paths in `internal/store`, `internal/mcp`, `internal/cli`, and parser adapters while preserving behavior.
- Reduced duplicated row-scanning and pagination parsing logic to improve maintainability.
- Cached MCP tool definition payload construction for lower repeated allocation overhead on `tools/list`.

### Notes

- This release is focused on code quality, readability, and safe internal optimization with no intended user-facing breaking changes.

## v1.0.2 - 2026-03-18

### Highlights

- Added `watch --jsonl`, benchmark improvements, and `config init` workflow updates.
- Added/expanded query, export, MCP, and parser capabilities (including Python and heuristic multi-language adapters).
- Improved indexing and store performance with batching, scoped edge resolution, and scan/stat tuning.
- Added cleaner local maintenance workflows (`clean`, doctor improvements, and setup/path guidance updates).

## v1.0.1 - 18-03-2026

### Fixes

- Corrected Go module path from `github.com/example/localcodegraph` to `github.com/isink17/codegraph`.
- Updated internal imports to match the published module path.
- Updated install docs with actual module path and Go install/PATH troubleshooting guidance.

## v1.0.0 - 18-03-2026

Initial public release of `codegraph`.

### Highlights

- Added a local-first Go CLI for installation, indexing, updates, stats, graph export, doctor checks, watch mode, and MCP serving.
- Added a SQLite-backed repository graph with explicit migrations and incremental file hashing.
- Added a parser abstraction with a working Go parser adapter and a clean seam for future Tree-sitter adapters.
- Added MCP stdio support with generic tools for indexing, symbol lookup, call graph navigation, impact analysis, related test discovery, semantic search, and graph stats.
- Added release automation for macOS, Linux, and Windows GitHub release artifacts.
- Added agent-oriented documentation for Codex-style clients, Gemini CLI, and Claude-compatible MCP configuration examples.
- Added initial automated tests for install flow, platform paths, incremental indexing, and MCP `graph_stats`.

### Notes

- This release targets local-first usage on macOS and Linux first, with Windows kept feasible by design.
- The parser subsystem is intentionally shaped for Tree-sitter-backed adapters, while the first shipping implementation uses the Go standard AST for reliability and simple installation.

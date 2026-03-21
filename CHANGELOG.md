# Changelog

## v1.0.4 - 2026-03-21

### Fixed

- Switched the default database location to per-repository `codegraph.sqlite` and excluded `codegraph.sqlite*` from indexing so repo-local DB mode works on repeated `index` and `serve` runs.
- Treated the previous global `db_dir` default as a legacy value so existing installs fall forward to repo-local DB behavior without manual config edits.
- Updated Codex MCP setup guidance and examples to use `config.toml` with `startup_timeout_sec = 60`.

## v1.0.3 - 2026-03-18

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

## v1.0.1 - 2026-03-18

### Fixes

- Corrected Go module path from `github.com/example/localcodegraph` to `github.com/isink17/codegraph`.
- Updated internal imports to match the published module path.
- Updated install docs with actual module path and Go install/PATH troubleshooting guidance.

## v1.0.0 - 2026-03-18

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

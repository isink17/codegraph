# Changelog

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

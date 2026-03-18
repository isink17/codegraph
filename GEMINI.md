# GEMINI.md

## Purpose

Use `codegraph` as a local MCP-backed repository context engine. Keep prompts focused on repository-level navigation, impact analysis, symbol discovery, and related test discovery.

## Recommended usage

- Run `codegraph install` once
- Run `codegraph index <repo>`
- Use `codegraph serve --repo-root <repo>` for stdio MCP integration
- Prefer MCP tools for repository context instead of shelling out repeatedly

## Agent guidance

- Ask for `graph_stats` first when you need a quick repository summary
- Use `find_symbol`, `search_symbols`, and `search_semantic` before broad text searches
- Use `find_callers`, `find_callees`, `get_impact_radius`, and `find_related_tests` when planning edits
- Re-run `update_graph` after local file changes if watcher mode is not active

## Constraints

- Core engine is client-agnostic
- Do not assume special Gemini-only MCP features
- Tool results are concise JSON-oriented outputs suitable for planning and follow-up actions

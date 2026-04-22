# AI Assistant Guidance (Vendor-Neutral)

This repository ships `codegraph`, a local-first code context engine and MCP server.

## Recommended Workflow

- Run `codegraph install` once to set up local configuration and optional editor/tool integration.
- Run `codegraph index <repo>` to build the initial graph.
- Use `codegraph serve --repo-root <repo>` for stdio MCP integration.
- Prefer MCP tools for repository context instead of repeated shelling out.

## Guidance For Tool-Driven Work

- Start with a quick repository summary (stats) when orienting on an unfamiliar codebase.
- Use symbol discovery and search tools before broad text searches.
- Use callers/callees/impact tools when planning edits.
- Re-run incremental update after local file changes if watcher mode is not active.

## Constraints

- Keep the core engine client-agnostic.
- Do not assume assistant-specific features beyond the MCP protocol.
- Tool results should remain concise and JSON-oriented to support planning and follow-up actions.


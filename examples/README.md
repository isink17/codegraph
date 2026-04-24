# MCP Examples

This directory contains minimal `codegraph` MCP server configuration examples for supported clients.

## Files

- `codex-mcp.toml`: Codex `config.toml` example
- `gemini-mcp.json`: Gemini CLI MCP JSON example
- `claude-desktop-mcp.json`: Claude Desktop MCP JSON example

## Common pattern

All examples start the same server command:

```text
codegraph serve --repo-root /absolute/path/to/repo
```

Replace `/absolute/path/to/repo` with the repository you want `codegraph` to serve.

## Codex

`codex-mcp.toml` includes:

- `command = "codegraph"`
- `args = ["serve", "--repo-root", "..."]`
- `startup_timeout_sec = 60`

Keep `startup_timeout_sec = 60` so Codex allows enough time for the server to open or migrate the local SQLite database on startup.

## Gemini CLI and Claude Desktop

The JSON examples are intentionally minimal:

- `command`: `codegraph`
- `args`: `["serve", "--repo-root", "/absolute/path/to/repo"]`

If `codegraph` is not on `PATH`, replace `command` and `args` with a form that launches it through `go run` from the repository checkout.

## Repo-local database

By default, `codegraph` stores its database in the served repository as:

```text
.codegraph/codegraph.sqlite
```

If you do not want to commit the local database, ignore:

```text
.codegraph/

# Legacy location (still recognized if present):
codegraph.sqlite*
```

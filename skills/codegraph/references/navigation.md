# Navigation recipes

Read this when task-level context was not enough, or you need exact symbol,
file, or subsystem discovery in an unfamiliar codebase.

## Task → files and symbols

`context_for_task(task="<natural-language task>")`.

- Results are ranked: direct matches first, then their callers/callees, then
  linked tests. Each symbol carries `symbol_id`, `stable_key`, and
  `qualified_name` — exactly what the drill-down tools accept.
- The token budget (`max_tokens`) sizes the returned page, not the candidate
  set. `has_more` + `next_cursor` continue the same ranking; replay the same
  task and options with the cursor.
- Source is never embedded. Drill into the few symbols that matter instead
  of re-running broader searches.

## Known or suspected name → symbol

- `find_symbol(query="<name or qualified name>")` — exact and fuzzy match.
- `search_symbols(query="<keyword>")` — full-text over names, signatures,
  and doc comments; better for vocabulary you are not sure about.
- `search_semantic(query="...")` — meaning-based ranking; strongest with
  embeddings enabled, degrades to token overlap otherwise.

## Reading a symbol cheaply

Escalate `detail` only as needed:

- `card` — identity and location; enough to choose between candidates.
- `skeleton` — signature, container, members; enough to inspect an API.
- `excerpt` — a bounded source window; enough to read an implementation.
- `full` — the large window, for the one symbol you are actually changing.

Typical: `card` list → one `excerpt`. Responses mark cut content
(`truncated`, notes) — widen only when a note says something was omitted.

## Relationships

- `find_callers(symbol=...)` — who depends on this. Prefer `symbol_id` or
  `qualified_name` when the bare name might be ambiguous.
- `find_callees(symbol=...)` — what this orchestrates.
- Leaf helper: callers usually matter. Orchestrator: callees usually matter.
  Investigating unfamiliar code: often both.

## Bulk discovery loop

```
search_symbols(query="resolve", limit=50, format="compact")  # pick a symbol_id
find_symbol(query="<qualified_name>", detail="excerpt")      # read the code
```

`format=compact` is a tabular encoding for card-level pages on the tools
whose schema lists it; it cuts repeated JSON keys on bulk results. It never
changes which rows are returned.

## Orientation in a new repository

- `architecture_overview` — languages, directories, entry points, hubs.
- `list_files(path=...)` — what is indexed under a directory.
- `graph_stats` — index size and freshness.

## Gateway discovery

In gateway mode, find specialized tools with short phrases when the need
arises: `tool_search("impact dependencies")`, `tool_search("related
tests")`, `tool_search("dead code")`, `tool_search("architecture")`,
`tool_search("audit")` — then `tool_call` the exact name. Do not enumerate
hidden tools up front, and never use `tool_search` for a tool that is
already listed.

## Then verify

Graph results locate code; confirm behavior by reading the located source.
If a graph answer conflicts with what the source says, trust the source and
see `trust.md`.

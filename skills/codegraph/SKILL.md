---
name: codegraph
description: Query a repository's CodeGraph index (MCP tools or the codegraph CLI) instead of broad grep and file reading. Use when locating the code relevant to a task or ticket, understanding an unfamiliar codebase, finding a symbol's callers or callees, estimating a change's blast radius, tracing dependencies, or finding tests related to a change.
license: FSL-1.1-Apache-2.0
---

# CodeGraph

CodeGraph indexes a repository into a local symbol/call graph and answers
structural questions through MCP tools or the `codegraph` CLI. Use a few
narrow graph queries to find what matters, then read only that source.
Graph evidence narrows the search; source is the final authority.

## First moves

- Vague or task-level request → `context_for_task` with the task text.
- Known symbol name → `find_symbol`.
- "Who uses this?" / "What does this use?" → `find_callers` / `find_callees`.
- Planning a risky or broad change → read `references/change-impact.md`.
- Empty, ambiguous, or surprising result → read `references/trust.md`.

## Default workflow

1. **Orient.** `context_for_task(task=...)` returns ranked files and symbols
   with identities (`symbol_id`, `qualified_name`) — never source. If the
   response sets `has_more`, follow `next_cursor` only when the first page
   was not enough. If it returns nothing useful, rephrase once with concrete
   names from the task, then fall back to `find_symbol`/`search_symbols` —
   and to ordinary source search if the graph keeps coming up empty.
2. **Identify.** Pick the few symbols that actually matter from that page.
3. **Drill down.** `find_symbol` with `detail=excerpt` (or `full`) for that
   small set only.
4. **Relationships.** `find_callers` / `find_callees` before changing a
   load-bearing symbol.
5. **Blast radius** when the change is broad: impact, dependency, and test
   tools — see `references/change-impact.md`.
6. **Verify in source.** Read the code you are about to change.

A one-symbol question needs only steps 2–3. Do not run the ladder ritually,
and do not restart with broad file search unless graph context proved
insufficient.

## Keep context small

- **Progressive detail:** `card` (default) → `skeleton` → `excerpt` → `full`.
  A card already carries the identity every follow-up call accepts. Never
  request `full` for dozens of candidates to discover which one matters.
- **Bulk pages:** pass `format=compact` on tools whose schema offers it
  (card detail only). Keep JSON when you need structured nested fields.
- **Pages are bounded.** Use modest limits; when a response reports
  `truncated`, `has_more`, or a total larger than the page, continue
  deliberately via `offset` or the cursor. Oversized limits are rejected,
  not trimmed — the tool schema states each bound.
- **Gateway mode:** if `tool_search` and `tool_call` are listed, the core
  tools (`context_for_task`, `find_symbol`, `find_callers`, `find_callees`)
  are still direct — call them directly, never through `tool_search`. For a
  specialized need (impact, related tests, dead code, architecture, audit),
  run `tool_search` with a short phrase, then `tool_call` the result. A tool
  absent from `tools/list` may still exist — discover it when the workflow
  needs it, not at session start.

## Trust boundaries

CodeGraph is evidence, not permission to guess.

- An **empty callers/callees result is not proof** that no relationship
  exists: dynamic dispatch, code generation, and less-supported languages
  leave gaps. Confirm with source search before deleting or breaking.
- **Ambiguous names:** several symbols can share a bare name, and a
  relationship query answers for one of them. When a name may be ambiguous,
  resolve it with `find_symbol` first and query by `qualified_name` or
  `symbol_id`. Never pick a same-name candidate because it "looks right".
- **Freshness:** after editing source, run `update_graph` (MCP) or
  `codegraph update .` before relying on impact or caller answers.
  `graph_stats` reports when the repo was last indexed; zero indexed files
  means it never was. Do not full-reindex before every query — incremental
  update is the normal mode.

Details, audit, and fallback rules: `references/trust.md`.

## Deeper workflows

- Read `references/navigation.md` when task-level context is not enough and
  you need exact symbol, file, or subsystem discovery recipes.
- Read `references/change-impact.md` when planning a change to a shared
  symbol, API, or subsystem, or before removing code.
- Read `references/trust.md` when a result is empty, ambiguous, or
  surprising, or when the index may be stale.

## If MCP is unavailable

- With the `codegraph` CLI installed, the same core workflow works:
  `codegraph find_symbol . <query>`, `codegraph find_callers . <symbol>`,
  `codegraph find_callees . <symbol>`,
  `codegraph get_impact_radius . --symbol <name>`, and
  `git diff --name-only | codegraph find_related_tests --stdin`.
- No index yet: `codegraph index .` once, then `codegraph update .` after
  changes (or `codegraph watch .` for automatic updates).
- Neither available: fall back to ordinary source search, and suggest
  installing CodeGraph (https://github.com/isink17/codegraph) rather than
  installing it unprompted.

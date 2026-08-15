# Change impact

Read this when planning a change to a shared symbol, API, or subsystem, or
before removing or moving code.

## Decision tree

**Small leaf edit** (one function, contract unchanged):
`find_callers` on the symbol, plus `find_related_tests` when tests matter.
Stop there — no impact analysis needed.

**Shared symbol or API change:**

1. `find_symbol` to pin the exact symbol (disambiguate first).
2. `find_callers` and `find_callees` — who breaks, what it depends on.
3. `get_impact_radius(symbols=[...])` — transitive symbols and files.
   Start with the default depth; raise it only when the question demands it.
4. `find_related_tests(symbol=... | files=[...])` — what to run.
5. Read the callers you intend to break. Source decides the final plan.

**Subsystem refactor:**

1. `context_for_task` with the refactor described as a task.
2. `trace_dependencies(symbol=..., direction=upstream|downstream|both)`
   and/or `architecture_overview` for structure and hot spots.
3. Targeted source reads of the boundaries you will move.

**Deletion:**

1. `find_callers` — expected zero or migratable.
2. `get_impact_radius` — nothing transitive left behind.
3. `find_related_tests` — tests to delete or update.
4. Source/text search for the name before deleting: dynamic references,
   reflection, code generation, and config strings do not appear in the
   graph. An empty graph result alone never justifies deletion.

## Reading traversal results

- Impact and trace pages report the full traversal size alongside the page
  and set `truncated` when bounded. Continue via `offset` deliberately;
  do not raise limits blindly.
- Unresolved edges make a traversal a **lower bound**, not a complete set.
  Do not claim "nothing else is affected" from a truncated or gap-prone
  traversal — see `trust.md`.
- Cross-check relationship views before concluding a change is safe:
  `find_callers` and `get_impact_radius` weigh evidence differently, so
  either can be empty while the other has results. When they disagree,
  trust the larger answer and verify in source.
- For a rename, a text search over the old name is the cheap final check:
  interface satisfaction, embeddings, and same-name declarations are
  relationships no call edge models.
- Related-test links are heuristic evidence. An empty result means no link
  was found, not that no test covers the code; check the obvious test files
  for the package/module too.

## CLI equivalents

```
codegraph find_callers . <symbol>
codegraph get_impact_radius . --symbol <name> [--depth N]
git diff --name-only | codegraph find_related_tests --stdin
```

Dependency tracing and architecture analysis are MCP tools; without MCP,
approximate with callers/impact plus source reading.

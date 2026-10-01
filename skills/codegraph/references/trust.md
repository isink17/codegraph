# Trust and verification

Read this when a result is empty, ambiguous, or conflicts with what the
source suggests; when the index may be stale; or during a high-impact
refactor or review.

## Kinds of answers — treat them differently

- **Missing target** — an unknown name is not an error. `find_callers`,
  `find_callees`, `find_related_tests`, and `trace_dependencies` succeed with
  an empty result and `target_found: false`; `get_impact_radius` lists the
  name under `seed_presence.missing`; `find_symbol` and `search_symbols`
  return `matched: false`. Check the spelling and the index (`graph_stats`)
  before concluding anything. `find_callers` on an unknown name may still
  return `unresolved_hints`: unresolved call sites that spell the name, not
  callers of an indexed symbol.
- **Empty result with `target_found: true`** — a valid answer, but for
  relationship queries it can also mean the construct is not modeled:
  dynamic dispatch, code generation, macros, or a language with weaker
  extraction. Absence of graph evidence is not evidence of absence.
- **Ambiguous name** — callers, callees, impact, related tests, and trace
  fail closed with `symbol is ambiguous` when multiple definitions match the
  selected lookup tier. Exact qualified names take precedence over name
  fallbacks; an exact `symbol_id` remains authoritative. Resolve the name with
  `find_symbol`, then query by `qualified_name` or `symbol_id`. Never choose a
  same-name candidate without source evidence.

## Fail closed

If the graph returns an unresolved target, an ambiguous result, or a
relationship set that looks suspiciously incomplete: verify in source or
with local text search. Do not invent a graph connection, and do not let a
plausible-looking candidate stand in for a verified one.

## Freshness

- `graph_stats` reports the last index time and pending dirty files; zero
  indexed files means the repository was never indexed.
- After editing source, run `update_graph` (MCP) or `codegraph update .`
  before relying on callers/impact answers. `codegraph watch .` keeps the
  index fresh automatically.
- `context_for_task` refuses a continuation cursor if the graph was
  re-indexed in between — start again without the cursor.
- Read-only questions against an unchanged tree need no update, and a full
  reindex before every query is waste.

## Audit

`audit` (MCP) / `codegraph audit .` produces a read-only integrity and
trust report over the indexed graph: unresolved references, suspicious
links, consistency findings. Run it when graph answers seem inconsistent
with the code, or before trusting the graph for a high-stakes refactor.
`examples=0` gives counts only.

## Cross-language links

`index` and `update` maintain cross-language links automatically as a
derived set; `cross_language_links` re-derives the same set on demand and
writes to the graph. Links are conservative and low-confidence: they need
an import bridge between the two files, never a call site. Treat every
cross-language edge as a hint to verify in source, never as a confirmed
relationship.

## Graph vs source authority

The graph is a ranked map of where to look; the source is what is true.
Prefer graph queries for semantic relationships, narrowing, and impact.
Prefer local text/source search when a literal string or syntax matters,
when a construct seems under-modeled, or when a graph fact is unresolved
or ambiguous. When they disagree, the source wins — and an `audit` plus
`update_graph` usually explains why.

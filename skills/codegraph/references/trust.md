# Trust and verification

Read this when a result is empty, ambiguous, or conflicts with what the
source suggests; when the index may be stale; or during a high-impact
refactor or review.

## Kinds of answers — treat them differently

- **Error ("symbol not found")** — the entity is not in the index. Check
  the spelling and the index (`graph_stats`) before concluding anything.
- **Empty result** — a valid answer, but for relationship queries it can
  also mean the construct is not modeled: dynamic dispatch, code
  generation, macros, or a language with weaker extraction. Absence of
  graph evidence is not evidence of absence.
- **Empty for an unknown symbol** — some relationship tools return an empty
  list for a symbol that does not exist at all, so "no callers" and "no
  such symbol" can look identical. When it matters, confirm the symbol
  exists with `find_symbol` before interpreting an empty relationship.
- **Ambiguous name** — several definitions can share a bare name, and a
  relationship query answers for one of them. Disambiguate with
  `find_symbol`, then query by `qualified_name` or `symbol_id`. Never
  choose a same-name candidate without source evidence.

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

Cross-language links are created only by explicitly calling
`cross_language_links` — indexing does not create them automatically. It
writes conservative, low-confidence links from import-bridge evidence, and
it mutates the graph. Treat every cross-language edge as a hint to verify
in source, never as a confirmed relationship.

## Graph vs source authority

The graph is a ranked map of where to look; the source is what is true.
Prefer graph queries for semantic relationships, narrowing, and impact.
Prefer local text/source search when a literal string or syntax matters,
when a construct seems under-modeled, or when a graph fact is unresolved
or ambiguous. When they disagree, the source wins — and an `audit` plus
`update_graph` usually explains why.

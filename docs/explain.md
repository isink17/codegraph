# Explaining edges

`codegraph explain` and the MCP tool `explain_edge` say why an edge of the
indexed graph is resolved or unresolved. Both are read-only and return the same
JSON document (the MCP tool returns it as `data`).

`explain_edge` is not listed in `tools/list` in either tool mode, so it costs a
default session nothing. Call it by name, or find it with `tool_search` and
call it through `tool_call` in `gateway` mode.

## Scope

The answer describes the **current graph state** only. Nothing about how an
edge was decided is recorded at resolve time, so the explanation is
reconstructed from what the graph holds now. It promises nothing about an
earlier index or update, and a re-index can change it.

## Selecting edges

Give one of:

- `edge_id` (CLI `--edge-id N`): one edge.
- `file` and `line` (CLI `--file F --line N`): every edge recorded on that
  line of that repository-relative file, optionally narrowed by `name`
  (CLI `--name NAME`), the exact destination spelling.

`limit` (default 20, max 500) and `offset` page a multi-edge selection. Edges
are ordered by file, line, destination name, kind and edge id. A selector that
matches no edge is an error; so is a selector with both forms or neither.

## Output

```json
{
  "scope": "current_graph_state",
  "not_evaluated": ["language_gate", "caller_kind_candidate", "bare_type_scope", "...", "own_module_import"],
  "partially_evaluated": ["broad_ambiguity"],
  "total": 1, "limit": 20, "offset": 0,
  "edges": [{
    "edge_id": 3,
    "source": {"symbol_id": 2, "name": "main", "qualified_name": "main.main"},
    "dst_name": "store.Missing", "kind": "calls",
    "file": "cmd/main.go", "line": 13, "language": "go",
    "status": "unresolved",
    "explanation": "refusing_rules",
    "refusing_rules": [{"id": "go_local_qualifier", "stage": "ownership", "disposition": "owned"}]
  }]
}
```

Per edge, `status` is `resolved` or `unresolved`, and `explanation` is one of:

- `persisted_resolution`: the edge is bound. `target`, `resolution_strategy`
  and `resolution_confidence` are the values the resolver stored. No other
  reason is given.
- `refusing_rules`: the edge is unresolved and every listed rule of the
  resolver's rule inventory provably refuses it from the generic resolution
  strategies. All such rules are listed, in inventory order; the list is not a
  causal story and does not say which rule "won".
- `unknown`: the edge is unresolved and no evaluated rule refuses it. This is
  not a proof that nothing did: the edge may have no candidate at all, a
  language pass may own it without proving anything, or a rule that was not
  evaluated may refuse it.

`stage` places a rule relative to candidate selection (`population`,
`ownership`, `chosen_candidate`, `broad_ambiguity`, `own_module`) and
`disposition` says what its refusal means (`ineligible`: no candidate this
caller may bind; `owned`: another pass decides the edge, or nothing does;
`ambiguous`: several equally valid candidates).

`not_evaluated` lists the rules never consulted: the chosen-candidate
restrictions (they judge a candidate the resolver would choose, and explaining
does not choose), `own_module_import` (its veto is computed by the pass that
also binds), and the rules that have no read-only Go counterpart.
`partially_evaluated` lists rules consulted for part of their domain only:
`broad_ambiguity` is evaluated for dotted spellings whose qualified name matched
nothing. Cross-language references are decided by their own pass from
import-bridge evidence, so no inventory rule is attributed to one: an
unresolved one is always `unknown`.

All reads of one call, the edges and every fact the rules are judged on, come
from a single read-only transaction.

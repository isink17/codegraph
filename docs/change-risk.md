# Change risk (`codegraph diff`)

`codegraph diff <base> <head>` and the MCP `diff` tool report `diff.risk`, an
evidence-backed change-risk assessment computed from the same semantic diff the
rest of the document shows. CLI and MCP share one implementation, so both
return the same `risk` for the same two commits.

The risk always covers the whole change set: `--limit` and `--offset` page the
change lists but never change `risk`.

## What it is and is not

- The score is an **ordinal evidence index** (`score_kind:
  evidence_index_not_probability`), not a calibrated failure probability.
- Every point comes from a cited dependent relationship in the two compared
  graphs. File churn, symbol counts, Git history and unresolved name matches
  never add points.
- No assessment proves a change safe. Only resolved edges are cited, so an
  unresolved, dynamic, reflective or cross-language dependent can still break.
  `risk.coverage` is therefore never `complete`.

## Assessments

| `assessment` | Meaning | `score` | `level` |
|---|---|---|---|
| `risk_evidence` | At least one rule cites a dependent relationship. | 1..100 | `low`, `medium` or `high` |
| `limited_evidence` | No rule fired, but `uncertainty` lists what the evidence cannot rule out. | 0 | `none` |
| `no_risk_evidence` | No rule fired and nothing in the change is uncertain. Still not proof of safety. | 0 | `none` |
| `insufficient_evidence` | Declarations or edges were not compared (indexing policy, schema, parser or resolver semantics differ, a pending transition, dirty or failed files). | `null` | `unknown` |

Levels: `low` from 1, `medium` from 25, `high` from 50.

## Rules (model `codegraph.change-risk/v1`)

A dependent relationship is one (dependent declaration, depended-on declaration)
pair taken from the resolved base edges, where the dependent still exists in
head. Several call sites between the same pair are one relationship, and each
relationship is cited by at most one rule.

A relationship is judged by what the same dependent references **by the same
name** in head (the last segment of the reference: `p.T.M`, `obj.M`, `ns::M`
are all `M`), not by the call site alone. A call site is a line and column, and
an edit that shifts lines can land an unrelated call on it; such coincidences
appear in `edges.retargeted` but are not cited as risk. A head reference that
resolves to another declaration the dependent already used in base belongs to
that other relationship.

| Rule | `change` | Cites | Points each | Cap |
|---|---|---|---|---|
| `removed_declaration_dependent` | `declaration_removed` | The declaration was removed and the dependent still references its name without resolving it. | 20 | 60 |
| `dependent_resolution_lost` | `resolution_lost` | The declaration still exists, but the dependent's references by that name no longer resolve. | 10 | 30 |
| `dependent_retargeted` | `binding_changed` | The dependent's references by that name now resolve to a different declaration and none to the original (or to the declaration `declaration_pairs` proves is the same). | 5 | 20 |
| `changed_declaration_dependent` | `declaration_changed` | A declaration kept its identity but changed visibility, staticness or arity while other declarations still resolve to it in head. | 5 | 20 |

A dependent that no longer references the name at all (its caller was updated)
is not cited. A removed declaration without any resolved base edge from a
surviving declaration is listed under `uncertainty`.

`score` is the sum of component points, clamped to 100. Each component lists
`points_each`, `max_points`, `count`, `points` and its `evidence` rows:
`change`, `declaration` and `dependent` (declaration identities as in the symbol
sections), `file` (the dependent's file), `sites` (base edge identities as in
the edge sections) and, where head references exist, `head_sites`,
`head_states` and `head_targets`.

`removed_declaration_dependents` keeps listing every resolved base edge into a
removed declaration with the head edge at the same site; it is the raw evidence
and is not scored.

## Uncertainty

`uncertainty` rows add no points; any row turns a zero score into
`limited_evidence`.

| `reason` | Meaning |
|---|---|
| `removed_declaration_without_resolved_dependents` | A removed declaration had no resolved base edge from a declaration that survives. Zero resolved callers is not evidence of zero callers. |
| `dependent_site_ambiguous` | Several head edges share the base edge's site. |
| `declaration_ambiguous`, `edge_ambiguous` | The diff could not match identities one to one. |
| `symbols_coverage_partial`, `edges_coverage_partial` | Some files were not compared, call capability is limited, or call columns are missing. |
| `changed_file_without_semantic_coverage` | A changed file has no recognized language, so nothing in it is in the graph. |
| `changed_file_jvm_scope_unknown` | A changed Java, Kotlin or Scala file while the JVM compilation scope is unknown. |
| `comparison_limitation` | Any other limitation from `comparison.limitations`. |

## CI gate

The gate is opt-in and runs in the local binary; it needs no hosted service.

```bash
codegraph diff origin/v2.0 HEAD --fail-on-risk medium
codegraph diff origin/v2.0 HEAD --fail-on-risk 20 --insufficient-evidence pass
```

- `--fail-on-risk VALUE`: a score `1..100`, or `low` (1), `medium` (25),
  `high` (50). The command fails when `score >= VALUE`.
- `--insufficient-evidence fail|pass`: what an `insufficient_evidence`
  assessment does; `fail` by default. Only valid with `--fail-on-risk`.

The full JSON document is printed first and is byte-identical to the ungated
output; one summary line goes to stderr, for example
`diff risk: fail, assessment risk_evidence, score 20 (low), threshold 1, 0 uncertainty rows, comparison limited`.

| Exit | With `--fail-on-risk` |
|---|---|
| 0 | Score below the threshold, or insufficient evidence with `--insufficient-evidence pass`. |
| 1 | Score at or above the threshold. |
| 2 | Invalid input (unknown, repeated or out-of-range flag, wrong arguments) or a failed comparison; no document is printed. |
| 3 | Insufficient evidence with the default `--insufficient-evidence fail`. |

Without `--fail-on-risk`, risk never changes the exit code: 0 on success, 1 on
any error, as before. `--insufficient-evidence` without `--fail-on-risk` is an
error.

The gate decides on `score` only. `limited_evidence` passes when its score is
below the threshold; read `uncertainty` (its count is in the summary line) to
decide whether that is enough for your pipeline.

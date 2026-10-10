# Edit safety (`safe_to_edit`)

`safe_to_edit` assesses whether editing one exact symbol or one file is safe
under explicitly named static-analysis rules over the indexed graph. It is
read-only and fail-closed: when the graph cannot vouch for an answer, the
status is `unknown` and there is no verdict.

A verdict is never a guarantee that an edit cannot break runtime behavior. It
holds only within the named rule and the evidence the graph records.

```sh
codegraph index . && codegraph safe_to_edit . --symbol infra.B
codegraph safe-to-edit . --file internal/infra/b.go
```

| Surface | Name | Notes |
|---|---|---|
| CLI | `safe_to_edit` | canonical name |
| CLI | `safe-to-edit` | alias; same output and exit code |
| MCP | `safe_to_edit` | listed in `full` mode; in `gateway` mode, find it with `tool_search` and run it with `tool_call` |

## Input

Exactly one target:

| CLI | MCP | Meaning |
|---|---|---|
| `--symbol NAME` | `symbol` | exact qualified name as stored (`find_symbol` shows it). It is never matched by short name, suffix or fuzzy search. If more than one symbol has the name, the call fails and lists their ids |
| `--symbol-id ID` | `symbol_id` | exact symbol id |
| `--file FILE` | `file` | repository-relative path of an indexed file. Directories are not supported |

The CLI also takes the repository as `[PATH]` or `--repo-root`; the MCP tool
always assesses the server's repository.

## Errors (no result)

Invalid input (no target, more than one, a negative id, a path outside the
repository), a target that does not exist, an ambiguous qualified name and an
unindexed repository are errors: the CLI prints the message and exits 2; the
MCP tool returns a tool error. Nothing is assessed and nothing is created.

## Result: `codegraph.safe_to_edit/v1`

| Field | Meaning |
|---|---|
| `schema` | `codegraph.safe_to_edit/v1` |
| `status` | `assessed` or `unknown` |
| `verdict` | `SAFE`, `CAUTION` or `UNSAFE` when assessed; `null` when unknown. Unknown never collapses into SAFE |
| `reason` | the deciding rule id when assessed; otherwise why the status is unknown (below) |
| `target` | `kind` (`symbol` or `file`), `file`, and `symbols` (id, qualified name, kind, language, file, start line, stable key). A file target lists every symbol it declares |
| `rules` | every rule with `id`, `outcome` (`fired`, `not_fired`, `not_evaluated`, `unknown`) and `detail` |
| `evidence` | `dependents_total`, `dependents_from_tests`, `dependents` (at most 50; `dependents_truncated`), `unresolved_candidates`, `constraint_status`, `constraint_violations`, `constraint_violation_rules` |
| `freshness` | the strict freshness verdict (`verdict`, `reasons`, `state`, `state_reasons`, `head_now`, `last_full_scan`), as `check_constraints --strict-freshness` computes it |
| `coverage` | `sufficient`, `required_languages` (the target's language) and the parser `limitations` of those languages |
| `limitations` | what the assessment cannot see, always including the fixed limits below |

A dependent is a resolved edge (any edge kind) into a target symbol from
outside the target: from another symbol for a symbol target, from another file
for a file target. An unresolved candidate is an edge with no resolved target
whose recorded name equals a target symbol's name or qualified name or ends in
`.` plus its name.

## Evaluation order

1. **Freshness** (`ES-FRESHNESS`). Unless the verdict is
   `full_coverage_at_head`, the status is unknown: `known_stale`,
   `insufficient_coverage` or `freshness_unknown`.
2. **Parser semantics and resolver policy.** A graph that needs a parser
   semantic upgrade is unknown: `parser_semantic_upgrade_required`. A graph
   whose edges for some indexed language were decided by an older resolver
   policy (the next `update` re-decides them) is unknown:
   `resolver_policy_redecide_required`; a policy marker this binary cannot
   honour counts the same.
3. **Coverage** (`ES-COVERAGE`). The target's language must be call-capable
   through one complete parser profile (no symbols-only, mixed, unknown or
   approximate parser). Otherwise unknown: `insufficient_coverage`.
4. **Verdict rules**, on an adequate graph only:

| Rule | Fires when | Result |
|---|---|---|
| `ES-CONSTRAINT-VIOLATION` | a `check_constraints` finding or cycle hop of the repo-root constraints document has a target symbol (or, for a file, the file) as its source or target | `UNSAFE` |
| (indeterminate constraints) | the constraints document is invalid, or its cycles were truncated | unknown: `constraints_indeterminate` |
| `ES-UNTESTED-DEPENDENTS` | there are dependents and none is in a test file | `CAUTION` |
| `ES-KNOWN-DEPENDENTS` | there are dependents | `CAUTION` |
| `ES-UNRESOLVED-CANDIDATES` | an unresolved edge may reach the target | evidence only |
| `ES-ISOLATION` | never: no isolation rule is implemented | — |

Everything else is unknown: `isolation_unproven`. No test link is not proof
that no test exists, and no dependent is not proof that nothing depends on the
target.

Evidence is reported whatever the status, so a stale graph still shows the
dependents and violations it found, with `verdict: null`.

## SAFE in this version

`SAFE` is part of the contract and of the exit codes, but no rule produces it.
Proving isolation would need evidence the graph does not record: no parser
claims complete call resolution, calls in package-level initializers are not
recorded for every language, and non-call uses (function values, callbacks,
interface and dynamic dispatch, reflection, linker directives, generated or
build-time wiring) are not edges. A symbol used only as a function value
therefore has no dependents and is `unknown`, not `SAFE`.

## Fixed limitations

- static graph evidence only: dynamic dispatch, reflection, function values,
  callbacks, code generation and build-time wiring are not dependents;
- no isolation rule, so SAFE is never produced;
- freshness is proven against the last full scan and HEAD; uncommitted edits
  and unqueued watcher events are never checked;
- dependents from another language are trusted only as far as that language's
  coverage; such languages' limitations are listed.

## Exit codes (CLI)

| Code | Meaning |
|---|---|
| 0 | assessed `SAFE` |
| 1 | assessed `CAUTION` or `UNSAFE` |
| 2 | invalid input, missing or ambiguous target, not indexed, setup error |
| 3 | unknown: known stale |
| 4 | unknown: any other reason |
| 5 | unknown: insufficient coverage (freshness or parser) |

The exit code is not the verdict; read `verdict` in the JSON. The MCP tool
returns the same document as `data` and has no exit code.

# Resolver correctness baseline (P1)

Synthetic adversarial baseline for CodeGraph edge resolution. This is our own
fixture, not a reproduction of anyone else's corpus or methodology, so the
numbers below are comparable only against later runs of the same fixture
version.

Purpose: measure the current resolver's false-edge behaviour before changing it.
No resolver semantics were changed to produce this measurement.

## Provenance

- Commit measured: `9177cefd6975bc737a92b2b61207469007315b5e`
  (working tree additionally contained the untracked `internal/audit` package
  and a `.gitignore` change; no product code was modified)
- Fixture version: `p1-adversarial-v1` — 6 cases, 15 files, languages
  go, java, kotlin, php, python, rust, swift
- Environment: go1.26.1, darwin/arm64, 11 CPUs, sqlite driver `sqlite`
  (`CGO_ENABLED` default, but the harness pins its own adapter set)
- Adapters pinned by the harness: go (AST), python (regex), and the heuristic
  adapters for java, kotlin, rust, swift, php, csharp, ruby, typescript, cpp.
  The harness does not use the CLI's default registry because that registry
  swaps every adapter on the `cgo` build tag, which would change the numbers.
- Fixture declares **no FFI and no bindings**, so every cross-language edge it
  produces is a name coincidence.

## Command

```bash
go run ./internal/audit/cmd/resolveraudit -repeats 5 -incremental
go run ./internal/audit/cmd/resolveraudit -repeats 5 -incremental -json out.json
```

## Observed totals

| counter | value |
|---|---|
| call edges inspected | 6 |
| resolved | 5 |
| unresolved | 1 |
| cross-language resolved | 3 |
| same-language resolved | 2 |
| unknown-language resolved | 0 |
| cases | 6 |
| expected-valid | 2 |
| expected-unresolved | 1 |
| correctly unresolved | 0 |
| miswired (unanimous across repeats) | 3 |
| miswired (worst case, any repeat) | 3–4 depending on sample |
| missing edge | 0 |
| test-shadow bindings (any repeat) | 0–1 depending on sample |
| cases with >1 candidate definition | 2 |

Edge-level counters (inspected / resolved / unresolved / cross-language /
same-language / unknown-language) and the ambiguity counter were identical in
every run observed. The case-classification counters move only because of the
nondeterminism recorded below: the harness classifies a case only when every
repeat agreed, and reports the worst case separately, so an intermittent miswire
cannot be rounded away.

## Per-case outcome

Sample below: 20 independent single-repeat invocations of the final harness,
fresh fixture tree and fresh database each time.

| case | scenario | expectation | observed | selected destination |
|---|---|---|---|---|
| A | python calls `sorted()`; only definition is Swift `func sorted` | invalid | **miswired** | `SortKitA.sorted` (swift), 20/20 |
| B | python calls a python function (control) | valid | **expected_valid** | `helpers_b.normalize_b` (python), 20/20 |
| C | `serialize_c` defined in java + kotlin + rust + php, called from python | invalid | **miswired** | java 18/20, kotlin 1/20, php 1/20 |
| D | Go emits `Sorter.parse_d`; only suffix match is a Java method | invalid | **miswired** | `ShapesD.Sorter.parse_d` (java), 20/20 |
| E | production python call with both a production and a test definition | valid | **expected_valid** in this sample | `config_e.load_config_e` 20/20 here; an earlier 20-run sample on the same machine selected `test_config_e.load_config_e` once, and a separate reviewer sample saw it 3 times in 20 |
| F | call to a name defined nowhere (honest negative) | unresolved | **expected_unresolved** | stays unresolved, 20/20 |

The Python/Swift-style miswire (case A) reproduces deterministically: a Python
builtin call binds to an unrelated Swift function purely because the names
match. Case D shows the same defect via the suffix strategies rather than exact
name matching, so it is not confined to one code path.

Case E resolves to the production definition in most runs, but that outcome is
an accident of symbol insertion order, not a rule: resolution has no notion of a
test, mock or fixture definition, and the test definition has been observed
winning. The harness therefore reports case E's competing test definition as a
shadow candidate even in runs where production won.

## Determinism

Selection is **not** deterministic on the current resolver.

- Over 20 independent single-repeat invocations: case C selected java 18,
  kotlin 1, php 1. Cases A, B, D and F were identical in all 20.
- Case E selected production in all 20 runs of that sample, but an earlier
  20-run sample on the same machine selected the test definition once, and an
  independent reviewer sample saw it 3 times in 20. Both cases C and E have also
  flipped inside a single 5-repeat invocation.
- The flip *rate* is not characterised. It differed between samples on the same
  machine, so the counts above are evidence that selection is unstable, not an
  estimate of how often.
- Cause: indexing runs on a worker pool and consumes results in completion
  order, so `symbols.id` values differ between runs over the same tree. The
  resolver then breaks same-name ties with `MIN(id)` (suffix strategies) or an
  unordered `LIMIT 1` (exact-name and method strategies), with no ambiguity
  check. The winner therefore follows insertion order.
- Consequence for this baseline: the harness reports a per-case destination only
  when every repeat agreed, and otherwise reports the sorted set of observed
  variants. Its `StableProjection` (fixture, edge counts, candidate ambiguity per
  case) is derived from persisted symbols rather than from a tie-break and was
  byte-identical across invocations; that projection is what later runs should be
  compared against.

## Full-index vs incremental parity

The two paths disagree. With `-incremental` the harness indexes a second,
equivalent fixture tree, then appends a comment to one caller and runs an update:

- full index → `ResolveMode=repo`, case C resolves (to one of the four foreign
  definitions)
- comment-only touch plus update → `ResolveMode=paths+names`, case C stays
  **unresolved**

The incremental path requires a unique name match, so an ambiguous name is
skipped; the full path picks an arbitrary candidate. A full index and an update
of an equivalent tree therefore produce different resolution for the same
ambiguous name. The comparison is between two separate runs, not a before/after
snapshot of one database, and it compares the per-case destination only.

## Known limitations

- Six hand-built cases, not a corpus. The counters describe this fixture only; a
  false-edge *rate* over real repositories is not measured here.
- The heuristic adapters used for java, kotlin, rust, swift and php emit
  definitions but no call edges, so every caller in the fixture is Python or Go.
  Miswires originating in the other direction are not covered.
- The harness pins its own adapter set. Under a `cgo` build the product CLI uses
  tree-sitter adapters instead, which extract different symbols; numbers from
  this fixture do not transfer to that configuration.
- Case D depends on the heuristic Java adapter producing a `module.Class.method`
  qualified name whose `dot_tail2` collides with the Go call name. An adapter
  change could turn this case into a no-op; `TestFixtureProducesExpectedDefinitions`
  fails if any expected definition stops being indexed.
- Nondeterminism means single-run numbers are a sample. Case C and case E flipped
  at roughly 1 in 20 in this session; a different machine or CPU count may show a
  different split.
- The harness observes only `calls` edges and reads the graph through the
  existing export APIs. It adds no production hooks and does not exercise
  `cross_language_ref` edges, which are created by a separate opt-in path.
- The harness models the resolver's candidate set by mirroring the store's
  `qualifiedSuffix`/`dotTail2`/`dotTail3` helpers plus a suffix match. If those
  helpers change in the store, the mirror must follow or ambiguity counts drift.
  The harness flags a divergence if the resolver ever selects a destination the
  mirror did not predict.
- `no_call_edge` currently means "no call edge row was persisted". The tests
  assume edge rows are always persisted and only `dst_symbol_id` varies. If P2
  chooses to stop persisting unresolvable call edges, that assumption and the
  `CallEdgesInspected == len(Cases())` assertion need revisiting; it would be a
  deliberate change, not a regression.

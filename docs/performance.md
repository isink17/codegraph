# Performance baseline and release floors

This page records the v2.0 performance baseline: cold index, no-op update,
single-file-edit update and graph-query latency on two pinned corpora, measured
with the real CLI on one machine. It sets regression floors derived from those
measurements. The floors are **local macOS arm64 floors for this machine
class**, not cross-platform guarantees and not CI budgets (no CI enforcement
exists). Methodology rules from
[benchmark-methodology.md](benchmark-methodology.md) apply.

Raw samples: [benchmarks/v2.0.0-baseline/](benchmarks/v2.0.0-baseline/)
(`samples.tsv` per run, `summary.tsv`, `queries.json` from `bench-queries`).

## Environment

| Item | Value |
|---|---|
| Machine | MacBook Pro `Mac15,6`, Apple M3 Pro, 11 cores (5 performance + 6 efficiency), 18 GiB RAM, on AC power |
| OS | macOS 27.0.1 (26A434) |
| Go | go1.26.1 darwin/arm64 |
| CodeGraph | `v2.0.0-dev+83828b8`: `v2.0` at `83828b8` plus the `bench-queries` target-selection fix committed with this page (the fix touches no index or update path) |
| Build | release shape from `.github/scripts/build-release-asset.sh`: `CGO_ENABLED=1 go build -ldflags "-X github.com/isink17/codegraph/internal/version.Version=<v>" ./cmd/codegraph`, no `-trimpath` |
| SQLite | `modernc.org/sqlite` v1.39.1 (pure Go, the only driver; CGO only enables the tree-sitter parsers) |
| Settings | default config, default DB performance profile, Git history enrichment on (default), no embeddings, `CODEGRAPH_HOME` isolated, `CODEGRAPH_NO_UPDATE_CHECK=1` |
| Load | interactive desktop, not idle: load average 2.5 to 3.5 throughout (IDE, media player, browser) |

## Corpora

| Corpus | Pin | Files indexed | Symbols | References | Edges | DB size |
|---|---|---|---|---|---|---|
| small: fmtlib/fmt | tag `11.1.4`, commit `123913715afeb8a437e6388b4473fcc4753e1c9a`, `git clone --depth 1` | 122 (70 C++, 4 Python, 1 TypeScript, 47 other) | 5,660 | 18,010 | 16,348 | 15.3 MiB |
| medium: CodeGraph | `git archive 83828b8` committed as a single-commit repo (tree `1e4937a8…`: the `83828b8` tree minus `ANALYSIS.md` and `npm/bin/codegraph.js`, which its own `.gitignore` excludes; both stay on disk) | 797 (613 Go, 93 Python, 9 C++, 4 Kotlin, 4 TypeScript, 74 other) | 8,186 | 200,176 | 76,443 | 67.0 MiB |

## Method

`scripts/release-bench/run.sh BIN CORPUS OUT COLD_RUNS UPDATE_RUNS EDIT_FILE...`
copies the corpus to a disposable temporary directory, runs every scenario
there as a separate CLI process under `/usr/bin/time -l`, and records wall time
and peak RSS. Edit targets must be regular files inside that copy; absolute,
traversal, and symlinked paths are rejected. The original repository and index
are left untouched:

- `cold_index`: delete the copy's `.codegraph`, then `codegraph index`. 5 runs.
- `noop_update`: `codegraph update` with nothing changed. 10 runs.
- `edit_update_<ext>_<file>`: append one newline to one file, then
  `codegraph update`; repeated 10 times per file in the disposable copy.
  Files: fmt `src/os.cc`, `support/printable.py`; CodeGraph
  `internal/query/context_budget.go`, `scripts/cg33-oracle/oracle.py`.
- queries: `codegraph bench-queries CORPUS --runs 20 --warmup 3` on the final
  index. Targets come from the graph (highest in- and out-degree symbols with a
  unique qualified name, most test-linked file), so they are stable across
  re-indexes.

Statistics are nearest-rank (every value reported is an observed sample, as in
`internal/latency`). With 5 cold runs, p95 equals the max. Process wall time has
10 ms resolution (`/usr/bin/time`); query latency is in-process, microsecond
resolution.

Reproduce (needs network once for the fmt clone):

```sh
W=$(mktemp -d)
git clone --depth 1 --branch 11.1.4 https://github.com/fmtlib/fmt.git "$W/fmt"
mkdir "$W/codegraph" && git archive 83828b8 | tar -x -C "$W/codegraph"
(cd "$W/codegraph" && git init -q && git add -A && git commit -qm snapshot)
CGO_ENABLED=1 go build -ldflags "-X github.com/isink17/codegraph/internal/version.Version=bench" -o "$W/codegraph-bin" ./cmd/codegraph
sh scripts/release-bench/run.sh "$W/codegraph-bin" "$W/fmt" "$W/out-fmt" 5 10 src/os.cc support/printable.py
sh scripts/release-bench/run.sh "$W/codegraph-bin" "$W/codegraph" "$W/out-codegraph" 5 10 internal/query/context_budget.go scripts/cg33-oracle/oracle.py
```

The summary arithmetic has a self-check: `sh scripts/release-bench/run_test.sh`.

## Results (2026-10-17)

Index and update, wall time per CLI process:

| Corpus | Scenario | n | p50 | p95 | max | peak RSS |
|---|---|---|---|---|---|---|
| fmt | cold index | 5 | 13.46 s | 13.62 s | 13.62 s | 239 MiB |
| fmt | no-op update | 10 | 0.34 s | 0.40 s | 0.40 s | 54 MiB |
| fmt | edit update, C++ | 10 | 5.56 s | 5.65 s | 5.65 s | 68 MiB |
| fmt | edit update, Python | 10 | 5.32 s | 5.58 s | 5.58 s | 66 MiB |
| CodeGraph | cold index | 5 | 101.08 s | 101.81 s | 101.81 s | 1018 MiB |
| CodeGraph | no-op update | 10 | 0.28 s | 0.33 s | 0.33 s | 83 MiB |
| CodeGraph | edit update, Go | 10 | 45.62 s | 45.98 s | 45.98 s | 138 MiB |
| CodeGraph | edit update, Python | 10 | 43.70 s | 46.23 s | 46.23 s | 137 MiB |

Phase split from the scan summary (one representative run; ms):

| Corpus | Scenario | parse | write | resolve | history | scan total |
|---|---|---|---|---|---|---|
| fmt | cold index | 3,187 | 1,020 | 7,571 | 324 | 9,141 |
| fmt | edit update, C++ | 10 | 13 | 5,166 | 289 | 5,481 |
| CodeGraph | cold index | 12,567 | 6,559 | 45,496 | 2,163 | 58,467 |
| CodeGraph | edit update, Go | 3 | 23 | 45,344 | 102 | 45,477 |

Graph queries, `bench-queries` p50 / p95 in ms (20 measured, 3 warmup):

| Scenario | fmt p50 | fmt p95 | CodeGraph p50 | CodeGraph p95 |
|---|---|---|---|---|
| graph_stats | 14.55 | 15.25 | 98.86 | 102.18 |
| list_files | 0.05 | 0.05 | 0.07 | 0.08 |
| symbol_lookup | 1.62 | 1.64 | 2.90 | 2.93 |
| symbol_search | 0.10 | 0.11 | 1.01 | 1.04 |
| find_callers | 1.96 | 2.02 | 8.89 | 9.31 |
| find_callees | 0.39 | 0.39 | 7.73 | 8.24 |
| related_tests | skipped (no test links) | | 2.73 | 2.75 |
| impact_radius | 6.70 | 6.87 | 35.55 | 37.94 |
| trace_dependencies | 0.18 | 0.18 | 2.57 | 2.70 |
| dead_code | 8.90 | 9.00 | 10.54 | 10.68 |
| context_for_task | 4.68 | 5.11 | 30.27 | 31.89 |
| architecture_overview | 33.64 | 33.86 | 191.01 | 201.04 |

## Findings

- A one-file edit costs almost a full resolve: `resolve_mode` is
  `paths+names` and takes as long as the cold-index resolve on CodeGraph
  (45.3 s vs 45.5 s) and 68% of it on fmt. Parse and write for the edited file
  take milliseconds. Incremental resolve is the first profiling target.
- Resolve dominates cold index (78% of scan time on CodeGraph, 83% on fmt).
- Cold index of the medium corpus peaks at about 1 GiB RSS.
- On the medium corpus `graph_stats` and `architecture_overview` exceed the
  50 ms reporting budget of `bench-queries`; every other query stays under
  40 ms p95.

## Release floors

A floor is the slowest result a release candidate may show on this machine
class for the same corpus pin and scenario before it counts as a regression
to explain.

- Wall time (index, update): measured p95 × 1.5, rounded up. Under the recorded
  background load, run-to-run spread (p95 / p50) was at most 6% for scenarios
  over 1 s and up to 18% for sub-second no-op updates, so 1.5×
  leaves room for thermal state and desktop load while still catching a
  regression of half again the baseline.
- Peak RSS: measured max × 1.25, rounded up. RSS varied by up to 13% across
  cold runs.
- Query latency: measured p95 × 2, rounded up to a whole millisecond, at least
  1 ms. Sub-10 ms queries move by large ratios from scheduler noise alone, so
  they need wider relative headroom.

| Corpus | Scenario | Floor |
|---|---|---|
| fmt | cold index | 20.5 s, 300 MiB |
| fmt | no-op update | 0.6 s |
| fmt | edit update (either language) | 8.5 s |
| CodeGraph | cold index | 153 s, 1300 MiB |
| CodeGraph | no-op update | 0.5 s |
| CodeGraph | edit update (either language) | 70 s |

| Query | fmt floor (ms) | CodeGraph floor (ms) |
|---|---|---|
| graph_stats | 31 | 205 |
| list_files | 1 | 1 |
| symbol_lookup | 4 | 6 |
| symbol_search | 1 | 3 |
| find_callers | 5 | 19 |
| find_callees | 1 | 17 |
| related_tests | n/a | 6 |
| impact_radius | 14 | 76 |
| trace_dependencies | 1 | 6 |
| dead_code | 18 | 22 |
| context_for_task | 11 | 64 |
| architecture_overview | 68 | 403 |

To check a candidate, rebuild at its commit, run the two `run.sh` commands
above and compare each p95 (and cold-index max RSS) with its floor. A result
over its floor blocks the release until it is explained or the baseline is
re-measured and this page updated. A result far under its floor after a
deliberate optimization is a reason to re-measure and tighten the floor.

## Limitations

- One machine, one session, desktop load present. Numbers do not transfer to
  Linux, Windows, amd64 or other Apple silicon; re-measure there before
  quoting them.
- Two corpora. No large (>500k LOC) corpus and no 100k-symbol query fixture
  yet.
- 5 cold runs per corpus, so cold p95 is the max. Index and update timings are
  process wall time at 10 ms resolution.
- Only the CGO build was measured. A `CGO_ENABLED=0` build indexes fewer
  languages, so its numbers are not comparable and were not taken.
- Git history enrichment is on: the fmt clone is shallow (1 commit) and the
  CodeGraph snapshot has 1 commit, so history cost on long histories is not
  represented.
- Raw samples are recorded TSV/JSON, not the `codegraph.benchmark_identity/v1`
  artifact; the identity fields are recorded on this page.
- No embeddings, no `watch`, no MCP transport overhead.

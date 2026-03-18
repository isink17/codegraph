# Optimization and Feature TODOs

## P0
- [x] Batch file metadata writes in scans.
  - Problem: `indexer.run` currently performs `MarkFileSeen`/`TouchFileMetadata` one-by-one, causing many small SQLite writes.
  - Target: add store batch APIs (chunked by e.g. 200-500 rows) and use them in `internal/indexer/indexer.go` to reduce write amplification.
- [x] Make watcher queueing errors visible and recoverable.
  - Problem: `watcher.Run` ignores `QueueDirtyFile` errors (`_ = w.store.QueueDirtyFile(...)`), silently dropping updates.
  - Target: count/report queue failures and retry/backoff or fail fast with explicit error path.
- [x] Avoid full unresolved-edge scan on every update.
  - Problem: `ResolveEdges` scans all unresolved edges for repo after each run, even for small path-limited updates.
  - Target: resolve by affected files/symbols first, fall back to global pass only when needed.

## P1
- [x] Precompute extension-to-adapter lookup in parser registry.
  - Problem: `Registry.AdapterFor` is linear over all adapters for each file.
  - Target: keep current interface but add fast-path map by extension to reduce per-file dispatch overhead.
- [x] Replace N+1 impact traversal queries with set-based traversal.
  - Problem: `ImpactRadius` issues repeated per-node queries per depth level.
  - Target: switch to batched edge expansion (`IN (...)`) or recursive CTE to reduce DB round trips.
- [x] Add configurable parser error policy.
  - Problem: a single parse error currently fails scan/update.
  - Target: support `fail_fast` (current) and `best_effort` modes with per-file parse error reporting in scan summary.

## P2
- [x] Consolidate duplicated tokenization logic.
  - Problem: token weight logic is duplicated in Go parser, Python parser, heuristic parser, and store search helpers.
  - Target: centralize into one shared utility package with tests to ensure consistent search behavior.
- [x] Improve heuristic adapters with comment/string stripping before regex parse.
  - Problem: symbols/imports can be falsely detected inside comments or string literals.
  - Target: lightweight preprocessor per language family to reduce false positives.
- [x] Add `deleted/total` scan percentages and timings in summaries.
  - Problem: scan summaries expose counts but limited operator insight for tuning.
  - Target: include useful derived metrics and phase timings (walk, parse, write, resolve).

## Feature Opportunities
- [x] Add first-class CLI query commands (`find-symbol`, `callers`, `callees`, `impact`, `search`) mirroring MCP tools.
- [x] Add MCP tool `supported_languages` with adapter list and file extensions.
- [ ] Expand `graph export` to include edges and symbol metadata in JSON and real edges in DOT output.
- [ ] Support `.gitignore` semantics (including `!` negation) in `.codegraphignore`.
- [ ] Add per-language indexing coverage stats (files parsed vs skipped vs parse_failed).
- [ ] Add optional `--jsonl` streaming output for long-running operations (`index`, `update`, `watch`) for easier client integration.

## Nice to Haves
- [ ] Add `codegraph config` command (`show`, `edit-path`, `validate`) for easier setup.
- [ ] Add `doctor --fix` mode for non-destructive autofixes (create dirs, suggest PATH export command).
- [ ] Add a tiny benchmark command that runs store/query/index micro-benchmarks and prints regression deltas.

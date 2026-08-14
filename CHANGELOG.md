# Changelog

## Unreleased

Progressive disclosure: symbol-shaped MCP results are returned at the smallest useful size by
default, and larger representations are asked for explicitly.

Query latency budget: local graph queries are measured against a deterministic ~100k-symbol
fixture, the measured bottlenecks are fixed, and the measurement is reproducible from the CLI.

### Added

- **`detail` argument** on `find_symbol`, `search_symbols`, `find_callers`, `find_callees`, and
  `get_impact_radius`, with one vocabulary shared by every tool: `card` (identity and location),
  `skeleton` (+ signature, visibility, container, doc summary, member declarations), `excerpt`
  (+ bounded source around the symbol), `full` (+ the symbol's complete indexed source and the
  `range`/`file_id` fields the pre-P13 payload carried). An unknown level is rejected rather than
  silently downgraded.
- **`--detail card|skeleton|excerpt|full`** on `find_symbol`, `search_symbols`, `find_callers`,
  `find_callees`, and `get_impact_radius` on the CLI, with the same per-level semantics as MCP.
- Source rendering for `excerpt` and `full`: files are read from disk on demand, confined to the
  repository root (with symlinks resolved on both sides), CRLF-normalised, clamped to the file
  that actually exists, and reported rather than failed when a file is gone or its indexed range
  is stale. An excerpt longer than its ceiling, or a range clamped to a file that has shrunk, is
  marked `truncated` and reports `available_start_line`/`available_end_line`. Source read from a
  file whose size or mtime no longer matches the index carries a `source_note` instead of being
  silently misattributed.
- Explicit bounds on the expensive levels, all of them reported: a skeleton's member list is
  capped and reports `member_count`; one response looks up members for a bounded number of
  containers and renders source for a bounded number of symbols, the rest carrying a
  `skeleton_note` or `source_note`; and one symbol's source is itself capped. Traversal tools
  have no result limit of their own, so without these a single `get_impact_radius` at
  `detail=full` would read the repository. Every bound sits well above any paged request, so an
  explicit `limit` is always honoured in full.
- `--detail` and the other query flags now take effect after the positional arguments as well as
  before them. Go's flag package stopped at the first positional, so `find_symbol . Foo --exact`
  silently dropped the flag.

- **`codegraph bench-queries [PATH]`** (alias of `bench_queries`) — benchmarks the local graph
  queries against an already-indexed repository and prints a `codegraph.query_bench/v1` JSON
  report with p50/p95/max per scenario. Read-only: it opens the existing database with
  `OpenReadOnly`, never indexes, never migrates, and never writes synthetic rows. Scenario
  arguments are selected deterministically from the repository's own graph (most-called symbol,
  highest fan-out symbol, most-linked test file); a scenario the graph cannot supply a target for
  is reported as `skipped` with a reason rather than replaced by an invented query. Flags:
  `--repo-root`, `--runs`, `--warmup`, `--budget-ms`, `--fail-over-budget` (which changes the exit
  status only, after the report has been written).
- Deterministic ~100k-symbol query-latency fixture and benchmark suite in `internal/store`, plus a
  three-layer MCP benchmark (store / dispatch / dispatch+encode) in `internal/mcp`.

### Changed

- **MCP default response size.** `find_symbol`, `search_symbols`, `find_callers`, `find_callees`,
  and `get_impact_radius` now default to `detail=card`, which is 54-74% smaller than the previous
  payload on this repository. No response field became unreachable: `detail=full` returns
  everything those tools returned before. The CLI default is deliberately unchanged, because its
  JSON is script-facing.
- **`find_callers` / `find_callees`** now dedupe, order and page inside SQLite instead of
  materialising the whole neighbourhood in Go. The result set and its ordering are unchanged.
  This also removes the `IN (?, ?, …)` list that grew with fan-in: the whole caller/callee set
  used to be read into Go and bound back into a second statement, which on a hub meant tens of
  thousands of bound variables — over `SQLITE_MAX_VARIABLE_NUMBER` on drivers still using the
  historical 999-variable limit. The neighbour set now never leaves SQLite.
- **Multi-name symbol lookup** (`FindCallees`' unresolved-destination fallback) resolves its
  four-stage cascade for all names at once. The per-name stage that needs a leading-wildcard
  `LIKE` now costs one scan per call instead of one per name.
- **`architecture_overview`** computes its entry-point and hub-symbol lists by aggregating on the
  edge side rather than grouping every symbol in the repository, and derives its file/symbol/edge
  totals from the breakdowns it already computes instead of a second full count.
- **`find_dead_code`** constrains the file join to the repository, which both restores repo
  isolation on that join and lets the ordering come from indexes.
- **`architecture_overview`'s `caller_count`/`callee_count`** are now scoped to the repository
  being described. The previous `LEFT JOIN edges` had no `repo_id` predicate, so in a database
  holding several repositories the reported degrees summed across all of them.

### Fixed

- **Unstable pagination in symbol search.** `search_symbols`, `find_symbol` and
  `find_symbol --exact` applied `LIMIT`/`OFFSET` with no `ORDER BY`, so which rows a page returned
  depended on storage order and consecutive pages could overlap or skip. They now use the same
  total order the rest of the symbol surface uses (`qualified_name`, `start_line`, `start_col`,
  `id`). This changes which rows a broad query returns, because previously that was arbitrary.
- **Unstable pagination in `search_semantic`.** Token-overlap search ordered by score alone, and
  weights come from a small fixed set, so ties were the rule and page boundaries fell arbitrarily.
  It now breaks ties by file path and qualified name.
- **`find_callees` variable-count limit.** The query that collects a symbol's unresolved
  destination names spliced every source symbol id into one `IN (?, ?, …)` list, which could
  exceed `SQLITE_MAX_VARIABLE_NUMBER` for an ambiguous name in a large repository. It is chunked.

### Performance

Measured on a deterministic 100k-symbol / 212k-edge fixture, p95 of 30 warm runs, same fixture
before and after:

| Query | Before | After |
|---|---|---|
| `find_callees` (300 unresolved destinations) | 8581 ms | 39.1 ms |
| `find_dead_code` (first page) | 128 ms | 0.57 ms |
| `find_callers` (hub, 18,432 distinct callers) | 236 ms | 38.2 ms |
| `architecture_overview` | 406 ms | 132 ms |
| `find_callers` (medium degree) | 42.9 ms | 14.4 ms |
| `find_callers` (low degree) | 38.2 ms | 13.4 ms |
| `search_semantic` | 35.0 ms | 24.4 ms |
| `find_callees` (hub, 4000 destinations) | 21.8 ms | 8.5 ms |

The deterministic-ordering fix costs latency where it buys correctness: `search_symbols` on a term
matching ~9% of a 100k-symbol graph went from 0.6 ms to 14.3 ms, because a page can no longer be
whatever SQLite reached first. It stays inside the 50 ms budget.

Two whole-graph operations remain over budget and are reported that way rather than narrowed:
`architecture_overview` (132 ms) aggregates every symbol and edge by definition, and
`get_impact_radius` at its default depth of 2 around a hot hub (476 ms) returns 34,564 affected
symbols — its cost tracks the size of the answer, and it has no result limit in its public
contract. Bounding either would be an API change, not an optimisation.

The `find_dead_code` figure is for the first page: the index change removes the sort, which is
what lets `LIMIT` stop the scan early, so the win depends on dead symbols appearing early in path
order. A repository whose alphabetically-first files are entirely live still scans further.

Three indexes were replaced by wider ones with the same leading columns and partiality
(migration 022), so the index count — and therefore the per-row write cost — is unchanged:
`idx_edges_repo_unresolved_name` → `idx_edges_repo_unresolved_name_src`, `idx_symbols_file_id` →
`idx_symbols_file_start`, `idx_symbol_tokens_token` → `idx_symbol_tokens_token_symbol`. Full and
incremental indexing are unchanged within noise; the database grows about 1.7%.

## v1.2.0 - 12-05-2026

C++ call graph support, DB rebuild, asset filtering, pagination correctness, and version UX improvements.

### Added

- **`codegraph index . --rebuild`** — drops the repo database before indexing; implies `--force`. Use after parser or indexer changes when stale rows must be cleared. Requires exclusive DB access (stop `codegraph serve` first if locked).
- **`codegraph version`** command and **`codegraph --version`** / **`codegraph -v`** flags — print installed version without any network calls.
- **C++ call graph:** member-pointer (`->`), member-value (`.`), static-qualified (`::`), and direct calls are now extracted as edges with structured evidence, enabling `find_callers` and `find_callees` across legacy C++ codebases.
- **Binary/asset deny-list:** the indexer now hard-skips files by extension before hashing or parsing. Skipped categories: Windows build artifacts (`.exe`, `.dll`, `.lib`, `.pdb`, `.obj`, …), archives (`.7z`, `.zip`, `.rar`), images, fonts, audio, video, game/engine formats (`.anm`, `.dff`, `.bsp`, `.dat`, `.bin`, …), and documents (`.pdf`, `.doc`, `.chm`).
- **Per-extension unknown coverage:** files landing in the `unknown` language bucket now accumulate per-extension sub-counts in `language_coverage.unknown.extensions`.
- **Versioncheck improvements:** overridable HTTP client and URL for testability, raised check timeout from 2 s to 5 s, structured `latestReleaseResult` type, comprehensive test suite.

### Changed

- **C++ qualified names** now use `Container::Name` (double-colon) instead of `Module.Container.Name` (dot). Fixes duplicated container names like `ApmMap.ApmMap::LoadWorldMap`.
- **`find_callers` / `find_callees` pagination** overhauled: all candidate IDs are collected (resolved edges + unresolved `dst_name` fallback), deduplicated, sorted once by `qualified_name / start_line / start_col / id`, then a single `offset + limit` slice is returned. Previously each path was paginated independently, producing incorrect or truncated pages when both paths had results.
- **`FindCallees` fallback lookup** is no longer N+1: distinct `dst_name` values are fetched in one query; symbol resolution is batched across all names rather than calling `lookupSymbolIDs` once per cursor row.
- **`startupVersionCheck`** is now deferred past the version flag / `version` command check, so `codegraph --version` and `codegraph version` never trigger a network round-trip.
- **`clean` command** description changed from "clean index data" to "database maintenance" to reflect its actual scope (VACUUM, FTS optimize, ANALYZE, WAL checkpoint, incremental vacuum).
- **Language coverage update** refactored into `updateLanguageCoverage` helper; unknown files now report per-extension sub-counts.
- **MCP stdio transport** simplified to newline-framed JSON (one JSON object per `\n`). Previous Content-Length header framing removed.
- README updated: build-from-source instructions, `--rebuild` usage, and `version` command documented. `version-check` references removed.

### Fixed

- Fixed `find_callers` returning empty results for valid C++ member-call references (e.g. `AgcmMinimap::F` → `pcsApmMap->LoadWorldMap` → `ApmMap::LoadWorldMap`).
- Fixed `find_callers` and `find_callees` returning wrong or duplicate results across pages when resolved and fallback result sets were independently limited and merged.
- Fixed `splitCallTarget` using a manual `strings.Cut` loop; replaced with `strings.LastIndex` for correctness on paths with multiple separators.
- Fixed C++ stable keys and qualified names including redundant container prefixes.

### Removed

- **`codegraph version-check`** command removed. Use `codegraph --version`, `codegraph -v`, or `codegraph version` for local version output. The background startup version check still runs automatically on normal commands.

### Upgrade Notes

- `--rebuild` replaces any manual "delete the DB and re-index" workflow. It is safe to run at any time on a clean working directory.
- MCP stdio protocol change (newline-framed vs Content-Length) may affect custom MCP client integrations that relied on the old framing.
- C++ unresolved-member fallback is name-based; results are included only when receiver type cannot be resolved to a symbol ID. This is best-effort for legacy codebases without full type resolution.

## v1.1.1 - 28-04-2026

Made `--repo-root` optional across all CLI commands and MCP tools. The server
now auto-detects the repository root so editor configs no longer require
hardcoded paths.

## Changes

### Features

* **Config:** Added `ResolveRepoRoot(flagValue, toolParam string)` in
  `internal/config/reporoot.go` with a 5-step fallback chain: per-call MCP
  tool parameter → CLI flag → `git rev-parse --show-toplevel` → `os.Getwd()`
  → error. Per-call tool parameter takes priority over the process-level flag.
* **CLI:** All commands that previously required `--repo-root` (`serve`,
  `watch`, `visualize`, `index`, `stats`, `query`, `doctor`, `clean`,
  `affected-tests`) now resolve the repo root through the shared helper,
  making the flag optional everywhere.
* **MCP:** `index_repo` and `update_graph` tools accept an optional `repo_root`
  parameter; when omitted, the shared resolver is used.
* **Install:** `codegraph install` no longer writes `--repo-root` into
  generated MCP config snippets for any editor (Claude Code, Cursor, Windsurf,
  Gemini CLI, Codex).

### Fixes

* Fixed macOS symlink path mismatch in `TestResolveRepoRoot_GitRootDetected`
  by normalizing paths with `filepath.EvalSymlinks` before comparison.
* Fixed incorrect resolution priority: per-call `toolParam` now correctly
  takes precedence over process-level `flagValue`.

### Docs

* Added "Repo Root Resolution" section to README.md documenting the fallback
  chain with correct priority order.
* Updated all MCP config snippets in README.md and examples/ to omit
  `--repo-root`.
* Updated `docs/ai-assistant-guidance.md`, `docs/codegraph-roadmap.md`, and
  `examples/README.md` to reflect optional `--repo-root`.

## Upgrade Notes

* No breaking changes. Existing configs with explicit `--repo-root` continue
  to work unchanged.

## v1.1.0 - 27-04-2026

A performance, correctness, and operability release focused on faster indexing, schema-backed edge resolution, streaming exports, and tighter DB lifecycle handling.

### Highlights
- **Indexing performance:** Repo-wide change detection is dramatically lighter — narrower DB projections, the existing-files load now overlaps the filesystem walk, per-row payloads are slimmed, and content-hash reads are deferred to the slow path. No-op repo updates do far less work and allocate substantially less.
- **Edge resolver scaling:** Slash-suffix, dot-tail2, and the dominant 2-dot dot-suffix paths are now schema-backed indexed joins instead of repo-wide symbol scans, removing large constant-factor and quadratic costs on bigger graphs (the 2-dot dot-suffix case is roughly three orders of magnitude faster on the large-scale benchmark). A steady-state guard short-circuits resolver work when no unresolved edges remain. Cross-file edge resolution on update runs stays path-scoped instead of going repo-wide.
- **Write-path allocations:** Symbol, edge, and batch insert SQL plus IN-placeholder strings are cached; edge source selection uses span/binary-search; residual allocations in the multi-file batch path are trimmed; tokenization allocations are reduced.
- **Streaming exports:** JSON and DOT exports stream through paged store calls. Peak memory is now O(page) instead of O(repo) for the unbounded paths, and the bounded-page CLI uses the same primitives.
- **Watch & update correctness:** Repo include/exclude is applied consistently; the dirty-file queue is crash-safe via claim/delete; chmod-only and directory-create events are ignored; `--jsonl` output is stable across `index`, `watch`, and `doctor`.
- **DB lifecycle hardening:** Per-version migrations run on a single connection under `BEGIN IMMEDIATE`; pragmas apply across the pool with a unified driver-aware busy-retry policy; `doctor` uses the same DSN/pragma path; `clean` and `doctor` add ANALYZE, WAL checkpoint, incremental vacuum, and a `--deep` integrity check.
- **Query correctness fixes:** `RelatedTests(file)` is correctly scoped via `target_file_id`; symbol lookup is deterministic; deleted-file graph rows are purged and cross-file references nullified, including ghost `test_links` rows pointing at deleted files; duplicate token-stat counting in batched writes is fixed; `FindDeadCode` is faster via dedicated indexes.
- **Operability:** New `index_smoke` runner produces compact perf-diff output; repo artifacts default to `.codegraph/` with legacy fallback; the CLI gains a command registry, per-command help, and canonical query command names with backward-compatible aliases; benchmarks capture `--sqlite-profile` and host context for reproducible perf comparisons.

## v1.0.9 - 16-04-2026

Improved Node.js repo indexing stability by hard-skipping common generated/tooling directories (for example `node_modules` and `.next`), refining default excludes, and clarifying ignore override behavior.

### Changed
- **indexer:** Enforce strict early skips for common Node.js generated directories; hardcoded skips are non-overridable via `.codegraphignore` negations. (#8)
- **config:** Centralize default exclude patterns to keep CLI/indexer behavior consistent. (#8)

### Fixed
- **sql:** Add explicit bounds/safety checks for path-filtering queries. (#8)
- **build/release:** Carry through `CGO_ENABLED=0` + tree-sitter cross-compilation fixes and release diagnostics. (#4, #5, #6)

### Docs
- **readme/changelog:** Update Node.js support status and release notes. (#8)

## v1.0.8 - 27-03-2026

### Fixed
- **build:** Restore `CGO_ENABLED=0` cross-compilation by splitting tree-sitter adapters behind `//go:build cgo` and using heuristic parsers in no-cgo builds. (#5)

### Changed
- **ci:** Add release build diagnostics to improve cross-platform release debugging. (ci/workflow)

## v1.0.7 - 27-03-2026

### Fixed
- **mcp:** Tighten MCP protocol compliance for stricter clients. (#3)

### Docs
- Add `v1.0.6` changelog entry. (docs)

## v1.0.6 - 26-03-2026

### Fixed
- **mcp:** Stop sending JSON-RPC responses to notifications; fix JSON Schema `required` handling; remove non-standard fields; route unhandled-method logging via configured stderr writer. (mcp)

### Changed
- `NewServer` accepts an `io.Writer` for error output, giving callers control over diagnostic logging. (mcp)

### Docs
- Add Claude Code MCP setup examples; add missing tools to MCP docs list; add short tool descriptions. (readme)

## v1.0.5 - 21-03-2026

### Fixed
- Default to a repo-local SQLite DB (while continuing to recognize legacy locations) and exclude repo DB artifacts from indexing. (config/store)

### Changed
- Treat prior global `db_dir` default as legacy so existing installs fall forward safely. (config)

## v1.0.4 - 18-03-2026

### Docs
- README update to include graph/export usage. (docs)

## v1.0.3 - 18-03-2026

### Added
- **cli:** `watch`, `benchmark`, `config init`, `clean`, `doctor --fix`, and `--jsonl` output for long-running/indexing workflows. (cli)
- **mcp/query:** Query commands + tools, including offset pagination and supported-languages introspection. (mcp)
- **parser:** Heuristic adapters for major languages plus a Python adapter. (parser)
- **export:** Include symbols + edges in graph exports; support export streaming. (export)

### Changed
- **indexer/scan:** `.codegraphignore` negation patterns; per-language scan coverage; best-effort parse policy; batched metadata writes and scoped edge resolution. (indexer)
- **performance:** Parallelize indexing and reduce allocation/IO overhead; improve watcher flush/coalescing; add scan phase timings; SQLite/store tuning. (perf)

### Notes
- **licensing:** Relicensed under FSL-1.1 to prevent commercial reselling. (license)

## v1.0.2 - 18-03-2026

### Fixed
- Installation hardening + README updates to unblock `go install` workflows. (install/docs)

## v1.0.1 - 18-03-2026

### Fixed
- Correct Go module path to `github.com/isink17/codegraph`; align imports and install docs accordingly. (install/docs)

## v1.0.0 - 18-03-2026

Initial public release of `codegraph`.

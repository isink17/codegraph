# Changelog

## Unreleased

Changes since `v1.0.9` (based on `git log v1.0.9..HEAD`).

### Changed
- **watch:** Apply repo include/exclude consistently; harden dirty-file draining. (#47)
- **watch:** Make dirty queue crash-safe via claim/delete; align watch config behavior. (#50)
- **watch:** Ignore chmod-only + directory-create events; add repeated-work stats. (#36)
- **indexer:** Target cross-file edge resolution on update runs (preserve path-scoped behavior; avoid repo-wide resolve). (#32, #33)
- **store:** Speed up edge resolver (dotted-edge indexes, dot-tail2 strategy, resolver symbol indexes). (#48)
- **store:** Scale `ResolveEdges` dot-suffix path and cap slash-suffix map growth; restrict dot-tail2 candidate set to single-dot `dst_name`. (#52)
- **store:** De-correlate `ResolveEdges` strategies 1/2/4 by distinct `dst_name`; add `BenchmarkResolveEdgesForNames_CrossFileScale`. (#53)
- **store:** Cache batch-insert SQL and speed up edge-source selection in the write path via `srcSymbolChooser` span/binary-search. (#55)
- **store:** Cache symbol insert SQL + IN-placeholder strings to reduce write-path allocations. (#58)
- **store:** Steady-state guard skips the full symbols scan in `resolveEdgesBySlashSuffix` when no unresolved non-slash edge remains; ~31× faster / ~1200× fewer allocs on the new no-unresolved bench. (#64)
- **store/indexer:** Trim repo-wide change-detection floor cost by narrowing `ExistingFiles` / `ExistingFilesForPaths` projection and filtering tombstones server-side; add `BenchmarkIndexerNoOpUpdateRepoWide`. (#61)
- **indexer:** Overlap `ExistingFiles` load with the FS walk on repo-wide scans (workers gate on `existingReady`; tasks chan buffer bumped to `workerCount*64`). 2k-file fixture ~17% faster, 5k-file ~13%. (#65)
- **store/resolver:** Schema-backed slash-suffix path via persisted `symbols.qualified_suffix` (migration 016) + partial index `idx_symbols_repo_qsuffix`; `resolveEdgesBySlashSuffix` now does an indexed equality JOIN instead of a Go-side hash filter over a full symbols scan. Large-scale (40k symbols) ~10% faster, ~11.3× fewer allocs. (#66)
- **export:** Add writer-based `JSONStream` for the unbounded JSON export path; CLI `graph export --format json` (no `--limit`, no focus) now streams via `ExportSymbolsPage` / `ExportEdgesPage` to stdout, dropping peak memory from ~3× O(repo) (full symbol + edge slices plus marshalled bytes) to O(pageSize). DOT path still buffered.
- **export:** Add writer-based `DOTStream` for the unbounded DOT export path; CLI `graph export --format dot` (no focus symbol) now streams nodes via a new server-side `ExportDOTNodeNamesPage` (`SELECT DISTINCT qualified_name … ORDER BY qualified_name LIMIT/OFFSET`) and edges via `ExportEdgesPage`, dropping peak memory from O(repo) (full `[]graph.Symbol` + `[]ExportEdge` slices + `strings.Builder` buffer) to O(pageSize). Output preserves the existing `digraph codegraph { … }` framing and alphabetically-sorted dedup'd node lines; edge order follows `ORDER BY id ASC` (already unspecified per `DOT()`'s contract). Focused DOT (`--symbol`) keeps the bounded `GraphSnapshot` path.
- **store:** Trim residual write-path allocations in `ReplaceFileGraphsBatch`; add multi-file batch bench. (#60)
- **export:** Page no-focus `JSONPaged` directly via `ExportSymbolsPage` / `ExportEdgesPage` so peak memory is O(page) instead of O(repo) on the bounded-page CLI path. (#62)
- **indexing/store:** Broad batching + reduced statement pressure across symbols/FTS/inserts; add/extend phase timings + write_stats counters. (#20, #21, #22, #24, #25, #26, #28, #29)
- **indexing:** Reduce tokenization allocations; add tokenize timing stats. (#30)
- **json/jsonl:** Stabilize `watch` and `doctor` machine-readable output (event envelopes; arrays always present; disable HTML escaping). (#38, #43)
- **cli/index:** Stabilize `--jsonl` scan payloads/envelopes (scan_kind, parse_ms, correlation fields; dedupe envelopes; handle phase write errors). (#40, #41, #42)
- **clean/doctor:** Add ANALYZE, WAL checkpoint, incremental vacuum; add `doctor --deep` integrity checks; expand DB diagnostics + FTS optimize. (#37, #39)
- **benchmark:** Add `--sqlite-profile` and capture sqlite_profile/host context. (#44)
- **store/bench:** Add chooser + callers/callees benchmarks; retry `applyPragmas` on `SQLITE_BUSY`. (#57)
- **store/bench:** Add per-strategy resolver microbenchmarks (slash-suffix, dot-tail2, dot-suffix). (#59)
- **store:** Speed up `FindDeadCode` with new `idx_refs_repo_symbol_id` / `idx_refs_repo_context_symbol_id` indexes. (#54)
- **cli:** Add `index_smoke` runner with compact jsonl + median baseline for perf diffs. (#45)
- **cli/config:** Default repo artifacts under `.codegraph/` (DB + bench gocache) with legacy DB fallback; harden repo DB path handling. (#49)
- **cli/help/commands:** Command registry + per-command help; canonical query command names with backward-compatible aliases; help/usage normalization. (#10, #12, #13, #14, #15, #16, #17, #18, #19)

### Fixed
- **store:** Purge-time delete of `test_links` whose `target_file_id` points at a deleted file. Previously the symbol-side companion (`target_symbol_id`) was nulled but the row survived with a stale `target_file_id`, surfacing as a ghost association in `RelatedTests(file=path)` because the path→id lookup does not filter `is_deleted`. Covered by new `TestPurgeDeletes_TestLinksTargetingDeletedFile` and `TestPurgeNullifiesEdgeAndRefSymbolReferences`.
- **store/indexer:** Purge deleted-file graph rows and nullify cross-file symbol references. (#46)
- **store:** Scope `RelatedTests(file)` correctly via `target_file_id`; make symbol lookup deterministic. (#54)
- **store/doctor:** Harden DB pragmas and migration lifecycle correctness; concurrent-open coverage via `TestOpen_ConcurrentMigrateIsSafe`. (#56)
- **store:** Fix duplicate token-stat counting in batched writes. (#60)

## v1.0.9 - 2026-04-16

Improved Node.js repo indexing stability by hard-skipping common generated/tooling directories (for example `node_modules` and `.next`), refining default excludes, and clarifying ignore override behavior.

### Changed
- **indexer:** Enforce strict early skips for common Node.js generated directories; hardcoded skips are non-overridable via `.codegraphignore` negations. (#8)
- **config:** Centralize default exclude patterns to keep CLI/indexer behavior consistent. (#8)

### Fixed
- **sql:** Add explicit bounds/safety checks for path-filtering queries. (#8)
- **build/release:** Carry through `CGO_ENABLED=0` + tree-sitter cross-compilation fixes and release diagnostics. (#4, #5, #6)

### Docs
- **readme/changelog:** Update Node.js support status and release notes. (#8)

## v1.0.8 - 2026-03-27

### Fixed
- **build:** Restore `CGO_ENABLED=0` cross-compilation by splitting tree-sitter adapters behind `//go:build cgo` and using heuristic parsers in no-cgo builds. (#5)

### Changed
- **ci:** Add release build diagnostics to improve cross-platform release debugging. (ci/workflow)

## v1.0.7 - 2026-03-27

### Fixed
- **mcp:** Tighten MCP protocol compliance for stricter clients. (#3)

### Docs
- Add `v1.0.6` changelog entry. (docs)

## v1.0.6 - 2026-03-26

### Fixed
- **mcp:** Stop sending JSON-RPC responses to notifications; fix JSON Schema `required` handling; remove non-standard fields; route unhandled-method logging via configured stderr writer. (mcp)

### Changed
- `NewServer` accepts an `io.Writer` for error output, giving callers control over diagnostic logging. (mcp)

### Docs
- Add Claude Code MCP setup examples; add missing tools to MCP docs list; add short tool descriptions. (readme)

## v1.0.5 - 2026-03-21

### Fixed
- Default to a repo-local SQLite DB (while continuing to recognize legacy locations) and exclude repo DB artifacts from indexing. (config/store)

### Changed
- Treat prior global `db_dir` default as legacy so existing installs fall forward safely. (config)

## v1.0.4 - 2026-03-18

### Docs
- README update to include graph/export usage. (docs)

## v1.0.3 - 2026-03-18

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

## v1.0.2 - 2026-03-18

### Fixed
- Installation hardening + README updates to unblock `go install` workflows. (install/docs)

## v1.0.1 - 2026-03-18

### Fixed
- Correct Go module path to `github.com/isink17/codegraph`; align imports and install docs accordingly. (install/docs)

## v1.0.0 - 2026-03-18

Initial public release of `codegraph`.

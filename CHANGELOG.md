# Changelog

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

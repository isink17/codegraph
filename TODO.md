# Optimization TODOs

## Note on references
- Context7 was used for Go/SQLite lookup, but documentation snippets could not be fetched in this session (`resolve` worked, `query` failed). The items below are based on direct code inspection and established Go/SQLite patterns.

## Medium priority
1. Add pagination/streaming for large query results.
- Why: callers/callees/symbol search return only `limit`; no cursor/offset for large repos.
- Where: `internal/store/store.go` query methods, `internal/mcp/server.go` tool args/schemas.
- Task: support `offset`/`cursor` in store + MCP tools.

2. Add cancellation-aware background flushing in watcher.
- Why: flush can run long and block event handling; no backpressure stats.
- Where: `internal/watcher/watcher.go` (`Run`).
- Task: decouple queue drain from event loop via worker goroutine, expose dropped/coalesced metrics.

3. Improve GitHub version check efficiency and resilience.
- Why: always requests full latest release payload when interval triggers.
- Where: `internal/versioncheck/versioncheck.go`.
- Task: use conditional request headers (`If-None-Match`/`If-Modified-Since`) and persist ETag/Last-Modified.

## Feature opportunities
1. Add `codegraph clean` command for stale DB files and cache maintenance.
- Where: `internal/cli/app.go`, `internal/store`.
- Scope: remove orphaned DB files, vacuum selected repos, print reclaimed size.

2. Add repo-level ignore file support (`.codegraphignore`).
- Why: easier local tuning than editing JSON config for quick excludes.
- Where: `internal/config`, `internal/indexer`.
- Task: merge ignore patterns from repo file + repo config excludes.

3. Add MCP tool for repo/list + scan/history.
- Why: clients cannot inspect known repos/scans without direct DB access.
- Where: `internal/mcp/server.go`, `internal/store/store.go`.
- Task: expose `list_repos`, `list_scans`, and latest scan errors.

4. Add parser adapters beyond Go behind current abstraction.
- Why: architecture already supports multi-language adapters.
- Where: `internal/parser/*`.
- Task: start with one high-value language adapter and shared tokenization/edge normalization rules.

## Nice-to-have
1. Consolidate duplicated PATH hint logic.
- Why: same helper logic exists in CLI install and doctor.
- Where: `internal/cli/app.go`, `internal/doctor/doctor.go`.
- Task: move into shared package to keep output behavior consistent.

2. Add structured benchmark targets for index/update/query.
- Why: no performance baseline regression guard.
- Where: `internal/indexer`, `internal/store`, `internal/mcp`.
- Task: add `go test -bench` suites with representative fixture repos.

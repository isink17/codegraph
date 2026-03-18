# Optimization TODOs

## Note on references
- Context7 was used for Go/SQLite lookup, but documentation snippets could not be fetched in this session (`resolve` worked, `query` failed). The items below are based on direct code inspection and established Go/SQLite patterns.

## Medium priority
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

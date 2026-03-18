# Optimization TODOs

## Note on references
- Context7 was used for Go/SQLite lookup, but documentation snippets could not be fetched in this session (`resolve` worked, `query` failed). The items below are based on direct code inspection and established Go/SQLite patterns.

## Nice-to-have
1. Consolidate duplicated PATH hint logic.
- Why: same helper logic exists in CLI install and doctor.
- Where: `internal/cli/app.go`, `internal/doctor/doctor.go`.
- Task: move into shared package to keep output behavior consistent.

2. Add structured benchmark targets for index/update/query.
- Why: no performance baseline regression guard.
- Where: `internal/indexer`, `internal/store`, `internal/mcp`.
- Task: add `go test -bench` suites with representative fixture repos.

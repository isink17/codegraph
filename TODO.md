# Optimization TODOs

## Note on references
- Context7 was used for Go/SQLite lookup, but documentation snippets could not be fetched in this session (`resolve` worked, `query` failed). The items below are based on direct code inspection and established Go/SQLite patterns.

## P2 (Lower impact / cleanup)
1. Stream hashing for non-parsed files and enforce max file size threshold.
- Current: `os.ReadFile` reads entire file into memory.
- Why: memory spikes on large files/binaries.
- Where: `internal/indexer/indexer.go` (`run`).
- Task: use streaming hash for files that will not be parsed; add max file size skip with reason.

2. Consolidate stats queries into fewer SQL calls.
- Current: multiple `QueryRowContext` calls per `stats`.
- Why: minor latency overhead.
- Where: `internal/store/store.go` (`Stats`).
- Task: combine counters into one query with subselects or cached scan summary path.

3. Reuse buffers in MCP framing path.
- Current: allocates new payload/frame buffers each response.
- Why: avoid small but frequent allocations under heavy MCP traffic.
- Where: `internal/mcp/server.go` (`writeResponse`, `readFrame`).
- Task: add small buffer pool (`sync.Pool`) for response frames.

4. Add index for dirty queue drain ordering.
- Current: `DrainDirtyFiles` orders by `queued_at` without explicit index covering order.
- Why: keeps watch flush predictable as queue grows.
- Where: `internal/store/schema/001_init.sql` (`dirty_files` table).
- Task: add migration with index on `(repo_id, queued_at)`.

# Graph freshness status

`codegraph stats` and the MCP tool `graph_stats` return the same read-only
`freshness` object, built by one function (`Store.FreshnessStatus`). It reports
what the stored graph and the host can prove about the graph's staleness. It
never says "fresh": changes on disk since the last scan are not checked
(`filesystem: "not_checked"`) unless a scan has run since.

Reading it never writes the database and never creates a file. It runs one
read-only transaction, one bounded `git rev-parse HEAD` (2 s), and one
non-blocking probe of the scan lock.

## Verdict

`verdict` is decided in this order; the first that applies wins.

| Verdict | Meaning |
| --- | --- |
| `index_incomplete` | No scan of this repository ever completed. |
| `writer_active` | A scan of this database holds the scan lock now, possibly before its row exists or while a newer scan already completed. Rows are changing; re-read after it finishes. |
| `abandoned_scan` | Scan rows read `running` while no process held the scan lock. Their process died; run `codegraph recover-scans`. |
| `liveness_unknown` | Rows read `running` and the lock could not be probed (no lock file, as with a scan by a binary that predates the lock, or an I/O error). |
| `stale_known` | A stored fact says the graph is behind: see `reasons`. |
| `consistent_with_last_scan` | No stored fact and no live writer contradicts the last completed scan. Edits since then are not checked. |

A repository that was never indexed is not a verdict: the command fails with
the usual not-indexed error.

## Dimensions

| Field | Values | Source |
| --- | --- | --- |
| `last_completed_scan` | id, kind, `started_at`, `finished_at`, `scope` (`full`, `paths`, `filtered`, `unknown` before scope recording) | `scans` |
| `latest_scan` | as above, any status; `error_text` when failed | `scans` |
| `coverage.last_full_scan`, `coverage.failed_after_last_full` | whole-repository evidence | `scans` |
| `running_scans.count`, `.after_last_completed` | rows still `running` | `scans` |
| `running_scans.liveness` | `active`, `abandoned`, `none`, `unknown` | scan lock probe |
| `dirty_queue.queued`, `.in_flight`, `.oldest_queued_at` | persisted watcher work | `dirty_files` |
| `resolver_policy`, `resolver_policy_pending` | `converged`, `pending` (with languages), `unknown` | stored resolver policy markers |
| `worktree.repo_root`, `.canonical_path` | the repository the graph belongs to | `repos` |
| `worktree.head_at_index`, `.head_now`, `.head_changed` | `unknown` when either HEAD cannot be read | history watermark, one git call |
| `watcher` | always `not_in_this_process` | neither `stats` nor `serve` runs a watcher, and no watcher liveness is persisted |
| `filesystem` | always `not_checked` | |

`state` (`known_stale`, `unknown`, `no_known_staleness`) and `reasons` keep
their earlier meaning. `scan_running_or_abandoned` now also fires when the
lock shows a live writer whose row is older than the last completed scan, or
that has not inserted its row yet.

## Writer liveness

Every scan holds a shared OS lock on `<db>.scan.lock` from before its row is
inserted until the row is closed. The status read tries to take that lock
exclusively without waiting (a few 10 ms retries, so another status read is
not mistaken for a scan). When it succeeds it keeps the lock across its row
read, so no scan can begin meanwhile and every `running` row it reads is
proven abandoned. A new scan waiting for the lock is delayed by at most one of
its 50 ms retries. Limits: the lock is host-local (a writer on another machine
sharing the database is not seen), a binary that predates the lock takes none,
and a `recover-scans` that collides with a status read refuses and can be
rerun.

## Watcher

`codegraph watch` is the only process that runs a watcher; its counters and
readiness are process-local (`watch --jsonl`). Events are written to the
dirty queue before any flush, so a stopped watcher leaves them for the next
`watch` or `update`. The watcher:

- queues the files inside a directory that is created, moved or copied into
  the repository (they raise no events of their own);
- queues the indexed files under a watched directory that is removed or moved
  out, so they leave the graph;
- answers an event-queue overflow with one reconciliation pass (the
  incremental full update, which skips files whose size and mtime did not
  change) instead of stopping, and counts it in `overflows`;
- ignores a new directory that is removed before it can be registered.

Other watcher errors still stop it with the error; claimed dirty paths stay
queued for the next run. Changes made while no watcher runs are not seen
until an `update`.

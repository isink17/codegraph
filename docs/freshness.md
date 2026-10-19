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
| `writer_active` | The scan lock is held and no other repository in the database has running rows: a scan is writing, possibly before its row exists or with a row older than a scan that already completed. Re-read after it finishes. |
| `abandoned_scan` | Scan rows read `running` while no lock-aware process held the scan lock; a scan by a binary older than the lock is not seen. Run `codegraph recover-scans`. |
| `liveness_unknown` | Rows read `running` and their writer cannot be told live or dead (no lock file, as with a binary that predates the lock, or an I/O error), or the lock is held while another repository in the same database has running rows. Never `consistent_with_last_scan` while the lock is held. |
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
lock shows a live writer whose row is older than the last completed scan.

## Writer liveness

Every scan holds a shared OS lock on `<db>.scan.lock` from before its row is
inserted until the row is closed. The status read tries to take that lock
exclusively without waiting, retrying 10 times 10 ms apart; reads in one
process take turns. When it succeeds it keeps the lock across its row read,
so no scan can begin meanwhile and every `running` row it reads has no
lock-aware owner. When every attempt fails the lock is held by a writer of
this repository unless another repository in the database has running rows:
a scan takes the lock before it inserts its row, and that insert can wait on
a busy database. With another repository's rows running the holder cannot be
attributed and liveness is `unknown`.

A new scan waiting for the lock is delayed by at most one of its 50 ms
retries, and `recover-scans` retries over the same 100 ms window before
refusing.

Limits:

- The lock is host-local: a writer on another machine sharing the database
  is not seen.
- A binary that predates the lock takes none; its live scans read
  `abandoned`.
- A status read in another process that holds the lock for more than about
  100 ms (a very slow disk) reads as a writer, and makes `recover-scans`
  refuse; rerun either.
- When repository A has a dead running row and repository B in the same
  database has a scan that holds the lock before inserting its row, A reads
  `writer_active` rather than `abandoned_scan` (the safe side).

## Watcher

`codegraph watch` is the only process that runs a watcher; its counters and
readiness are process-local (`watch --jsonl`). Events are written to the
dirty queue before any flush, so a stopped watcher leaves them for the next
`watch` or `update`. The watcher:

- queues the files inside a directory that is created, moved or copied into
  the repository (they raise no events of their own);
- queues the indexed files under a watched directory that is removed or moved
  out, so they leave the graph;
- answers an event-queue overflow by re-registering the whole watch tree
  (directories created during the dropped events were never registered) and
  then one reconciliation pass (the
  incremental full update, which skips files whose size and mtime did not
  change) instead of stopping, and counts it in `overflows`;
- ignores a new directory that is removed before it can be registered.

Other watcher errors still stop it with the error; claimed dirty paths stay
queued for the next run. Changes made while no watcher runs are not seen
until an `update`.

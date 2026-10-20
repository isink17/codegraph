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
| `liveness_unknown` | Rows read `running` and their writer cannot be told live or dead (no lock file, as with a binary that predates the lock), the lock could not be probed, or the lock is held while another repository in the same database has running rows. Never `consistent_with_last_scan` while the lock is held or unprobeable. |
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
| `parser_profile`, `parser_profile_unconverged` | `converged`, `unconverged` (with languages), `unknown` | per-file parser provenance |
| `worktree.repo_root`, `.canonical_path` | the repository the graph belongs to | `repos` |
| `worktree.head_at_index`, `.head_now`, `.head_changed` | `unknown` when either HEAD cannot be read | history watermark, one git call |
| `watcher` | always `not_in_this_process` | neither `stats` nor `serve` runs a watcher, and no watcher liveness is persisted |
| `filesystem` | always `not_checked` | |

`state` (`known_stale`, `unknown`, `no_known_staleness`) and `reasons` keep
their earlier meaning. `scan_running_or_abandoned` now also fires when the
lock shows a live writer whose row is older than the last completed scan.
Two reasons are newer:

- `reconcile_pending`: a watcher started, or lost events or queue writes,
  and no full scan that began after that has completed. It is a database
  setting, not a schema change; any completed full scan clears it, including
  `codegraph update` from another process.
- `parser_profile_unconverged`: a language's files carry more than one
  parser profile, or none recorded (indexed before provenance was kept). A
  scan converges every file of a language, so this is a transition left
  unfinished. The stored files are compared with each other only: a binary
  whose parser profile differs from every stored file is not detected until
  its next scan converges them.

Both make the verdict `stale_known`, and so count as known staleness wherever
freshness gates a decision: `check_constraints --strict-freshness` exits 3
(known stale) and `safe_to_edit` returns no verdict, with reason `known_stale`
and exit 3, as for any other stale reason.

## Writer liveness

Every scan holds a shared OS lock on `<db>.scan.lock` from before its row is
inserted until the row is closed. The status read tries to take that lock
exclusively without waiting, retrying 10 times 10 ms apart; reads in one
process take turns. When it succeeds it keeps the lock across its row read,
so no scan can begin meanwhile and every `running` row it reads has no
lock-aware owner. When every attempt fails the hold is attributed to a writer
of this repository (assumed, not proven) unless another repository in the
database has running rows: a scan takes the lock before it inserts its row,
and that insert can wait on a busy database. With another repository's rows
running the holder cannot be attributed and liveness is `unknown`.

When the lock cannot be probed (an open or lock error other than a missing
file) liveness is `unknown` and the verdict `liveness_unknown`, since a
writer may hold it. A missing lock file means no lock-aware scan has ever
run on this database, so none can hold it; liveness is `unknown` and the
verdict follows the rows (a pre-lock binary's scan is not seen).

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
- Repository A, with or without running rows, reads `writer_active` while
  repository B in the same database has a scan that holds the lock before
  inserting its row; a dead row of A then reads `writer_active` rather than
  `abandoned_scan` (the safe side).

## Watcher

`codegraph watch` is the only process that runs a watcher; its state and
counters are process-local (`watch --jsonl`, `stats.state`). Events are
written to the dirty queue before any flush, so a stopped watcher leaves them
for the next `watch` or `update`.

Every start records `reconcile_pending`, registers the watch tree, and runs
one reconciliation pass (the incremental full update, which skips files whose
size and mtime did not change) together with the queued work, before it
reports ready, so starting a watcher costs one incremental `update`. Changes
made while no watcher ran are found by that pass, not by events. A pass that is cancelled or crashes leaves `reconcile_pending`
(and usually a `failed` scan row); the next start runs it again.

`stats.state` is:

| State | Meaning |
| --- | --- |
| `reconciling` | The start-up pass has not completed. |
| `watching` | The tree is registered and a pass completed after the last failure. |
| `recovering` | A retryable failure left registration or queued work unverified. |

Retryable failures are the OS running out of watches or descriptors
(`ENOSPC`, `EMFILE`, `ENFILE`) and SQLite reporting busy or locked. On one the
watcher records `reconcile_pending`, keeps every claimed path in the dirty
queue, and retries after a delay that doubles from 250 ms to 30 s. A failed
registration is retried by re-registering the whole tree (the old watches are
released first) and re-reading the repository config; a failed flush or queue
write by a full pass. It reads `watching` again only after a pass that began
after the latest failure completes with the tree registered. Every other
error stops the watcher with that error: an unreadable or invalid repository
config after a change, a closed or corrupt database, a missing or unreadable
repository root, a refused parser downgrade, cancellation.

A repository config that is not valid JSON, as a file caught mid-write reads,
is the one config error that is retried, whichever reads it first: an event,
a recovery attempt or a flush (the indexer reads the config too). The watcher
reads `recovering` with `last_transient: config_invalid`, stops flushing,
keeps the dirty queue, and re-reads the config on the registration backoff.
Only those scheduled attempts count: after 5 that still find it malformed,
about 8 s with the default delays, the watcher stops; a burst of events from
one slow write spends none. A valid config resumes it with a reconciliation
pass under the new rules. Valid JSON the config cannot hold (a wrong type)
stops it at once.

The watcher also:

- queues the files inside a directory that is created, moved or copied into
  the repository (they raise no events of their own);
- queues the indexed files under a watched directory that is removed or moved
  out, so they leave the graph;
- answers an event-queue overflow by re-registering the whole watch tree and
  then one reconciliation pass instead of stopping, and counts it in
  `overflows`;
- registers a `.codegraph` directory created while it runs and reads the
  config inside;
- ignores a new directory that is removed before it can be registered.

While its state is not `watching`, a watcher also holds a shared OS lock on
`<database>.watch-<repo id>.lock`, and freshness reports `reconcile_pending`
while any process holds it. A full scan by another process therefore cannot
clear the reason while a live watcher is still reconciling or recovering: the
scan covers changes made before it, not events the watcher keeps missing. The
operating system releases the lock when the watcher exits, however it exits,
so a crashed watcher leaves only the persisted marker, which the next
completed full scan clears. Like the scan lock, it is local to the host.

Limits: no other watcher state is persisted, so `stats` and `graph_stats`
keep `watcher: "not_in_this_process"`; the lock says that some live watcher
has not reconciled, not which one or why.

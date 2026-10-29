# Graph freshness status

## CG-105 bounded acceptance record (29-10-2026)

Task `c28-cg105-freshness-acceptance` evaluated merged head
`783d1222ecdbfb8fec6b54090f34678c58e6a368` (also the local `origin/v2.0`),
tree `5c7f420df0576f637d7707e61099fb8806c1785c`, on macOS arm64,
Go 1.26.1, CGO enabled. This record changes documentation only; the tested
production and regression-test files remain exactly those of that tree.
It is a bounded local acceptance record, not ticket closure or independent
review of a subsequent PR head.

| Acceptance area | Local classification and existing evidence |
| --- | --- |
| HEAD anchoring | Pass: `TestFreshnessHead*` covers path-scoped scans, full scans, detached/reverted HEAD, history absence/watermark fallback and unavailable Git. |
| Scan evidence and coverage | Pass: `TestFreshnessCoverage*`, `TestFreshnessRunningScan`, `TestFreshnessReconcilePendingUntilLaterFullScan`; failed, overlapping, legacy and incomplete scans retain conservative status. |
| Writer liveness | Pass: `TestFreshness*Lock*`, `TestFreshness*Probe*`, `TestFreshness*Writer*`; pre-insert writers, other repositories and probe failures do not claim consistency. |
| Snapshot and read-only status | Pass: `TestSnapshot*`, `TestFreshnessStatusIsReadOnly`, `TestLiveWatcherSeenByReadOnlyStore`; live locks and post-snapshot changes remain visible without status writes. |
| Watcher lock protocol | Pass: `TestWatcherLock*`, `TestLiveWatcher*`; startup/recovery, missing/erroring locks, concurrent readers, second watchers, takeover and killed subprocesses. |
| Reconciliation and event loss | Pass: watcher tests for offline changes, interrupted startup, overflow, moved-in/out and removed directories, vanished registration targets, and stop-before-flush/restart. Overflow and resource failures use injected errors; directory events use the native macOS watcher. |
| Recovery and config changes | Pass: watcher registration/flush/queue-write retries, malformed config across event/flush/startup paths, retry-attempt accounting, invalid values, and degraded state surviving another process's full scan. |
| Parser provenance | Pass: profile convergence, compatibility classification and obsolete-profile freshness propagation in store/watcher and cross-surface parity tests. |
| Consumer parity | Pass: `TestFreshnessParityAcrossSurfaces` exercises CLI stats, MCP `graph_stats` through `serve`, strict constraints and edit safety for obsolete parsers, watcher lifecycle and moved HEAD/full update. `TestFreshnessParityWithScanLockedBeforeRow` checks a subprocess lock holder. |
| Watch lifecycle and MCP bootstrap | Pass: `TestRunWatchJSONL*`, `TestWatchStopsGracefullyOnSignal`, `TestServe*`; readiness/cancellation, signal shutdown, `tools/list` and `tools/call` on the production serve path. |
| Active-write snapshot/open refusal | Preserved, not newly exercised: active-scan exit 2 with no verdict remains an acceptable fail-closed result. A successfully opened read with a held scan lock instead reaches freshness-unknown (exit 4); the pre-insert lock parity test covers that separate path. No attempt was made to turn an open failure into a verdict. |
| Native Windows runtime | Deferred to release preparation, as requested. Neither cross-compilation nor this macOS run establishes Windows lock, watcher, path or file-handle behavior. Native Linux was not run here either. |
| Original CG-105 checklist | Unverified against the issue: no tracked CG-105 checklist was found and `gh issue view 105 --json title,body,state` failed because GitHub CLI authentication is unavailable. Coordinator must reconcile the original wording, especially any workload/performance or additional platform criteria; the rows above must not imply those passed. |
| Independent exact-head review | Pending with coordinator if a PR is proposed. Review must name its final commit SHA, with focused checks on that head. No PR, push or merge is performed by this task. |

Reproduction commands (each test invocation has an explicit timeout):

```sh
go test -count=1 -timeout 5m ./internal/store ./internal/watcher -run 'Test(Freshness|Snapshot|LiveWatcher|Watcher|ParserProfile|Unconverged|Health|Degraded|RelPath|Run)'
go test -count=3 -timeout 5m ./internal/store ./internal/watcher -run 'Test(Freshness.*(Lock|Probe|Writer)|LiveWatcher|WatcherLock|SnapshotFreshness|Watcher(ReconcilesAfterEventOverflow|RetriesTransientFlushFailure|RetriesMalformedConfigSeenByFlush|Liveness|ConfigPause|StartupFlush))'
go test -count=1 -timeout 5m ./internal/cli ./internal/mcp ./internal/constraints ./internal/editsafety -run 'Test(FreshnessParity|CheckConstraintsCLIStrict|CheckConstraintsToolStrictRunningScan|SafeToEditCLIConstraintsAndFreshness|SafeToEditCLIFreshnessUnknownOutsideGit|SafeToEditTool|StrictFreshness|DecideOrder)'
go test -count=1 -timeout 5m ./internal/cli -run 'Test(ReadOnlyCommandsOnUnindexedRepoCreateNothing|SafeToEditCLIDeterministicAndReadOnly|RecoverScansCommand|RunWatchJSONL|WatchStopsGracefullyOnSignal|Serve)'
go vet ./internal/store ./internal/watcher
go build -o /dev/null ./cmd/codegraph
```

The full focused-package gate also passed:

```sh
PATH=/opt/homebrew/bin:$PATH go test -count=1 -timeout 20m ./internal/store ./internal/watcher
```

Store completed in 204.941 s and watcher in 11.640 s. `git diff --check`
passed. All commands above passed. `/dev/null` is only the build output sink used
on this macOS host. The first command and the final consumer-parity rerun
used `PATH=/opt/homebrew/bin:$PATH` to select installed Homebrew Git.
With Apple's `/usr/bin/git`, the HEAD tests failed because denied `xcrun`
cache writes emitted warnings which the shared `gittest` fixture's combined
output included in its returned commit ID. Re-running `TestFreshnessHead*`
with Homebrew Git passed all seven tests. This is classified as an
environment/fixture limitation, not a newly reproduced freshness defect;
the shared fixture is outside this task's edit scope. No production or
regression-test correction is proposed.

`codegraph stats` and the MCP tool `graph_stats` return the same read-only
`freshness` object, built by one function (`Store.FreshnessStatus`). It reports
what the stored graph and the host can prove about the graph's staleness. It
never says "fresh": changes on disk since the last scan are not checked
(`filesystem: "not_checked"`) unless a scan has run since.

Reading it never writes the database and never creates a file. It runs one
read-only transaction, one bounded `git rev-parse HEAD` (2 s), and
non-blocking probes of the scan lock and the repository's watcher locks.

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
| `parser_profile_compatibility` | `state` `current`, `obsolete`, `unknown`; `reason`; `languages` | per-file parser provenance and the reading binary's parsers |
| `worktree.repo_root`, `.canonical_path` | the repository the graph belongs to | `repos` |
| `worktree.head_at_index`, `.head_at_index_source`, `.head_now`, `.head_changed` | `unknown` when either HEAD cannot be read; source `last_full_scan`, `git_history_watermark` or `none` | last full scan's `head_at_finish`, else history watermark; one git call |
| `watcher` | always `not_in_this_process` | neither `stats` nor `serve` runs a watcher, and no watcher liveness is persisted |
| `live_watcher` | `watching`, `unverified`, `none`, `unknown` | watcher lock probes; see [Watcher](#watcher) |
| `filesystem` | always `not_checked` | |

`state` (`known_stale`, `unknown`, `no_known_staleness`) and `reasons` keep
their earlier meaning. `scan_running_or_abandoned` now also fires when the
lock shows a live writer whose row is older than the last completed scan.
Three reasons are newer:

- `reconcile_pending`: a watcher started, or lost events or queue writes,
  and no full scan that began after that has completed. It is a database
  setting, not a schema change; any completed full scan clears it, including
  `codegraph update` from another process.
- `parser_profile_unconverged`: a language's files carry more than one
  parser profile, or none recorded (indexed before provenance was kept). A
  scan converges every file of a language, so this is a transition left
  unfinished. The stored files are compared with each other only: a binary
  whose parser profile differs from every stored file is not detected until
  its next scan converges them; `parser_profile_compatibility` does.
- `parser_profile_obsolete`: `parser_profile_compatibility.state` is
  `obsolete` (see [Parser compatibility](#parser-compatibility)).

All three make the verdict `stale_known`, and so count as known staleness wherever
freshness gates a decision: `check_constraints --strict-freshness` exits 3
(known stale) and `safe_to_edit` returns no verdict, with reason `known_stale`
and exit 3, as for any other stale reason.

## HEAD

`head_at_index` is HEAD at the finish of the last completed full scan
(`head_at_index_source: "last_full_scan"`). A path-scoped `update` or watch
flush never changes it, so after a commit only some of whose files were
rescanned, `head_changed` is `yes` and `head_moved` makes the verdict
`stale_known` until a full scan runs at the current HEAD. HEADs are compared by
commit id, so checking out the full scan's commit again, attached or detached,
reads unchanged. Without a completed full scan that recorded its HEAD (rows from
before coverage recording, or Git unreadable during the scan), the anchor is
the Git history watermark (`git_history_watermark`): HEAD as probed by the
latest scan that reached its history phase, path-scoped ones included, so
`head_changed: "no"` there does not mean the whole graph was rebuilt at that
HEAD. With neither, `head_at_index` is `unknown` (source `none`).

## Parser compatibility

`parser_profile` compares the stored files with each other;
`parser_profile_compatibility` compares them with the parsers of the binary
reading them, the registry the indexer plans profile transitions with. A
language is:

| State | Reason | Meaning |
| --- | --- | --- |
| `current` | | every stored file carries this binary's profile |
| `obsolete` | `older_version` | some files carry an older version of this binary's parser family (`<family>:<language>:v<n>`); this binary's next scan reparses them |
| `unknown` | `call_capable_build_required` | call-capable or unrecorded data that this binary, which emits no call edges for the language, refuses to replace, as a build without CGO reading tree-sitter data |
| `unknown` | `other_parser_family` | another parser family wrote it, such as `go-ast` data read by a tree-sitter build |
| `unknown` | `newer_version` | a newer binary wrote it |
| `unknown` | `unrecorded_provenance` | indexed before provenance was recorded |
| `unknown` | `no_adapter` | this binary has no parser for the language |

A language is `obsolete` only when every stored profile that differs from
this binary's is an older version of its family. One profile this binary
cannot place makes the whole language `unknown`, however many others are
merely older, with the first reason in the table's order that any of its
profiles has (`call_capable_build_required`, `other_parser_family`,
`newer_version`, `unrecorded_provenance`), so the order profiles are read in
never changes it.

The object's `state` is `obsolete` when any language is, else `unknown` when
any is, else `current`, with the `reason` of the first such language;
`languages` lists every language that is not current, with its
`stored_profiles` and `current_profile`. When the provenance cannot be read
it is `unknown` with reason `provenance_unreadable`, and `no_parser_registry`
when the reading process has no parser registry. Only `obsolete` is a stale
reason: an `unknown` compatibility leaves the verdict alone, since a graph a
richer build wrote is not stale for that build. A build without CGO
therefore never calls tree-sitter data obsolete or current. It is one
aggregate query, the one `parser_profile` already runs.

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

A read-only handle (`check_constraints --strict-freshness` and `safe_to_edit`
on the CLI) reads rows from a snapshot taken when it opened the database and
probes the live database's scan lock, never the snapshot's, holding it only
while it reads the snapshot rows, as above: a held lock reads
`writer_active`. A `running` row in the
snapshot may belong to a scan that finished after the snapshot was taken, so
with the lock free it reads `unknown` (`liveness_unknown`), never
`abandoned`. After the probe the read compares the database's files (size,
modification time, identity of the database, WAL and journal) with the ones
the snapshot copied (an empty or missing WAL or journal counts as one
state, as SQLite creates and removes them when connections open and close);
any difference, including a scan that began and finished between the
snapshot and the read, makes strict freshness `unknown` with
`scan_activity_during_check`. It errs both ways: a write that keeps a file's
size and modification time on a file system with coarse timestamps, or whose
metadata Windows has not yet updated for a file another process holds open,
is missed; a checkpoint, or a write by another repository's scan or watcher
in the same database, reads as a change.

A new scan waiting for the lock is delayed by at most one of its 50 ms
retries, and `recover-scans` retries over the same 100 ms window before
refusing.

Limits:

- The lock is host-local: a writer on another machine sharing the database
  is not seen.
- A lock file removed while a scan holds it hides that scan; its `running`
  row still reads `liveness_unknown`, but a scan that has not yet inserted
  its row is not seen.
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

A running watcher flushes only the paths it saw change, which never counts
as a full scan, so after every commit freshness reads `stale_known` with
`head_moved` until a full `codegraph update` or a watcher restart reconciles.

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

### Watcher locks

A watcher holds two OS locks beside the database, both local to the host and
both released by the operating system when the watcher exits, however it
exits; nothing about a watcher is stored.

- `<database>.watch-<repo id>.lock`, shared, while its state is not
  `watching` or it does not hold the live lock. Freshness reports
  `reconcile_pending` while any process holds it, so a full scan by another
  process cannot clear the reason while a live watcher is still reconciling
  or recovering: the scan covers changes made before it, not events the
  watcher keeps missing. The hold that follows a failure is best effort, like
  the marker written with it: it waits up to 1 s for status reads probing the
  lock, outside the lock that guards the watcher's state, and a hold that
  fails leaves only the marker.
- `<database>.watch-<repo id>.live.lock`, exclusive, for its whole run.
  Status reads probe it shared, so they never mistake one another for a
  watcher and never wait on each other.

A watcher takes the first lock before the second and releases them in
reverse. `live_watcher` reads:

| Value | Meaning |
| --- | --- |
| `unverified` | the first lock is held: a live watcher of this repository is starting up or recovering, or runs without the live lock (see the limits below) |
| `watching` | the live lock is held, before and after the first lock was found free: a live watcher reconciled |
| `none` | neither is held, or no lock file exists |
| `unknown` | a lock could not be probed (a permission or lock error), or the live lock file exists without the first one, which a watcher never leaves: the first was removed and may still be held. `reconcile_pending` is kept until the next `codegraph watch` recreates it |

A crashed watcher leaves `none` and only the persisted `reconcile_pending`
marker, which the next completed full scan clears. Repositories in one
database have separate locks and are reported apart. The read never creates
a lock file, and a read-only handle (`check_constraints --strict-freshness`
and `safe_to_edit` on the CLI read a snapshot of the database) probes the
live database's locks, so every surface sees the same watcher.

Limits:

- The locks are host-local: a watcher on another machine sharing the
  database over a network file system reads `none`, as no lock is seen.
- A lock file removed while a watcher holds it hides that hold: the watcher
  keeps locking the removed file. Removing only the first reads `unknown`
  (above); removing both reads `none`, and a full scan by another process
  can then clear `reconcile_pending` while the watcher still recovers.
  Never remove these files while a watcher runs.
- A watcher built before the live lock takes only the first lock; it reads
  `unverified` while it holds it and `none` otherwise.
- A second watcher of the same repository waits up to 1 s for the live lock
  and then runs without it. It keeps the first lock for as long as it runs
  without the live lock, even after it reconciles: while two watchers run, and
  after the first exits, status reads `unverified` with `reconcile_pending`,
  never `watching` or `none`. It tries for the live lock again only when it
  next reconciles, after a recovery, and then reads `watching`; restart it to
  read `watching` sooner. Each of these tries, at startup and at every
  reconciliation, waits up to 1 s for the live lock and can delay event
  draining and shutdown by as long; a single watcher is unaffected. Both
  watchers flush; their writes are serialized by the database, not by these
  locks.
- One read can be wrong for an instant: a watcher that exits and another that
  starts within the same read, or a watcher whose locks the operating system
  is releasing as it dies, can read `watching` for that read. Nothing is
  persisted, so the next read is right.
- A CLI `check_constraints --strict-freshness` or `safe_to_edit` reads rows
  from a snapshot taken when it opened the database and probes the live
  locks at read time; see [Writer liveness](#writer-liveness).

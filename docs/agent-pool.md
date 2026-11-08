# Agent pool

Rules for coding agents (Claude Code, Codex, others) that work on this
repository in parallel on the owner's machine. `scripts/agent-pool/pool.sh`
enforces the leasing part; both agents use that one script.

## Slots

| Slot | Path | Use |
|------|------|-----|
| `codegraph-worker-a` | `~/Projekti/codegraph-worker-a` | implementation |
| `codegraph-worker-b` | `~/Projekti/codegraph-worker-b` | implementation |
| `codegraph-review` | `~/Projekti/codegraph-review` | review only |

The coordinator works in the primary checkout and is the only writer there.
No other worktrees are created without owner approval; when both workers are
busy, report `BLOCKED_POOL_BUSY` instead.

A slot is **idle** when it is a worktree of the primary repository, its tree is clean (`git status --porcelain
--untracked-files=all` is empty), its HEAD is detached and equal to a freshly
fetched `origin/v2.0`, no process has its cwd inside it, and it has no lease.

## Leasing

Leases live in `~/Projekti/codegraph-evidence/pool/<slot>.lease`.

```sh
scripts/agent-pool/pool.sh status
scripts/agent-pool/pool.sh assess <slot>
scripts/agent-pool/pool.sh acquire --slot auto --task <branch> --agent <name> [--pr N] [--pid PID]
scripts/agent-pool/pool.sh release --slot <slot> --token <token>
scripts/agent-pool/pool.sh review-checkout --slot codegraph-review --token <token> --branch <branch> --head <sha>
scripts/agent-pool/pool.sh prepare --slot <slot> | --all
scripts/agent-pool/pool.sh migrate [--dry-run]     # owner only; see "Version 2 slot records"
scripts/agent-pool/pool.sh unmigrate               # owner only
```

- A lease record carries `version=1`, a non-secret `lease_id`, the slot,
  task, agent, PR, owner, host, an optional PID, the time, and the token. A
  lease written before versioning has no `version` field: it stays
  recognizable and busy. A
  `version` this script does not know, or a lease missing a required field,
  makes the slot `inconsistent`, and `release`, `review-checkout` and
  `reclaim` refuse it; such a lease is never reinterpreted or removed on the
  owner's behalf. `status` still prints its fields, and every slot reports
  `lease=present`, `lease=absent` or `lease=unknown`, so a reader never
  mistakes an unreadable record for no record at all: `inconsistent` on its
  own cannot tell them apart. A lease is called absent only when the lease
  directory can be searched (or does not exist and its nearest existing
  ancestor can be searched); otherwise `status` refuses and `assess` reports
  the lease as unknown. A dangling link at the lease path counts as a
  present lease everywhere: `status` and `assess` report it, and `acquire`
  and `prepare` refuse the slot without touching it. Until the recovery protocol lands, a corrupted or unsupported lease
  has no `pool.sh` remedy at all — `release` and `reclaim` both refuse it, and
  clearing it means inspecting the file by hand.
- `assess <slot>` prints read-only evidence about one slot: its state, the
  lease without its token, whether any process has its cwd there, counts of
  changed, untracked and ignored files, HEAD, branch, and whether HEAD is in
  the base or held by any local or remote-tracking ref. It never fetches,
  locks, or changes anything, and it authorizes nothing: recovering a lease still needs
  the owner. Every unavailable observation prints as unknown, never as
  absence.
- Every check fails closed: a failed fetch, a Git error, a slot that is not
  a worktree of this repository, or process inspection that cannot run
  (`lsof` missing or failing) leaves the slot unleased and not free.
- `acquire` fetches `origin`, revalidates that the slot is idle, then creates the lease
  atomically (written in full, then hard-linked into place; `link` fails if a
  lease exists). Exactly one of several concurrent callers wins; a lease is
  never overwritten. It prints the ownership token; keep it. `--slot auto`
  takes the first idle worker or exits 3 with `BLOCKED_POOL_BUSY`.
- `--pid` is optional. It records a long-lived process of the agent
  session so `status` can report the lease `stale-suspected` once that
  process is gone. Without a PID there is no automatic stale detection: the
  lease stays busy until its owner releases it with the token, or an
  administrator recovers it explicitly after verifying the owner is gone
  (see Recovery). There is no time-based expiry, and a missing PID is never
  permission to reclaim a slot.
- The review slot is leased only with `--review`, and only for review:
  read-only commands and the token-validated `review-checkout` operation. No
  edits, commits, stashes or branches. Return it to `origin/v2.0` afterwards.
- Review checkout must use `review-checkout`, which validates the matching lease
  token, review-slot ownership, clean/unused slot, remote branch head, and exact
  SHA before checkout. A refused acquire therefore cannot authorize checkout
  through supported orchestration. This does not prevent a trusted owner from
  running arbitrary Git commands outside this workflow.
- After acquiring: `git -C <slot> switch -c <task-branch> origin/v2.0`, do
  bounded work, commit, open a PR to `v2.0`, get an independent review of the
  exact head SHA. Evidence goes to `~/Projekti/codegraph-evidence/<wave>/`,
  never only in the slot.
- `release` needs the lease's token, no process with its cwd in the slot, a
  clean tree (a failing `git status` is an error, never "clean"), a
  successful `fetch --prune`, a valid `origin/v2.0`, and a HEAD that is
  integrated by one of:
  - an ancestor of `origin/v2.0`;
  - contained in a branch that still exists on `origin`;
  - contained in a local branch, and merging it into `origin/v2.0` (`git
    merge-tree`) yields exactly the `origin/v2.0` tree. This is the
    squash-merge case where GitHub deleted the PR branch: every change is in
    the base, and the commits stay reachable from the local branch, which
    release keeps. A detached commit that no local branch holds is never
    released on content alone.

  It then detaches the slot at `origin/v2.0` and removes the lease (in a
  migrated pool: replaces it with a free record). Any failure keeps the
  lease and the work. The task branch is kept.
- `prepare` moves idle, unleased slots forward to a freshly fetched
  `origin/v2.0` after merges, so they can be leased again. Under the slot
  lock it requires: a valid worktree of the repository, no lease, no process
  in the slot (and process inspection that works), a detached HEAD, a clean
  tree, a successful fetch, a valid base, and a HEAD that is an ancestor of
  the base. It then runs `git switch --detach <base>` and verifies the result.
  A slot already at the base is left as is. Anything else is refused and the
  slot is left unchanged. `--all` prepares each slot independently; it is
  not all-or-nothing, and exits 1 if any slot was refused. A slot whose lock
  is held by another `pool.sh` run is refused, not waited for.

## States

`status` reports each slot as:

- `free`: idle and unleased.
- `busy`: leased.
- `stale-suspected`: leased, the recorded process on this host is gone, and
  no process uses the slot. Never freed automatically.
- `inconsistent`: unleased but not idle (dirty, on a branch, behind
  `origin/v2.0`, or in use by a process), or the lease is malformed or of an
  unsupported schema version. The `lease=` line says which of the two it is.
  Treat as busy; its owner is the task named by its branch or open PR.

There is no time-based expiry.

Every lease change takes a per-slot lock (`pool/.<slot>.lock`, a directory
created with `mkdir`). The lock is advisory: it serialises `pool.sh` runs
and nothing else, so it does not fence a process that writes to the slot or
the lease directory without taking it. A lock left behind by a crashed run
blocks the slot and is reported; remove it by hand only after checking that
no `pool.sh` process is running. `migrate` and `unmigrate` take every slot
lock, so one held lock refuses them.

## Recovery

- `reclaim` is disabled: it exits 1 and changes nothing, in every pool. It
  freed a slot on PID death alone and wrote the token into the archive file
  name and `audit.log`. A stale-suspected lease stays until its holder
  releases it; an orphaned one needs the owner (CG-150 recovery protocol).
  Copies of `pool.sh` older than this change still offer it; do not run them.
- Unfinished work in a slot is archived before the slot is recycled, to
  `~/Projekti/codegraph-evidence/<slot>-recovery-<date>/`: HEAD,
  `status --porcelain=v2`, `diff --binary HEAD`, raw changed and untracked
  files, `SHA256SUMS`, `RESTORE.md`. Verify by applying the patch to a clean
  export of HEAD and byte-comparing.
- Never `reset --hard`, `clean`, `stash`, or `worktree remove --force` a slot.
  If safe recovery is impossible, stop and get explicit owner approval.
- Never recycle a slot whose HEAD is an open PR's head or carries its commits.
  Old worktrees are removed with `git worktree remove` only after their
  content is verified integrated and the owner approved the exact list.

## Recovery state machine

`scripts/agent-pool/recovery_state.py` models orphan lease recovery
(CG-150, phases 2a and 2b-1). It is a library with tests and nothing else.
`pool.sh`, the coordinator and the campaign do not run its state machine
(`pool.sh` only borrows its record parser, see "Version 2 slot records"), and
it has no command-line entry point. It cannot quarantine, reclaim or reassign
a real slot, and it never reads or writes the pool: the caller passes the live slot
record as text and does any replacement itself.

One recovery operation, named by an operation ID, moves one transition at a
time:

| From | To | Requires |
|---|---|---|
| (none) | `ACTIVE` | adoption of the live lease under a new operation ID; every earlier operation of the slot must be terminal |
| `ACTIVE` | `RECOVERY_REQUESTED` | an authorization; the live record is still the adopted lease |
| `ACTIVE` or `RECOVERY_REQUESTED` | `CANCELLED` | an authorization; the live record is the adopted lease unchanged, or proves it released; never after quarantine |
| `RECOVERY_REQUESTED` | `QUARANTINED` | the owner's authorization, the live record still the adopted lease, and a caller hook that replaces it with a quarantined record; the slot generation moves on |
| `QUARANTINED` | `QUIESCENCE_VERIFIED` | the owner's quiescence evidence, by the method the authorization named (containment teardown, owner-controlled quiescence or the reboot runbook), accounting for old coordinators, wrappers, escaped descendants, remote writers and automatic restarts |
| `QUIESCENCE_VERIFIED` | `PRESERVED` | a complete inventory of dirty, untracked and ignored files and unpublished commits, each with a verified copy |
| `PRESERVED` | `READY_FOR_REASSIGNMENT` | a final check showing the slot clean, unleased and at the preserved HEAD |

`CANCELLED` and `READY_FOR_REASSIGNMENT` are terminal for the operation.
`READY_FOR_REASSIGNMENT` leaves the slot record quarantined; nothing in this
phase reassigns it. A cancelled operation cannot resume: a new recovery needs
a new operation ID and a fresh adoption.

Slot records it reads:

- Version 1 leases (`version=1`, `token=`).
- Version 2 records (`version=2`, `slot`, `generation`, `state=free|leased|quarantined`).
  A leased record carries `lease_id`, the holder fields and `lease_token=`; a
  quarantined record carries `recovery_op`, the holder fields and exactly one
  of `lease_id` or `legacy_digest`, never a token. A version 2 record with a
  `token=` line is refused. Free and quarantined records are never adopted.
- Leases written before versioning, only when the owner explicitly asks for a
  legacy adoption. Their identity is the SHA-256 of the record without its
  `token=` line. It proves only that the bytes other than the token did not
  change, not that the holder is gone, quiescent or consents. Before
  quarantine the live token is also checked against the stored verifier, so a
  re-acquired lease with the same other bytes is not taken for the adopted one.

Unknown versions, states or fields, repeated fields, any character that is
not printable ASCII and missing required fields are refused. Error messages never quote a value from
the record.

Bindings:

- Every authorization and every piece of evidence names the slot, the
  operation, the transition, the adopted identity (`identity_kind` and
  `identity`), the slot generation, and the store sequence number of the state
  it leaves. A copy from another slot, operation, transition, generation or
  step is refused.
- Request and cancel need the same authorization class: once pool.sh gates on
  recovery, a request blocks the holder's own release.
- The only trust level accepted is `same-uid-attestation`. It records that a
  process running as the owner's user attested the decision. A 0600 file, a
  TTY or a same-user file is not proof of owner approval. No signed level
  exists until a verifier and a trust anchor the agents cannot write exist.
- Before quarantine the live record must be the adopted lease at the adopted
  generation. After quarantine it must be this operation's quarantined record
  at the new generation; the original lease bytes are never compared again.
- Every step after quarantine also needs the recovery token the caller chose
  at quarantine. It must not be the lease token. Tokens are stored only as
  salted HMAC verifiers.
- A missing PID, a stale heartbeat, no process cwd or a rotated token is
  never quiescence evidence. The module records the owner's evidence and
  checks that it is complete. It does not observe processes itself.

Storage and ordering:

- Records live in `<root>/<slot>/<operation>/` under a root the caller names,
  whose parent must exist. Directories are created 0700 and files 0600; a
  umask can only narrow that. A symlink, a non-regular file, another owner or group or other
  permission bits make the record corrupt.
- Every state and marker write goes to a private temporary file, is flushed
  (`F_FULLFSYNC` on macOS, `fsync` elsewhere), renamed over the destination,
  and the directory is flushed. Journal appends are flushed. A directory's
  parent is flushed every time it is ensured, not only when it was created.
- Adoption builds the operation in a private `.adopt-*` directory and
  renames it into place, so an interrupted adoption leaves no operation; an
  unpublished `.adopt-*` directory is ignored and never removed.
- A transition appends an intent, replaces the state, appends a completion,
  and for a terminal state then writes the `TERMINAL` marker. The quarantine
  intent also names the token-free digests of the live record before and
  after; between the intent and the state the caller's hook must replace the
  live record with the expected one only if it still equals the old one, and
  return the text it wrote. Anything else leaves the quarantine unfinished.
- A trailing intent without a completion leaves the operation unfinished,
  whether or not the state was replaced. While it exists, cancel, new
  adoptions and every other transition are refused; only the exact retry of
  the same transition proceeds, reusing the intent. For the quarantine,
  `reconcile` compares the live record with the intent: the old record means
  the replacement did not happen and the retry may run it; the new record
  means it did and the retry may only finish; anything else is corrupt and
  needs the owner.
- A storage failure, a hook failure or a crash never leaves a free, ready or
  terminal record and never reuses a generation.
- The journal is replayed on every read. It must alternate intents and
  completions from adoption, one legal step at a time, with the right slot
  generation and operation, and each completion must repeat the intent before
  it. Nothing may follow a terminal state.
- `terminal` accepts the marker only when the state and journal agree with
  it; a marker never overrides an inconsistent record. A terminal state whose
  marker is missing is not terminal until the same transition is repeated.
- Without a held lock the store takes its own `mkdir` lock per slot. A caller
  that already holds the pool's slot lock passes it as `held_lock`; the store
  then checks it exists and never takes or removes it. A lock left by a crash
  is never broken automatically.
- The read-only queries do not take the lock. A read during a transition can
  report an operation as unfinished or corrupt, never as ready.
- Windows is refused outright: the durability guarantees above cannot be met
  there. It has not been run on Windows.

Tests use disposable stores and synthetic slot records only:
`python3 -m unittest test_recovery_state` from `scripts/agent-pool`.

## Plane tracking

The coordinator keeps the Plane ticket of every PR in step with GitHub.

- Sync after each completed review, merge, acceptance audit, and each
  meaningful change in the remaining acceptance criteria.
- One coordinator-owned comment per ticket and PR, tagged
  `codegraph-coordinator-sync/v1 isink17/codegraph#<PR>`, updated in place.
  Comments without that tag are never edited. More than one tagged comment
  for the same PR is an error to reconcile by hand.
- The comment states: PR URL, exact reviewed head, verdict and blocking
  count, test commands with their actual results and who ran them, merge
  status and squash SHA, related acceptance criteria, remaining work and
  blockers, platform verification, evidence paths, and the sync time.
- A sync counts only after the persisted comment is read back from the API.
- Check the PR-to-ticket mapping before writing, and check that Plane is
  reachable before merging. A failed post-merge sync keeps the merge, is
  logged as unsynchronized in the evidence directory, is retried, and stops
  further merges until it reconciles or the owner grants an exception.
- A merge or an approving review never completes a ticket. `Done` needs every
  acceptance criterion evaluated with evidence, blockers resolved or
  dispositioned, and the owner's approval of that transition. Otherwise the
  ticket keeps its state.
- Plane credentials never appear in logs, commits, comments or reports.

## Version 2 slot records

Phase 2b-2a taught `pool.sh` to read version 2 slot records; phase 2b-2b adds
the writes behind the migration marker. No live pool has been migrated: that
is a separate, owner-run cutover.

**Strict parsing.** A lease file that may be version 2, or is ambiguous
about its version, is never read in bash. It is handed to
`scripts/agent-pool/pool_record.py`, which applies the rules of
`recovery_state.parse_record`. That covers any line starting `lease_token=`,
`state=`, `generation=`, `recovery_op=` or `legacy_digest=`, more than one
`version=` line, and any `version=2...` line. Legacy, version 1 and other
unknown-version leases keep the bash path unchanged.

The helper interface is:

    "$PYTHON" -I -B pool_record.py inspect <slot> <path>

- `PYTHON` is `CG_POOL_PYTHON`, or `python3` on `PATH`. Python 3.9 or later
  is required; it has been tested with 3.9.6 (macOS system) and 3.14.5. The
  canonical tool bundle of the cutover will pin the interpreter path the same
  way.
- `-I` ignores the environment and user site; `-B` writes no bytecode.
- Output is `key=value` lines, the first one `record=`:
  - `record`: `absent`, `legacy`, `v1`, `v2` or `invalid`;
  - `state`: `free`, `leased` or `quarantined`;
  - `generation`;
  - `lease_id` or `legacy_digest`;
  - `recovery_op`;
  - `public.<field>` for the holder fields;
  - `reason` for an invalid record, never quoting a record value.
- Exit 0 means classified; 2 is usage; 3 means Python is too old.
- The file is opened without following symlinks, and must be a regular,
  UTF-8 file of at most 64 KiB.
- `pool.sh` discards the helper's stderr. A missing interpreter, any non-zero
  exit, or output that does not start with `record=` makes the record
  `unknown`. It is never parsed in bash instead.

**Pool modes.** The schema marker `$LEASES/.schema` decides, and every
mutator reads it again after taking the slot lock; that reading is the one it
acts on. `migrate` and `unmigrate` change the marker only while holding every
slot lock.

| Marker | Mode | Mutators |
|---|---|---|
| absent | version 1 (unmigrated) | as before; a version 2 or ambiguous record or recovery state is refused unchanged |
| `2` (trailing newlines ignored) | migrated | write version 2 records through `pool_record.py`; refuse every version 1 or legacy lease, missing record and recovery state |
| anything else, a symlink or not a regular file (including `unmigrating`) | unknown | every mutator refuses |

`reclaim` is disabled in every mode. Acquire metadata (`--task`, `--agent`,
`--pr`, `--owner`, `--pid`) must be one line of printable ASCII in every mode,
because `status` prints it and readers split lines on more than `\n`.

**Python is required in a migrated pool.** Every read and write of a version 2
record runs `pool_record.py` under Python 3.9 or later. A missing, failing,
too old or misbehaving interpreter refuses the operation; nothing falls back
to bash. The interpreter is trusted: it is part of the tool bundle.

**Writes.** Under the slot lock `pool.sh` holds, `pool_record.py` checks that
the lock exists, the marker is `2`, the slot has no recovery state and the
record is a regular version 2 file, then the compare-and-swap preconditions:

| Command | Precondition | Result |
|---|---|---|
| acquire | free record at the generation read under the lock; the slot idle after a fresh fetch | leased record at the next generation with a new `lease_id` and `lease_token`; prints `leased <slot> token=<hex32>` |
| release | leased record whose token matches (constant-time), at the `lease_id` and generation read under the lock; the usual release checks | free record at the same generation |
| review-checkout | leased record whose token matches | no record change |
| prepare | free record | no record change |

A write is a private `O_EXCL|O_NOFOLLOW` 0600 temporary file, flushed
(`F_FULLFSYNC` on macOS, `fsync` elsewhere), renamed over the record (or
linked, for a record that must not exist yet), then the directory flushed.
A failure before the rename leaves the record unchanged. A directory flush that
fails after the rename is reported (helper exit 4): the new record is in place
but may not survive a power loss. For acquire that can leave a lease no caller
holds a token for; the slot stays busy, `--slot auto` stops instead of trying
the next worker, and the owner inspects. acquire prints the token only after
the flush, so one generation is never issued to two token holders.

The caller's token reaches the helper on stdin, never in its arguments. No
token appears in status, assess, messages, file names or logs.

**Migration.** Owner-run, never by an agent or the coordinator.

- `migrate --dry-run` takes no lock, does not fetch and writes nothing; it
  reports, per slot, what `migrate` would do and exits 1 if it would refuse.
- `migrate` holds every slot lock, fetches, and requires every slot to be idle
  with no recovery state and either no record or the free record of
  generation 0 (left by an interrupted run). Anything else, including a version
  1 or legacy lease, refuses the whole migration and writes nothing: a leased
  slot is never rewritten, so its holder must release it first. It then
  creates each free record, verifies them, and writes the marker last.
- An interrupted migration leaves free records without a marker. They fence
  their slots in the unmigrated pool; running `migrate` again completes it,
  `unmigrate` rolls it back.
- After migration an absent record never means free: `status` prints
  `record_state=missing` and every mutator refuses. No command restores a
  missing record, because nothing records its last generation; the owner
  decides.

**Rollback.** `unmigrate` holds every slot lock and accepts only the free
record of generation 0 (no lease was ever issued on that slot since
migration), plus, when the marker is not `2`, an absent record or a version 1
or legacy lease. Anything else refuses and changes nothing: a leased,
quarantined or released (generation 1 or later) record, other content, a
missing record under marker `2`, recovery state or an unknown marker. It then
rewrites the marker to `unmigrating`, which makes every mutator refuse,
removes the records, and removes the marker last. A run that fails part way
leaves the pool refusing every change until `unmigrate` is run again.

Old `pool.sh` copies are fenced by the record's presence (see below), not by
the marker: copies older than phase 2b-2a still acquire an absent path, so
none of them may run against a migrated pool.

**Status rendering.** Every detail key is printed at most once per slot, no
token or `lease_token` is ever printed, and the status exit code is unchanged.

| Record | Header | Detail lines |
|---|---|---|
| v2 `free` | `free` only when the slot is otherwise idle and has no recovery state; otherwise `inconsistent` with the reasons | `lease=absent`, `record_state=free`, `generation=` |
| v2 `leased` | `busy` or `stale-suspected`, on the same evidence as version 1 | `lease=present`, `record_state=leased`, `version=2`, `slot=`, `lease_id=`, the holder fields, `generation=` |
| v2 `quarantined` | `inconsistent`, never a new header word | `lease=present`, `record_state=quarantined`, `recovery_op=`, `version=2`, `slot=`, `lease_id=` or `legacy_digest=`, the holder fields, `generation=` |
| refused by the parser | `inconsistent` | `lease=present`, `record_state=invalid`, and the allowlisted public fields of its printable lines |
| helper unavailable or failing | `inconsistent` | `lease=present`, `record_state=unknown`, the same allowlisted fields |
| no record, schema marker `$LEASES/.schema` present (valid `2` or not) | `inconsistent` | `lease=absent`, `record_state=missing` |
| recovery state: `$LEASES/recovery` exists and holds an entry for the slot, or cannot be searched | never `free` | `recovery=unknown` |

For a v2 `free` record, `lease=absent` means there is no active lease. The
permanent record file itself is present.

`assess` prints the same state, plus `pool-schema-marker:` and `recovery:`
lines, and the status detail lines for a strictly parsed record.

**Fencing.** `acquire`, `release`, `prepare` and `review-checkout` refuse and
change nothing when any of these holds. They exit 1, except that
`acquire --slot auto` exits 3 (`BLOCKED_POOL_BUSY`) when every worker is
refused for its record or recovery state, as for any other occupied lease
path:

- the marker is unknown. `acquire` and `prepare` refuse before touching any
  slot, and the marker is read again under each slot lock;
- the slot has recovery state;
- without the marker: the slot holds a strictly parsed record, including a v2
  `free` record;
- with marker `2`: the record is missing, unreadable, invalid, quarantined, a
  version 1 or legacy lease, or not in the state the command needs.

Every `pool.sh` that ever shipped refuses every mutator on version 2 records
and leaves them unchanged: `test_pool_v2.History` extracts each one from git,
and `test_pool_v2_writes.History` runs each against the records this
`pool.sh` writes.
The pre-version-1 copies accept any record that carries a `token=` line, so a
version 2 record must never contain one.

## Tests

`scripts/agent-pool/pool_test.sh` runs against a throwaway pool and never
touches the live one. `test_pool_v2` covers reading version 2 records against
the current and every historical `pool.sh`, the strict reader and the
coordinator and campaign consumers. `test_pool_v2_writes` covers migration,
rollback, every version 2 write, races, fault injection at each persistence
point, interpreter failures, every historical `pool.sh` against the records
the writer produces, and the consumers. All run in throwaway pools:
`python3 -m unittest test_pool_v2 test_pool_v2_writes` from `scripts/agent-pool`.
Hosted CI runs `test_recovery_state`, `test_pool_v2`, `test_pool_v2_writes`
and `pool_test.sh` on Linux (job `pool-safety`); the coordinator and campaign
suites run only locally on macOS. Windows is not run.

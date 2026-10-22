# Agent campaign supervisor

`scripts/agent-coordinator/campaign.py` keeps a development campaign moving
for a wall-clock budget (default 120 minutes) without further prompts: it
plans owner-authorized work whose Plane ticket is open, drives
`coordinator.py` pass after pass, merges approved PRs through the existing
merge authority, and synchronises Plane. It adds no second coordinator: task
lifecycle, leases, agents, continuation and review stay in `coordinator.py`
(`docs/agent-coordinator.md`) and `scripts/agent-pool/pool.sh`
(`docs/agent-pool.md`).

```
owner ── authorization.json ──┐                Plane (read; sync comments only via plane-sync.sh)
                              ▼                   ▲
                     campaign.py (PM loop) ───────┘
                      │  budget · STOP · plan · merge · sync · live stream
                      ▼
                coordinator.py run --once  (lifecycle authority, state in <dir>/coordinator)
                      │
          pool.sh (only lease authority)
        ┌─────────────┼──────────────────┐
   worker-a        worker-b          review slot
   implementer     implementer       reviewer (read-only sandbox)
   codex           codex             codex, separate process
                      │
     merge authority: merge-gate.sh --merge   or   merge-exception.sh --merge (+ ci-local.sh)
```

## Roles

| Role | Who | Writes | Isolation |
|---|---|---|---|
| PM | `campaign.py` (deterministic) and `campaign.py propose` (one read-only Codex run) | campaign state, plans, evidence | owner process; the proposal run is sandboxed read-only |
| Worker A / B | implementation agent launched by the coordinator in a leased worker slot | its slot, its branch | coordinator sandbox; private `CODEX_HOME`; no keychain, state or lease reads |
| Reviewer | separate agent process in the review slot, at the exact PR head | nothing (scratch only) | read-only sandbox; sees only the task, the diff and the tree, never the implementer's prompt or output |
| Auditor | `campaign.py status`, `coordinator.py status`, `pool.sh status` | nothing | read-only |

An implementer never reviews its own work: review is a new process, a new
session, another slot, with its own prompt. Native Codex subagents
(`multi_agent`, stable in 0.163.0) are not used for any role: they run
inside the parent's process and inherit its sandbox and approval policy, so
a custom agent's `sandbox_mode` is no isolation boundary.

## Authorization

`<dir>/authorization.json`, written by the owner, is the only source of work
and merge rights:

```json
{
  "campaign": "c19",
  "expires": "2026-10-31",
  "merge": {"mode": "exception",
            "exception_dir": "/Users/ivan/Projekti/codegraph-evidence/20261022-c19/exception"},
  "tickets": {
    "CG-12": {"tasks": [
      {"id": "cg12-fixture", "description": "…", "agent": "codex", "review": {"agent": "codex"},
       "scope": ["internal/store/lua_*_test.go"], "acceptance": ["…"],
       "validate": ["go test ./internal/store -run Lua"], "max_attempts": 3, "depends_on": []}
    ]}
  }
}
```

- Tasks are coordinator task specs (`docs/agent-coordinator.md`). A task id
  may appear once; the file is rejected otherwise, and replanning never
  duplicates a task.
- A task is planned only while its ticket's Plane state group is
  `backlog`, `unstarted` or `started`. A pending task whose ticket becomes
  completed or cancelled is cancelled (`interrupted`); running work is left
  alone.
- `merge.mode`:
  - `none` (default): stop at `awaiting_ci` / `ready_for_owner`; the owner merges.
  - `gate`: `ready_for_owner` tasks are merged with the `merge-gate.sh`
    committed on the remote base (`--merge`, after a verification pass).
  - `exception`: the owner's temporary local-CI exception. `exception_dir`
    holds the owner's `merge-exception.sh` and `ci-local.sh` and the
    campaign's ruleset snapshot (`../ruleset-24420032-start.json`), as in
    `codegraph-evidence/20261021-campaign14/exception/`. The supervisor runs
    `ci-local.sh` on the exact approved head in a worker it leases through
    `pool.sh` (sandboxed, Go file packages as PR tests), then
    `merge-exception.sh PR HEAD BASE REVIEW CI_DIR` to verify and again with
    `--merge`. That script owns every merge rule: expiry, review header,
    workflow disabled, local CI steps, no failed or pending checks, ruleset
    snapshot, minimal relaxation, restoration and equality check, squash
    verification. The supervisor never touches rulesets itself.
- `expires`: after this date the file is refused.
- Every task and its review must use `"agent": "codex"`: only Codex runs in
  the coordinator's strict sandbox (Claude Code needs the keychain,
  `command` agents run owner argv).
- The file is re-read on every pass. The authorized set is the tasks of
  tickets listed now whose Plane group is open; it is written to
  `<dir>/coordinator/authorized.json`, the coordinator's gate (see
  `docs/agent-coordinator.md`). An unreadable, invalid or expired file, or
  no answer from Plane, authorizes nothing (fail closed): no launch,
  continuation, review, branch update or merge, while running agents
  finish. Removing a task records `deauthorized` and stops new work on it;
  its branch, commits, PR and evidence are kept, and it continues if it is
  authorized again. The merge step re-reads the file once more right
  before merging. Plane sync of a merge that already happened is
  reconciliation and still runs.

`campaign.py propose --dir D CG-N` (PM decomposition) runs one read-only,
sandboxed Codex process on the ticket's Plane text and writes
`<dir>/proposals/CG-N-<time>.json`, validated as a task list. It is never
planned automatically: the owner reviews it and copies the steps into
`authorization.json`.

## Loop

Each pass, until STOP, the deadline, a halt, or idle:

1. Read every authorized ticket from Plane. Plane unreachable: nothing is
   planned or merged this pass; running agents and reviews continue.
2. Plan new tasks of open tickets (`coordinator.py plan`), cancel pending
   tasks of closed ones.
3. `coordinator.py run --once`: reconcile, continue unfinished agents,
   review, publish, release.
4. Stream new agent output to the terminal.
5. Merge (mode `gate`/`exception`): retry unsynchronised Plane comments
   first (at most once a minute); update PR branches left behind the base by
   an earlier merge (`gh pr update-branch`, a merge commit, never a force
   push, at most 3 times per task; the new head voids the review); then
   merge at most one approved task and synchronise its ticket with
   `plane-sync.sh` (`CG_CAMPAIGN_PLANE_SYNC`), which reports success only
   after API read-back.
6. Idle when no task can move without the owner and nothing authorized is
   left to plan.

Safety rules:

- Completion is never the agent's claim: the coordinator's checks decide
  implementation, an independent review decides approval, GitHub decides
  merged.
- At most `MAX_MERGE_ATTEMPTS` (2) merge attempts per task; failed local CI
  counts as one. A worker busy for local CI is not an attempt.
- The `--merge` step runs in its own session with its output in its log,
  and Ctrl-C/SIGTERM wait until it exits. A `merging` marker (PR, head,
  base) is saved before it starts. Its outcome is never taken from the
  script's output or from a missing acknowledgement: after the step, and
  at start-up for a marker a crash, power loss or kill left, the
  supervisor reconciles from GitHub (`gh pr view --json
  state,headRefOid,mergeCommit`) and, in exception mode, a read-back of
  ruleset 24420032 compared with the campaign snapshot. Merged at the
  approved head with the ruleset intact: recorded and queued for Plane
  sync (the queue is saved before the sync, so a crash between merge and
  sync retries the sync, never the merge). Still open at that head with
  the ruleset intact: nothing happened. Anything else, including an
  unreadable GitHub or ruleset: `halt`.
- Merge script exit 3 (ruleset restoration failed) or an uncertain outcome
  sets `halt` in `campaign.json`: the supervisor exits with 1 and refuses
  to merge on every later run until the owner has verified the PR, ruleset
  24420032 and Plane and removed `halt` (and `merging`) from
  `campaign.json`.
- A task whose ticket is no longer open in Plane is not merged.
- ci-local output is untrusted: it is copied without following links,
  iteratively, within limits (depth 12, 5000 files, 256 MiB per file,
  1 GiB in all); `summary.json` must be at most 1 MiB, well-formed and of
  the expected types. Links or exceeded limits reject the evidence of that
  head on that base permanently (`ci_rejected`, kept across restarts); an
  I/O error only fails the attempt. Agent output, Plane and gh responses
  are parsed with the same bounds; malformed input is never success and
  never crashes the supervisor.
- A failed post-merge sync is recorded in `unsynced`; no further merge
  happens until it succeeds. The merge itself is kept.
- Plane is written only by `plane-sync.sh` (one marked comment per ticket
  and PR). Ticket states are never changed: a merge does not complete a
  ticket. The local queue holds task attempts, leases, dependencies and
  merge records only, never ticket content.

## Files

| Path | Content |
|---|---|
| `<dir>/authorization.json` | owner-written, read-only to the supervisor |
| `<dir>/campaign.json` | deadline, task→ticket map, merge records, `unsynced`, `halt`, open ci-local lease marker; atomic writes |
| `<dir>/events.jsonl` | append-only, fsynced campaign events |
| `<dir>/coordinator/` | the coordinator's own state (`state.json`, `events.jsonl`, tokens, runs) |
| `<dir>/plans/` | each planned batch as submitted |
| `<dir>/merges/pr-N/<head>/` | ci-local evidence, merge log, Plane sync body and log |
| `<dir>/STOP` | stop request |

`campaign.lock` (flock) admits one supervisor per directory. Agents can
neither read nor write `<dir>` (it is passed to the coordinator as
`CG_COORD_DENY_READ`). The supervisor refuses to start when `<dir>`, the
exception directory or `plane-sync.sh` lies where agents may write
(`/private/tmp`, `/dev`, `CG_COORD_SANDBOX_WRITABLE`). `ci-local.sh` is
passed to the sandbox as text because the evidence root is unreadable
there. Proposals may use only the PM fields with `codex`
agents; a `command` agent is refused.

## Operating

Live terminal (inside a plain terminal, or as a background terminal of an
interactive Codex session, where `unified_exec` keeps it running and `/ps`
lists it):

```sh
D=~/Projekti/codegraph-evidence/20261022-c19
python3 scripts/agent-coordinator/campaign.py run --dir "$D" --minutes 120
```

Agent output is streamed from every run's `output.log`, and the position
in each is saved (`<dir>/stream-offsets.json`), so a restarted supervisor
continues where the last one stopped, without loss or repetition; a
damaged offsets file resumes at the end. The logs stay authoritative.
Lines are prefixed `[time] [task:kind@slot]` for agent activity (commands
with exit codes, agent messages), `[coordinator]`, `[pm]`, `[plane]`,
`[task:ci-local]`, `[task:merge]`. Every byte also stays on disk: agent
`output.log` in the coordinator run directory, merge and CI logs under
`merges/`, events in both `events.jsonl` files. Long test subprocesses are
part of an agent run or of ci-local, not separate agents.

- Stop: `campaign.py stop --dir "$D"` (or `touch "$D/STOP"`). The supervisor
  exits after its current step; running agents finish; nothing is launched
  or continued. Remove the file to resume.
- Interrupt: Ctrl-C (or SIGTERM) exits with 130; agents keep running. During
  a merge both wait until the merge script exits; the script itself ignores
  them too (only SIGKILL stops it, which leaves `halt` set).
- Resume: rerun the same `run` command. The stored deadline is kept unless
  `--minutes` is given; the coordinator adopts running agents and finished
  ones by their exit files; an interrupted ci-local lease is released or
  reported.
- Status: `campaign.py status --dir "$D"` (works while `run` is active).

## Codex 0.163.0

Verified on this host: `codex exec` with `--json`, `--ephemeral`,
`--dangerously-bypass-approvals-and-sandbox` and a private `CODEX_HOME`
runs inside the coordinator's `sandbox-exec` profile (codex's own sandbox
cannot nest and fails with `Operation not permitted` on its app-server);
`unified_exec`, `multi_agent` and `hooks` are stable. The owner's
`~/.codex/config.toml` is not changed. Codex agents use the default model
of the login unless `CG_COORD_CODEX_CONFIG` names a `config.toml` to seed,
for example:

```toml
model = "gpt-5.6-luna"
model_reasoning_effort = "medium"
```

## Tests

```sh
TMPDIR=$(getconf DARWIN_USER_TEMP_DIR) python3 scripts/agent-coordinator/test_coordinator.py
TMPDIR=$(getconf DARWIN_USER_TEMP_DIR) python3 scripts/agent-coordinator/test_campaign.py
# dry run with the real codex CLI against a throwaway repository (model calls):
CG_DEMO_REAL_CODEX=1 TMPDIR=$(getconf DARWIN_USER_TEMP_DIR) \
  python3 scripts/agent-coordinator/test_campaign.py RealCodex
```

The campaign tests use fake codex, Plane, ci-local, merge-exception and
plane-sync executables against a throwaway remote and pool; they never
reach the live pool, Plane or GitHub.

## Limitations

- Single host, macOS only. Campaigns run Codex agents only.
- The sandbox is an allow-list for reads, not an inventory of secrets:
  anything placed inside an allowed directory (the slot, the repository's
  git directory, tool install prefixes, the Go module cache, `/opt`,
  `/Library`, `/Applications`, `/private/tmp`) is readable to agents. File
  metadata is visible everywhere.
- Codex agents can open TCP connections to port 443 of any host (no
  host-name allow-list in seatbelt). What they could send is what they
  can read: the task's code, toolchains, and their own Codex login.
- A Codex agent can read and use its own login copy.
- Validation and ci-local have no network beyond localhost: modules must
  already be in the Go module cache, and tests needing the internet fail.
- `ci-local.sh` runs PR code; tests could forge its evidence files inside
  their sandbox. Review of the code is the guard, as in the manual
  campaigns.
- Branch updates can conflict; such a PR stays `blocked` for the owner.
- One merge per pass, one review at a time, two implementers.
- `halt` and a ci-local lease whose token was lost need the owner.
- Revocation does not stop an agent already running; it stops what comes
  after it.

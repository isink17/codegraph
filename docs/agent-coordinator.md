# Agent coordinator

`scripts/agent-coordinator/coordinator.py` runs bounded development tasks on
the agent pool (`docs/agent-pool.md`): implementation on the two workers,
independent review in the review slot, and PR/CI tracking on GitHub up to
the point where the owner can merge. It is one Python standard-library file
with no dependencies, no daemon and no database.

It does not own slots. Every lease is acquired, inspected, prepared and
released through `scripts/agent-pool/pool.sh`; the coordinator never writes
a lease file, never reclaims a lease and never creates a worktree. It never
merges, force-pushes or runs `merge-gate.sh --merge`.

## Commands

```sh
C=scripts/agent-coordinator/coordinator.py
$C plan tasks.json          # validate and queue tasks
$C run [--once] [--poll S]  # reconcile, then schedule until nothing can move
$C resume                   # alias of run, for use after a crash or restart
$C status                   # task states plus pool.sh status
$C cancel <task-id>         # stop a task's agent; work and lease are kept
```

`cancel` needs the coordinator lock. It cannot run while a `run` loop is in
the foreground: stop the loop (Ctrl-C; agents keep running), cancel, then
`resume`. Cancellation is therefore never immediate while a loop runs.
`cancel` sends SIGTERM to the agent's process group and waits
`CG_COORD_CANCEL_WAIT` seconds (default 5). If any process of the group is
still alive, it prints `cancellation incomplete`, exits 1 and leaves the
task in its state with its lease and work; the slot is not reused. The task
becomes `interrupted` (never published) once `resume` sees the group gone.

`run` without `--once` stays in the foreground while agents run and polls
every `--poll` seconds (default 30). It exits when no agent is running and
no further step is possible (waiting for review capacity, CI or the owner).
Exit code 1 means a task is failed or interrupted, or the command was refused.

Run it from a checkout whose `scripts/agent-pool/pool.sh` is the current
`v2.0` version, or point `CG_COORD_POOL` at one.

## Task specification

```json
{"tasks": [{
  "id": "lua-fixture",
  "description": "Add a Lua resolver fixture for nested requires",
  "base": "v2.0",
  "branch": "task/lua-fixture",
  "depends_on": [],
  "agent": "claude",
  "scope": ["internal/parser/treesitter/lua_*", "internal/store/lua_*_test.go"],
  "acceptance": ["nested require resolves to the module file", "no new unresolved edges"],
  "validate": ["go test ./internal/store -run Lua", "go vet ./internal/..."],
  "allowed_tools": ["Bash(go test:*)", "Bash(go vet:*)"],
  "review": {"agent": "claude", "focus": "resolver correctness"},
  "prompt": "optional extra instructions"
}]}
```

| Field | Meaning |
|---|---|
| `id` | unique, `[A-Za-z0-9._-]`, ≤64 chars |
| `description`, `acceptance`, `validate`, `scope` | required |
| `base` | integration branch, default `v2.0` |
| `branch` | task branch, default `task/<id>`; unique per queue |
| `depends_on` | task ids that must be **completed** (PR merged) first; cycles are rejected |
| `agent` | `claude`, `codex` or `command` (argv in `command`; used by tests) |
| `scope` | `fnmatch` patterns (`*` also matches `/`). Every added, changed or deleted path must match one (renames count as both paths). A task whose scope may overlap another started, unmerged task waits until that task's PR is merged. |
| `validate` | shell commands the coordinator runs in the slot after the agent exits |
| `allowed_tools` | extra Claude Code tool rules for the implementer |
| `review` | reviewer `agent` (`claude`, `codex`, `command`), optional `focus` |
| `publish` | default `true`; `false` stops after validation and keeps the lease |
| `pr` | adopt an existing open PR: the task starts at review |

Review is always required: the merge gate demands it.

## Lifecycle

```
pending ─▶ running ─▶ awaiting_review ─▶ reviewing ─┬▶ awaiting_ci ─▶ ready_for_owner ─▶ completed
   ▲                                                  │      (MERGE_READY)          (PR merged by owner)
   └──── review_changes_required ◀────────────────────┘
blocked · failed · interrupted   (need the owner; nothing is retried automatically)
```

1. **pending → running.** Dependencies completed, no started and unmerged
   task with an overlapping scope, fewer than two implementation tasks running. `pool.sh
   prepare --all` moves idle slots to the current base, then `pool.sh
   acquire --slot auto` takes a worker or reports `BLOCKED_POOL_BUSY`; the
   task stays queued. The coordinator creates (or, for rework, switches to)
   the task branch and launches the agent detached, in its own session, with
   the slot as its working directory.
2. **Agent exit.** Success is never inferred from the exit code. The
   coordinator requires exit 0, a clean tree, the task branch still checked
   out, at least one new commit, every changed file inside `scope`, and
   every `validate` command passing (logs kept as evidence). It then pushes
   the branch (never with force), opens a PR to `base` with `gh` if none is
   open, and releases the worker through `pool.sh release`.
3. **Review.** One review at a time. The review slot is leased with
   `--review` (busy: `BLOCKED_REVIEW_BUSY`, the task waits), detached at the
   PR head that GitHub reports, and a reviewer runs in a new session with
   only Read, Grep and Glob; the coordinator writes the full diff against
   `origin/<base>` to a file the reviewer may read. No shell is allowed:
   `git diff --output` would write files. Afterwards the slot must still be clean and at that head,
   otherwise the review is invalid. The coordinator writes the merge-gate
   artifact itself (`PR`, `Reviewed-Head` = the head it checked out,
   `Verdict`, `Blocking`) and releases the slot. `REQUEST_CHANGES` or any
   blocking finding sends the task back to an implementation worker with the
   findings in its prompt.
4. **CI and readiness.** GitHub is the source of truth. On every pass:
   PR merged → `completed`; closed → `blocked`; head differs from the
   approved head → the review is void, back to `awaiting_review`; PR head
   does not contain the current `origin/<base>` → `blocked` (stale base)
   until the owner updates the branch, which changes the head and so
   requires new CI and a new review; any failed, cancelled or timed-out
   check (every check counts, also several with one name) → `blocked` until
   a rerun is green; pending checks or no `ci`
   check → `awaiting_ci`. When all is green, `merge-gate.sh PR HEAD REVIEW`
   (with `--base <base>`) runs in verification mode. The gate is the one
   committed on `origin/<base>` (copied to `<state>/gates/<blob>.sh`), never
   the main checkout's copy, which may be old or modified; `CG_COORD_GATE`
   overrides it. Either must print a usage naming `--base` when run without
   arguments (exit 2), otherwise the task is blocked with `merge gate
   unavailable` and rechecked on every pass; only a pass reports
   **MERGE_READY**. A refusal (for example `mergeable` still `UNKNOWN`) is
   rechecked on every pass.

## Safety contract

- **Leases.** `pool.sh` is the only lease authority. Tokens are written to
  mode-0600 files in `<state>/tokens/` (mode 0700) and appear nowhere else:
  not in `state.json`, events, logs, prompts, PR bodies or review artifacts.
  The coordinator releases only leases whose tokens it holds; `pool.sh`
  refuses any release that would lose work (dirty tree, unpushed HEAD,
  process in the slot). Stale leases are reported, never reclaimed.
- **Exclusive coordinator.** `flock` on `<state>/coordinator.lock`. A second
  coordinator on the same state refuses; a crashed one leaves no lock.
  Coordinators with different state directories are still serialised per
  slot by `pool.sh`'s atomic acquire.
- **Crash recovery.** `run`/`resume` reconciles before acting, and each
  step that a crash could cut in half is a recorded transaction:
  - *Acquire.* Before `pool.sh acquire` the coordinator saves a `starting`
    marker with a fresh transaction id, passed to pool.sh as the lease
    `owner=coordinator:<id>`. pool.sh's output (with the token) and exit
    status go to private files in `<state>/tokens/`. Afterwards, and after
    any crash, the files and `pool.sh status` decide: a lease with that
    owner and a matching token in the output is adopted and the task
    continues; no live pool.sh for the transaction, no slot lock and no
    lease with that owner means nothing was leased, so the marker is
    cleared. Anything else (malformed output, a lease without a readable
    token, a slot lock left by a killed pool.sh) keeps the marker, marks
    the task `interrupted` and names the owner id to inspect. While any
    marker is open, no slot is acquired for any task. A lease is never
    deleted or overwritten.
  - *Launch.* The task is saved as `running`/`reviewing` with its run
    directory before the process exists. The wrapper writes its PID before
    running the agent and the exit status after it, and its command line
    names the run directory. Reconcile adopts a live wrapper it finds that
    way, treats a missing PID file with no live wrapper as proof that the
    agent never started (`interrupted`, lease kept, nothing relaunched), and
    otherwise matches PID **and** start time. A process gone without exit
    status is `interrupted`; while any process of its group is alive it
    counts as running.
  - *Release.* The next status is saved as a `releasing` intent before
    `pool.sh release`. If a crash follows, the lease decides: still ours →
    the intent is dropped; gone → the intent applies, but only after GitHub
    confirms that the PR is open at the published head (a released slot
    proves the release, not that the task finished); otherwise the task is
    `interrupted` for the owner. The worker is never reacquired.
  A missing `state.json` with tokens or events present, or a malformed one,
  is refused and left untouched. `CG_COORD_FAULT=<point>` makes the
  coordinator exit at a named point; the tests use it to cut every step.
- **Processes.** `cancel` signals only a process group whose leader still
  has the recorded PID and start time, or, if the leader has exited, a
  group that still exists (a PID cannot be reused while it names a live
  group). Unverifiable processes are not signalled. Cancellation never
  touches the work tree, branch or lease.
- **Secret isolation.** Agents run as the owner's user, so file modes do
  not hide tokens from them, and prompt instructions are not a boundary.
  On macOS every agent and every validation command runs under
  `sandbox-exec` with reads and writes of `<state>` and the pool's lease
  directory denied; a reviewer may read only its diff directory there. On
  other platforms Claude Code agents are refused; `command` agents
  (owner-written argv) run unsandboxed. The Codex adapter is disabled:
  `codex exec` applies its own Seatbelt sandbox, which cannot nest inside
  ours, and its `read-only` mode still reads every file.
- **Agents.** Launching needs a planned task with scope and acceptance
  criteria. Claude Code runs with `-p --output-format json --session-id
  <uuid> --permission-mode dontAsk --allowedTools …`: tools outside the
  list are denied, never prompted, and no bypass flag is ever passed.
  Reviewers get only `Read Grep Glob` plus `--disallowedTools Edit Write
  NotebookEdit`, and `--add-dir` for the diff file.
  Authentication is the owner's job (`claude login`); the coordinator never
  handles credentials.
- **Git.** No force push, no reset, no clean, no stash, no worktree
  creation, no branch deletion. Failed tasks keep their slot, branch and
  files for the owner.

## State and evidence

`CG_COORD_STATE` (default `~/Projekti/codegraph-evidence/coordinator/`):

| Path | Content |
|---|---|
| `state.json` | task states; written atomically (temp file, fsync, rename) |
| `events.jsonl` | append-only, fsynced event log |
| `tokens/` | lease tokens, 0600; acquire transaction files |
| `gates/` | merge gates copied from `origin/<base>` |
| `runs/<task>-<kind>-…/` | agent output, exit status, validation logs, review artifact |

Other variables: `CG_COORD_POOL` (pool.sh), `CG_COORD_GH` (`gh`),
`CG_COORD_GATE` (merge gate override), `CG_COORD_CANCEL_WAIT`, and
pool.sh's own `CG_POOL_*`.

## Tests

```sh
python3 scripts/agent-coordinator/test_coordinator.py
```

Each test builds a throwaway bare remote, three worktree slots and a lease
directory, and uses a fake `gh`, a fake merge gate and shell-script agents.
It never touches the live pool. Crash windows are cut with
`CG_COORD_FAULT`; `CG_TEST_COORD=<path>` runs the suite against another
coordinator version.

## Limitations

- Single host. One review at a time. At most two implementation tasks.
- Scope overlap is decided on literal pattern prefixes, conservatively; it
  does not inspect the actual diff of other tasks.
- `cancel` cannot run while a `run` loop holds the lock (see Commands).
- Tasks whose scopes overlap never run implementation at the same time.
  Two tasks that both already have an open PR (adopted PRs, or rework) do
  not wait for each other's merge, so their PRs may touch the same files;
  the second to merge needs the owner to update its branch (stale base).
  `status` shows `waiting_on=<task>` for a task held back by an overlap.
- An agent's descendants that leave its process group (a new session) are
  not tracked or signalled.
- `failed`, `blocked` and `interrupted` tasks need the owner: there is no
  retry command and no automatic lease recovery. The owner inspects the
  slot and either fixes and releases the lease with the token in
  `<state>/tokens/`, or uses `pool.sh reclaim`.
- Stale base and failing CI are detected, not repaired.
- The Codex adapter is disabled (see Secret isolation) and has never been
  run end to end.
- The Claude Code adapter's tool list must include every command the agent
  needs (for example `Bash(go test:*)`), via `allowed_tools`.

## Possible later additions

- A Plane adapter that creates tasks from work items and posts status back.
- A `retry` command for failed tasks after owner inspection.
- Diff-based overlap detection.

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
| `max_attempts` | agent executions per implementation round, 1–5, default `1` (see Continuation) |

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
   every `validate` command passing (logs kept as evidence). An agent that
   fails these checks (non-zero exit, dirty tree, no new commit, a change
   outside scope, a failing or dirtying validation) is **continued**: the
   same slot and branch, a new execution whose prompt names the failed check
   and says that the checks, not the agent's claim, decide completion. Up to
   `max_attempts` executions, then `failed`. Leaving the branch or planting
   tool configuration is never continued. While the STOP file exists,
   nothing is launched or continued (a stopped continuation becomes
   `interrupted`). It then pushes
   the branch (never with force), opens a PR to `base` with `gh` if none is
   open, and releases the worker through `pool.sh release`.
3. **Review.** One review at a time. The review slot is leased with
   `--review` (busy: `BLOCKED_REVIEW_BUSY`, the task waits), detached at the
   PR head that GitHub reports, and a reviewer runs in a new session in a
   read-only sandbox: it may write only its scratch directory, `/tmp` and
   `/dev`, not the review slot and not the repository's objects or refs.
   A Claude Code reviewer gets only Read, Grep and Glob; a Codex reviewer has
   a shell, bounded by that sandbox. The coordinator writes the full diff
   against `origin/<base>` to a file the reviewer may read. Afterwards the
   slot must still be clean and at that head,
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
   committed on the remote's `<base>`: it is fetched into a private bare
   repository `<state>/gate-repo` (agents can write the shared object store,
   and git trusts objects it already has), checked against `git ls-remote`,
   and copied to `<state>/gates/<blob>.sh`; never
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
  directory denied; a reviewer may read only its diff directory there.
  Because the coordinator, pool.sh and the owner's own tools (Claude Code,
  Go, git, gh) later run outside the sandbox, writes are denied by
  default. An agent may write only its slot (not its `.git` pointer), the
  repository's objects and branch refs (`refs/heads`, not remote-tracking
  refs, which decide the merge gate and the base), its own worktree admin
  files (not their `config*`, `commondir` or `gitdir`), a private scratch
  directory that becomes its `TMPDIR`, `GOCACHE`, `GOPATH` and
  `XDG_CACHE_HOME` (removed when the run ends; the shared Go module cache
  stays readable), `/tmp`, `/dev`, and directories
  listed in `CG_COORD_SANDBOX_WRITABLE` (colon-separated; an entry that
  would contain the state, the lease directory, the repository, `~/.claude`,
  `~/.claude.json`, `~/.gitconfig`, `~/.config/git`, `~/.config/gh`, the
  coordinator, pool.sh or a PATH directory is refused). Nothing under `~/.claude` or `~/.claude.json` is writable:
  the owner's own sessions load its settings, hooks, memory, shell
  snapshots, session environments and IDE locks. Claude Code 2.1.296 runs
  without writing there (verified); a private `CLAUDE_CONFIG_DIR` is not
  used because it loses the Keychain login. PATH binaries, `~/go`,
  `~/.gitconfig`, the slot's `.claude/`, `.mcp.json`, `CLAUDE.local.md`
  and `.envrc` (git-ignored, so they would survive release into the owner's
  next session in that slot; the coordinator also fails a task that leaves
  any of them), shared git hooks and config, pool.sh and the
  coordinator are unwritable. `git` and `gh` are resolved to absolute
  paths at start-up, and the coordinator's git calls disable hooks and
  fsmonitor. On
  other platforms Claude Code and Codex agents are refused; `command` agents
  (owner-written argv) run unsandboxed.
- **Codex.** `codex exec --json --ephemeral
  --dangerously-bypass-approvals-and-sandbox <prompt>`, only ever inside the
  coordinator's sandbox above (Codex's own Seatbelt sandbox cannot nest in
  it, so ours is the boundary; verified with codex-cli 0.163.0: a write
  outside the slot fails with `Operation not permitted`). Its `CODEX_HOME`
  is a private directory in the run's scratch directory, seeded with a copy
  of `~/.codex/auth.json` (`CG_COORD_CODEX_AUTH`) and, if
  `CG_COORD_CODEX_CONFIG` names one, an owner-written `config.toml`; the
  owner's `~/.codex` (config, hooks, rules, MCP servers with their
  credentials, sessions, shell snapshots) is never loaded or written.
  The login copy is never written back, since the agent controls it; if
  Codex refreshed it during a run, the coordinator says so, because a
  refresh may rotate the token the owner's copy holds (`codex login` fixes
  that). The run's JSONL events in `output.log` are the transcript.
- **Strict profile.** Codex agents, `command` agents, validation commands
  and the campaign's ci-local and PM runs read by allow-list only: the
  system (`/System`, `/usr`, `/bin`, `/sbin`, `/Library` without its
  keychains, `/opt`, `/Applications`, `/private/etc`, `/private/var/db`,
  `/dev`; not `/private/tmp`, which holds other sessions' files), the
  install directories of the tools they run
  (codex, node, go, git, python3, …; a prefix containing the home
  directory or a protected path, or lying directly in the home directory
  like `~/.local` or `~/go`, is never used), the Go module cache and
  GOROOT, `CG_COORD_SANDBOX_READABLE` entries, the slot, the repository's
  git directory, owner-writable directories and their scratch directory.
  Nothing else in the home directory is readable (synthetic canaries in
  `~/.aws`, `~/.ssh`, `~/.gitconfig`, shell history, `~/Documents`, the
  keychain, the evidence root and the primary checkout were verified
  unreadable; `go build` with cgo, `go test` and `git commit` work).
  File metadata (existence, size) stays visible. Writes: only the scratch
  directory, `/dev`, owner-listed directories and, for implementers, the
  slot and the git objects and branch refs a commit needs. The process
  starts from an empty environment (`env -i`) plus `PATH`, `HOME`, `USER`,
  `LOGNAME`, `LANG`, `LC_*`, `TERM`, `SHELL`, `PWD`, the scratch-based
  `TMPDIR`/Go/XDG variables, and names listed in `CG_COORD_SANDBOX_ENV`;
  tokens and agent sockets in the coordinator's environment never reach
  it. `GIT_CONFIG_GLOBAL` is `/dev/null`, so commits use the repository's
  own identity. cgo builds use the Command Line Tools' `clang` directly
  (`CC`, `SDKROOT` resolved outside the sandbox, and the Command Line
  Tools' `usr/bin` first on `PATH`, so `git` and `python3` in validation
  commands are Apple's), never through `xcrun`,
  whose lookup cache in the per-user temp directory the owner's own
  builds trust; that cache is neither readable nor writable inside. The
  scratch directory must not be under `/tmp` (refused). Network:
  outbound only to this host (seatbelt's `localhost`, which includes the
  host's own interface addresses) and DNS; Codex agents additionally reach
  TCP port 443 of any host, for their model API — seatbelt cannot filter
  by host name, so there is no destination allow-list, and what they can
  send is what they can read. Claude Code agents are refused unless the
  owner sets `CG_COORD_ALLOW_CLAUDE=1`; they then run in the legacy
  profile (reads allowed except the evidence root, coordinator
  state, `CG_COORD_DENY_READ`, `~/.ssh`, `~/.config/gh`, `~/.netrc`,
  `~/.git-credentials`, `~/.codex`; keychain readable; network open),
  because Claude Code keeps its login in the keychain — where `gh` also
  keeps the owner's GitHub token, so a Claude agent can read it. The
  campaign supervisor refuses them. Both adapters default to `codex`.
- **Verified content.** Agents can write the shared object store, and git
  does not re-hash objects it already has. Before a review, the PR's head
  branch is fetched from the remote into the coordinator's private
  repository (`<state>/gate-repo`) and must be exactly the head under
  review; after the review slot is checked out, its files must hash to
  that head's tree in the private repository (`checkout_mismatch`
  otherwise, the task fails with the review lease kept). The diff the
  reviewer reads comes from the private repository. The campaign's
  ci-local checkout is verified the same way, with ignored files counted
  (Go compiles ignored directories too).
- **Authorization gate.** With `CG_COORD_AUTHORIZED` set (the campaign
  sets it), a task is launched, continued, resumed after an acquire or
  reviewed only while its id is in that file; an unreadable or malformed
  file authorizes nothing. A refused continuation keeps lease and work as
  `blocked` (`deauthorized: …`) and resumes when the task is authorized
  again (only if it still holds its lease; otherwise it is `interrupted`
  for the owner); a refused launch or review leaves the task where it is.
  Agents already running finish. Each refusal is a `deauthorized` event.
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
| `gates/`, `gate-repo/` | merge gates, and the private repository they are read from |
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
- Lease tokens are passed to `pool.sh release` as an argument, so a
  sandboxed agent can see another task's token in `ps` for the duration of
  that call (needs a pool.sh change to pass it on stdin).
- An open acquire transaction whose lease exists but whose token was lost
  (pool.sh killed after writing the lease) blocks all acquisition until the
  owner releases or reclaims that lease with the token from the lease file;
  the coordinator then clears the marker by itself on the next pass.
- A process adopted without a start time (its wrapper died before the
  coordinator recorded it, or `ps` failed at launch) is tracked by its
  process group only; a reused PID leading a new group would keep the
  task `running` (lease kept; `cancel` refuses) until the owner inspects.
- Agents can write the shared object store (their commits need it), so an
  agent could plant objects that change what other slots check out at a
  genuine SHA. That content still runs only inside later sandboxed agents
  and validation, but it could make validation pass falsely or reach a
  later PR. Only the merge gate is read from a private store.
- Other ignored files an agent leaves in a slot (build output such as a
  `codegraph` binary, `.codegraph/` indexes) survive release; they run
  only if a later session executes them.
- Branch refs are shared by all slots, so an agent could fast-forward
  another task's local branch; the coordinator pushes only its own task's
  branch and never with force.
- `/tmp` stays writable for tools that ignore `TMPDIR`. Nothing the
  coordinator or pool.sh executes lives there, but Claude Code keeps small
  per-session files there (for example its Bash working directory), which
  an agent could alter.
- Sandboxed Claude Code sessions keep no transcript under `~/.claude`;
  the agent's output log in the run directory is the record.
- A run whose agent is still alive when the owner takes over keeps its
  scratch directory under the system temp directory.
- An agent's descendants that leave its process group (a new session) are
  not tracked or signalled.
- `failed`, `blocked` and `interrupted` tasks need the owner: there is no
  retry command and no automatic lease recovery. The owner inspects the
  slot and either fixes and releases the lease with the token in
  `<state>/tokens/`, or uses `pool.sh reclaim`.
- Stale base and failing CI are detected, not repaired here (the campaign
  supervisor updates stale branches, see `docs/agent-campaign.md`). Rework
  fast-forwards the local task branch to the remote one first, so a base
  merged in on GitHub is kept.
- A Codex agent can read its own login (`auth.json` in its `CODEX_HOME`).
  In the opt-in legacy profile, a Claude Code agent can read the keychain
  (including `gh`'s GitHub token), other owner files outside the denied
  paths, and the network.
- Tests must run with a `TMPDIR` outside `/private/tmp`, which agents may
  write; the fixture refuses otherwise.
- The Claude Code adapter's tool list must include every command the agent
  needs (for example `Bash(go test:*)`), via `allowed_tools`.

## Possible later additions

- A `retry` command for failed tasks after owner inspection.
- Diff-based overlap detection.

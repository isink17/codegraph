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

A slot is **idle** when its tree is clean (`git status --porcelain
--untracked-files=all` is empty), its HEAD is detached and equal to a freshly
fetched `origin/v2.0`, no process has its cwd inside it, and it has no lease.

## Leasing

Leases live in `~/Projekti/codegraph-evidence/pool/<slot>.lease`.

```sh
scripts/agent-pool/pool.sh status
scripts/agent-pool/pool.sh acquire --slot auto --task <branch> --agent <name> [--pr N] [--pid PID]
scripts/agent-pool/pool.sh release --slot <slot> --token <token>
```

- `acquire` revalidates that the slot is idle, then creates the lease
  atomically (written in full, then hard-linked into place; `link` fails if a
  lease exists). Exactly one of several concurrent callers wins; a lease is
  never overwritten. It prints the ownership token; keep it. `--slot auto`
  takes the first idle worker or exits 3 with `BLOCKED_POOL_BUSY`.
- `--pid` records a long-lived process of the agent session. Without it the
  lease is never reported stale.
- The review slot is leased only with `--review`, and only for review:
  read-only commands and `git checkout --detach <reviewed-sha>`. No edits,
  commits, stashes or branches. Return it to `origin/v2.0` afterwards.
- After acquiring: `git -C <slot> switch -c <task-branch> origin/v2.0`, do
  bounded work, commit, open a PR to `v2.0`, get an independent review of the
  exact head SHA. Evidence goes to `~/Projekti/codegraph-evidence/<wave>/`,
  never only in the slot.
- `release` needs the lease's token, no process with its cwd in the slot, a
  clean tree, and a HEAD that is pushed or merged (detached or not). It then detaches the
  slot at `origin/v2.0` and removes the lease. Any failure keeps the lease and
  the work. The task branch is kept.

## States

`status` reports each slot as:

- `free`: idle and unleased.
- `busy`: leased.
- `stale-suspected`: leased, the recorded process on this host is gone, and
  no process uses the slot. Never freed automatically.
- `inconsistent`: unleased but not idle (dirty, on a branch, behind
  `origin/v2.0`, or in use by a process), or the lease is malformed. Treat as
  busy; its owner is the task named by its branch or open PR.

There is no time-based expiry.

Every lease change takes a per-slot lock (`pool/.<slot>.lock`, a directory
created with `mkdir`). A lock left behind by a crashed run blocks the slot
and is reported; remove it by hand only after checking that no `pool.sh`
process is running.

## Recovery

- A stale-suspected lease is cleared only by an explicit
  `pool.sh reclaim --slot <slot> --token <token> --reason <text>`. The lease
  is moved to `pool/archive/` and the action appended to `pool/archive/audit.log`;
  the slot's files are not touched.
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

## Tests

`scripts/agent-pool/pool_test.sh` runs against a throwaway pool and never
touches the live one.

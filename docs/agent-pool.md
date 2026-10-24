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
scripts/agent-pool/pool.sh acquire --slot auto --task <branch> --agent <name> [--pr N] [--pid PID]
scripts/agent-pool/pool.sh release --slot <slot> --token <token>
scripts/agent-pool/pool.sh review-checkout --slot codegraph-review --token <token> --branch <branch> --head <sha>
scripts/agent-pool/pool.sh prepare --slot <slot> | --all
```

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

  It then detaches the slot at `origin/v2.0` and removes the lease. Any
  failure keeps the lease and the work. The task branch is kept.
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
  `origin/v2.0`, or in use by a process), or the lease is malformed. Treat as
  busy; its owner is the task named by its branch or open PR.

There is no time-based expiry.

Every lease change takes a per-slot lock (`pool/.<slot>.lock`, a directory
created with `mkdir`). A lock left behind by a crashed run blocks the slot
and is reported; remove it by hand only after checking that no `pool.sh`
process is running.

## Recovery

- A stale-suspected lease is cleared only by an explicit
  `pool.sh reclaim --slot <slot> --token <token> --reason <text>`, with the
  token read from `token=` in `pool/<slot>.lease` (`status` hides it). The
  token guards against reclaiming a lease that changed, not against a
  deliberate reclaim. The lease
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

## Tests

`scripts/agent-pool/pool_test.sh` runs against a throwaway pool and never
touches the live one.

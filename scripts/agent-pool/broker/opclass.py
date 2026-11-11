"""Static operation table (r4 §6.4, r3 §2.2–§2.4). A literal constant: no class or
principal is ever computed from a verb name. Every pair not listed refuses
UNKNOWN_OPERATION; every delegated row is disabled."""

from collections import namedtuple

from .refusal import Code, Refused

READ_ONLY = "READ_ONLY"
AVAILABILITY_ONLY = "AVAILABILITY_ONLY"
WORKTREE_MUTATING = "WORKTREE_MUTATING"
OWNER_ADMIN = "OWNER_ADMIN"
CLASSES = (READ_ONLY, AVAILABILITY_ONLY, WORKTREE_MUTATING, OWNER_ADMIN)

POOLCTL = "poolctl"
POOLCTL_OWNER = "poolctl-owner"

ANY = "any"
OWNER = "owner"

Op = namedtuple("Op", "entry verb cls principal delegable")

TABLE = (
    Op(POOLCTL, "status", READ_ONLY, ANY, None),
    Op(POOLCTL, "inspect", READ_ONLY, ANY, None),
    Op(POOLCTL, "reconcile-readonly", READ_ONLY, ANY, None),
    Op(POOLCTL_OWNER, "status", READ_ONLY, OWNER, None),
    Op(POOLCTL_OWNER, "inspect", READ_ONLY, OWNER, None),
    Op(POOLCTL_OWNER, "reconcile-readonly", READ_ONLY, OWNER, None),
    Op(POOLCTL_OWNER, "stop", AVAILABILITY_ONLY, OWNER, "delegate.stop"),
    Op(POOLCTL_OWNER, "mark-suspect", AVAILABILITY_ONLY, OWNER, "delegate.mark_suspect"),
    Op(POOLCTL_OWNER, "acquire", WORKTREE_MUTATING, OWNER, "delegate.acquire"),
    Op(POOLCTL_OWNER, "release", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "clear-suspect", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "adopt", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "request", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "cancel", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "freeze-start", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "freeze-continue", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "thaw", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "quarantine", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "quiescence", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "snapshot", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "verify-preservation", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "preservation", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "ready", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "resume-step", WORKTREE_MUTATING, OWNER, None),
    Op(POOLCTL_OWNER, "resume", OWNER_ADMIN, OWNER, None),
    Op(POOLCTL_OWNER, "custody-destroy", OWNER_ADMIN, OWNER, None),
)

# Removed from the PR-1 table; each needs its own reviewed class and transition rows.
REMOVED = frozenset(("prepare", "review-gate", "release-preserve", "rebind", "migrate", "unmigrate",
                     "accept-one-way", "break-lock", "set-config"))

DELEGATED_ROWS = {"delegate.stop": False, "delegate.mark_suspect": False, "delegate.acquire": False}

_INDEX = {(op.entry, op.verb): op for op in TABLE}
OWNER_VERBS = frozenset(op.verb for op in TABLE if op.entry == POOLCTL_OWNER and op.cls != READ_ONLY)


def lookup(entry, verb):
    op = _INDEX.get((entry, verb))
    if op is None:
        raise Refused(Code.UNKNOWN_OPERATION)
    return op


def authorize(entry, verb, *, owner, delegated=False):
    """The class check of A1 §A2.6 step 1. `owner` is True only for an invocation
    carrying an OwnerCapability; `delegated` asks for a delegated row."""
    op = lookup(entry, verb)
    if delegated:
        delegated_row(op)
    if op.principal == OWNER and not owner:
        raise Refused(Code.OWNER_REQUIRED)
    return op


def delegated_row(op, **_inputs):
    """Every delegated row is disabled while no TRUSTED_OBSERVER producer exists
    (r3 §2.4); no input combination enables one."""
    raise Refused(Code.DELEGATION_DISABLED)

"""The composed lifecycle (r4 §6 with A1 §A1.2–§A1.3): components G, L, A, F, R,
K, the consistency rules, the legal product rows, the custody rows and
completion_mode with the A1 §A2.6 precedence. Pure functions; every tuple not
listed refuses."""

from collections import namedtuple

import recovery_state as rs

from . import anchor, custody, libview
from .refusal import Code, Refused

G = namedtuple("G", "stop principals_ok ledger_ok one_way")
# R: state is the library state, or a pending intent's target; mode is one of
# none (no operation), done, pre, post (pending), nomarker (terminal, no
# marker), marked (terminal with marker), corrupt, busy.
R = namedtuple("R", "state mode reconcile")
R_NONE = R(None, "none", None)
R_CORRUPT = R(None, "corrupt", None)
R_BUSY = R(None, "busy", None)
OpInfo = namedtuple("OpInfo", "op_id identity gen request_gate qi")
Components = namedtuple("Components", "g l a f r k op")

F_VALUES = ("NONE", "MOVE_INTENT", "MOVED", "REVOKE_INTENT", "REVOKED", "FREEZE_INCOMPLETE", "FREEZE_BLOCKED",
            "THAW_INTENT", "SNAPSHOT_INTENT", "SNAPSHOTTED", "SNAPSHOT_FAILED", "VERIFY_INTENT", "VERIFIED",
            "VERIFY_FAILED", "UNKNOWN")
F_INTENTS = ("MOVE_INTENT", "REVOKE_INTENT", "THAW_INTENT", "SNAPSHOT_INTENT", "VERIFY_INTENT")
P7_F = ("MOVE_INTENT", "MOVED", "REVOKE_INTENT", "REVOKED", "FREEZE_INCOMPLETE", "FREEZE_BLOCKED", "THAW_INTENT")
P13_F = ("REVOKED", "SNAPSHOT_INTENT", "SNAPSHOTTED", "SNAPSHOT_FAILED", "VERIFY_INTENT", "VERIFY_FAILED",
         "VERIFIED")
TIER_C_VERBS = ("acquire", "release", "mark-suspect", "clear-suspect")

K_TERMINAL_READY = f"TERMINAL({rs.READY})"
K_TERMINAL_CANCELLED = f"TERMINAL({rs.CANCELLED})"

# Verbs per row, each with the F values it needs (None: any F of the row).
ROW_VERBS = {
    "P1": {"acquire": None, "mark-suspect": None},
    "P2": {"release": None, "mark-suspect": None, "adopt": None},
    "P3": {"clear-suspect": None},
    "P4": {"clear-suspect": None, "adopt": None},
    "P5": {"request": None, "cancel": None},
    "P5'": {"resume-step": None},
    "P6": {"freeze-start": None, "cancel": None},
    "P7": {"freeze-continue": ("MOVED", "REVOKE_INTENT", "FREEZE_INCOMPLETE", "FREEZE_BLOCKED"), "thaw": None,
           "quarantine": ("REVOKED",), "resume-step": F_INTENTS},
    "P7a": {"resume-step": None},
    "P8": {"resume-step": None}, "P9": {"resume-step": None}, "P10": {"resume-step": None},
    "P10a": {"resume-step": None},
    "P11": {"quiescence": None},
    "P12": {"resume-step": None},
    "P13": {"snapshot": ("REVOKED", "SNAPSHOT_FAILED", "VERIFY_FAILED"),
            "verify-preservation": ("SNAPSHOTTED", "VERIFY_FAILED"), "preservation": ("VERIFIED",),
            "resume-step": F_INTENTS},
    "P14": {"resume-step": None}, "P15": {"ready": None}, "P16": {"resume-step": None},
    "P17": {"resume-step": None},
    "P18": {"custody-destroy": None},
    "P19": {"resume-step": None}, "P20": {"resume-step": None},
    "P21": {"resume-step": None},
}
ROW_K = {"P5": ("ABSENT",), "P5'": ("ABSENT",), "P6": ("ABSENT", "PERSISTED"), "P7": ("ABSENT", "PERSISTED"),
         "P7a": ("PERSISTED",), "P8": ("PERSISTED", "BOUND"), "P9": ("PERSISTED", "BOUND"),
         "P10": ("PERSISTED", "BOUND"), "P10a": ("PERSISTED", "BOUND"), "P11": ("PERSISTED", "BOUND"),
         "P12": ("BOUND",), "P13": ("BOUND",), "P14": ("BOUND",), "P15": ("BOUND",), "P16": ("BOUND",),
         "P17": ("BOUND",), "P18": ("BOUND", K_TERMINAL_READY, "DESTROYED", "DESTROYED*")}
RESUME_MODE = {"P5'": "library_pending", "P8": "library_pending", "P9": "library_pending",
               "P10": "library_pending", "P12": "library_pending", "P14": "library_pending",
               "P16": "library_pending", "P19": "library_pending", "P17": "complete_terminal",
               "P20": "complete_terminal", "P7a": "anchor_pending", "P10a": "anchor_pending",
               "P21": "anchor_pending", "P7": "f_intent", "P13": "f_intent"}
QI_ROWS = ("P8", "P9", "P10", "P10a", "P11", "P12", "P13", "P14", "P15", "P16", "P17", "P18")


def g_pre(rec):
    """RecoveryKey generation (A1 §A1.1): the quarantine binding's generation."""
    return rec["slot_generation"] - 1 if rec["state"] in rs.AFTER_QUARANTINE else rec["slot_generation"]


def qi(rec, pending):
    return (pending is not None and pending["to"] == rs.QUARANTINED) or rec["state"] in rs.AFTER_QUARANTINE


def r_component(rec, pending, marker, reconcile=None):
    if pending is not None:
        return R(pending["to"], "pre" if rec["seq"] != pending["seq"] else "post", reconcile)
    if rec["state"] in rs.TERMINAL_STATES:
        if marker is None:
            return R(rec["state"], "nomarker", None)
        if marker != rs._marker(rec):
            return R_CORRUPT
        return R(rec["state"], "marked", None)
    return R(rec["state"], "done", None)


def k_legal(r, op):
    """A1 §A1.2: the K values legal for the key's operation."""
    if r.mode == "none":
        return ("ABSENT",)
    st, mode = r.state, r.mode
    if st == rs.ACTIVE or (st == rs.REQUESTED and mode in ("pre", "post")):
        return ("ABSENT",)
    if st == rs.REQUESTED:
        return ("ABSENT", "PERSISTED")
    if st == rs.CANCELLED:
        if not op.request_gate:
            return ("ABSENT",)
        if mode == "marked":
            return ("ABSENT", "PERSISTED", K_TERMINAL_CANCELLED, "DESTROYED", "DESTROYED*")
        return ("ABSENT", "PERSISTED")
    if st == rs.QUARANTINED:
        return ("PERSISTED", "BOUND")
    if st == rs.READY and mode == "marked":
        return ("BOUND", K_TERMINAL_READY, "DESTROYED", "DESTROYED*")
    return ("BOUND",)


def check_k(r, k, op):
    if k == custody.CORRUPT:
        raise Refused(Code.CUSTODY_CORRUPT)
    if r.mode in ("corrupt", "busy"):
        return
    if k not in k_legal(r, op):
        raise Refused(Code.CUSTODY_ABSENT if k == "ABSENT" else Code.CUSTODY_CONFLICT)


def _adopted(c):
    return (c.op is not None and c.l.kind in (libview.LEASED, libview.SUSPECT_LEASED) and c.l.g == c.op.gen
            and c.l.lid == c.op.identity)


def _quarantined(c):
    return (c.op is not None and c.l.kind == libview.QUARANTINED and c.l.g == c.op.gen + 1
            and c.l.op == c.op.op_id and c.l.lid == c.op.identity)


def row(c):
    """The legal product row (r4 §6.2 amended by A1 §A1.3), or None."""
    a, f, r, l = c.a, c.f, c.r, c.l
    stable, pend = a.kind == anchor.STABLE, a.kind == anchor.PENDING
    st, mode = r.state, r.mode
    idle = mode == "none" or (st == rs.CANCELLED and mode == "marked")
    if idle and f == "NONE":
        if pend:
            if a.verb in TIER_C_VERBS and l.digest in (a.pre, a.post):
                return "P21"
            return None
        return {libview.FREE: "P1", libview.LEASED: "P2", libview.SUSPECT_FREE: "P3",
                libview.SUSPECT_LEASED: "P4"}.get(l.kind) if stable else None
    adopted, quar = _adopted(c), _quarantined(c)
    if stable and adopted and f == "NONE":
        if (st, mode) == (rs.ACTIVE, "done"):
            return "P5"
        if st == rs.REQUESTED and mode in ("pre", "post"):
            return "P5'"
        if (st, mode) == (rs.REQUESTED, "done"):
            return "P6"
        if st == rs.CANCELLED and mode in ("pre", "post"):
            return "P19"
        if (st, mode) == (rs.CANCELLED, "nomarker"):
            return "P20"
    if adopted and (st, mode) == (rs.REQUESTED, "done"):
        if stable and f in P7_F:
            return "P7"
        if pend and a.verb == "quarantine" and f == "REVOKED" and l.digest == a.pre:
            return "P7a"
    if pend and a.verb == "quarantine" and f == "REVOKED" and st == rs.QUARANTINED:
        if mode == "pre" and r.reconcile == "pre" and adopted and l.digest == a.pre:
            return "P8"
        if mode == "pre" and r.reconcile == "post" and quar and l.digest == a.post:
            return "P9"
        if mode == "post" and quar and l.digest == a.post:
            return "P10"
        if mode == "done" and quar and l.digest == a.post:
            return "P10a"
    if stable and quar:
        if (st, mode, f) == (rs.QUARANTINED, "done", "REVOKED"):
            return "P11"
        if st == rs.QUIESCENT and mode in ("pre", "post") and f == "REVOKED":
            return "P12"
        if (st, mode) == (rs.QUIESCENT, "done") and f in P13_F:
            return "P13"
        if f == "VERIFIED":
            return {(rs.PRESERVED, "pre"): "P14", (rs.PRESERVED, "post"): "P14", (rs.PRESERVED, "done"): "P15",
                    (rs.READY, "pre"): "P16", (rs.READY, "post"): "P16", (rs.READY, "nomarker"): "P17",
                    (rs.READY, "marked"): "P18"}.get((st, mode))
    return None


def decide(c, verb):
    """A1 §A2.6 steps 3–5 and 7 for a slot verb. Returns (row, resume_mode)."""
    if not c.g.principals_ok:
        raise Refused(Code.GATE_ADMIN)
    if not c.g.ledger_ok:
        raise Refused(Code.LEDGER_UNAVAILABLE)
    if c.g.stop and verb not in ("resume", "resume-step", "stop"):
        raise Refused(Code.STOPPED)
    if c.r.mode == "busy":
        raise Refused(Code.BUSY)
    if c.r.mode == "corrupt" or c.a.kind in (anchor.ABSENT, anchor.CORRUPT) or c.f == "UNKNOWN" \
            or c.l.kind == libview.UNKNOWN:
        raise Refused(Code.STATE_UNKNOWN)
    if (c.a.kind == anchor.STABLE and c.l.digest != c.a.digest) or (
            c.a.kind == anchor.PENDING and c.l.digest not in (c.a.pre, c.a.post)):
        raise Refused(Code.ANCHOR_MISMATCH)
    check_k(c.r, c.k, c.op)
    rid = row(c)
    if rid is None:
        raise Refused(Code.STATE_UNKNOWN)
    if rid in ROW_K and c.k not in ROW_K[rid]:
        raise Refused(Code.STATE_UNKNOWN)
    if verb == "thaw" and rid in QI_ROWS:
        raise Refused(Code.THAW_AFTER_QUARANTINE)
    allowed = ROW_VERBS[rid]
    if verb not in allowed or (allowed[verb] is not None and c.f not in allowed[verb]):
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE)
    if verb == "adopt" and not c.g.one_way:
        raise Refused(Code.ONE_WAY_NOT_ACCEPTED)
    return rid, RESUME_MODE.get(rid) if verb == "resume-step" else None


def custody_row(r, k, op, terminal):
    """A1 §A1.3 custody rows for custody-destroy on the named operation.
    Returns "full", "unlink" or "done"."""
    if k == custody.CORRUPT:
        raise Refused(Code.CUSTODY_CORRUPT)
    if r.mode == "marked" and terminal == r.state == rs.READY and op.qi:
        allowed = ("BOUND", K_TERMINAL_READY)
    elif r.mode == "marked" and terminal == r.state == rs.CANCELLED and op.request_gate and not op.qi:
        allowed = ("PERSISTED", K_TERMINAL_CANCELLED)
    else:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE)
    if k == "DESTROYED*":
        return "unlink"
    if k == "DESTROYED":
        return "done"
    if k not in allowed:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE)
    return "full"


INITIATE = "INITIATE"
RESUME_PENDING = "RESUME_PENDING"
COMPLETE_TERMINAL = "COMPLETE_TERMINAL"
ALREADY_COMPLETE = "ALREADY_COMPLETE"


def completion_mode(rec, pending, marker, transition, is_burned_doc):
    """A1 §A2.6 step 6 for the library transition `transition`.
    is_burned_doc(digest) is True for a stored document whose digest is burned."""
    to = {v: k for k, v in rs.TRANSITION.items()}[transition]
    if pending is not None:
        if pending["to"] != to:
            raise Refused(Code.PENDING_INTENT)
        return RESUME_PENDING
    if rec["state"] == to:
        gate = rec["gates"].get(transition)
        if gate is None or not is_burned_doc(gate):
            raise Refused(Code.STATE_UNKNOWN)
        if to in rs.TERMINAL_STATES:
            if marker is None:
                return COMPLETE_TERMINAL
            if marker != rs._marker(rec):
                raise Refused(Code.STATE_UNKNOWN)
        return ALREADY_COMPLETE
    if to in rs.STEPS.get(rec["state"], ()):
        return INITIATE
    raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE)

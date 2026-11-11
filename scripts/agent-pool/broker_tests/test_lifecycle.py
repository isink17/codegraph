"""Combined lifecycle (C6): K consistency table (A1 §A1.2), product rows
(r4 §6.2 + A1 §A1.3) against an independently written oracle over a finite
abstraction, decide() precedence, completion_mode and the custody rows."""

import itertools
import unittest

import harness  # noqa: F401

import recovery_state as rs
from broker import anchor, libview, lifecycle
from broker.lifecycle import R
from broker.refusal import Code, Refused

OP = lifecycle.OpInfo("op1", "1" * 32, 1, True, False)
PRE, POST, OTHER = "a" * 64, "b" * 64, "c" * 64
ST = {"ACTIVE": rs.ACTIVE, "REQ": rs.REQUESTED, "QUAR": rs.QUARANTINED, "QV": rs.QUIESCENT, "PRES": rs.PRESERVED,
      "READY": rs.READY, "CAN": rs.CANCELLED}


def ls():
    out = []
    for kind, g, lid, op in ((libview.FREE, 1, None, None), (libview.LEASED, 1, OP.identity, None),
                             (libview.LEASED, 1, "2" * 32, None), (libview.LEASED, 2, OP.identity, None),
                             (libview.SUSPECT_FREE, 1, None, None), (libview.SUSPECT_LEASED, 1, OP.identity, None),
                             (libview.QUARANTINED, 2, OP.identity, "op1"), (libview.QUARANTINED, 2, OP.identity, "op9"),
                             (libview.QUARANTINED, 2, "2" * 32, "op1"),
                             (libview.QUARANTINED, 1, OP.identity, "op1"), (libview.UNKNOWN, None, None, None)):
        for d in (PRE, POST, OTHER):
            out.append(libview.L(kind, g, lid, op, d))
    return out


def as_():
    out = [anchor.A(anchor.STABLE, d, None, None, None, "x") for d in (PRE, POST)]
    out += [anchor.A(anchor.PENDING, None, PRE, POST, v, "x") for v in ("quarantine", "acquire", "release")]
    out += [anchor.A(anchor.ABSENT, None, None, None, None, None), anchor.A(anchor.CORRUPT, None, None, None, None,
                                                                            None)]
    return out


def rs_():
    out = [lifecycle.R_NONE, lifecycle.R_CORRUPT, lifecycle.R_BUSY]
    for st in ST.values():
        for mode in ("done", "pre", "post", "nomarker", "marked"):
            if mode in ("nomarker", "marked") and st not in rs.TERMINAL_STATES:
                continue
            if mode == "done" and st in rs.TERMINAL_STATES:
                continue
            recs = ("pre", "post") if (st, mode) == (rs.QUARANTINED, "pre") else (None,)
            out += [R(st, mode, rc) for rc in recs]
    return out


# The oracle: r4 §6.2 rows as data, written independently of lifecycle.row().
ADOPTED = lambda l: l.kind in (libview.LEASED, libview.SUSPECT_LEASED) and (l.g, l.lid) == (1, OP.identity)  # noqa
QUAR = lambda l: l.kind == libview.QUARANTINED and (l.g, l.lid, l.op) == (2, OP.identity, "op1")  # noqa
STABLE = lambda l, a: a.kind == anchor.STABLE and l.digest == a.digest  # noqa
PENDQ = lambda a: a.kind == anchor.PENDING and a.verb == "quarantine"  # noqa
IDLE = lambda r: r.mode == "none" or (r.state, r.mode) == (rs.CANCELLED, "marked")  # noqa
ORACLE = [
    ("P1", lambda l, a, f, r: l.kind == libview.FREE and STABLE(l, a) and f == "NONE" and IDLE(r)),
    ("P2", lambda l, a, f, r: l.kind == libview.LEASED and STABLE(l, a) and f == "NONE" and IDLE(r)),
    ("P3", lambda l, a, f, r: l.kind == libview.SUSPECT_FREE and STABLE(l, a) and f == "NONE" and IDLE(r)),
    ("P4", lambda l, a, f, r: l.kind == libview.SUSPECT_LEASED and STABLE(l, a) and f == "NONE" and IDLE(r)),
    ("P5", lambda l, a, f, r: ADOPTED(l) and STABLE(l, a) and f == "NONE" and r[:2] == (rs.ACTIVE, "done")),
    ("P5'", lambda l, a, f, r: ADOPTED(l) and STABLE(l, a) and f == "NONE" and r.state == rs.REQUESTED
     and r.mode in ("pre", "post")),
    ("P6", lambda l, a, f, r: ADOPTED(l) and STABLE(l, a) and f == "NONE" and r[:2] == (rs.REQUESTED, "done")),
    ("P7", lambda l, a, f, r: ADOPTED(l) and STABLE(l, a) and f in lifecycle.P7_F and r[:2] == (rs.REQUESTED,
                                                                                                "done")),
    ("P7a", lambda l, a, f, r: ADOPTED(l) and PENDQ(a) and l.digest == a.pre and f == "REVOKED"
     and r[:2] == (rs.REQUESTED, "done")),
    ("P8", lambda l, a, f, r: ADOPTED(l) and PENDQ(a) and l.digest == a.pre and f == "REVOKED"
     and r == (rs.QUARANTINED, "pre", "pre")),
    ("P9", lambda l, a, f, r: QUAR(l) and PENDQ(a) and l.digest == a.post and f == "REVOKED"
     and r == (rs.QUARANTINED, "pre", "post")),
    ("P10", lambda l, a, f, r: QUAR(l) and PENDQ(a) and l.digest == a.post and f == "REVOKED"
     and r[:2] == (rs.QUARANTINED, "post")),
    ("P10a", lambda l, a, f, r: QUAR(l) and PENDQ(a) and l.digest == a.post and f == "REVOKED"
     and r[:2] == (rs.QUARANTINED, "done")),
    ("P11", lambda l, a, f, r: QUAR(l) and STABLE(l, a) and f == "REVOKED" and r[:2] == (rs.QUARANTINED, "done")),
    ("P12", lambda l, a, f, r: QUAR(l) and STABLE(l, a) and f == "REVOKED" and r.state == rs.QUIESCENT
     and r.mode in ("pre", "post")),
    ("P13", lambda l, a, f, r: QUAR(l) and STABLE(l, a) and f in lifecycle.P13_F and r[:2] == (rs.QUIESCENT, "done")),
    ("P14", lambda l, a, f, r: QUAR(l) and STABLE(l, a) and f == "VERIFIED" and r.state == rs.PRESERVED
     and r.mode in ("pre", "post")),
    ("P15", lambda l, a, f, r: QUAR(l) and STABLE(l, a) and f == "VERIFIED" and r[:2] == (rs.PRESERVED, "done")),
    ("P16", lambda l, a, f, r: QUAR(l) and STABLE(l, a) and f == "VERIFIED" and r.state == rs.READY
     and r.mode in ("pre", "post")),
    ("P17", lambda l, a, f, r: QUAR(l) and STABLE(l, a) and f == "VERIFIED" and r[:2] == (rs.READY, "nomarker")),
    ("P18", lambda l, a, f, r: QUAR(l) and STABLE(l, a) and f == "VERIFIED" and r[:2] == (rs.READY, "marked")),
    ("P19", lambda l, a, f, r: ADOPTED(l) and STABLE(l, a) and f == "NONE" and r.state == rs.CANCELLED
     and r.mode in ("pre", "post")),
    ("P20", lambda l, a, f, r: ADOPTED(l) and STABLE(l, a) and f == "NONE" and r[:2] == (rs.CANCELLED, "nomarker")),
    ("P21", lambda l, a, f, r: a.kind == anchor.PENDING and a.verb in lifecycle.TIER_C_VERBS
     and l.digest in (a.pre, a.post) and f == "NONE" and IDLE(r)),
]


class Rows(unittest.TestCase):
    def test_rows_match_the_oracle_over_the_abstraction(self):
        g = lifecycle.G(False, True, True, True)
        n = 0
        for l, a, f, r in itertools.product(ls(), as_(), lifecycle.F_VALUES, rs_()):
            if a.kind in (anchor.ABSENT, anchor.CORRUPT) or l.kind == libview.UNKNOWN or f == "UNKNOWN" \
                    or r.mode in ("corrupt", "busy"):
                continue
            if (a.kind == anchor.STABLE and l.digest != a.digest) or (
                    a.kind == anchor.PENDING and l.digest not in (a.pre, a.post)):
                continue  # the r4 §6.1 consistency rule refuses before any row
            c = lifecycle.Components(g, l, a, f, r, "ABSENT", OP if r.mode != "none" else None)
            matches = [rid for rid, pred in ORACLE if pred(l, a, f, r)]
            self.assertLessEqual(len(matches), 1, (l, a, f, r, matches))
            self.assertEqual(lifecycle.row(c), matches[0] if matches else None, (l, a, f, r))
            n += 1
        self.assertGreater(n, 20000)

    def test_every_row_is_reachable_in_the_abstraction(self):
        g = lifecycle.G(False, True, True, True)
        seen = set()
        for l, a, f, r in itertools.product(ls(), as_(), lifecycle.F_VALUES, rs_()):
            c = lifecycle.Components(g, l, a, f, r, "ABSENT", OP if r.mode != "none" else None)
            if l.kind != libview.UNKNOWN and a.kind not in (anchor.ABSENT, anchor.CORRUPT):
                seen.add(lifecycle.row(c))
        self.assertEqual(seen - {None}, set(lifecycle.ROW_VERBS))


K_TABLE = {  # A1 §A1.2, transcribed: (state, mode, request_gate) -> legal K
    ("none",): {"ABSENT"},
}


def k_expected(r, request_gate):
    if r.mode == "none":
        return {"ABSENT"}
    st, mode = r.state, r.mode
    if st == rs.ACTIVE or (st == rs.REQUESTED and mode != "done"):
        return {"ABSENT"}
    if st == rs.REQUESTED:
        return {"ABSENT", "PERSISTED"}
    if st == rs.CANCELLED and not request_gate:
        return {"ABSENT"}
    if st == rs.CANCELLED and mode == "marked":
        return {"ABSENT", "PERSISTED", "TERMINAL(CANCELLED)", "DESTROYED", "DESTROYED*"}
    if st == rs.CANCELLED:
        return {"ABSENT", "PERSISTED"}
    if st == rs.QUARANTINED:
        return {"PERSISTED", "BOUND"}
    if (st, mode) == (rs.READY, "marked"):
        return {"BOUND", "TERMINAL(READY_FOR_REASSIGNMENT)", "DESTROYED", "DESTROYED*"}
    return {"BOUND"}


KS = ("ABSENT", "PERSISTED", "BOUND", "TERMINAL(READY_FOR_REASSIGNMENT)", "TERMINAL(CANCELLED)", "DESTROYED",
      "DESTROYED*", "CORRUPT")


class KConsistency(unittest.TestCase):
    def test_k_consistency_table_exhaustive(self):
        for r in rs_():
            if r.mode in ("corrupt", "busy"):
                continue
            for gate in (False, True):
                op = lifecycle.OpInfo("op1", "1" * 32, 1, gate, False)
                for k in KS:
                    legal = k_expected(r, gate)
                    try:
                        lifecycle.check_k(r, k, op)
                        got = None
                    except Refused as e:
                        got = e.code
                    if k in legal:
                        self.assertIsNone(got, (r, gate, k))
                    elif k == "CORRUPT":
                        self.assertIs(got, Code.CUSTODY_CORRUPT)
                    elif k == "ABSENT":
                        self.assertIs(got, Code.CUSTODY_ABSENT, (r, gate, k))
                    else:
                        self.assertIs(got, Code.CUSTODY_CONFLICT, (r, gate, k))


def comp(row_l, row_a, f, r, k="ABSENT", g=None):
    return lifecycle.Components(g or lifecycle.G(False, True, True, True), row_l, row_a, f, r, k, OP)


L_ADOPTED = libview.L(libview.LEASED, 1, OP.identity, None, PRE)
A_STABLE_PRE = anchor.A(anchor.STABLE, PRE, None, None, None, "x")
L_QUAR = libview.L(libview.QUARANTINED, 2, OP.identity, "op1", POST)
A_STABLE_POST = anchor.A(anchor.STABLE, POST, None, None, None, "x")


class Decide(unittest.TestCase):
    def code(self, c, verb):
        try:
            lifecycle.decide(c, verb)
        except Refused as e:
            return e.code
        return None

    def test_precedence(self):
        c = comp(L_ADOPTED, A_STABLE_PRE, "NONE", R(rs.ACTIVE, "done", None))
        self.assertIsNone(self.code(c, "request"))
        self.assertIs(self.code(c._replace(g=lifecycle.G(True, False, True, True)), "request"), Code.GATE_ADMIN)
        self.assertIs(self.code(c._replace(g=lifecycle.G(True, True, False, True)), "request"),
                      Code.LEDGER_UNAVAILABLE)
        self.assertIs(self.code(c._replace(g=lifecycle.G(True, True, True, True)), "request"), Code.STOPPED)
        self.assertIsNone(self.code(c._replace(g=lifecycle.G(True, True, True, True), r=R(rs.REQUESTED, "pre",
                                                                                            None)), "resume-step"))
        self.assertIs(self.code(c._replace(r=lifecycle.R_BUSY), "request"), Code.BUSY)
        self.assertIs(self.code(c._replace(r=lifecycle.R_CORRUPT), "request"), Code.STATE_UNKNOWN)
        self.assertIs(self.code(c._replace(a=A_STABLE_POST), "request"), Code.ANCHOR_MISMATCH)
        self.assertIs(self.code(c._replace(k="CORRUPT"), "request"), Code.CUSTODY_CORRUPT)
        self.assertIs(self.code(c._replace(k="BOUND"), "request"), Code.CUSTODY_CONFLICT)
        self.assertIs(self.code(c, "quarantine"), Code.VERB_NOT_ALLOWED_IN_STATE)

    def test_thaw_after_quarantine_and_f_conditions(self):
        p7 = comp(L_ADOPTED, A_STABLE_PRE, "MOVED", R(rs.REQUESTED, "done", None))
        self.assertIsNone(self.code(p7, "thaw"))
        self.assertIsNone(self.code(p7, "freeze-continue"))
        self.assertIs(self.code(p7, "quarantine"), Code.VERB_NOT_ALLOWED_IN_STATE)
        self.assertIsNone(self.code(p7._replace(f="REVOKED"), "quarantine"))
        p11 = comp(L_QUAR, A_STABLE_POST, "REVOKED", R(rs.QUARANTINED, "done", None), "PERSISTED")
        self.assertIs(self.code(p11, "thaw"), Code.THAW_AFTER_QUARANTINE)
        self.assertIs(self.code(p11, "cancel"), Code.VERB_NOT_ALLOWED_IN_STATE)
        self.assertIsNone(self.code(p11, "quiescence"))
        self.assertIs(self.code(p11._replace(r=R(rs.QUIESCENT, "done", None)), "quiescence"), Code.CUSTODY_CONFLICT)

    def test_adopt_needs_one_way(self):
        p2 = lifecycle.Components(lifecycle.G(False, True, True, False), L_ADOPTED, A_STABLE_PRE, "NONE",
                                  lifecycle.R_NONE, "ABSENT", None)
        self.assertIs(self.code(p2, "adopt"), Code.ONE_WAY_NOT_ACCEPTED)
        self.assertIsNone(self.code(p2._replace(g=lifecycle.G(False, True, True, True)), "adopt"))

    def test_row_k_column(self):
        p7a = comp(L_ADOPTED, anchor.A(anchor.PENDING, None, PRE, POST, "quarantine", "x"), "REVOKED",
                   R(rs.REQUESTED, "done", None), "ABSENT")
        self.assertIs(self.code(p7a, "resume-step"), Code.STATE_UNKNOWN)
        self.assertEqual(lifecycle.decide(p7a._replace(k="PERSISTED"), "resume-step"), ("P7a", "anchor_pending"))

    def test_resume_modes(self):
        self.assertEqual(lifecycle.decide(comp(L_QUAR, A_STABLE_POST, "VERIFIED", R(rs.READY, "nomarker", None),
                                               "BOUND"), "resume-step"), ("P17", "complete_terminal"))
        self.assertEqual(lifecycle.decide(comp(L_ADOPTED, A_STABLE_PRE, "MOVE_INTENT",
                                               R(rs.REQUESTED, "done", None)), "resume-step"), ("P7", "f_intent"))
        self.assertIs(self.code(comp(L_ADOPTED, A_STABLE_PRE, "MOVED", R(rs.REQUESTED, "done", None)),
                                "resume-step"), Code.VERB_NOT_ALLOWED_IN_STATE)


def rec(state, gates=None, seq=1, gen=1):
    return {"state": state, "seq": seq, "slot_generation": gen, "op_id": "op1",
            "gates": gates if gates is not None else {g: "d" + g for g in rs.GATES.get(state, ())}}


class CompletionMode(unittest.TestCase):
    def mode(self, r, pending, marker, t, burned=True):
        try:
            return lifecycle.completion_mode(r, pending, marker, t, lambda d: burned)
        except Refused as e:
            return e.code

    def test_table(self):
        r = rec(rs.REQUESTED)
        self.assertEqual(self.mode(r, None, None, "quarantine"), lifecycle.INITIATE)
        self.assertEqual(self.mode(r, None, None, "request"), lifecycle.ALREADY_COMPLETE)
        self.assertIs(self.mode(r, None, None, "request", burned=False), Code.STATE_UNKNOWN)
        self.assertIs(self.mode(r, None, None, "ready"), Code.VERB_NOT_ALLOWED_IN_STATE)
        self.assertIs(self.mode(r, {"to": rs.CANCELLED}, None, "quarantine"), Code.PENDING_INTENT)
        self.assertEqual(self.mode(r, {"to": rs.QUARANTINED}, None, "quarantine"), lifecycle.RESUME_PENDING)
        c = rec(rs.CANCELLED, {"cancel": "x"}, seq=1)
        self.assertEqual(self.mode(c, None, None, "cancel"), lifecycle.COMPLETE_TERMINAL)
        self.assertEqual(self.mode(c, None, rs._marker(c), "cancel"), lifecycle.ALREADY_COMPLETE)
        self.assertIs(self.mode(c, None, b"state=x\n", "cancel"), Code.STATE_UNKNOWN)


class CustodyRows(unittest.TestCase):
    def test_rows(self):
        ready = R(rs.READY, "marked", None)
        cancelled = R(rs.CANCELLED, "marked", None)
        qi_op = lifecycle.OpInfo("op1", "1" * 32, 1, True, True)
        req_op = lifecycle.OpInfo("op1", "1" * 32, 1, True, False)
        act_op = lifecycle.OpInfo("op1", "1" * 32, 1, False, False)
        cases = [(ready, "BOUND", qi_op, rs.READY, "full"),
                 (ready, "TERMINAL(READY_FOR_REASSIGNMENT)", qi_op, rs.READY, "full"),
                 (ready, "DESTROYED*", qi_op, rs.READY, "unlink"), (ready, "DESTROYED", qi_op, rs.READY, "done"),
                 (cancelled, "PERSISTED", req_op, rs.CANCELLED, "full"),
                 (cancelled, "TERMINAL(CANCELLED)", req_op, rs.CANCELLED, "full"),
                 (cancelled, "ABSENT", req_op, rs.CANCELLED, Code.VERB_NOT_ALLOWED_IN_STATE),
                 (cancelled, "PERSISTED", act_op, rs.CANCELLED, Code.VERB_NOT_ALLOWED_IN_STATE),
                 (R(rs.CANCELLED, "nomarker", None), "PERSISTED", req_op, None, Code.VERB_NOT_ALLOWED_IN_STATE),
                 (R(rs.QUARANTINED, "done", None), "BOUND", qi_op, None, Code.VERB_NOT_ALLOWED_IN_STATE),
                 (ready, "PERSISTED", qi_op, rs.READY, Code.VERB_NOT_ALLOWED_IN_STATE),
                 (ready, "CORRUPT", qi_op, rs.READY, Code.CUSTODY_CORRUPT)]
        for r, k, op, terminal, expect in cases:
            try:
                got = lifecycle.custody_row(r, k, op, terminal)
            except Refused as e:
                got = e.code
            self.assertEqual(got, expect, (r, k))


if __name__ == "__main__":
    unittest.main()

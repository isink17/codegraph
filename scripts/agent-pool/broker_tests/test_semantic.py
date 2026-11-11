"""PR1-3 acceptance (A1 §A3.4): semantic equivalence of interrupted-and-resumed
runs with uninterrupted runs, the comparator's coverage, and its detection
obligations. The library runs unmodified; nothing patches secrets or _now."""

import copy
import json
import unittest

import harness
from harness import QUARANTINE, QUIESCENCE, SLOT, Env, reason
from semantic import RunView, semantic_equal

import recovery_state as rs
from broker.refusal import Outcome
from broker.stores import Crash

CHAIN = [("adopt", {}), ("request", {}), ("freeze-start", {}), ("freeze-continue", {}), ("quarantine", QUARANTINE),
         ("quiescence", QUIESCENCE), ("snapshot", {}), ("verify-preservation", {}), ("preservation", {}),
         ("ready", {})]
CANCEL_ACTIVE = [("adopt", {}), ("cancel", {})]
CANCEL_REQUESTED = [("adopt", {}), ("request", {}), ("cancel", {})]
SEQ = {"request": 1, "quarantine": 2, "quiescence": 3, "preservation": 4, "ready": 5}
LIBRARY_POINTS = ("before_intent", "after_intent", "after_record", "before_state_rename", "after_state",
                  "after_complete", "before_marker_rename")


def run(chain, crash=None):
    """Runs the chain; crash = (verb, point). Returns (env, expectations)."""
    env = Env()
    env.seed_leased()
    resumed, extra, anchors = set(), 0, []
    for verb, extra_fields in chain:
        step = dict(slot=SLOT, op_id="op1", reason=reason(), **extra_fields)
        if crash and crash[0] == verb:
            env.faults.point = "library:" + crash[1]
            try:
                r = env.run(verb, **step)
                raise AssertionError(f"no crash at {crash}: {r}")
            except Crash:
                pass
            env.faults.point = None
            seq = SEQ.get(verb, 2 if chain is CANCEL_REQUESTED else 1)
            point = crash[1]
            if point in ("before_intent", "before_publish"):
                if verb == "quarantine":
                    ok(env.run("resume-step", slot=SLOT, op_id="op1", reason=reason()))
                    extra += 1
                    anchors += [("pending", "quarantine"), ("abort", "quarantine")]
                ok(env.run(verb, **step))
                extra += 1
            elif point == "after_complete" and verb not in ("quarantine", "cancel", "ready"):
                pass
            else:
                ok(env.run("resume-step", slot=SLOT, op_id="op1", reason=reason()))
                extra += 1
                if point not in ("after_complete", "before_marker_rename"):
                    resumed.add(seq)
        else:
            ok(env.run(verb, **step))
    return env, {"resumed_seqs": resumed, "extra_burns": extra, "extra_anchor": anchors}


def ok(r):
    if r.outcome is not Outcome.COMMITTED:
        raise AssertionError(r)


def points_for(verb):
    if verb == "adopt":
        return ("before_publish",)
    pts = [p for p in LIBRARY_POINTS if p != "after_record" or verb == "quarantine"]
    if verb not in ("cancel", "ready"):
        pts.remove("before_marker_rename")
    return pts


class Equivalence(unittest.TestCase):
    def compare(self, chain, crash):
        ref_env, _ = run(chain)
        act_env, expect = run(chain, crash)
        try:
            semantic_equal(RunView(ref_env, SLOT, "op1"), RunView(act_env, SLOT, "op1"), **expect)
        finally:
            ref_env.close()
            act_env.close()

    def test_uninterrupted_runs_semantically_equal_not_byte_equal(self):
        a, _ = run(CHAIN)
        b, _ = run(CHAIN)
        try:
            ra, rb = RunView(a, SLOT, "op1"), RunView(b, SLOT, "op1")
            self.assertNotEqual(json.dumps(ra.state, sort_keys=True), json.dumps(rb.state, sort_keys=True))
            self.assertTrue(semantic_equal(ra, rb, resumed_seqs=set(), extra_burns=0, extra_anchor=[]))
        finally:
            a.close()
            b.close()

    def test_fault_matrix_semantic_equivalence(self):
        n = 0
        for chain in (CHAIN, CANCEL_ACTIVE, CANCEL_REQUESTED):
            verbs = [v for v, _ in chain if v in ("adopt", "cancel") or v in SEQ]
            if chain is not CHAIN:
                verbs = ["cancel"]
            for verb in verbs:
                for point in points_for(verb):
                    with self.subTest(chain=len(chain), verb=verb, point=point):
                        self.compare(chain, (verb, point))
                        n += 1
        self.assertGreaterEqual(n, 37)


class Coverage(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ref_env, _ = run(CHAIN)
        cls.act_env, _ = run(CHAIN)
        cls.ref = RunView(cls.ref_env, SLOT, "op1")
        cls.act = RunView(cls.act_env, SLOT, "op1")

    @classmethod
    def tearDownClass(cls):
        cls.ref_env.close()
        cls.act_env.close()

    def fails(self, mutate):
        act = copy.deepcopy(self.act)
        mutate(act)
        with self.assertRaises(AssertionError):
            semantic_equal(self.ref, act, resumed_seqs=set(), extra_burns=0, extra_anchor=[])

    def test_baseline_is_equal(self):
        self.assertTrue(semantic_equal(self.ref, self.act, resumed_seqs=set(), extra_burns=0, extra_anchor=[]))

    def test_comparator_covers_every_field(self):
        for k in sorted(rs.RECORD_KEYS):
            with self.subTest(record=k):
                def m(a, k=k):
                    v = a.state[k]
                    a.state[k] = (v + 1 if type(v) is int else {**v, "x": "y"} if type(v) is dict
                                  else ("0" if v[0] != "0" else "1") + v[1:])
                self.fails(m)
        for k in rs.ENTRY_KEYS:
            with self.subTest(entry=k):
                def m(a, k=k):
                    e = next(e for e in a.journal if e["to"] == rs.QUARANTINED)
                    e[k] = {"pre": "0" * 64, "post": "1" * 64} if k == "effect" else (
                        e[k] + 1 if type(e[k]) is int else "X" + str(e[k])[1:])
                self.fails(m)

    def test_detects_wrong_token(self):
        def m(a):
            a.custody["token"] = "9" * 32
        self.fails(m)

    def test_detects_wrong_generation(self):
        self.fails(lambda a: a.state.__setitem__("slot_generation", a.state["slot_generation"] + 1))

    def test_detects_corrupted_verifier_and_salt_swap(self):
        self.fails(lambda a: a.state.__setitem__("verifier", "0" * 64))
        self.fails(lambda a: a.state.__setitem__("salt", "0" * 32))

    def test_detects_missing_custody(self):
        self.fails(lambda a: setattr(a, "custody", None))

    def test_detects_mismatched_pending_intent(self):
        self.fails(lambda a: a.journal[-2].__setitem__("to", rs.PRESERVED))

    def test_detects_unauthorized_transition(self):
        def m(a):
            a.burns = [b for b in a.burns if b["auth_digest"] != a.state["gates"]["ready"]]
        self.fails(m)

    def test_detects_changed_evidence(self):
        def m(a):
            d = a.state["gates"]["preservation"]
            a.docs[d] = dict(a.docs[d], head="b" * 40)
        self.fails(m)

    def test_detects_incorrect_resumed(self):
        self.fails(lambda a: a.journal[-1].__setitem__("resumed", True))

    def test_detects_duplicate_burn(self):
        self.fails(lambda a: a.burns.append(dict(a.burns[-1])))


if __name__ == "__main__":
    unittest.main()

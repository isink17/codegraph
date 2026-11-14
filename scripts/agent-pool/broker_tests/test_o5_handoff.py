"""H-EST boundary input (PR-3a contract R4.1 §4.6, §4.7 H1–H13; review finding
N-R4.1-1). PR-3a builds and validates the abstract H-EST PreBasis only: the
containment-handoff verb and its effect are PR-4, and without PR-3b no
establishment verdict exists, so `handoff_allowed` refuses. A positive row
validates the interface; it authorizes no handoff and no release."""

import unittest

import harness
from harness import SLOT, Env, reason
from broker import barrier, freeze, o5contract as oc
from broker._testsupport import o5
from broker.refusal import Code, Outcome, Refused

OP = "op1"


class HEst(unittest.TestCase):
    def setUp(self):
        self.env = Env(auto_handoff=False)
        self.addCleanup(self.env.close)
        self.env.seed_leased()
        self.env.seed_leased(slot=harness.O5_SLOTS[1])
        self.go("adopt", "request", "freeze-start", "freeze-continue", "quarantine", "quiescence")
        self.cycle = self.cycles()[-1]

    def go(self, *verbs):
        for v in verbs:
            kw = {"quarantine": harness.QUARANTINE, "quiescence": harness.QUIESCENCE}.get(v, {})
            r = self.env.run(v, slot=SLOT, op_id=OP, reason=reason(), **kw)
            self.assertIs(r.outcome, Outcome.COMMITTED, (v, r))

    def cycles(self):
        w = self.env.world
        return freeze.cycles(w, SLOT, freeze.burned_docs(w, w.ledger.entries()))

    def doc(self, **over):
        c = self.cycle
        return dict({"v": 1, "doc": "containment-handoff", "verb": "containment-handoff", "slot": SLOT, "op_id": OP,
                     "slot_generation": c.g_pre, "cycle_id": c.cycle_id, "barrier_id": c.barrier_id}, **over)

    def basis(self, **kw):
        o5.observe(self.env, SLOT)
        return freeze.basis_for(self.env.world, SLOT, "handoff", **kw)

    def refuses(self, code, detail, **kw):
        snap = self.env.snapshot()
        with self.assertRaises(Refused) as e:
            self.basis(**kw)
        self.assertEqual((e.exception.code, e.exception.detail), (code, detail))
        self.assertEqual(self.env.snapshot(), snap)

    def test_h1_h6_valid_cycle_holder_chain(self):
        """H1/H6 and the R4-B2 regression: holder_chain is the cycle's chain."""
        pre, sel = self.basis(doc=self.doc())
        self.assertEqual((pre.holder_chain, pre.barrier_id), (self.env.barrier_id, self.env.barrier_id))
        self.assertIsNone(pre.release_target)
        self.assertEqual((pre.cycle_cutover_head, pre.cutover_head), (self.cycle.cutover_head,) * 2)
        self.assertEqual((pre.g_pre, pre.cycle_id), (self.cycle.g_pre, self.cycle.cycle_id))
        self.assertEqual(sorted(k for k in oc.H_EST_KINDS if sel.digest_of(k) is None), [])

    def test_n_r4_1_1_proposed_document_is_not_burned(self):
        """The proposed document is validated without a burn and is never treated
        as a committed authorization."""
        doc = self.doc()
        self.basis(doc=doc)
        burned = {e["auth_digest"] for e in self.env.world.ledger.entries()}
        self.assertNotIn(freeze.canonical.digest(doc), burned)
        self.assertNotIn(freeze.canonical.digest(doc), self.env.world.authz.docs)

    def test_h2_h3_h11_null_or_foreign_holder_chain(self):
        self.refuses(Code.STATE_UNKNOWN, "barrier_binding", doc=self.doc(barrier_id=None))
        self.refuses(Code.STATE_UNKNOWN, "barrier_binding", doc=self.doc(barrier_id="e" * 64))
        self.refuses(Code.STATE_UNKNOWN, "barrier_binding", doc=self.doc(cycle_id="c" * 64))

    def test_h4_other_slot(self):
        self.refuses(Code.STATE_UNKNOWN, "barrier_binding", doc=self.doc(slot=harness.O5_SLOTS[1]))

    def test_h5_generation_before_quarantine(self):
        env = Env(auto_handoff=False)
        self.addCleanup(env.close)
        env.seed_leased()
        self.env = env
        self.go("adopt", "request", "freeze-start", "freeze-continue")
        self.cycle = self.cycles()[-1]
        self.refuses(Code.STATE_UNKNOWN, "barrier_binding", doc=self.doc())

    def test_h7_revoked_or_changed_head(self):
        o5.cutover_step(self.env, "model-b-rebaseline")
        self.refuses(Code.STATE_UNKNOWN, "cutover_changed", doc=self.doc())
        o5.cutover_step(self.env, "model-b-revoke")
        self.refuses(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(model_a)", doc=self.doc())

    def test_h8_missing_protected_evidence(self):
        o5.observe(self.env, SLOT)
        self.env.world.o5.stores["evidence"].by_kind["claimed_set_observation"] = []
        with self.assertRaises(Refused) as e:
            freeze.basis_for(self.env.world, SLOT, "handoff", doc=self.doc())
        self.assertEqual(e.exception.detail, "evidence_missing")

    def test_h9_premature_release_target(self):
        self.refuses(Code.STATE_UNKNOWN, "barrier_binding", doc=self.doc(), release_target=self.env.barrier_id)

    def test_h10_guard_and_absent_provider(self):
        prov = self.env.world.o5.preservation
        pre, sel = self.basis(doc=self.doc())
        v = prov.handoff_establish(pre, sel.public(), None)
        freeze.guard(v, "H-EST", pre.digest, sel, oc.H_EST_KINDS)
        for bad in (dict(v._f, pre="1" * 64), dict(v._f, cell="H-VAL"), dict(v._f, inputs={})):
            with self.assertRaises(Refused):
                freeze.guard(oc.HandoffVerdict(oc._PSEAL, **bad), "H-EST", pre.digest, sel, oc.H_EST_KINDS)
        c = o5.chain(self.env, self.env.barrier_id)
        args = (c, barrier.ESTABLISHED, "P13", SLOT, self.cycle, False)
        guard = lambda x: freeze.guard(x, "H-EST", x.pre, sel, oc.H_EST_KINDS)  # noqa: E731
        barrier.handoff_allowed(*args, [v, v], [pre.digest, pre.digest], guard)
        with self.assertRaises(Refused) as e:
            barrier.handoff_allowed(*args, [None, None], [pre.digest, pre.digest], guard)
        self.assertEqual(e.exception.detail, "handoff_unavailable")
        with self.assertRaises(Refused) as e:
            barrier.handoff_allowed(c, barrier.ESTABLISHED, "P13", SLOT, self.cycle, True, [v, v],
                                    [pre.digest] * 2, guard)
        self.assertIs(e.exception.code, Code.STOPPED)
        prov.refuse["H-EST"] = (Code.STATE_UNKNOWN, "x")
        failed = prov.handoff_establish(pre, sel.public(), None)
        with self.assertRaises(Refused) as e:
            barrier.handoff_allowed(*args, [failed, failed], [pre.digest] * 2, guard)
        self.assertEqual(e.exception.detail, "handoff_failed")

    def test_h12_released_holder(self):
        o5.release_chain(self.env, self.env.barrier_id)
        self.refuses(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(released)", doc=self.doc())

    def test_h13_superseded_exists(self):
        o5.synthetic_handoff(self.env, SLOT)
        self.refuses(Code.VERB_NOT_ALLOWED_IN_STATE, "superseded_exists", doc=self.doc())

    def test_wrong_verb_document(self):
        self.refuses(Code.FIELD_INVALID, "handoff document", doc=self.doc(verb="barrier-release"))


if __name__ == "__main__":
    unittest.main()

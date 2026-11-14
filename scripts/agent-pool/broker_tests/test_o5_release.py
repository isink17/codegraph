"""Barrier lifecycle and release eligibility (PR-3a contract R2 §5–§6, R3 §6,
R4 §3.5, R4.1 §3.7). PR-3a decides D1/D2; D3/D4/D3H/D4H only via a PR-3b
Discharge (here the test-only DischargeDouble). No release record is ever
written by PR-3a: `release_check` is the pure PR-4 V/R precondition."""

import unittest

import harness
from harness import SLOT, Env, reason
from broker import barrier, freeze, o5contract as oc
from broker._testsupport import o5
from broker.refusal import Code, Outcome, Refused

OTHER = "codegraph-worker-y"
PD = (Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(preservation_dependent)")


def release(env, bid=None, **kw):
    snap, items = env.snapshot(), dict(env.world.o5.stores["barriers"].items)
    try:
        out = freeze.release_check(env.world, bid or env.barrier_id, **kw)
    except Refused as e:
        out = (e.code, e.detail)
    assert env.snapshot() == snap and env.world.o5.stores["barriers"].items == items, "release_check wrote"
    return out


class Base(unittest.TestCase):
    env_kw = {}

    def setUp(self):
        self.env = Env(**self.env_kw)
        self.addCleanup(self.env.close)
        self.env.seed_leased()
        self.env.seed_leased(slot=OTHER)

    def run_(self, verb, slot=SLOT, op="op1", **kw):
        kw = dict({"quarantine": harness.QUARANTINE, "quiescence": harness.QUIESCENCE}.get(verb, {}), **kw)
        return self.env.run(verb, slot=slot, op_id=op, reason=reason(), **kw)

    def ok(self, *verbs, slot=SLOT, op="op1"):
        for v in verbs:
            r = self.run_(v, slot, op)
            self.assertIs(r.outcome, Outcome.COMMITTED, (v, r))

    def to_p13(self, slot=SLOT, op="op1"):
        self.ok("adopt", "request", "freeze-start", "freeze-continue", "quarantine", "quiescence", slot=slot, op=op)

    def discharges(self):
        self.env.world.o5.preservation = o5.DischargeDouble(self.env.world.observer)


class D1D2(Base):
    def test_idle_slots_are_d1(self):
        self.assertEqual(release(self.env), [(SLOT, "D1"), (OTHER, "D1")])

    def test_positive_pre_freeze_lifecycle(self):
        """A17 / A67 / A104: freeze-start NONE (S3), release D1, released."""
        self.env.freeze_results["freeze-start"] = "NONE"
        self.ok("adopt", "request", "freeze-start")
        self.assertEqual([c.closure for c in freeze.cycles(self.env.world, SLOT, freeze.burned_docs(
            self.env.world, self.env.world.ledger.entries()))], ["none_no_move"])
        self.assertEqual(release(self.env), [(SLOT, "D1"), (OTHER, "D1")])
        o5.release_chain(self.env, self.env.barrier_id)
        self.assertEqual(o5.chain(self.env, self.env.barrier_id).state, barrier.RELEASED)

    def test_thaw_cancel_is_d2(self):
        """A27 / PR3A-B1: thaw -> NONE -> cancel -> marker -> D2; P19/P20 pending_step."""
        self.ok("adopt", "request", "freeze-start", "thaw")
        self.env.faults.point = "library:before_marker_rename"
        with self.assertRaises(BaseException):
            self.run_("cancel")
        self.env.faults.point = None
        self.assertEqual(release(self.env), (Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(pending_step)"))
        self.ok("resume-step")
        self.assertEqual(release(self.env), [(SLOT, "D2"), (OTHER, "D1")])

    def test_pre_freeze_cancel_is_d1(self):
        """A28."""
        self.ok("adopt", "request", "cancel")
        self.assertEqual(release(self.env)[0], (SLOT, "D1"))

    def test_any_dependency_refuses_the_pool(self):
        """A13 / A105 / A29: one slot at P7 blocks the pool release."""
        self.ok("adopt", "request", "freeze-start", slot=OTHER, op="op2")
        self.assertEqual(release(self.env), PD)
        self.ok("adopt", "request", "freeze-start", "thaw")
        self.assertEqual(release(self.env), PD)

    def test_every_row_p7_to_p18_is_preservation_dependent_without_discharge(self):
        """A14 / A50: the synthetic provider never discharges."""
        self.ok("adopt", "request", "freeze-start")
        for verb in ("freeze-continue", "quarantine", "quiescence", "snapshot", "verify-preservation",
                     "preservation", "ready"):
            self.assertEqual(release(self.env), PD, verb)
            self.ok(verb)
        self.assertEqual(release(self.env), PD)
        self.env.world.o5.preservation = None
        self.assertEqual(release(self.env), PD)


class D1Refusals(unittest.TestCase):
    """A30: an eligible-looking row never proves safety (pure release_case)."""

    def view(self, closure, row="P6", barrier_id="B"):
        cyc = freeze.Cycle("c" * 64, SLOT, "op1", 1, "a" * 64, {}, barrier_id, "h" * 64, "", closure, [], {}, None)
        return barrier.SlotView(SLOT, row, None, True, "NONE", [cyc], cyc, [], False)

    def test_open_or_ready_cycles_refuse_d1(self):
        target = barrier.Chain("B", barrier.ESTABLISHED, None, (), None, None, {}, (), None, (SLOT,), (601,),
                               "POOL_MAINTENANCE", "h" * 64, {SLOT: 1}, {})
        for closure in ("open", "ready_pending", "ready"):
            for row in ("P2", "P6"):
                with self.assertRaises(Refused) as e:
                    barrier.release_case(target, self.view(closure, row), {})
                self.assertEqual((e.exception.code, e.exception.detail), PD)
        self.assertEqual(barrier.release_case(target, self.view("none_no_move"), {}), ("D1", None))
        self.assertEqual(barrier.release_case(target, self.view("thaw_complete"), {}), ("D2", None))
        self.assertEqual(barrier.release_case(target, self.view("thaw_complete", barrier_id="X"), {}), ("D1", None))


class Global(Base):
    def test_stop_halt_revoked_and_states(self):
        """A12 / A48 / A55 / A88 and R2 §6.4."""
        self.assertEqual(release(self.env, halt=True), (Code.VERB_NOT_ALLOWED_IN_STATE, "halt"))
        self.env.run("stop", reason=reason())
        self.assertEqual(release(self.env), (Code.STOPPED, ""))
        self.env.run("resume", reason=reason())
        o5.cutover_step(self.env, "model-b-revoke")
        self.assertEqual(release(self.env), (Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(model_a)"))

    def test_interrupted_release_resume(self):
        """A16 / A31 / A103: a RELEASING chain needs the resume; it re-runs the whole check."""
        o5.append_record(self.env, self.env.barrier_id, "release_requested")
        self.assertEqual(release(self.env), (Code.VERB_NOT_ALLOWED_IN_STATE, "release_in_progress"))
        self.assertEqual(release(self.env, resume=True), [(SLOT, "D1"), (OTHER, "D1")])
        self.env.run("stop", reason=reason())
        self.assertEqual(release(self.env, resume=True), (Code.STOPPED, ""))
        self.env.run("resume", reason=reason())
        self.ok("adopt", "request")
        self.assertEqual(release(self.env, resume=True)[0], (SLOT, "D1"))

    def test_invalid_chain_refuses(self):
        o5.append_record(self.env, self.env.barrier_id, "released")
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "barrier_invalid"))


class Discharges(Base):
    """A40 / A41 / A43 / A44 / A45 / A51 and R3 §6.7 traces."""

    def test_own_d3_and_d4(self):
        self.to_p13()
        self.assertEqual(release(self.env), PD)
        self.discharges()
        self.assertEqual(release(self.env), [(SLOT, "D3"), (OTHER, "D1")])
        self.ok("snapshot", "verify-preservation", "preservation", "ready")
        self.assertEqual(release(self.env), [(SLOT, "D4"), (OTHER, "D1")])

    def test_later_pool_barrier_after_a_historical_handoff(self):
        self.to_p13()
        self.discharges()
        self.assertEqual(release(self.env), [(SLOT, "D3"), (OTHER, "D1")])
        o5.release_chain(self.env, self.env.barrier_id)
        b2 = o5.synthetic_barrier(self.env, [SLOT, OTHER])
        self.assertEqual(release(self.env, b2), [(SLOT, "D3H"), (OTHER, "D1")])
        self.ok("adopt", "request", "freeze-start", slot=OTHER, op="op2")
        self.assertEqual(release(self.env, b2), PD)

    def test_missing_handoff_refuses(self):
        env = Env(auto_handoff=False)
        self.addCleanup(env.close)
        self.env = env
        env.seed_leased()
        env.seed_leased(slot=OTHER)
        self.to_p13()
        self.discharges()
        self.assertEqual(release(self.env), PD)

    def test_discharge_must_bind_this_slot_and_cycle(self):
        """A30: a Discharge claiming D3 for another cycle is not accepted."""
        self.to_p13()
        prov = o5.DischargeDouble(self.env.world.observer)
        real = prov.discharge

        def forged(view, holder, pre, sel):
            d = real(view, holder, pre, sel)
            return oc.Discharge(oc._PSEAL, **dict(d._f, cycle_id="c" * 64))
        prov.discharge = forged
        self.env.world.o5.preservation = prov
        self.assertEqual(release(self.env), PD)

    def test_historical_handoff_is_never_a_new_authorization(self):
        """A92 / A93: a RELEASED chain cannot back a new freeze-start."""
        self.to_p13()
        o5.release_chain(self.env, self.env.barrier_id)
        r = self.run_("freeze-start", slot=OTHER, op="op2")
        self.assertIs(r.outcome, Outcome.REFUSED)
        self.ok("adopt", "request", slot=OTHER, op="op2")
        r = self.env.run("freeze-start", slot=OTHER, op_id="op2", reason=reason(), barrier_id=self.env.barrier_id)
        self.assertEqual((r.code, r.detail), (Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(released)"))


class Establishment(unittest.TestCase):
    """A9 / A11 / A47 / A107: overlap, scope, pending step, barrier_id resolution."""

    def test_overlap_scope_and_pending(self):
        env = Env()
        self.addCleanup(env.close)
        chains = {env.barrier_id: o5.chain(env, env.barrier_id)}
        enum = {s: o5.AGENT_UID for s in harness.O5_SLOTS}
        with self.assertRaises(Refused) as e:
            barrier.establish_allowed(oc.SYNTHETIC, chains, "POOL_MAINTENANCE", sorted(enum), [o5.AGENT_UID], enum, [])
        self.assertEqual(e.exception.detail, "barrier_in_use")
        for scope, slots in (("SLOT_ISOLATED", [SLOT]), ("POOL_MAINTENANCE", [SLOT])):
            with self.assertRaises(Refused) as e:
                barrier.establish_allowed(oc.SYNTHETIC, {}, scope, slots, [o5.AGENT_UID], enum, [])
            self.assertEqual(e.exception.detail, "barrier_scope")
        with self.assertRaises(Refused) as e:
            barrier.establish_allowed(oc.SYNTHETIC, {}, "POOL_MAINTENANCE", sorted(enum), [o5.AGENT_UID], enum, [SLOT])
        self.assertEqual(e.exception.detail, "pending_step")
        invalid = {"x": barrier._invalid("x", "gap")}
        with self.assertRaises(Refused):
            barrier.establish_allowed(oc.SYNTHETIC, invalid, "POOL_MAINTENANCE", sorted(enum), [o5.AGENT_UID], enum, [])
        o5.release_chain(env, env.barrier_id)
        chains = {env.barrier_id: o5.chain(env, env.barrier_id)}
        barrier.establish_allowed(oc.SYNTHETIC, chains, "POOL_MAINTENANCE", sorted(enum), [o5.AGENT_UID], enum, [])

    def test_activating_chain_and_ambiguous_resolution(self):
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        env.seed_leased()
        o5.synthetic_model_b(env, list(harness.O5_SLOTS))
        o5.synthetic_barrier(env, list(harness.O5_SLOTS), upto="activation_attempted")
        env.auto_handoff = env.o5_fixtures = True
        for verb in ("adopt", "request"):
            env.run(verb, slot=SLOT, op_id="op1", reason=reason())
        r = env.run("freeze-start", slot=SLOT, op_id="op1", reason=reason())
        self.assertEqual((r.code, r.detail), (Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(activating)"))
        env.world.o5.stores["barriers"].items["barrier/" + "e" * 64 + "/0000000000"] = b"{}"
        r = env.run("freeze-start", slot=SLOT, op_id="op1", reason=reason())
        self.assertEqual((r.code, r.detail), (Code.FIELD_INVALID, "barrier_ambiguous"))


if __name__ == "__main__":
    unittest.main()

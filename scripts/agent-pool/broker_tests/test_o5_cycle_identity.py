"""PR-3a.1 (PR-3b contract R4.1 §2): the canonical cycle identity reaches the
F effect, the PR-3b result check and the `refreshed` record as one value,
derived from the burned freeze-start document and never from a later
authorization. Expected identities are recomputed here from the ledger, not
read back from the code under test."""

import json
import types
import unittest
from unittest import mock

import harness
from broker import authz, freeze, invocation, o5contract as oc
from broker._testsupport import capability, o5
from broker.refusal import Code, Outcome, Refused
from broker.stores import Crash
from harness import SLOT, Env, reason

OP = "op1"
OTHER = harness.O5_SLOTS[1]
EXTRA = {"quarantine": harness.QUARANTINE, "quiescence": harness.QUIESCENCE}


class Base(unittest.TestCase):
    env_kw = {}
    gen = 1

    def setUp(self):
        self.env = Env(**dict(self.env_kw, o5_fixtures=self.gen == 1))
        self.addCleanup(self.env.close)
        self.env.seed_leased(gen=self.gen)
        self.env.seed_leased(slot=OTHER)
        if self.gen != 1:
            o5.synthetic_model_b(self.env, list(harness.O5_SLOTS))
            self.env.barrier_id = o5.synthetic_barrier(self.env, list(harness.O5_SLOTS), gens={SLOT: self.gen})
            self.env.o5_fixtures = True
        self.effects, self.checks = [], []
        inner = self.env.world.freeze

        def effect(verb, slot, op, ctx, cid):
            self.effects.append((verb, cid))
            return inner(verb, slot, op, ctx, cid)
        self.env.world.freeze = effect
        prov = self.env.world.o5.preservation
        if prov is not None:
            real = prov.check_fresult

            def check(ctx, verb, res, cid):
                self.checks.append((verb, cid))
                return real(ctx, verb, res, cid)
            prov.check_fresult = check

    def run_(self, verb, **kw):
        return self.env.run(verb, slot=SLOT, op_id=OP, reason=reason(), **dict(EXTRA.get(verb, {}), **kw))

    def ok(self, *verbs):
        for verb in verbs:
            r = self.run_(verb)
            self.assertIs(r.outcome, Outcome.COMMITTED, (verb, r))

    def crash(self, point, verb):
        self.env.faults.point = point
        try:
            with self.assertRaises(Crash):
                self.run_(verb)
        finally:
            self.env.faults.point = None

    def docs(self):
        w = self.env.world
        return freeze.burned_docs(w, w.ledger.entries())

    def start_auths(self):
        """Burned freeze-start documents of SLOT, in ledger order."""
        docs = self.docs()
        return [e["auth_digest"] for e in self.env.world.ledger.entries()
                if (docs.get(e["auth_digest"]) or {}).get("verb") == "freeze-start"
                and docs[e["auth_digest"]].get("slot") == SLOT]

    def last_auth(self):
        return self.env.world.ledger.entries()[-1]["auth_digest"]

    def expected(self, n=-1):
        return oc.cycle_id(SLOT, self.gen, OP, self.start_auths()[n])

    def refreshed(self):
        """(owner_auth, cycle_id) of every `refreshed` record for SLOT."""
        return [(r["owner_auth"], r["detail"]["cycle_id"]) for r in freeze.refreshed_records(self.env.world)
                if r["detail"].get("slot") == SLOT]

    def cycles(self):
        return freeze.cycles(self.env.world, SLOT, self.docs())

    def burn_classes(self):
        w = self.env.world
        entries, docs = w.ledger.entries(), self.docs()
        cyc = freeze.cycles(w, SLOT, docs)
        return [freeze.classify_burn(w, SLOT, docs, entries, a, cyc)
                for a in freeze.freeze_start_burns(w, docs, entries, SLOT)]

    def frozen(self):
        """The O5 state a W-phase refusal must leave untouched."""
        w = self.env.world
        return (json.dumps(w.fwal.entries, sort_keys=True), dict(w.o5.stores["barriers"].items),
                dict(w.o5.stores["journals"].items), dict(w.o5.stores["artifacts"].items), list(self.effects),
                list(self.checks))

    def unfinished_without_effect(self, verb, **kw):
        """A refusal inside W: the document is burned (merged invoke maps it to
        UNFINISHED); no F WAL entry, no effect, no check, no barrier record."""
        o5.observe(self.env, SLOT)
        before, n = self.frozen(), len(self.env.world.ledger.entries())
        r = invocation.invoke(self.env.world, json.dumps(dict(
            {"v": 1, "verb": verb, "slot": SLOT, "op_id": OP, "reason": reason()}, **kw)).encode(),
            self.env.confirmer)
        self.assertIs(r.outcome, Outcome.UNFINISHED, r)
        self.assertEqual(self.frozen(), before)
        self.assertEqual(len(self.env.world.ledger.entries()), n + 1)
        return r

    def refused_without_write(self, verb, code, observe=True, **kw):
        if observe:
            o5.observe(self.env, SLOT)
        snap, writes, before = self.env.snapshot(), list(self.env.world.writes()), self.frozen()
        r = invocation.invoke(self.env.world, json.dumps(dict(
            {"v": 1, "verb": verb, "slot": SLOT, "op_id": OP, "reason": reason()}, **kw)).encode(),
            self.env.confirmer)
        self.assertEqual((r.outcome, r.code), (Outcome.REFUSED, code), r)
        self.assertEqual((self.env.snapshot(), list(self.env.world.writes()), self.frozen()),
                         (snap, writes, before))
        return r


def substitute_cycle(pick):
    """A TCB fault: the gate's authenticated cycle is replaced by `pick(gate)`."""
    real = invocation.o5freeze.check

    def check(world, plan, *a, **k):
        gate = real(world, plan, *a, **k)
        return gate if gate is None else gate._replace(cycle=pick(gate))
    return mock.patch.object(invocation.o5freeze, "check", check)


class Propagation(Base):
    def test_full_trace_through_invoke_uses_one_identity(self):
        """Positive trace: every F effect, every check and every `refreshed`
        record of the cycle carries the id derived from the burned freeze-start."""
        self.ok("adopt", "request", "freeze-start")
        cid = self.expected()
        self.assertEqual(cid, oc.cycle_id(SLOT, 1, OP, self.start_auths()[0]))
        self.ok("freeze-continue", "quarantine", "quiescence", "snapshot", "verify-preservation", "preservation",
                "ready")
        verbs = ["freeze-start", "freeze-continue", "snapshot", "verify-preservation"]
        self.assertEqual(self.effects, [(v, cid) for v in verbs])
        self.assertEqual(self.checks, self.effects)
        self.assertEqual({c for _, c in self.refreshed()}, {cid})
        self.assertGreaterEqual(len(self.refreshed()), 2)
        self.assertEqual([c.cycle_id for c in self.cycles()], [cid])
        self.assertEqual(self.cycles()[0].closure, "ready")

    def test_freeze_start_identity_is_derived_from_its_own_burn(self):
        self.ok("adopt", "request", "freeze-start")
        auth = self.start_auths()[0]
        self.assertEqual(auth, self.last_auth())
        self.assertEqual(self.effects, [("freeze-start", oc.cycle_id(SLOT, 1, OP, auth))])
        self.assertEqual(self.checks, self.effects)
        self.assertEqual(self.refreshed(), [(auth, oc.cycle_id(SLOT, 1, OP, auth))])
        self.assertNotEqual(self.effects[0][1], oc.cycle_id(SLOT, 1, OP, auth[::-1]))

    def test_freeze_continue_keeps_the_cycle(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue")
        cont = self.last_auth()
        cid = self.expected()
        self.assertEqual(self.effects, [("freeze-start", cid), ("freeze-continue", cid)])
        self.assertIn((cont, cid), self.refreshed())
        self.assertNotEqual(cid, oc.cycle_id(SLOT, 1, OP, cont))

    def test_thaw_keeps_the_cycle(self):
        self.ok("adopt", "request", "freeze-start", "thaw")
        cid = self.expected()
        self.assertEqual(self.effects, [("freeze-start", cid), ("thaw", cid)])
        self.assertEqual(self.checks, self.effects)
        self.assertNotEqual(cid, oc.cycle_id(SLOT, 1, OP, self.last_auth()))

    def test_a_new_cycle_of_the_same_operation_gets_a_new_identity(self):
        """Same slot, operation and generation; only the freeze-start
        authorization differs."""
        self.ok("adopt", "request", "freeze-start", "thaw")
        self.assertEqual(self.cycles()[0].closure, "thaw_complete")
        self.ok("freeze-start", "freeze-continue")
        first, second = self.start_auths()
        c1, c2 = oc.cycle_id(SLOT, 1, OP, first), oc.cycle_id(SLOT, 1, OP, second)
        self.assertNotEqual(c1, c2)
        self.assertEqual(self.effects, [("freeze-start", c1), ("thaw", c1), ("freeze-start", c2),
                                        ("freeze-continue", c2)])
        self.assertEqual([c.cycle_id for c in self.cycles()], [c1, c2])


class OtherGeneration(Base):
    gen = 2

    def test_generation_is_part_of_the_identity(self):
        self.ok("adopt", "request", "freeze-start")
        auth = self.start_auths()[0]
        self.assertEqual(self.docs()[auth]["slot_generation"], 2)
        self.assertEqual(self.effects, [("freeze-start", oc.cycle_id(SLOT, 2, OP, auth))])
        self.assertNotEqual(self.effects[0][1], oc.cycle_id(SLOT, 1, OP, auth))


class Resume(Base):
    def test_move_intent_resume_uses_the_original_freeze_start(self):
        self.ok("adopt", "request")
        self.crash("fwal:append:after", "freeze-start")
        self.assertEqual(self.effects, [])
        cid = self.expected()
        self.ok("resume-step")
        resume = self.last_auth()
        self.assertEqual(self.effects, [("freeze-start", cid)])
        self.assertEqual(self.checks, self.effects)
        self.assertNotEqual(cid, oc.cycle_id(SLOT, 1, OP, resume))
        self.assertEqual(self.refreshed(), [(resume, cid)])
        self.assertEqual(self.burn_classes(), ["COMPLETED"])

    def test_later_intent_resume_uses_the_existing_cycle(self):
        self.ok("adopt", "request", "freeze-start")
        cid = self.expected()
        self.crash("fwal:append:after", "freeze-continue")
        self.ok("resume-step")
        self.assertEqual(self.effects, [("freeze-start", cid), ("freeze-continue", cid)])
        self.assertNotEqual(cid, oc.cycle_id(SLOT, 1, OP, self.last_auth()))

    def test_snapshot_and_verify_resume_keep_the_cycle(self):
        self.ok("adopt", "request", "freeze-start", "freeze-continue", "quarantine", "quiescence")
        cid = self.expected()
        self.crash("fwal:append:after", "snapshot")
        self.ok("resume-step")
        self.crash("fwal:append:after", "verify-preservation")
        self.ok("resume-step")
        self.assertEqual([c for _, c in self.effects], [cid] * 4)
        self.assertEqual([v for v, _ in self.effects][2:], ["snapshot", "verify-preservation"])

    def test_conflicting_original_authorization_refuses(self):
        """R4.1 §2.2 defensive cross-check (model N-BX1-5): reachable only by a
        TCB fault that hands W a cycle whose freeze-start is not the intent's."""
        self.ok("adopt", "request")
        self.crash("fwal:append:after", "freeze-start")
        forged = "f" * 64

        def pick(gate):
            c = gate.cycle
            return c._replace(freeze_start_auth=forged, cycle_id=oc.cycle_id(c.slot, c.g_pre, c.op_id, forged))
        with substitute_cycle(pick):
            self.unfinished_without_effect("resume-step")
        self.assertEqual(self.burn_classes(), ["INTENT_PENDING"])

    def test_resume_with_no_authenticated_cycle_refuses(self):
        self.ok("adopt", "request")
        self.crash("fwal:append:after", "freeze-start")
        with substitute_cycle(lambda gate: None):
            self.unfinished_without_effect("resume-step")
        self.ok("resume-step")
        self.assertEqual(self.effects, [("freeze-start", self.expected())])


class Substitution(Base):
    def two_cycles(self):
        self.ok("adopt", "request", "freeze-start", "thaw", "freeze-start")
        old, cur = self.cycles()
        return old, cur

    def test_previous_cycle_substitution_refuses(self):
        """The substituted cycle is self-consistent and of the same slot,
        operation and generation; it does not end at the operation's F WAL."""
        old, cur = self.two_cycles()
        self.assertEqual((old.slot, old.op_id, old.g_pre), (cur.slot, cur.op_id, cur.g_pre))
        with substitute_cycle(lambda gate: old):
            self.unfinished_without_effect("freeze-continue")
        self.ok("freeze-continue")
        self.assertEqual(self.effects[-1], ("freeze-continue", cur.cycle_id))

    def test_other_slot_cycle_substitution_refuses(self):
        self.ok("adopt", "request", "freeze-start")
        self.env.run("adopt", slot=OTHER, op_id="op2", reason=reason())
        self.env.run("request", slot=OTHER, op_id="op2", reason=reason())
        r = self.env.run("freeze-start", slot=OTHER, op_id="op2", reason=reason())
        self.assertIs(r.outcome, Outcome.COMMITTED, r)
        foreign = freeze.cycles(self.env.world, OTHER, self.docs())[-1]
        with substitute_cycle(lambda gate: foreign):
            self.unfinished_without_effect("freeze-continue")

    def test_wrong_slot_cycle_refuses(self):
        """Only the slot differs: self-consistent id, same F WAL entries."""
        self.ok("adopt", "request", "freeze-start")
        with substitute_cycle(lambda gate: gate.cycle._replace(slot=OTHER, cycle_id=oc.cycle_id(
                OTHER, gate.cycle.g_pre, OP, gate.cycle.freeze_start_auth))):
            self.unfinished_without_effect("freeze-continue")

    def test_recomputed_identity_mismatch_refuses(self):
        self.ok("adopt", "request", "freeze-start")
        with substitute_cycle(lambda gate: gate.cycle._replace(cycle_id="c" * 64)):
            self.unfinished_without_effect("freeze-continue")

    def test_wrong_generation_cycle_refuses(self):
        self.ok("adopt", "request", "freeze-start")
        with substitute_cycle(lambda gate: gate.cycle._replace(g_pre=2, cycle_id=oc.cycle_id(
                SLOT, 2, OP, gate.cycle.freeze_start_auth))):
            self.unfinished_without_effect("freeze-continue")

    def test_cycle_handed_to_freeze_start_refuses(self):
        self.ok("adopt", "request", "freeze-start", "thaw")
        old = self.cycles()[0]
        with substitute_cycle(lambda gate: old):
            self.unfinished_without_effect("freeze-start")
        self.assertEqual(self.burn_classes(), ["COMPLETED", "ABANDONED_PRE_INTENT"])

    def test_synthetic_effect_rejects_a_foreign_identity(self):
        """The test effect itself validates what it receives."""
        self.ok("adopt", "request", "freeze-start")
        with self.assertRaises(ValueError):
            o5.expect_cycle(self.env.world, SLOT, OP, "c" * 64)
        self.assertEqual(o5.expect_cycle(self.env.world, SLOT, OP, self.expected()), self.expected())

    def test_synthetic_check_rejects_an_unstamped_result(self):
        self.ok("adopt", "request", "freeze-start")
        cid = self.expected()
        prov, gate = self.env.world.o5.preservation, types.SimpleNamespace(pre=types.SimpleNamespace(
            cycle_id=cid, slot=SLOT, op_id=OP))
        good = o5.fresult("REVOKED", "freeze-continue", SLOT, OP, cid)
        type(prov).check_fresult(prov, gate, "freeze-continue", good, cid)
        for res, c in ((o5.fresult("REVOKED", "freeze-continue", SLOT, OP, "c" * 64), cid),
                       (good, "c" * 64)):
            with self.assertRaises(ValueError):
                type(prov).check_fresult(prov, gate, "freeze-continue", res, c)


class Helper(unittest.TestCase):
    """_f_cycle_id and _f_complete over hand-built inputs."""

    AUTH = "a" * 64

    def facts(self, **kw):
        base = dict(slot=SLOT, op_id=OP, op=types.SimpleNamespace(gen=1), l=None, f_entry=None)
        return invocation.Facts(**dict(base, **kw))

    def doc(self, **kw):
        return dict({"verb": "freeze-start", "slot": SLOT, "op_id": OP, "slot_generation": 1}, **kw)

    def test_freeze_start_derivation(self):
        gate = types.SimpleNamespace(cycle=None)
        self.assertEqual(invocation._f_cycle_id(gate, self.facts(), "freeze-start", self.AUTH, self.doc()),
                         oc.cycle_id(SLOT, 1, OP, self.AUTH))

    def test_freeze_start_binding_mismatches_refuse(self):
        gate = types.SimpleNamespace(cycle=None)
        cases = [("freeze-start", self.doc(slot=OTHER)), ("freeze-start", self.doc(op_id="op2")),
                 ("freeze-start", self.doc(slot_generation=2)), ("freeze-start", self.doc(slot_generation="1")),
                 ("freeze-start", self.doc(verb="thaw")), ("freeze-continue", self.doc()),
                 ("freeze-start", {})]
        for verb, doc in cases:
            with self.subTest(verb=verb, doc=doc), self.assertRaises(Refused) as e:
                invocation._f_cycle_id(gate, self.facts(), verb, self.AUTH, doc)
            self.assertEqual((e.exception.code, e.exception.detail), (Code.STATE_UNKNOWN, "barrier_binding"))

    def test_missing_or_malformed_identity_never_reaches_the_effect(self):
        calls = []
        world = types.SimpleNamespace(freeze=lambda *a: calls.append(a))
        plan = types.SimpleNamespace(o5=types.SimpleNamespace(ctx=None))
        for cid in (None, "", "c" * 63, "C" * 64, "g" * 64, "c" * 64 + "\n", 7, b"c" * 64):
            with self.subTest(cid=cid), self.assertRaises(Refused) as e:
                invocation._f_complete(world, plan, self.facts(), None, "freeze-start", self.AUTH, cid)
            self.assertEqual(e.exception.detail, "cycle_inconsistent")
        self.assertEqual(calls, [])

    def test_the_identity_is_required(self):
        """No default: the effect, the check and the W helpers take it positionally."""
        import inspect
        for fn, name in ((invocation._f_complete, "cid"), (o5.SyntheticPreservation.check_fresult, "cycle_id"),
                         (o5.fresult, "cycle_id")):
            p = inspect.signature(fn).parameters[name]
            self.assertIs(p.default, inspect.Parameter.empty, fn)
        env = Env()
        self.addCleanup(env.close)
        with self.assertRaises(TypeError):
            env.world.freeze("freeze-start", SLOT, OP, None)


class Crashes(Base):
    def test_crash_after_the_burn_before_the_intent(self):
        """The abandoned burn named no effect; a re-issued freeze-start derives
        its identity from its own burn, not from the abandoned one."""
        self.ok("adopt", "request")
        self.crash("fwal:append:before", "freeze-start")
        self.assertEqual((self.effects, self.burn_classes()), ([], ["ABANDONED_PRE_INTENT"]))
        abandoned = self.start_auths()[0]
        self.ok("freeze-start")
        fresh = self.start_auths()[1]
        self.assertEqual(self.effects, [("freeze-start", oc.cycle_id(SLOT, 1, OP, fresh))])
        self.assertNotEqual(self.effects[0][1], oc.cycle_id(SLOT, 1, OP, abandoned))

    def test_crash_after_the_intent(self):
        self.ok("adopt", "request")
        self.crash("fwal:append:after", "freeze-start")
        self.assertEqual((self.effects, self.burn_classes()), ([], ["INTENT_PENDING"]))
        self.ok("resume-step")
        self.assertEqual(self.effects, [("freeze-start", self.expected())])

    def test_crash_inside_the_effect_does_not_replay_on_retry(self):
        """The effect ran (it raised Crash after its first write): the burn is
        INTENT_PENDING and only resume-step, never a second freeze-start, may complete it."""
        self.ok("adopt", "request")
        inner = self.env.world.freeze

        def boom(verb, slot, op, ctx, cid):
            inner(verb, slot, op, ctx, cid)
            raise Crash("effect:after_header")
        self.env.world.freeze = boom
        with self.assertRaises(Crash):
            self.run_("freeze-start")
        self.assertEqual(self.effects, [("freeze-start", self.expected())])
        r = self.run_("freeze-start")
        self.assertIs(r.outcome, Outcome.REFUSED)
        self.assertEqual(len(self.effects), 1)

    def test_crash_after_completion_before_refreshed(self):
        self.ok("adopt", "request")
        self.crash("barriers:put:before", "freeze-start")
        cid = self.expected()
        self.assertEqual((self.burn_classes(), self.refreshed()), (["COMPLETED"], []))
        self.assertIs(self.run_("resume-step").outcome, Outcome.ALREADY_COMPLETE)
        self.ok("freeze-continue")
        self.assertEqual(self.effects, [("freeze-start", cid), ("freeze-continue", cid)])
        self.assertEqual(self.refreshed(), [(self.last_auth(), cid)])

    def test_refreshed_with_an_invalid_completion_binding(self):
        """B2 regression through the new interface: the effect is never invoked."""
        self.ok("adopt", "request", "freeze-start")
        done = [e for e in self.env.world.fwal.log(SLOT) if e["kind"] == "complete"][-1]
        o5.append_record(self.env, self.env.barrier_id, "refreshed", auth=done["auth_digest"], verb="freeze-start",
                         detail={"slot": SLOT, "cycle_id": self.expected(), "completion": "e" * 64})
        n = len(self.effects)
        self.refused_without_write("freeze-continue", Code.STATE_UNKNOWN)
        self.assertEqual(len(self.effects), n)


class Regressions(Base):
    """PR #544 fixes B1, B2, B2-R and B3 still hold with the five-argument effect."""

    def test_b1_protected_mode_refuses_before_any_effect(self):
        self.ok("adopt", "request")
        self.env.world.ctx = authz.Ctx(capability.owner_capability(), authz.PROTECTED)
        r = self.refused_without_write("freeze-start", Code.QUIESCENCE_NOT_ESTABLISHED, observe=False)
        self.assertEqual(r.detail, "barrier_unavailable(protected_mode_unsupported)")
        self.assertEqual(self.effects, [])

    def test_b2_refresh_without_completion_never_reruns_the_effect(self):
        self.ok("adopt", "request")
        self.crash("fwal:append:after", "freeze-start")
        auth = self.env.world.fwal.log(SLOT)[0]["auth_digest"]
        o5.append_record(self.env, self.env.barrier_id, "refreshed", auth=auth, verb="freeze-start",
                         detail={"slot": SLOT, "cycle_id": self.expected(), "completion": "d" * 64})
        self.assertEqual(self.burn_classes(), ["STATE_UNKNOWN(cycle_inconsistent)"])
        self.refused_without_write("resume-step", Code.STATE_UNKNOWN)
        self.assertEqual(self.effects, [])

    def test_b2r_unknown_cycle_refresh_invalidates_the_chain(self):
        self.ok("adopt", "request", "freeze-start")
        o5.append_record(self.env, self.env.barrier_id, "refreshed", verb="freeze-start",
                         detail={"cycle_id": "c" * 64, "completion": "d" * 64})
        self.refused_without_write("freeze-continue", Code.STATE_UNKNOWN)
        self.assertEqual(len(self.effects), 1)

    def test_b3_unbound_burn_is_not_abandoned(self):
        self.ok("adopt", "request")
        w = self.env.world
        h = freeze.head_of(w, w.ledger.entries(), self.docs()).digest
        o5.burn(self.env, o5._owner_doc(self.env, "freeze-start", slot=SLOT, op_id=OP, slot_generation=2,
                                        f_from="NONE", o5={"barrier_id": self.env.barrier_id, "cutover_head": h}))
        self.assertEqual(self.burn_classes(), ["STATE_UNKNOWN(barrier_binding)"])
        self.refused_without_write("freeze-start", Code.STATE_UNKNOWN)
        self.assertEqual(self.effects, [])


class Refusals(Base):
    def test_pr3b_absent_refuses_before_any_effect(self):
        self.ok("adopt", "request")
        self.env.world.o5.preservation = None
        r = self.refused_without_write("freeze-start", Code.PROVENANCE_INSUFFICIENT)
        self.assertEqual((r.detail, self.effects), ("preservation_unavailable", []))

    def test_context_change_between_v_and_r(self):
        self.ok("adopt", "request")
        o5.observe(self.env, SLOT)
        real = self.env.confirmer

        def confirmer(text, nonce):
            o5.observe(self.env, SLOT)
            return real(text, nonce)
        self.env.confirmer = confirmer
        r = self.refused_without_write("freeze-start", Code.CONTEXT_CHANGED, observe=False)
        self.assertEqual((r.outcome, self.effects), (Outcome.REFUSED, []))

    def test_identity_is_not_a_request_field(self):
        """No caller can supply or steer the identity."""
        self.ok("adopt", "request", "freeze-start")
        for verb in ("freeze-continue", "resume-step"):
            self.refused_without_write(verb, Code.FIELD_UNKNOWN, cycle_id=self.expected())
        self.assertEqual(len(self.effects), 1)

    def test_non_owner_cannot_reach_the_effect(self):
        self.ok("adopt", "request", "freeze-start")
        self.env.world.ctx = authz.Ctx(object(), authz.SYNTHETIC)
        self.refused_without_write("freeze-continue", Code.OWNER_REQUIRED)
        self.assertEqual(len(self.effects), 1)


class CarryForward(Base):
    """PR #545 review N2–N5 (owner disposition 2026-11-15)."""

    def test_w_phase_generation_mismatch_through_invoke(self):
        """N2: a W-phase `_f_cycle_id` generation mismatch, driven through the
        merged invoke: UNFINISHED, the burn committed, no intent, no effect, the
        burn ABANDONED_PRE_INTENT and the release D1."""
        self.ok("adopt", "request")
        real = invocation._f_cycle_id

        def skewed(gate, facts, verb, digest, doc):
            return real(gate, facts, verb, digest, dict(doc, slot_generation=doc["slot_generation"] + 1))
        with mock.patch.object(invocation, "_f_cycle_id", skewed):
            self.unfinished_without_effect("freeze-start")
        self.assertEqual(self.burn_classes(), ["ABANDONED_PRE_INTENT"])
        self.assertEqual(dict(freeze.release_check(self.env.world, self.env.barrier_id)).get(SLOT), "D1")

    def test_burned_document_of_another_generation_is_not_abandoned(self):
        """N2: a burned freeze-start whose document names another generation
        than the library's pre-quarantine generation classifies
        STATE_UNKNOWN(barrier_binding), never ABANDONED_PRE_INTENT, and the
        release refuses for the slot."""
        self.ok("adopt", "request")
        w = self.env.world
        head = freeze.head_of(w, w.ledger.entries(), self.docs()).digest
        doc = o5._owner_doc(self.env, "freeze-start", slot=SLOT, op_id=OP, slot_generation=self.gen + 1,
                            f_from="NONE", o5={"barrier_id": self.env.barrier_id, "cutover_head": head})
        o5.burn(self.env, doc)
        self.assertEqual(self.burn_classes(), ["STATE_UNKNOWN(barrier_binding)"])
        with self.assertRaises(Refused) as e:
            freeze.release_check(self.env.world, self.env.barrier_id)
        self.assertEqual((e.exception.code, e.exception.detail), (Code.STATE_UNKNOWN, "STATE_UNKNOWN(barrier_binding)"))

    def test_harness_effect_rejects_a_foreign_identity(self):
        """N3: the harness effect itself (World.freeze) refuses an identity that
        is not the pending intent's cycle, and accepts the right one."""
        self.ok("adopt", "request", "freeze-start")
        ctx = types.SimpleNamespace(pre=types.SimpleNamespace(cycle_id=self.expected()))
        with self.assertRaises(ValueError):
            self.env.world.freeze("freeze-continue", SLOT, OP, ctx, "c" * 64)
        res = self.env.world.freeze("freeze-continue", SLOT, OP, ctx, self.expected())
        self.assertEqual(res.bindings, o5.stamp(res.bindings, SLOT, OP, self.expected()))

    def test_synthetic_check_compares_the_context_cycle(self):
        """N4: a result consistently stamped with a foreign identity, passed with
        that same identity, is refused only by the `ctx.pre.cycle_id` comparison."""
        self.ok("adopt", "request", "freeze-start")
        prov = self.env.world.o5.preservation
        gate = types.SimpleNamespace(pre=types.SimpleNamespace(cycle_id=self.expected(), slot=SLOT, op_id=OP))
        foreign = o5.fresult("REVOKED", "freeze-continue", SLOT, OP, "c" * 64)
        with self.assertRaisesRegex(ValueError, "not the context's cycle"):
            type(prov).check_fresult(prov, gate, "freeze-continue", foreign, "c" * 64)
        first = types.SimpleNamespace(pre=types.SimpleNamespace(cycle_id=None, slot=SLOT, op_id=OP))
        type(prov).check_fresult(prov, first, "freeze-start", o5.fresult("MOVED", "freeze-start", SLOT, OP, "c" * 64),
                                 "c" * 64)

    def test_second_freeze_start_after_an_effect_crash_refuses_with_its_code(self):
        """N5: the exact refusal of a second freeze-start once an intent is pending."""
        self.ok("adopt", "request")
        inner = self.env.world.freeze

        def boom(verb, slot, op, ctx, cid):
            inner(verb, slot, op, ctx, cid)
            raise Crash("effect:after_header")
        self.env.world.freeze = boom
        with self.assertRaises(Crash):
            self.run_("freeze-start")
        self.env.world.freeze = inner
        r = self.run_("freeze-start")
        self.assertEqual((r.outcome, r.code, r.detail), (Outcome.REFUSED, Code.VERB_NOT_ALLOWED_IN_STATE, ""))
        self.assertEqual(self.burn_classes(), ["INTENT_PENDING"])


if __name__ == "__main__":
    unittest.main()

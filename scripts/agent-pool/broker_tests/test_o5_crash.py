"""PR-3a crash classification (contract R4 §2) and the R4.1 open-cycle rule for
READY_PENDING (R4.1 §3.3, §3.7, §3.8 T1–T12), driven through the merged PR-1
invocation with injected FaultPlan crash points. Every negative case checks
that nothing was written."""

import unittest

import harness
from broker import custody
from harness import SLOT, Env, reason
from broker import cutover, freeze, o5contract as oc
from broker._testsupport import o5
from broker.refusal import Code, Outcome, Refused
from broker.stores import Crash

OP = "op1"


def run(env, verb, **kw):
    return env.run(verb, slot=SLOT, op_id=OP, reason=reason(), **kw)


def ok(test, env, verb, **kw):
    r = run(env, verb, **kw)
    test.assertIs(r.outcome, Outcome.COMMITTED, (verb, r))


def crash(env, point, verb, **kw):
    env.faults.point = point
    try:
        run(env, verb, **kw)
    except Crash:
        return True
    finally:
        env.faults.point = None
    return False


def docs(env):
    w = env.world
    return freeze.burned_docs(w, w.ledger.entries())


def cycles(env):
    return freeze.cycles(env.world, SLOT, docs(env))


def burn_classes(env):
    w = env.world
    entries = w.ledger.entries()
    d = freeze.burned_docs(w, entries)
    cyc = freeze.cycles(w, SLOT, d)
    return [freeze.classify_burn(w, SLOT, d, entries, a, cyc) for a in freeze.freeze_start_burns(w, d, entries, SLOT)]


def release(env, barrier_id=None, **kw):
    try:
        return freeze.release_check(env.world, barrier_id or env.barrier_id, **kw)
    except Refused as e:
        return (e.code, e.detail)


def row(env):
    from broker import invocation, lifecycle
    w = env.world
    f = invocation._read_slot(w, SLOT, w.ledger.entries())
    c = lifecycle.Components(invocation._g(w), f.l, f.a, f.f, f.r, f.k, f.op)
    return lifecycle.row(c)


def head(env):
    w = env.world
    e = w.ledger.entries()
    return freeze.head_of(w, e, freeze.burned_docs(w, e))


PD = (Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(preservation_dependent)")


class BurnClassification(unittest.TestCase):
    """R4 §2.2 / A64′, A73–A84 over the merged W order."""

    def setUp(self):
        self.env = Env()
        self.addCleanup(self.env.close)
        self.env.seed_leased()
        self.env.seed_leased(slot=harness.O5_SLOTS[1])
        ok(self, self.env, "adopt")
        ok(self, self.env, "request")

    def test_crash_before_the_burn_is_not_committed(self):
        """A73: document stored, no burn: nothing to classify; D1."""
        self.assertTrue(crash(self.env, "ledger:append:start", "freeze-start"))
        self.assertEqual(burn_classes(self.env), [])
        self.assertEqual(release(self.env), [(SLOT, "D1"), (harness.O5_SLOTS[1], "D1")])

    def test_crash_after_the_burn_before_the_intent_is_abandoned(self):
        """A74 / A64′: ABANDONED_PRE_INTENT, D1; a re-issued freeze-start proceeds."""
        self.assertTrue(crash(self.env, "fwal:append:before", "freeze-start"))
        self.assertEqual(burn_classes(self.env), ["ABANDONED_PRE_INTENT"])
        self.assertEqual(cycles(self.env), [])
        self.assertEqual(release(self.env), [(SLOT, "D1"), (harness.O5_SLOTS[1], "D1")])
        ok(self, self.env, "freeze-start")
        self.assertEqual(sorted(burn_classes(self.env)), ["ABANDONED_PRE_INTENT", "COMPLETED"])

    def test_abandoned_burn_with_a_trace_is_inconsistent(self):
        """A64′ negative / A79: a journal for the cycle exists but no intent."""
        self.assertTrue(crash(self.env, "fwal:append:before", "freeze-start"))
        auth = freeze.freeze_start_burns(self.env.world, docs(self.env), self.env.world.ledger.entries(), SLOT)[0]
        cid = oc.cycle_id(SLOT, 1, OP, auth)
        self.env.world.o5.stores["journals"].items[f"journal/{cid}/0000000000"] = b"{}"
        self.assertEqual(burn_classes(self.env), ["STATE_UNKNOWN(cycle_inconsistent)"])
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "STATE_UNKNOWN(cycle_inconsistent)"))
        r = run(self.env, "freeze-start")
        self.assertEqual((r.code, r.detail), (Code.STATE_UNKNOWN, "STATE_UNKNOWN(cycle_inconsistent)"))

    def test_burn_with_its_document_deleted_is_auth_missing(self):
        """A80: found by its binding key; never regenerated, never D1."""
        self.assertTrue(crash(self.env, "fwal:append:before", "freeze-start"))
        auth = freeze.freeze_start_burns(self.env.world, docs(self.env), self.env.world.ledger.entries(), SLOT)[0]
        del self.env.world.authz.docs[auth]
        self.assertEqual(burn_classes(self.env), ["STATE_UNKNOWN(auth_missing)"])
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "STATE_UNKNOWN(auth_missing)"))

    def test_crash_after_the_intent_is_intent_pending(self):
        """A75–A77: the effect may have run; never treated as absent."""
        for point in ("fwal:append:after",):
            self.assertTrue(crash(self.env, point, "freeze-start"))
            self.assertEqual(burn_classes(self.env), ["INTENT_PENDING"])
            self.assertEqual(release(self.env), PD)
            ok(self, self.env, "resume-step")
            self.assertEqual(burn_classes(self.env), ["COMPLETED"])

    def test_effect_raising_leaves_intent_pending(self):
        def boom(verb, slot, op, ctx):
            raise Crash("effect:after_header")
        self.env.world.freeze = boom
        with self.assertRaises(Crash):
            run(self.env, "freeze-start")
        self.assertEqual(burn_classes(self.env), ["INTENT_PENDING"])
        self.assertEqual(release(self.env), PD)

    def test_out_of_relation_result_is_unfinished_with_the_intent_pending(self):
        """C1′: an effect result outside the F relation raises in W."""
        self.env.world.freeze = lambda verb, slot, op, ctx: oc.FResult("VERIFIED", "", {})
        r = run(self.env, "freeze-start")
        self.assertIs(r.outcome, Outcome.UNFINISHED)
        self.assertEqual(burn_classes(self.env), ["INTENT_PENDING"])

    def test_tampered_f_wal_is_state_unknown(self):
        """A78: a value outside the F relation; no exemption, release refuses."""
        ok(self, self.env, "freeze-start")
        auth = self.env.world.fwal.entries[SLOT][0]["auth_digest"]
        self.env.world.fwal.entries[SLOT].append({"op_id": OP, "kind": "complete", "value": "VERIFIED",
                                                  "reason": "", "bindings": {}, "auth_digest": auth})
        self.assertEqual(burn_classes(self.env), ["STATE_UNKNOWN(fwal)"])
        r = run(self.env, "freeze-continue")
        self.assertIs(r.code, Code.STATE_UNKNOWN)

    def test_classification_is_read_only_and_stable_under_stop(self):
        """A82."""
        self.assertTrue(crash(self.env, "fwal:append:before", "freeze-start"))
        snap, writes = self.env.snapshot(), list(self.env.world.writes())
        for _ in range(3):
            self.assertEqual(burn_classes(self.env), ["ABANDONED_PRE_INTENT"])
        self.env.run("stop", reason=reason())
        self.assertEqual(burn_classes(self.env), ["ABANDONED_PRE_INTENT"])
        self.assertEqual(release(self.env), (Code.STOPPED, ""))
        self.assertNotEqual(self.env.snapshot(), snap)  # only the STOP gate record was written
        self.assertEqual([w for w in self.env.world.writes() if w[0] not in ("authz", "ledger", "gates")],
                         [w for w in writes if w[0] not in ("authz", "ledger", "gates")])

    def test_crash_between_completion_and_refreshed(self):
        """A83: COMPLETED(MOVED), no refreshed, chain state unaffected."""
        before = len(o5.chain(self.env, self.env.barrier_id).records)
        self.assertTrue(crash(self.env, "barriers:put:before", "freeze-start"))
        self.assertEqual(burn_classes(self.env), ["COMPLETED"])
        c = o5.chain(self.env, self.env.barrier_id)
        self.assertEqual((len(c.records), c.state), (before, "ESTABLISHED"))
        ok(self, self.env, "freeze-continue")

    def test_refreshed_without_its_cycle_is_inconsistent(self):
        """A84 / R3 §6.6."""
        o5.append_record(self.env, self.env.barrier_id, "refreshed", verb="freeze-start",
                         detail={"slot": SLOT, "cycle_id": "c" * 64, "completion": "d" * 64})
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "barrier_invalid"))


OTHER = harness.O5_SLOTS[1]


class RefreshedIntegrity(unittest.TestCase):
    """B2 (R4 §2.2 last row, §2.4, A84): every `refreshed` must bind exactly one
    completion of its own cycle, authorization, slot and chain."""

    def setUp(self):
        self.env = Env()
        self.addCleanup(self.env.close)
        self.env.seed_leased()
        self.env.seed_leased(slot=OTHER)
        ok(self, self.env, "adopt")
        ok(self, self.env, "request")
        self.effects = []
        inner = self.env.world.freeze

        def counted(verb, slot, op, ctx):
            self.effects.append(verb)
            return inner(verb, slot, op, ctx)
        self.env.world.freeze = counted

    def cycle(self):
        return cycles(self.env)[-1]

    def completion(self):
        return [e for e in self.env.world.fwal.log(SLOT) if e["kind"] == "complete"][-1]

    def add(self, detail, auth=None, bid=None, verb="freeze-start"):
        return o5.append_record(self.env, bid or self.env.barrier_id, "refreshed", auth=auth, verb=verb,
                                detail=detail)

    def refused(self, verb, detail="STATE_UNKNOWN(cycle_inconsistent)"):
        o5.observe(self.env, SLOT)
        snap, writes, n = self.env.snapshot(), list(self.env.world.writes()), len(self.effects)
        r = harness.invocation.invoke(self.env.world, harness.json.dumps(
            {"v": 1, "verb": verb, "slot": SLOT, "op_id": OP, "reason": reason()}).encode(), self.env.confirmer)
        self.assertEqual((r.outcome, r.code, r.detail), (Outcome.REFUSED, Code.STATE_UNKNOWN, detail))
        self.assertEqual((self.env.snapshot(), list(self.env.world.writes()), len(self.effects)), (snap, writes, n))

    def test_valid_completion_and_refresh(self):
        ok(self, self.env, "freeze-start")
        self.assertEqual((burn_classes(self.env), self.cycle().closure), (["COMPLETED"], "open"))
        ok(self, self.env, "freeze-continue")
        self.assertEqual(self.cycle().closure, "open")

    def test_a84_known_cycle_with_refresh_but_no_completion(self):
        """The review's CE2: the effect must never be re-run."""
        self.assertTrue(crash(self.env, "fwal:append:after", "freeze-start"))
        auth = self.env.world.fwal.log(SLOT)[0]["auth_digest"]
        self.add({"slot": SLOT, "cycle_id": self.cycle().cycle_id, "completion": "d" * 64}, auth=auth)
        self.assertEqual(burn_classes(self.env), ["STATE_UNKNOWN(cycle_inconsistent)"])
        self.refused("resume-step")
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "barrier_invalid"))
        self.assertEqual(self.effects, [])  # the crash preceded the effect; nothing re-ran it

    def test_unknown_cycle_refresh(self):
        self.add({"slot": SLOT, "cycle_id": "c" * 64, "completion": "d" * 64})
        self.refused("freeze-start", "cycle_inconsistent")
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "barrier_invalid"))

    def test_completion_digest_mismatch(self):
        ok(self, self.env, "freeze-start")
        done = self.completion()
        self.add({"slot": SLOT, "cycle_id": self.cycle().cycle_id, "completion": "e" * 64},
                 auth=done["auth_digest"])
        self.refused("freeze-continue")

    def completed_without_refresh(self):
        """Completion durable, its own `refreshed` not yet written (crash between
        the two writes): the only state where a crafted record is not a duplicate."""
        self.assertTrue(crash(self.env, "barriers:put:before", "freeze-start"))
        return self.completion()

    def test_refresh_bound_to_another_slot(self):
        done = self.completed_without_refresh()
        self.add({"slot": OTHER, "cycle_id": self.cycle().cycle_id, "completion": freeze.canonical.digest(done)},
                 auth=done["auth_digest"])
        self.refused("freeze-continue")

    def test_completion_from_another_authorization(self):
        done = self.completed_without_refresh()
        self.add({"slot": SLOT, "cycle_id": self.cycle().cycle_id, "completion": freeze.canonical.digest(done)})
        self.refused("freeze-continue")

    def test_matching_crafted_refresh_is_consistent(self):
        """Control for the three tests above: the same record with the right
        slot, chain and authorization is accepted."""
        done = self.completed_without_refresh()
        self.add({"slot": SLOT, "cycle_id": self.cycle().cycle_id, "completion": freeze.canonical.digest(done)},
                 auth=done["auth_digest"])
        self.assertEqual(self.cycle().closure, "open")
        ok(self, self.env, "freeze-continue")

    def test_duplicate_refresh_for_one_completion(self):
        ok(self, self.env, "freeze-start")
        done = self.completion()
        self.add({"slot": SLOT, "cycle_id": self.cycle().cycle_id, "completion": freeze.canonical.digest(done)},
                 auth=done["auth_digest"])
        self.refused("freeze-continue")

    def test_refresh_in_another_chain(self):
        done = self.completed_without_refresh()
        b2 = o5.synthetic_barrier(self.env, [OTHER])
        self.add({"slot": SLOT, "cycle_id": self.cycle().cycle_id, "completion": freeze.canonical.digest(done)},
                 auth=done["auth_digest"], bid=b2)
        self.refused("freeze-continue")

    def test_crash_after_completion_before_refresh_is_consistent(self):
        self.assertTrue(crash(self.env, "barriers:put:before", "freeze-start"))
        self.assertEqual((burn_classes(self.env), self.cycle().closure), (["COMPLETED"], "open"))
        ok(self, self.env, "freeze-continue")
        self.assertEqual(self.effects, ["freeze-start", "freeze-continue"])

    def test_crash_after_refresh_persistence_is_consistent(self):
        self.assertTrue(crash(self.env, "barriers:put:after", "freeze-start"))
        self.assertEqual((burn_classes(self.env), self.cycle().closure), (["COMPLETED"], "open"))
        self.assertIs(run(self.env, "resume-step").outcome, Outcome.ALREADY_COMPLETE)
        ok(self, self.env, "freeze-continue")
        self.assertEqual(self.effects, ["freeze-start", "freeze-continue"])

    def test_malformed_f_wal(self):
        ok(self, self.env, "freeze-start")
        self.env.world.fwal.entries[SLOT][-1]["bindings"] = "not-a-map"
        self.assertEqual(burn_classes(self.env), ["STATE_UNKNOWN(fwal)"])
        r = run(self.env, "freeze-continue")
        self.assertIs(r.code, Code.STATE_UNKNOWN)
        self.assertEqual(self.effects, ["freeze-start"])


class ChainRefreshIntegrity(unittest.TestCase):
    """B2-R: every `refreshed` is validated at the chain boundary against cycle
    facts built from burned freeze-start documents and a classifiable F WAL.
    A record binding no exact known completion makes the chain INVALID."""

    def setUp(self):
        self.env = Env()
        self.addCleanup(self.env.close)
        self.env.seed_leased()
        self.env.seed_leased(slot=OTHER)
        ok(self, self.env, "adopt")
        ok(self, self.env, "request")
        self.effects = []
        inner = self.env.world.freeze

        def counted(verb, slot, op, ctx):
            self.effects.append(verb)
            return inner(verb, slot, op, ctx)
        self.env.world.freeze = counted

    def chain(self, bid=None):
        return o5.chain(self.env, bid or self.env.barrier_id)

    def cid(self):
        return cycles(self.env)[-1].cycle_id

    def last_done(self):
        return [e for e in self.env.world.fwal.log(SLOT) if e["kind"] == "complete"][-1]

    def bad(self, detail, auth=None, bid=None):
        o5.append_record(self.env, bid or self.env.barrier_id, "refreshed", auth=auth, verb="freeze-start",
                         detail=detail)

    def assert_invalid(self, bid=None):
        c = self.chain(bid)
        self.assertEqual((c.state, c.reason), ("INVALID", "refreshed"))

    def refused(self, verb, **kw):
        o5.observe(self.env, SLOT)
        snap, writes, n = self.env.snapshot(), list(self.env.world.writes()), len(self.effects)
        barriers = dict(self.env.world.o5.stores["barriers"].items)
        kw = dict({"quarantine": harness.QUARANTINE, "quiescence": harness.QUIESCENCE}.get(verb, {}), **kw)
        r = harness.invocation.invoke(self.env.world, harness.json.dumps(dict(
            {"v": 1, "verb": verb, "slot": SLOT, "op_id": OP, "reason": reason()}, **kw)).encode(), self.env.confirmer)
        self.assertIs(r.outcome, Outcome.REFUSED, (verb, r))
        self.assertIn(r.code, (Code.STATE_UNKNOWN, Code.FIELD_INVALID), (verb, r))
        self.assertEqual((self.env.snapshot(), list(self.env.world.writes()), len(self.effects)), (snap, writes, n))
        self.assertEqual(self.env.world.o5.stores["barriers"].items, barriers)
        return r

    # -- the review's two variants
    def test_b2r_unknown_cycle_naming_the_slot(self):
        ok(self, self.env, "freeze-start")
        self.bad({"slot": SLOT, "cycle_id": "c" * 64, "completion": "d" * 64})
        self.assert_invalid()
        for verb in ("freeze-continue", "thaw"):
            self.assertEqual(self.refused(verb).detail, "barrier_invalid")
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "barrier_invalid"))
        self.assertEqual(self.effects, ["freeze-start"])

    def test_b2r_slot_less_unknown_cycle(self):
        ok(self, self.env, "freeze-start")
        self.bad({"cycle_id": "c" * 64, "completion": "d" * 64})
        self.assert_invalid()
        for verb in ("freeze-continue", "thaw"):
            self.refused(verb)
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "barrier_invalid"))

    def test_b2r_slot_less_on_an_idle_pool_never_releases(self):
        self.bad({"cycle_id": "c" * 64, "completion": "d" * 64})
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "barrier_invalid"))
        r = run(self.env, "freeze-start")
        self.assertEqual((r.code, r.detail), (Code.FIELD_INVALID, "barrier_ambiguous"))

    # -- positive traces
    def test_valid_refresh_and_completion_without_refresh(self):
        ok(self, self.env, "freeze-start")
        self.assertEqual(self.chain().state, "ESTABLISHED")
        self.assertEqual(len(self.chain().refreshed), 1)
        env = self.env
        self.setUp()
        self.assertTrue(crash(self.env, "barriers:put:before", "freeze-start"))
        self.assertEqual((self.chain().state, len(self.chain().refreshed)), ("ESTABLISHED", 0))
        self.assertIs(run(self.env, "resume-step").outcome, Outcome.ALREADY_COMPLETE)
        self.assertEqual(self.effects, ["freeze-start"])  # ran once before the crash; never replayed
        ok(self, self.env, "freeze-continue")
        self.assertEqual(self.effects, ["freeze-start", "freeze-continue"])
        self.assertIsNot(env, self.env)

    def test_valid_records_for_two_cycles(self):
        ok(self, self.env, "freeze-start")
        ok(self, self.env, "thaw")
        ok(self, self.env, "freeze-start")
        self.assertEqual(len(cycles(self.env)), 2)
        self.assertEqual((self.chain().state, len(self.chain().refreshed)), ("ESTABLISHED", 2))
        ok(self, self.env, "freeze-continue")

    def test_abandoned_pre_intent_unaffected(self):
        self.assertTrue(crash(self.env, "fwal:append:before", "freeze-start"))
        self.assertEqual(self.chain().state, "ESTABLISHED")
        self.assertEqual(release(self.env), [(SLOT, "D1"), (OTHER, "D1")])

    # -- negative matrix (direct validator)
    def matrix_case(self, detail_fn, auth_fn=None, bid=None):
        ok(self, self.env, "freeze-start")
        done = self.last_done()
        self.bad(detail_fn(done), auth=auth_fn(done) if auth_fn else done["auth_digest"], bid=bid)
        self.assert_invalid(bid)

    def test_refresh_without_completion(self):
        self.assertTrue(crash(self.env, "fwal:append:after", "freeze-start"))
        self.bad({"slot": SLOT, "cycle_id": self.cid(), "completion": "d" * 64},
                 auth=self.env.world.fwal.log(SLOT)[0]["auth_digest"])
        self.assert_invalid()
        self.refused("resume-step")
        self.assertEqual(self.effects, [])

    def test_unknown_slot(self):
        ok(self, self.env, "freeze-start")
        done = self.last_done()
        self.bad({"slot": "codegraph-worker-q", "cycle_id": self.cid(), "completion": freeze.canonical.digest(done)},
                 auth=done["auth_digest"])
        c = self.chain()
        self.assertEqual(c.state, "INVALID")
        self.assertIn(c.reason, ("slot", "refreshed"))

    def test_other_authorized_slot(self):
        self.matrix_case(lambda d: {"slot": OTHER, "cycle_id": self.cid(), "completion": freeze.canonical.digest(d)})

    def test_wrong_completion_digest(self):
        self.matrix_case(lambda d: {"slot": SLOT, "cycle_id": self.cid(), "completion": "e" * 64})

    def test_another_operations_authorization(self):
        self.assertTrue(crash(self.env, "barriers:put:before", "freeze-start"))
        done = self.last_done()
        other = o5.burn(self.env, o5._owner_doc(self.env, "freeze-start", slot=OTHER, op_id="op2", slot_generation=1))
        self.bad({"slot": SLOT, "cycle_id": self.cid(), "completion": freeze.canonical.digest(done)}, auth=other)
        self.assert_invalid()

    def test_duplicate_and_conflicting_records(self):
        self.matrix_case(lambda d: {"slot": SLOT, "cycle_id": self.cid(), "completion": freeze.canonical.digest(d)})

    def test_other_chain(self):
        ok(self, self.env, "freeze-start")
        done = self.last_done()
        b2 = o5.synthetic_barrier(self.env, [SLOT, OTHER])
        self.bad({"slot": SLOT, "cycle_id": self.cid(), "completion": freeze.canonical.digest(done)},
                 auth=done["auth_digest"], bid=b2)
        self.assert_invalid(b2)

    def test_malformed_details_fail_closed(self):
        for detail in ({"slot": SLOT, "cycle_id": ["x"], "completion": "d" * 64},
                       {"slot": SLOT, "cycle_id": "c" * 64, "completion": {"x": 1}},
                       {"slot": "", "cycle_id": "c" * 64, "completion": "d" * 64},
                       {"slot": SLOT, "cycle_id": "c" * 64, "completion": "d" * 64, "extra": "x"}, {}):
            self.setUp()
            ok(self, self.env, "freeze-start")
            self.bad(detail)
            self.assertEqual(self.chain().state, "INVALID")
            self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "barrier_invalid"))
            self.refused("freeze-continue")

    def test_closed_historical_cycle(self):
        self.env.freeze_results["freeze-start"] = "NONE"
        ok(self, self.env, "freeze-start")
        self.assertEqual(cycles(self.env)[-1].closure, "none_no_move")
        self.bad({"slot": SLOT, "cycle_id": self.cid(), "completion": "e" * 64}, auth=self.last_done()["auth_digest"])
        self.assert_invalid()
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "barrier_invalid"))

    def test_invalid_refresh_in_a_released_chain(self):
        """The release records follow the bad refresh in a structurally valid
        sequence; the chain is INVALID, never RELEASED."""
        import json
        bid = self.env.barrier_id
        self.bad({"slot": SLOT, "cycle_id": "c" * 64, "completion": "d" * 64})
        st = self.env.world.o5.stores["barriers"]
        keys = sorted(k for k in st.keys() if k.split("/")[1] == bid)
        first = json.loads(st.get(keys[0]))
        base = {k: first[k] for k in o5.barrier.IMMUTABLE}
        prev = freeze.canonical.sha256(st.get(keys[-1]))
        for i, kind in enumerate(("release_requested", "release_authorized", "released")):
            auth = o5.burn(self.env, o5._owner_doc(self.env, "barrier-release", slot="o5-barrier",
                                                   slot_generation=900 + i))
            prev = o5.barrier_record(self.env, bid, len(keys) + i, prev, kind, auth, base)
        self.assert_invalid()
        r = self.env.run("clear-suspect", slot=SLOT, reason=reason())
        self.assertEqual(r.detail, "barrier_in_use")

    def test_corrupt_f_wal_leaves_no_cycle_facts(self):
        ok(self, self.env, "freeze-start")
        self.env.world.fwal.entries[SLOT][-1]["bindings"] = "not-a-map"
        self.assertEqual(self.chain().state, "INVALID")
        self.assertEqual(self.chain().reason, "refreshed")

    def test_unclassifiable_f_wal_supplies_no_facts_even_with_an_intact_completion(self):
        """The refreshed completion is untouched; a later entry makes the log
        unclassifiable, so no cycle facts exist and the chain is INVALID."""
        ok(self, self.env, "freeze-start")
        self.assertEqual(self.chain().state, "ESTABLISHED")
        done = self.last_done()
        self.env.world.fwal.entries[SLOT].append({"op_id": OP, "kind": "intent", "value": "BOGUS",
                                                  "auth_digest": done["auth_digest"]})
        self.assertEqual(burn_classes(self.env), ["STATE_UNKNOWN(fwal)"])
        self.assertEqual((self.chain().state, self.chain().reason), ("INVALID", "refreshed"))

    def test_cycle_generation_must_match_the_chain(self):
        """A crafted cycle bound to a chain whose authorized generation differs."""
        h = head(self.env).digest
        b2 = o5.synthetic_barrier(self.env, [SLOT, OTHER], gens={SLOT: 5, OTHER: 1})
        doc = o5._owner_doc(self.env, "freeze-start", slot=SLOT, op_id=OP, slot_generation=1, f_from="NONE",
                            o5={"barrier_id": b2, "cutover_head": h})
        auth = o5.burn(self.env, doc)
        w = self.env.world
        w.fwal.entries[SLOT] = [{"op_id": OP, "kind": "intent", "value": "MOVE_INTENT", "auth_digest": auth}]
        done = {"op_id": OP, "kind": "complete", "value": "MOVED", "reason": "", "bindings": {"early": "e" * 64},
                "auth_digest": auth, "utc": 1}
        w.fwal.entries[SLOT].append(done)
        self.assertEqual(self.cid(), harness.o5.oc.cycle_id(SLOT, 1, OP, auth))
        self.bad({"slot": SLOT, "cycle_id": self.cid(), "completion": freeze.canonical.digest(done)}, auth=auth,
                 bid=b2)
        self.assert_invalid(b2)

    def test_every_operation_path_refuses_an_invalid_chain(self):
        """freeze-continue, thaw, resume-step, quarantine, quiescence, snapshot and
        verify-preservation each meet the invalid chain at their own row."""
        steps = [("freeze-start", None), ("freeze-continue", ("freeze-continue", "thaw", "resume-step")),
                 ("quarantine", ("quarantine",)), ("quiescence", ("quiescence",)),
                 ("snapshot", ("snapshot",)), ("verify-preservation", ("verify-preservation",))]
        env_kw = {"auto_handoff": False}
        for i, (_, probes) in enumerate(steps):
            if probes is None:
                continue
            self.env = Env(**env_kw)
            self.addCleanup(self.env.close)
            self.env.seed_leased()
            self.env.seed_leased(slot=OTHER)
            self.effects = []
            for verb in ("adopt", "request") + tuple(v for v, _ in steps[:i]):
                kw = {"quarantine": harness.QUARANTINE, "quiescence": harness.QUIESCENCE}.get(verb, {})
                ok(self, self.env, verb, **kw)
            inner = self.env.world.freeze
            self.env.world.freeze = lambda verb, slot, op, ctx: (self.effects.append(verb), inner(verb, slot, op,
                                                                                                  ctx))[1]
            self.bad({"cycle_id": "c" * 64, "completion": "d" * 64})
            for verb in probes:
                self.refused(verb)
            self.assertEqual(self.effects, [])
            self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "barrier_invalid"))


class BurnBinding(unittest.TestCase):
    """B3 (R4 §2.2 `barrier_binding` row, §2.3 condition 4, A81): only an exactly
    bound burn of this slot's operation can be ABANDONED_PRE_INTENT and D1."""

    def setUp(self):
        self.env = Env()
        self.addCleanup(self.env.close)
        self.env.seed_leased()
        self.env.seed_leased(slot=OTHER)
        ok(self, self.env, "adopt")
        ok(self, self.env, "request")

    def doc(self, **over):
        h = head(self.env).digest
        base = {"slot": SLOT, "op_id": OP, "slot_generation": 1, "f_from": "NONE",
                "o5": {"barrier_id": self.env.barrier_id, "cutover_head": h}}
        return o5._owner_doc(self.env, over.pop("verb", "freeze-start"), **dict(base, **over))

    def key(self, slot=SLOT, op=OP, gen=1):
        return freeze.canonical.digest({"verb": "freeze-start", "slot": slot, "op_id": op, "slot_generation": gen})

    def bad(self, doc, **burn_kw):
        o5.burn(self.env, doc, **burn_kw)
        self.assertEqual(burn_classes(self.env), ["STATE_UNKNOWN(barrier_binding)"])
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "STATE_UNKNOWN(barrier_binding)"))
        snap = self.env.snapshot()
        r = run(self.env, "freeze-start")
        self.assertEqual((r.code, r.detail), (Code.STATE_UNKNOWN, "STATE_UNKNOWN(barrier_binding)"))
        self.assertEqual(self.env.snapshot(), snap)

    def test_valid_abandoned_burn_stays_d1(self):
        o5.burn(self.env, self.doc())
        self.assertEqual(burn_classes(self.env), ["ABANDONED_PRE_INTENT"])
        self.assertEqual(release(self.env), [(SLOT, "D1"), (OTHER, "D1")])

    def test_legitimate_crash_after_the_burn(self):
        self.assertTrue(crash(self.env, "fwal:append:before", "freeze-start"))
        self.assertEqual(burn_classes(self.env), ["ABANDONED_PRE_INTENT"])
        self.assertEqual(release(self.env), [(SLOT, "D1"), (OTHER, "D1")])

    def test_a81_foreign_operation(self):
        """The review's CE3."""
        self.bad(self.doc(op_id="ghost"))

    def test_wrong_generation(self):
        self.bad(self.doc(slot_generation=2))

    def test_wrong_generation_under_the_right_key(self):
        self.bad(self.doc(slot_generation=2), binding=self.key(gen=1))

    def test_foreign_operation_under_this_slots_key(self):
        self.bad(self.doc(op_id="ghost"), binding=self.key())

    def test_foreign_slot_under_this_slots_key(self):
        self.bad(self.doc(slot=OTHER), binding=self.key())

    def test_wrong_verb_under_this_slots_key(self):
        self.bad(self.doc(verb="freeze-continue"), binding=self.key())

    def test_wrong_binding_digest(self):
        self.bad(self.doc(), binding=self.key(gen=2))

    def test_wrong_authorization_kind(self):
        self.bad(self.doc(), auth_kind="resume")

    def test_missing_cycle_identity(self):
        self.bad(self.doc(o5={"barrier_id": self.env.barrier_id}))

    def test_missing_document(self):
        doc = self.doc()
        auth = o5.burn(self.env, doc)
        del self.env.world.authz.docs[auth]
        self.assertEqual(burn_classes(self.env), ["STATE_UNKNOWN(auth_missing)"])

    def test_duplicate_burn(self):
        w = self.env.world
        auth = o5.burn(self.env, self.doc())
        entries = w.ledger.entries()
        dup = entries + [dict(entries[-1])]
        d = freeze.burned_docs(w, entries)
        self.assertEqual(freeze.classify_burn(w, SLOT, d, dup, auth, []), "STATE_UNKNOWN(fork)")


class ReadyPending(unittest.TestCase):
    """R4.1 §3.8: T1–T12 and the R4-B1 counterexample."""

    def setUp(self):
        self.env = Env()
        self.addCleanup(self.env.close)
        self.env.seed_leased()
        self.env.seed_leased(slot=harness.O5_SLOTS[1])
        harness.to_quarantine(self.env)
        for verb, kw in (("quarantine", harness.QUARANTINE), ("quiescence", harness.QUIESCENCE), ("snapshot", {}),
                         ("verify-preservation", {}), ("preservation", {})):
            ok(self, self.env, verb, **kw)
        self.assertEqual(row(self.env), "P15")

    def closure(self):
        return [c.closure for c in cycles(self.env)]

    def at(self, point):
        self.assertTrue(crash(self.env, point, "ready"))
        return row(self.env)

    def test_t1_p16_resume_reaches_p18(self):
        self.assertEqual(self.at("library:after_intent"), "P16")
        self.assertEqual(self.closure(), ["ready_pending"])
        ok(self, self.env, "resume-step")
        self.assertEqual((row(self.env), self.closure()), ("P18", ["ready"]))

    def test_t2_t3_p17_resume_reaches_p18_and_closes(self):
        """The R4-B1 counterexample: under the literal R4 G4 this resume refused
        cycle_absent forever; R4.1 admits it and the cycle closes at P18."""
        for point in ("library:after_complete", "library:before_marker_rename"):
            env = self.env
            self.assertEqual(self.at(point), "P17")
            self.assertEqual(self.closure(), ["ready_pending"])
            self.assertTrue(freeze.is_open(cycles(env)[0]))
            ok(self, env, "resume-step")
            self.assertEqual((row(env), self.closure()), ("P18", ["ready"]))
            self.assertIs(run(env, "resume-step").outcome, Outcome.ALREADY_COMPLETE)
            self.assertIs(run(env, "ready").outcome, Outcome.ALREADY_COMPLETE)
            self.setUp()

    def test_t1_t2_without_pr3b_refuse_handoff_unavailable(self):
        """N-R4.1-2: G5 (handoff basis) precedes G8."""
        for point, want in (("library:after_intent", "P16"), ("library:before_marker_rename", "P17")):
            self.assertEqual(self.at(point), want)
            self.env.world.o5.preservation = None
            snap = self.env.snapshot()
            r = run(self.env, "resume-step")
            self.assertEqual((r.code, r.detail), (Code.QUIESCENCE_NOT_ESTABLISHED, "handoff_unavailable"))
            self.assertEqual(self.env.snapshot(), snap)
            self.setUp()

    def test_t4_missing_marker_is_p17_only_while_custody_is_bound(self):
        """N-R4.1-3."""
        ok(self, self.env, "ready")
        view = self.env.world.library.read(SLOT, OP)
        self.assertEqual(self.env.world.custody.lookup(
            custody.recovery_key(SLOT, OP, 1)).k, "BOUND")
        (self.env.world.library.store._op_dir(SLOT, OP) / "TERMINAL").unlink()
        self.assertEqual((row(self.env), self.closure()), ("P17", ["ready_pending"]))
        self.assertIsNotNone(view)
        h = head(self.env)
        with self.assertRaises(Refused) as e:
            cutover.cutover_allowed(oc.SYNTHETIC, "rebaseline", h, [], self.closure(), 1)
        self.assertEqual(e.exception.detail, "cycle_open")
        self.assertEqual(release(self.env), PD)
        ok(self, self.env, "resume-step")
        self.assertEqual(self.closure(), ["ready"])

    def test_t4_negative_after_custody_destroy(self):
        """N-R4.1-3: with custody no longer BOUND a deleted marker is refused by
        PR-1 and is never resumable as READY_PENDING."""
        ok(self, self.env, "ready")
        ok(self, self.env, "custody-destroy")
        (self.env.world.library.store._op_dir(SLOT, OP) / "TERMINAL").unlink()
        snap = self.env.snapshot()
        r = run(self.env, "resume-step")
        self.assertIs(r.outcome, Outcome.REFUSED)
        self.assertEqual(self.env.snapshot(), snap)

    def test_t5_corrupt_marker(self):
        ok(self, self.env, "ready")
        (self.env.world.library.store._op_dir(SLOT, OP) / "TERMINAL").write_bytes(b"corrupt\n")
        self.assertEqual(self.closure(), ["STATE_UNKNOWN(marker)"])
        snap = self.env.snapshot()
        self.assertIs(run(self.env, "resume-step").code, Code.STATE_UNKNOWN)
        self.assertEqual(self.env.snapshot(), snap)
        with self.assertRaises(Refused) as e:
            cutover.cutover_allowed(oc.SYNTHETIC, "rebaseline", head(self.env), [], self.closure(), 1)
        self.assertEqual((e.exception.code, e.exception.detail), (Code.STATE_UNKNOWN, "STATE_UNKNOWN(marker)"))
        self.assertIs(release(self.env)[0], Code.STATE_UNKNOWN)

    def test_t6_t7_rebaseline_and_revoke_refuse_while_pending(self):
        for point in ("library:after_intent", "library:before_marker_rename"):
            self.at(point)
            for kind in ("rebaseline", "revoked"):
                with self.assertRaises(Refused) as e:
                    cutover.cutover_allowed(oc.SYNTHETIC, kind, head(self.env), ["RELEASED"], self.closure(), 1)
                self.assertEqual(e.exception.detail, "cycle_open")
            self.setUp()

    def test_t8_release_while_pending(self):
        self.at("library:before_marker_rename")
        self.assertEqual(release(self.env), PD)
        prov = self.env.world.o5.preservation
        self.env.world.o5.preservation = o5.DischargeDouble(self.env.world.observer)
        self.assertEqual(release(self.env), [(SLOT, "D3"), (harness.O5_SLOTS[1], "D1")])
        self.env.world.o5.preservation = None
        self.assertEqual(release(self.env), PD)
        self.env.world.o5.preservation = prov
        r = self.env.run("release", slot=SLOT, reason=reason())
        self.assertIs(r.outcome, Outcome.REFUSED)

    def test_t9_stop_during_pending_completion(self):
        self.at("library:before_marker_rename")
        self.env.run("stop", reason=reason())
        ok(self, self.env, "resume-step")
        self.assertEqual(self.closure(), ["ready"])
        self.assertEqual(release(self.env), (Code.STOPPED, ""))
        self.assertIs(run(self.env, "thaw").code, Code.STOPPED)

    def test_t10_p18_closure_then_eligible_operations(self):
        ok(self, self.env, "ready")
        self.assertEqual(self.closure(), ["ready"])
        self.assertEqual(release(self.env), PD)
        self.env.world.o5.preservation = o5.DischargeDouble(self.env.world.observer)
        self.assertEqual(release(self.env), [(SLOT, "D4"), (harness.O5_SLOTS[1], "D1")])
        o5.release_chain(self.env, self.env.barrier_id)
        cutover.cutover_allowed(oc.SYNTHETIC, "rebaseline", head(self.env), ["RELEASED"], self.closure(), 1)

    def test_t10_ready_without_d_ready_document(self):
        """N-R4.1-4: the READY gate document must carry exactly D-READY."""
        ok(self, self.env, "ready")
        rec = self.env.world.library.read(SLOT, OP).rec
        del self.env.world.authz.docs[rec["gates"]["ready"]]
        self.assertEqual(self.closure(), ["STATE_UNKNOWN(auth_missing)"])

    def test_t10_ready_closure_needs_exactly_d_ready(self):
        """N-R4.1-4 (pure): the gate document must carry D-READY; a resume
        document must carry the cell of its own resume mode."""
        import copy
        ok(self, self.env, "ready")
        w = self.env.world
        d = docs(self.env)
        gate = w.library.read(SLOT, OP).rec["gates"]["ready"]
        self.assertEqual(freeze._ready_closure(w, SLOT, OP, d), "ready")
        for cell in ("D-PRES", "D-READY-T", None):
            bad = copy.deepcopy(d)
            if cell is None:
                del bad[gate]["o5"]["decision"]
            else:
                bad[gate]["o5"]["decision"]["cell"] = cell
            self.assertEqual(freeze._ready_closure(w, SLOT, OP, bad), "STATE_UNKNOWN(auth_binding)")
        bad = copy.deepcopy(d)
        bad["f" * 64] = {"verb": "resume-step", "op_id": OP, "slot": SLOT, "transition": "ready",
                         "mode": "complete_terminal", "o5": {"decision": {"cell": "D-READY-R"}}}
        self.assertEqual(freeze._ready_closure(w, SLOT, OP, bad), "STATE_UNKNOWN(auth_binding)")

    def test_t11_rebaseline_after_p18_then_d4h(self):
        ok(self, self.env, "ready")
        o5.release_chain(self.env, self.env.barrier_id)
        h0 = head(self.env).digest
        o5.cutover_step(self.env, "model-b-rebaseline")
        h1 = head(self.env)
        self.assertEqual(h1.state, "ACTIVE")
        b3 = o5.synthetic_barrier(self.env, [SLOT])
        self.env.world.o5.preservation = o5.DischargeDouble(self.env.world.observer)
        self.assertEqual(release(self.env, b3), [(SLOT, "D4H")])
        pre, _ = freeze.basis_for(self.env.world, SLOT, "release", release_target=b3, kind="D4H")
        self.assertEqual((pre.cycle_cutover_head, pre.current_cutover_head, pre.cutover_head),
                         (h0, h1.digest, h0))
        self.assertEqual((pre.holder_chain, pre.barrier_id, pre.release_target),
                         (self.env.barrier_id, self.env.barrier_id, b3))

    def test_t11_changed_head_while_pending_is_cutover_changed(self):
        self.at("library:before_marker_rename")
        o5.cutover_step(self.env, "model-b-rebaseline")
        self.env.world.o5.preservation = o5.DischargeDouble(self.env.world.observer)
        self.assertEqual(release(self.env), (Code.STATE_UNKNOWN, "cutover_invalid(identity)"))
        r = run(self.env, "resume-step")
        self.assertEqual((r.code, r.detail), (Code.STATE_UNKNOWN, "cutover_changed"))

    def test_t12_missing_historical_evidence_refuses(self):
        ok(self, self.env, "ready")
        o5.release_chain(self.env, self.env.barrier_id)
        o5.cutover_step(self.env, "model-b-rebaseline")
        b3 = o5.synthetic_barrier(self.env, [SLOT])
        self.assertEqual(release(self.env, b3), PD)
        self.env.world.o5.preservation = o5.DischargeDouble(self.env.world.observer, refuse={})
        self.env.world.o5.preservation.discharge = lambda *a: None
        self.assertEqual(release(self.env, b3), PD)


if __name__ == "__main__":
    unittest.main()

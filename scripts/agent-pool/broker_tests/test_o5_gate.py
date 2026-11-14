"""The universal O5 gate through the merged PR-1 invocation (PR-3a contract R2
§3, R3 §2–§3, R4 §1). Every refusal asserts byte-identical stores, no new
write, no freeze effect and no `superseded` / `release_*` record."""

import json
import unittest

import harness
from harness import SLOT, Env, reason
from broker import authz, barrier, cutover, evidence, freeze, invocation, o5contract as oc
from broker._testsupport import capability, o5
from broker.authz_txn import TxnError
from broker.refusal import Code, Outcome, Refused

OP = "op1"


class Base(unittest.TestCase):
    env_kw = {}

    def setUp(self):
        self.env = Env(**self.env_kw)
        self.addCleanup(self.env.close)
        self.env.seed_leased()
        self.effects = []
        inner = self.env.world.freeze

        def counted(verb, slot, op, ctx):
            self.effects.append(verb)
            return inner(verb, slot, op, ctx)
        self.env.world.freeze = counted

    def run_(self, verb, observe=True, **kw):
        fields = dict({"slot": SLOT, "op_id": OP, "reason": reason()}, **kw)
        if observe:
            return self.env.run(verb, **fields)
        return invocation.invoke(self.env.world, json.dumps(dict({"v": 1, "verb": verb}, **fields)).encode(),
                                 self.env.confirmer)

    def ok(self, *verbs):
        for verb in verbs:
            kw = {"quarantine": harness.QUARANTINE, "quiescence": harness.QUIESCENCE}.get(verb, {})
            r = self.run_(verb, **kw)
            self.assertIs(r.outcome, Outcome.COMMITTED, (verb, r))

    def refused(self, verb, code, detail=None, observe=True, **kw):
        kw = dict({"quarantine": harness.QUARANTINE, "quiescence": harness.QUIESCENCE}.get(verb, {}), **kw)
        if observe:
            o5.observe(self.env, SLOT)
        snap, writes, effects = self.env.snapshot(), list(self.env.world.writes()), list(self.effects)
        barriers = dict(self.env.world.o5.stores["barriers"].items)
        r = self.run_(verb, observe=False, **kw)
        self.assertEqual((r.outcome, r.code), (Outcome.REFUSED, code), r)
        if detail is not None:
            self.assertEqual(r.detail, detail)
        self.assertEqual(self.env.snapshot(), snap)
        self.assertEqual(list(self.env.world.writes()), writes)
        self.assertEqual(self.effects, effects)
        self.assertEqual(self.env.world.o5.stores["barriers"].items, barriers)
        return r

    def to(self, *verbs):
        self.ok("adopt", "request", *verbs)


class Heads(Base):
    def test_model_a_refuses_every_o5_verb(self):
        """A2 / I1."""
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        self.env = env
        env.seed_leased()
        self.ok("adopt", "request")
        self.refused("freeze-start", Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(model_a)")

    def test_revoked_head_refuses_everything_except_thaw(self):
        """A3, A36: never ALREADY_COMPLETE; thaw stays possible under REVOKED."""
        self.to("freeze-start")
        o5.cutover_step(self.env, "model-b-revoke")
        self.refused("freeze-continue", Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(model_a)")
        self.refused("resume-step", Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(model_a)")
        self.ok("thaw")

    def test_revoked_head_at_p11_is_not_already_complete(self):
        self.to("freeze-start", "freeze-continue", "quarantine")
        o5.cutover_step(self.env, "model-b-revoke")
        self.refused("quarantine", Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(model_a)")

    def test_pending_and_invalid_heads(self):
        self.to()
        o5.cutover_step(self.env, "model-b-rebaseline", record=False, head=False)
        self.refused("freeze-start", Code.STATE_UNKNOWN, "cutover_invalid(pending_record)")
        env = Env()
        self.addCleanup(env.close)
        self.env = env
        env.seed_leased()
        self.ok("adopt", "request")
        auth = env.world.ledger.entries()[0]["auth_digest"]
        del env.world.authz.docs[auth]
        self.refused("freeze-start", Code.STATE_UNKNOWN, "cutover_invalid(auth_missing)")

    def test_unknown_slot(self):
        """A4."""
        other = "codegraph-worker-q"
        self.env.seed_leased(slot=other)
        for verb in ("adopt", "request"):
            r = self.env.run(verb, slot=other, op_id=OP, reason=reason())
            self.assertIs(r.outcome, Outcome.COMMITTED)
        snap = self.env.snapshot()
        r = self.env.run("freeze-start", slot=other, op_id=OP, reason=reason())
        self.assertEqual((r.code, r.detail), (Code.FIELD_INVALID, "slot_unknown"))
        self.assertEqual(self.env.snapshot(), snap)


class Protected(Base):
    def test_protected_mode_is_a_constant_refusal(self):
        """A18: owner capability on a protected context, every synthetic fixture
        present and a forged provider object injected: refused before any of it."""
        self.to()

        class Forged:
            def __getattr__(self, name):
                raise AssertionError("a protected path consulted the provider")
        self.env.world.o5.preservation = Forged()
        self.env.world.ctx = authz.Ctx(capability.owner_capability(), authz.PROTECTED)
        self.assertEqual(oc.mode(self.env.world), oc.PROTECTED)
        self.refused("freeze-start", Code.QUIESCENCE_NOT_ESTABLISHED,
                     "barrier_unavailable(protected_mode_unsupported)", observe=False)
        for call in (lambda: freeze.release_check(self.env.world, self.env.barrier_id),
                     lambda: freeze.basis_for(self.env.world, SLOT, "handoff", doc={})):
            with self.assertRaises(Refused) as e:
                call()
            self.assertEqual(e.exception.detail, "barrier_unavailable(protected_mode_unsupported)")

    def test_synthetic_evidence_is_never_selected_in_protected_mode(self):
        o5.observe(self.env, SLOT)
        picked, missing, _ = evidence.select(self.env.world.o5.stores["evidence"], oc.TMB_KINDS, slot=SLOT, gen=1,
                                             op_id=None, cycle=None, barrier_id=None, now=self.env.world.clock(),
                                             mode=oc.PROTECTED)
        self.assertEqual((picked, sorted(missing)), ({}, sorted(oc.TMB_KINDS)))


class DirectPreconditions(Base):
    """B1 (R2 §3.2, A18, A32): every B precondition refuses outside synthetic mode
    as its first act, whoever calls it; only the exact SYNTHETIC constant passes."""

    class Fake(str):
        def __eq__(self, other):
            return True

        __hash__ = str.__hash__

    def calls(self, mode):
        w = self.env.world
        h = freeze.head_of(w, w.ledger.entries(), freeze.burned_docs(w, w.ledger.entries()))
        c = o5.chain(self.env, self.env.barrier_id)
        enum = {s: o5.AGENT_UID for s in harness.O5_SLOTS}
        o5.release_chain(self.env, self.env.barrier_id)
        released = {c.id: o5.chain(self.env, self.env.barrier_id)}
        views = freeze.release_views(w, c, freeze.burned_docs(w, w.ledger.entries()), w.ledger.entries(),
                                     {s: [] for s in c.slots})
        return {
            "cutover_allowed": lambda: cutover.cutover_allowed(mode, "rebaseline", h, [], [], 1),
            "establish_allowed": lambda: barrier.establish_allowed(mode, released, "POOL_MAINTENANCE", sorted(enum),
                                                                   [o5.AGENT_UID], enum, []),
            "handoff_allowed": lambda: barrier.handoff_allowed(mode, None, "ESTABLISHED", "P13", SLOT, None, False,
                                                               [], [], None),
            "release_allowed": lambda: barrier.release_allowed(mode, c, "ESTABLISHED", h, views, {}, False, False,
                                                               False, lambda *a: None),
        }

    def test_protected_missing_and_forged_modes_refuse(self):
        self.to()
        bad = (oc.PROTECTED, None, True, 1, object(), "synthetic", self.Fake("PROTECTED"), b"SYNTHETIC")
        for mode in bad:
            for name, call in self.calls(mode).items():
                snap, writes = self.env.snapshot(), list(self.env.world.writes())
                with self.assertRaises(Refused, msg=(name, mode)) as e:
                    call()
                self.assertEqual(e.exception.detail, "barrier_unavailable(protected_mode_unsupported)")
                self.assertEqual((self.env.snapshot(), list(self.env.world.writes())), (snap, writes))
            self.setUp()
            self.to()
        for name in self.calls(None):
            with self.assertRaises(TypeError):
                getattr({"cutover_allowed": cutover, "establish_allowed": barrier, "handoff_allowed": barrier,
                         "release_allowed": barrier}[name], name)()
        self.assertEqual(self.effects, [])

    def test_synthetic_mode_keeps_the_pure_decisions(self):
        self.env.seed_leased(slot=harness.O5_SLOTS[1])
        self.to()
        out = {}
        for name, call in self.calls(oc.SYNTHETIC).items():
            try:
                out[name] = call()
            except Refused as e:
                out[name] = e.detail
        self.assertIsNone(out["cutover_allowed"])
        self.assertIsNone(out["establish_allowed"])
        self.assertEqual(out["handoff_allowed"], "barrier_binding")
        self.assertEqual(out["release_allowed"], [(SLOT, "D1"), (harness.O5_SLOTS[1], "D1")])

    def test_a32_each_precondition_starts_with_the_constant_refusal(self):
        import ast
        broker = harness.HERE.parent / "broker"
        for mod, names in (("cutover.py", ("cutover_allowed",)),
                           ("barrier.py", ("establish_allowed", "handoff_allowed", "release_allowed"))):
            tree = ast.parse((broker / mod).read_text())
            fns = {n.name: n for n in tree.body if isinstance(n, ast.FunctionDef)}
            for name in names:
                fn = fns[name]
                self.assertEqual(fn.args.args[0].arg, "mode", name)
                self.assertEqual(fn.args.defaults, [], name)
                body = [b for b in fn.body if not (isinstance(b, ast.Expr) and isinstance(b.value, ast.Constant))]
                first = body[0]
                self.assertEqual(ast.unparse(first), "oc.require_synthetic(mode)", name)
        tree = ast.parse((broker / "o5contract.py").read_text())
        fns = {n.name: n for n in tree.body if isinstance(n, ast.FunctionDef)}
        req = [b for b in fns["require_synthetic"].body if not isinstance(b, ast.Expr)]
        self.assertEqual(len(req), 1)
        self.assertEqual(ast.unparse(req[0]), "if type(mode) is not str or mode != SYNTHETIC:\n    refuse_protected()")
        ref = fns["refuse_protected"].body
        self.assertEqual(len(ref), 1)
        self.assertIsInstance(ref[0], ast.Raise)
        self.assertTrue(all(isinstance(a, (ast.Constant, ast.Attribute)) for a in ref[0].exc.args))


class AbsentPreservation(Base):
    """A24, A59, A99 and R3 §2.2: no provider means no F effect and no
    preservation-dependent step; nothing is written, nothing is synthesized."""

    env_kw = {"auto_handoff": False}

    def drop(self):
        self.env.world.o5.preservation = None

    def test_freeze_start(self):
        self.to()
        self.drop()
        self.refused("freeze-start", Code.PROVENANCE_INSUFFICIENT, "preservation_unavailable")

    def test_p7_cells(self):
        self.to("freeze-start")
        self.drop()
        for verb in ("freeze-continue", "thaw"):
            self.refused(verb, Code.PROVENANCE_INSUFFICIENT, "preservation_unavailable")

    def test_quarantine_and_quiescence(self):
        self.to("freeze-start", "freeze-continue")
        self.drop()
        self.refused("quarantine", Code.PROVENANCE_INSUFFICIENT, "preservation_unavailable")
        self.env.world.o5.preservation = o5.SyntheticPreservation(self.env.world.observer)
        self.ok("quarantine")
        self.drop()
        self.refused("quiescence", Code.PROVENANCE_INSUFFICIENT, "preservation_unavailable")

    def test_p13_on_both_bases(self):
        self.to("freeze-start", "freeze-continue", "quarantine", "quiescence")
        self.drop()
        self.refused("snapshot", Code.PROVENANCE_INSUFFICIENT, "preservation_unavailable")
        self.env.world.o5.preservation = o5.SyntheticPreservation(self.env.world.observer)
        o5.synthetic_handoff(self.env, SLOT)
        self.drop()
        self.refused("snapshot", Code.QUIESCENCE_NOT_ESTABLISHED, "handoff_unavailable")
        for verb in ("verify-preservation", "preservation"):
            self.assertIs(self.run_(verb).outcome, Outcome.REFUSED)

    def test_no_pr1_fallback_runs_the_effect(self):
        self.to()
        self.drop()
        self.refused("freeze-start", Code.PROVENANCE_INSUFFICIENT)
        self.assertEqual(self.effects, [])


class Provider(Base):
    """A57: the structural guard (R3 §7.9, R4 §4.4)."""

    def at_p7(self):
        self.to("freeze-start", "freeze-continue")

    def patch(self, **changes):
        prov = self.env.world.o5.preservation
        real = prov.decide

        def decide(ctx, selection, view):
            d = real(ctx, selection, view)
            f = dict(d._f, **changes)
            return oc.PreservationDecision(oc._PSEAL, **f)
        prov.decide = decide

    def test_wrong_cell(self):
        self.at_p7()
        self.patch(cell="D-READY")
        self.refused("quarantine", Code.STATE_UNKNOWN, "decision_incomplete")

    def test_no_inputs(self):
        self.at_p7()
        self.patch(inputs={})
        self.refused("quarantine", Code.STATE_UNKNOWN, "decision_incomplete")

    def test_inputs_outside_the_selection(self):
        self.at_p7()
        self.patch(inputs={"ep_manifest": "1" * 64, "journal_head": "2" * 64})
        self.refused("quarantine", Code.STATE_UNKNOWN, "decision_incomplete(digest)")

    def test_other_context(self):
        self.at_p7()
        self.patch(ctx="3" * 64)
        self.refused("quarantine", Code.STATE_UNKNOWN, "decision_incomplete")

    def test_refusal_code_is_passed_through(self):
        self.at_p7()
        self.env.world.o5.preservation.refuse["D-QUAR"] = (Code.PROVENANCE_INSUFFICIENT, "p2_refused")
        self.refused("quarantine", Code.PROVENANCE_INSUFFICIENT, "p2_refused")

    def test_failed_validation_verdict(self):
        self.to("freeze-start", "freeze-continue", "quarantine", "quiescence")
        self.env.world.o5.preservation.refuse["H-VAL"] = (Code.STATE_UNKNOWN, "x")
        self.refused("snapshot", Code.QUIESCENCE_NOT_ESTABLISHED, "handoff_invalid")

    def test_check_fresult_rejection_leaves_the_intent_pending(self):
        self.to()
        self.env.world.o5.preservation.refuse["check_fresult"] = (None, None)
        r = self.run_("freeze-start")
        self.assertIs(r.outcome, Outcome.UNFINISHED)
        self.assertEqual(self.env.world.fwal.log(SLOT)[-1]["kind"], "intent")


class ContextChange(Base):
    """A15 / A22 / V9: a change between V and R refuses CONTEXT_CHANGED."""

    def between(self, mutate, code=Code.CONTEXT_CHANGED, detail=None):
        """R recomputes the whole gate; the first failing step decides."""
        self.to()
        o5.observe(self.env, SLOT)
        real = self.env.confirmer

        def confirmer(text, nonce):
            mutate()
            return real(text, nonce)
        self.env.confirmer = confirmer
        writes, effects = list(self.env.world.writes()), list(self.effects)
        r = self.run_("freeze-start", observe=False)
        self.assertEqual((r.outcome, r.code), (Outcome.REFUSED, code))
        if detail is not None:
            self.assertEqual(r.detail, detail)
        self.assertEqual((list(self.env.world.writes()), self.effects), (writes, effects))
        self.assertEqual(self.env.world.fwal.log(SLOT), [])

    def test_newer_relevant_observation(self):
        self.between(lambda: o5.observe(self.env, SLOT))

    def test_head_appended(self):
        self.between(lambda: o5.cutover_step(self.env, "model-b-rebaseline"), Code.STATE_UNKNOWN, "barrier_binding")

    def test_release_requested_appended(self):
        self.between(lambda: o5.append_record(self.env, self.env.barrier_id, "release_requested"),
                     Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(releasing)")


class Barrier(Base):
    env_kw = {"auto_handoff": False}

    def test_stale_observation(self):
        self.to()
        o5.observe(self.env, SLOT)
        self.env.world.clock.t += 700
        self.refused("freeze-start", Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(stale)", observe=False)

    def test_channel_after_c0_is_barrier_broken(self):
        """A53, A20: at P13 the N cell refuses barrier_broken, never ALREADY_COMPLETE."""
        self.to("freeze-start", "freeze-continue", "quarantine", "quiescence")
        chans = {"channels": dict({n: "neutralized" for n in o5.NEUTRALIZED}, new_agent_job="present")}
        o5.observe(self.env, SLOT, overrides={"agent_channels": chans})
        self.refused("quiescence", Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_broken", observe=False)

    def test_thaw_is_exempt_from_baseline_and_writer_checks(self):
        """A36: a BROKEN chain and a drifted baseline do not block the owner's thaw."""
        self.to("freeze-start")
        chans = {"channels": {"new_agent_job": "present"}}
        o5.observe(self.env, SLOT, overrides={"agent_channels": chans, "principals": {"digest": "9" * 64}})
        r = self.run_("thaw", observe=False)
        self.assertIs(r.outcome, Outcome.COMMITTED)

    def test_stop_refuses_thaw_and_its_resume(self):
        """A12, C3."""
        self.to("freeze-start")
        self.env.run("stop", reason=reason())
        self.refused("thaw", Code.STOPPED)
        self.env.run("resume", reason=reason())
        self.env.faults.point = "fwal:append:after"
        try:
            self.run_("thaw")
        except BaseException:  # noqa: BLE001 - the injected crash
            pass
        self.env.faults.point = None
        self.env.run("stop", reason=reason())
        self.refused("resume-step", Code.STOPPED)

    def test_stop_refuses_a_thaw_intent_resume_in_lifecycle(self):
        """C3 (pure): lifecycle.decide refuses it before the gate's thaw basis."""
        from broker import anchor, libview, lifecycle
        c = lifecycle.Components(lifecycle.G(True, True, True, True), libview.L(libview.LEASED, 1, "l", "op1", "d"),
                                 anchor.A(anchor.STABLE, "d", None, None, None, None), "THAW_INTENT",
                                 lifecycle.R("ACTIVE", "done", None), "ABSENT", lifecycle.OpInfo("op1", "l", 1, True,
                                                                                               False))
        with self.assertRaises(Refused) as e:
            lifecycle.decide(c, "resume-step")
        self.assertIs(e.exception.code, Code.STOPPED)
        with self.assertRaises(Refused) as e:
            lifecycle.decide(c._replace(f="MOVE_INTENT"), "resume-step")
        self.assertIsNot(e.exception.code, Code.STOPPED)

    def test_freeze_start_refuses_while_any_cycle_of_the_slot_is_open(self):
        """G4 (pure): `freeze-start` needs no open cycle on the slot."""
        self.to()
        w = self.env.world
        facts = invocation._read_slot(w, SLOT, w.ledger.entries())
        spec = freeze.cell_spec("freeze-start", "P6", "NONE", False)
        for closure, code in (("open", Code.VERB_NOT_ALLOWED_IN_STATE), ("ready_pending", Code.VERB_NOT_ALLOWED_IN_STATE),
                              ("STATE_UNKNOWN(fwal)", Code.STATE_UNKNOWN)):
            cyc = freeze.Cycle("c" * 64, SLOT, "op0", 1, "a" * 64, {}, "b" * 64, "h" * 64, "", closure, [], {}, None)
            with self.assertRaises(Refused) as e:
                freeze._g4(w, spec, facts, [cyc], {}, w.ledger.entries())
            self.assertIs(e.exception.code, code)
        cyc = freeze.Cycle("c" * 64, SLOT, "op0", 1, "a" * 64, {}, "b" * 64, "h" * 64, "", "none_no_move", [], {},
                           None)
        self.assertIsNone(freeze._g4(w, spec, facts, [cyc], {}, w.ledger.entries()))

    def test_tier_c_writers_refuse_under_a_live_chain(self):
        """A10 / C18: acquire, release, clear-suspect refuse; mark-suspect is allowed."""
        r = self.env.run("release", slot=SLOT, reason=reason())
        self.assertEqual((r.code, r.detail), (Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use"))
        self.assertIs(self.env.run("mark-suspect", slot=SLOT, reason=reason()).outcome, Outcome.COMMITTED)
        r = self.env.run("clear-suspect", slot=SLOT, reason=reason())
        self.assertEqual(r.detail, "barrier_in_use")
        o5.release_chain(self.env, self.env.barrier_id)
        self.assertIs(self.env.run("clear-suspect", slot=SLOT, reason=reason()).outcome, Outcome.COMMITTED)

    def test_refreshed_only_on_the_barrier_basis(self):
        """R4 §2.4 / A63."""
        self.to("freeze-start", "freeze-continue")
        kinds = [json.loads(v)["kind"] for k, v in sorted(self.env.world.o5.stores["barriers"].items.items())]
        self.assertEqual(kinds.count("refreshed"), 2)
        self.ok("quarantine", "quiescence")
        o5.synthetic_handoff(self.env, SLOT)
        o5.release_chain(self.env, self.env.barrier_id)
        n = len(self.env.world.o5.stores["barriers"].items)
        self.ok("snapshot")
        self.assertEqual(len(self.env.world.o5.stores["barriers"].items), n)
        self.assertEqual(o5.chain(self.env, self.env.barrier_id).state, "RELEASED")


class Quiescence(Base):
    """A34: quiescence_check rules 1–6 on the confirmed document."""

    def at_p11(self):
        self.to("freeze-start", "freeze-continue", "quarantine")

    def test_contradicting_component(self):
        self.at_p11()
        o5.observe(self.env, SLOT, overrides={"holder_scan_2": {"result": "contradicts"}})
        self.refused("quiescence", Code.QUIESCENCE_NOT_ESTABLISHED, "contradicts", observe=False)

    def test_disagreeing_pair(self):
        self.at_p11()
        o5.observe(self.env, SLOT, overrides={"tree_vector_2": {"vector": "8" * 64}})
        self.refused("quiescence", Code.QUIESCENCE_NOT_ESTABLISHED, "disagree", observe=False)

    def test_observed_before_the_revocation(self):
        """Rule 5 (pure): an observation older than the REVOKED completion."""
        doc = {"slot": SLOT, "op_id": OP, "o5": {"g_pre": 1}}
        obj = {"data": {"result": "consistent"}, "observed_at": 100,
               "subject": {"slot": SLOT, "op_id": OP, "slot_generation": 1}}
        picked = {k: ("a" * 64, obj) for k in oc.QUIES_KINDS}
        comps = [{"name": k, "sha256": "a" * 64} for k in oc.QUIES_KINDS]
        store = self.env.world.o5.stores["evidence"]
        with self.assertRaises(Refused) as e:
            freeze.quiescence_check(comps, picked, store, doc, 101, 150, "owner-quiescence")
        self.assertEqual(e.exception.detail, "stale")
        with self.assertRaises(Refused) as e:
            freeze.quiescence_check(comps, picked, store, doc, 50, 150, "reboot-runbook")
        self.assertEqual(e.exception.detail, "missing kind")

    def test_component_not_bound_to_the_selection(self):
        """A37 / rule 6."""
        self.at_p11()
        obs = self.env.world.observer
        real = obs.components
        obs.components = lambda verb, slot, op, sel=None: [
            capability.observed(k, "7" * 64 if k == "session_scan" else d, role="supporting")
            for k, d in sorted((sel or {}).items()) if k in oc.QUIES_KINDS] if verb == "quiescence" else real(
            verb, slot, op)
        self.refused("quiescence", Code.QUIESCENCE_NOT_ESTABLISHED, "details")


class Documents(Base):
    def test_decision_field_collision_refuses(self):
        """A38."""
        self.to("freeze-start", "freeze-continue", "quarantine", "quiescence", "snapshot", "verify-preservation")
        obs = self.env.world.observer
        real = obs.preservation
        obs.preservation = lambda slot, op: dict(real(slot, op), o5={"x": 1})
        self.refused("preservation", Code.FIELD_INVALID)
        obs.preservation = lambda slot, op: dict(real(slot, op), verb="x")
        self.refused("preservation", Code.FIELD_INVALID)

    def test_documents_bind_the_o5_context(self):
        self.to("freeze-start")
        doc = [json.loads(v) for v in self.env.world.authz.docs.values() if json.loads(v).get("verb") ==
               "freeze-start"][0]
        self.assertEqual(sorted(doc["o5"]), ["barrier_head", "barrier_id", "ctx", "cutover_head", "enumeration",
                                             "g_pre", "pre", "selection"])
        self.assertEqual(doc["o5"]["barrier_id"], self.env.barrier_id)


class Ingestion(unittest.TestCase):
    """F4 §5.1–§5.2: the ingestion path cannot write any Gated store."""

    def test_capability_has_no_public_constructor(self):
        with self.assertRaises(TypeError):
            evidence.IngestCapability()
        with self.assertRaises(TypeError):
            evidence._new_ingest(object())

    def test_ingest_capability_is_not_a_commit_txn(self):
        env = Env()
        self.addCleanup(env.close)
        cap = o5.ingest_capability()
        for store in (env.world.authz, env.world.fwal, env.world.o5.stores["barriers"]):
            with self.assertRaises(TxnError):
                store._begin(cap, "put")

    def test_ingest_refuses_authority_fields_and_provenance(self):
        env = Env()
        self.addCleanup(env.close)
        base = {"schema": oc.S_EVIDENCE, "kind": "principals", "observed_at": 1, "producer": evidence.PRODUCER,
                "subject": {"scope": "pool", "slot": "", "slot_generation": 0, "op_id": "", "cycle": "",
                            "barrier_id": ""}, "provenance": "ROOT_OBSERVED", "role": "decisive",
                "mode": oc.SYNTHETIC, "data": {}}
        store = env.world.o5.stores["evidence"]
        evidence.ingest(o5.ingest_capability(), store, base)
        for bad in (dict(base, provenance="OWNER_DECISION"), dict(base, data={"auth_digest": "x"}),
                    dict(base, kind="superseded"), dict(base, producer="agent")):
            with self.assertRaises(Refused):
                evidence.ingest(o5.ingest_capability(), store, bad)
        with self.assertRaises(TypeError):
            evidence.ingest(object(), store, base)


class Sealed(unittest.TestCase):
    """I14: a seal is an accident guard; no public constructor."""

    def test_boundary_types_need_their_seal(self):
        for t in (oc.PreBasis, oc.ContainmentContext, oc.HandoffVerdict, oc.PreservationDecision, oc.Discharge):
            with self.assertRaises(TypeError):
                t(object())
        with self.assertRaises(TypeError):
            oc.PreservationDecision(oc._CSEAL, cell="D-QUAR", ctx="", inputs={}, result="PROCEED", code=None,
                                    detail=None, doc_fields={})


if __name__ == "__main__":
    unittest.main()

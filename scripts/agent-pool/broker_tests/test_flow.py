"""End-to-end owner invocations against the unmodified recovery library on
synthetic stores: INITIATE, RESUME_PENDING, COMPLETE_TERMINAL, ALREADY_COMPLETE,
custody after confirmation, cancellation and cleanup, interrupted quarantine,
the write barrier, revalidation, crash rows and token scope
(r4 §4.4, §5.4, §8.3; A1 §A1.9, §A2.9, §A3.3; C2, C3, C5, C10)."""

import json
import unittest

import harness
from harness import QUARANTINE, QUIESCENCE, SLOT, Env, reason

import recovery_state as rs
from broker import authz, canonical, custody, invocation, ledger, lifecycle
from broker._testsupport import capability
from broker.refusal import Code, Outcome
from broker.stores import Crash


class Base(unittest.TestCase):
    def setUp(self):
        self.env = Env()
        self.lid, self.lease_token = self.env.seed_leased()

    def tearDown(self):
        self.env.close()

    def run_ok(self, verb, **extra):
        r = self.env.run(verb, slot=SLOT, reason=reason(), **extra)
        self.assertIs(r.outcome, Outcome.COMMITTED, f"{verb}: {r}")
        return r

    def refused(self, verb, code, **extra):
        before, writes = self.env.snapshot(), len(self.env.world.writes())
        r = self.env.run(verb, slot=SLOT, reason=reason(), **extra)
        self.assertEqual((r.outcome, r.code), (Outcome.REFUSED, code), f"{verb}: {r}")
        self.assertEqual(self.env.snapshot(), before, f"{verb} changed a store")
        self.assertEqual(len(self.env.world.writes()), writes)
        return r

    def crash(self, point, verb, error=None, **extra):
        self.env.faults.point, self.env.faults.error = point, error
        if error is None:
            with self.assertRaises(Crash):
                self.env.run(verb, slot=SLOT, reason=reason(), **extra)
            return None
        r = self.env.run(verb, slot=SLOT, reason=reason(), **extra)
        self.assertIs(r.outcome, Outcome.UNFINISHED, r)
        self.env.faults.error = None
        return r

    def facts(self):
        w = self.env.world
        return invocation._read_slot(w, SLOT, w.ledger.entries())

    def row(self):
        f = self.facts()
        c = lifecycle.Components(invocation._g(self.env.world), f.l, f.a, f.f, f.r, f.k, f.op)
        return lifecycle.row(c), f

    def rkey(self, op="op1"):
        f = self.facts()
        return custody.recovery_key(SLOT, op, f.op.gen)

    def check_digest(self, op="op1"):
        st = self.env.world.custody.lookup(self.rkey(op))
        return st.record["receipt"]["check_digest"] if st.record else None


class HappyPath(Base):
    def test_initiate_to_ready_and_custody_cleanup(self):
        harness.to_ready(self.env)
        st, gen, pending = self.env.op_state("op1")
        self.assertEqual((st, gen, pending), (rs.READY, 2, False))
        self.assertEqual(self.env.world.library.store.terminal(SLOT, "op1"), rs.READY)
        self.assertEqual(self.facts().k, "BOUND")
        self.run_ok("custody-destroy", op_id="op1")
        self.assertEqual(self.facts().k, "DESTROYED")
        r = self.env.run("custody-destroy", slot=SLOT, op_id="op1", reason=reason())
        self.assertIs(r.outcome, Outcome.ALREADY_COMPLETE)

    def test_every_gate_and_evidence_is_a_burned_stored_document(self):
        harness.to_ready(self.env)
        w = self.env.world
        burned = {e["auth_digest"] for e in w.ledger.entries()}
        rec, _ = w.library.store._read(SLOT, "op1")
        for t, d in rec["gates"].items():
            self.assertIn(d, burned, t)
            doc = json.loads(w.authz.get(d))
            self.assertEqual(canonical.digest(doc), rs.digest(doc))
            self.assertEqual(doc["trust"], authz.TRUST_SYNTHETIC)
            self.assertTrue(canonical.secret_free(doc))

    def test_already_complete_writes_nothing(self):
        for verb in ("adopt", "request"):
            self.run_ok(verb, op_id="op1")
            before = self.env.snapshot()
            r = self.env.run(verb, slot=SLOT, op_id="op1", reason=reason())
            self.assertIs(r.outcome, Outcome.ALREADY_COMPLETE, verb)
            self.assertEqual(self.env.snapshot(), before)
        self.refused("adopt", Code.STATE_UNKNOWN, op_id="op1")

    def test_cancel_from_active_and_repeat(self):
        self.run_ok("adopt", op_id="op1")
        self.run_ok("cancel", op_id="op1")
        self.assertEqual(self.env.world.library.store.terminal(SLOT, "op1"), rs.CANCELLED)
        self.assertEqual(self.row()[0], "P2")
        before = self.env.snapshot()
        self.assertIs(self.env.run("cancel", slot=SLOT, op_id="op1", reason=reason()).outcome,
                      Outcome.ALREADY_COMPLETE)
        self.assertIs(self.env.run("resume-step", slot=SLOT, reason=reason()).outcome, Outcome.ALREADY_COMPLETE)
        self.assertEqual(self.env.snapshot(), before)
        self.run_ok("adopt", op_id="op2")


class Refusals(Base):
    def test_unauthorized_transitions_write_nothing(self):
        self.refused("request", Code.VERB_NOT_ALLOWED_IN_STATE, op_id="op1")
        self.run_ok("adopt", op_id="op1")
        self.refused("quarantine", Code.VERB_NOT_ALLOWED_IN_STATE, op_id="op1", **QUARANTINE)
        self.refused("ready", Code.VERB_NOT_ALLOWED_IN_STATE, op_id="op1")
        self.refused("thaw", Code.VERB_NOT_ALLOWED_IN_STATE, op_id="op1")
        self.refused("request", Code.VERB_NOT_ALLOWED_IN_STATE, op_id="op9")
        self.refused("adopt", Code.VERB_NOT_ALLOWED_IN_STATE, op_id="op2")
        self.refused("custody-destroy", Code.VERB_NOT_ALLOWED_IN_STATE, op_id="op1")

    def test_declined_expired_session_and_trust_refusals(self):
        self.run_ok("adopt", op_id="op1")
        self.env.declines = True
        self.refused("request", Code.CONFIRMATION_FAILED, op_id="op1")
        self.env.declines = False
        self.env.world.window = 1
        self.env.world.clock.t += 0
        real = self.env.world.clock

        def jump():
            real.t += 5
            return real.t
        self.env.world.clock = jump
        self.refused("request", Code.EXPIRED, op_id="op1")
        self.env.world.clock, self.env.world.window = real, 300
        valid = [True]
        self.env.ctx.principal._valid = lambda: valid[0]
        orig = self.env.confirmer

        def flip(text, line):
            valid[0] = False
            return orig(text, line)
        self.env.confirmer = flip
        self.refused("request", Code.SESSION_INVALID, op_id="op1")

    def test_reused_nonce_is_a_replay(self):
        self.run_ok("adopt", op_id="op1")
        self.env.world.nonces = lambda: "5" * 32
        self.run_ok("request", op_id="op1")
        self.refused("freeze-start", Code.REPLAY, op_id="op1")

    def test_confirm_checks_nonce_and_session(self):
        data = canonical.canon({"v": 1, "doc": "stop", "reason": "r"})
        good = lambda text, line: line.split(" ", 1)[1]  # noqa: E731
        self.assertEqual(authz.confirm(data, "a" * 32, good, authz.SyntheticOwner()), canonical.sha256(data))
        for confirmer, principal, code in (
                (lambda t, l: "b" * 32, authz.SyntheticOwner(), Code.CONFIRMATION_FAILED),
                (lambda t, l: None, authz.SyntheticOwner(), Code.CONFIRMATION_FAILED),
                (good, authz.SyntheticOwner(valid=lambda: False), Code.SESSION_INVALID)):
            with self.assertRaises(authz.Refused) as e:
                authz.confirm(data, "a" * 32, confirmer, principal)
            self.assertIs(e.exception.code, code)

    def test_revalidate_refuses_burned_digests(self):
        doc = {"issued_at": 0, "not_after": 10, "nonce_digest": "1" * 64}
        p = authz.SyntheticOwner()
        for burned in ({"2" * 64}, {"1" * 64}):
            with self.assertRaises(authz.Refused) as e:
                authz.revalidate(doc, "2" * 64, "3" * 64, "3" * 64, 5, burned, p)
            self.assertIs(e.exception.code, Code.REPLAY)

    def test_stop_allows_only_resume_and_resume_step(self):
        self.env.run("stop", reason=reason())
        self.refused("adopt", Code.STOPPED, op_id="op1")
        self.assertIs(self.env.run("stop", reason=reason()).outcome, Outcome.ALREADY_COMPLETE)
        self.assertIs(self.env.run("resume", reason=reason()).outcome, Outcome.COMMITTED)
        self.run_ok("adopt", op_id="op1")

    def test_principal_gate(self):
        self.env.world.principals_ok = False
        self.refused("adopt", Code.GATE_ADMIN, op_id="op1")

    def test_one_way_gate(self):
        self.env.world.one_way = False
        self.refused("adopt", Code.ONE_WAY_NOT_ACCEPTED, op_id="op1")

    def test_wrong_generation_or_identity_is_an_anchor_mismatch(self):
        self.run_ok("adopt", op_id="op1")
        w = self.env.world
        w.tierc.records[SLOT] = w.tierc.records[SLOT].replace("generation=1", "generation=2")
        self.refused("request", Code.ANCHOR_MISMATCH, op_id="op1")
        w.tierc.records[SLOT] = w.tierc.records[SLOT].replace("generation=2", "generation=1").replace(
            f"lease_id={self.lid}", "lease_id=" + "9" * 32)
        self.refused("request", Code.ANCHOR_MISMATCH, op_id="op1")


class ShortcutPrecedence(Base):
    """Review B1: no ALREADY_COMPLETE shortcut may skip the A1 §A2.6 step 4-5 checks."""

    def tamper_tierc(self):
        w = self.env.world
        w.tierc.records[SLOT] = w.tierc.records[SLOT].replace("task=t1", "task=tX")

    def f_unknown(self):
        self.env.world.fwal.entries.setdefault(SLOT, []).append(
            {"op_id": "op1", "kind": "complete", "value": "BOGUS",
             "auth_digest": self.env.world.ledger.entries()[-1]["auth_digest"]})

    def test_shortcuts_refuse_contradictory_state(self):
        self.run_ok("adopt", op_id="op1")
        self.run_ok("request", op_id="op1")
        self.tamper_tierc()
        self.refused("resume-step", Code.ANCHOR_MISMATCH, op_id="op1")
        self.refused("adopt", Code.ANCHOR_MISMATCH, op_id="op1")
        self.refused("request", Code.ANCHOR_MISMATCH, op_id="op1")

    def test_shortcuts_refuse_unknown_f(self):
        self.run_ok("adopt", op_id="op1")
        self.run_ok("request", op_id="op1")
        self.f_unknown()
        for verb in ("resume-step", "request", "adopt"):
            self.refused(verb, Code.STATE_UNKNOWN, op_id="op1")

    def test_shortcuts_refuse_custody_states(self):
        harness.to_quarantine(self.env)
        self.run_ok("quarantine", op_id="op1", **QUARANTINE)
        w = self.env.world
        kd = canonical.digest(self.rkey())
        good = w.custody.records[kd]
        w.custody.records[kd] = good[:-3] + b"00}"
        self.refused("resume-step", Code.CUSTODY_CORRUPT, op_id="op1")
        del w.custody.records[kd]
        self.refused("resume-step", Code.CUSTODY_ABSENT, op_id="op1")
        self.refused("quarantine", Code.CUSTODY_ABSENT, op_id="op1", **QUARANTINE)

    def test_repeat_adopt_with_a_corrupt_anchor_log(self):
        self.run_ok("adopt", op_id="op1")
        self.env.world.anchors[SLOT].data += b"*\n"
        self.refused("adopt", Code.STATE_UNKNOWN, op_id="op1")

    def test_refused_quiescence_on_p11_with_persisted_custody(self):
        harness.to_quarantine(self.env)
        self.crash("library:after_complete", "quarantine", op_id="op1", **QUARANTINE)
        self.run_ok("resume-step", op_id="op1")
        w = self.env.world
        st = w.custody.lookup(self.rkey())
        w.custody._store(self.rkey(), dict(st.record, state=custody.PERSISTED, bound=[]))
        self.assertEqual((self.row()[0], self.facts().k), ("P11", "PERSISTED"))
        self.env.declines = True
        self.refused("quiescence", Code.CONFIRMATION_FAILED, op_id="op1", **QUIESCENCE)
        self.env.declines = False
        self.assertEqual(self.facts().k, "PERSISTED")
        self.run_ok("quiescence", op_id="op1", **QUIESCENCE)
        self.assertEqual(self.facts().k, "BOUND")

    def test_owner_check_precedes_field_checks(self):
        env = Env(store_kind=authz.PROTECTED, principal=authz.SyntheticOwner())
        try:
            r = env.run("request", slot=SLOT, op_id="op1", reason=reason(), bogus=1)
            self.assertEqual(r.code, Code.OWNER_REQUIRED)
        finally:
            env.close()


class Trust(unittest.TestCase):
    def test_protected_context_refuses_until_rs1(self):
        env = Env(store_kind=authz.PROTECTED, principal=capability.owner_capability())
        try:
            env.seed_leased()
            r = env.run("adopt", slot=SLOT, op_id="op1", reason=reason())
            self.assertIs(r.outcome, Outcome.COMMITTED)
            before = env.snapshot()
            r = env.run("request", slot=SLOT, op_id="op1", reason=reason())
            self.assertEqual((r.outcome, r.code), (Outcome.REFUSED, Code.TRUST_INSUFFICIENT))
            self.assertEqual(env.snapshot(), before)
        finally:
            env.close()

    def test_synthetic_owner_cannot_act_on_a_protected_store(self):
        env = Env(store_kind=authz.PROTECTED, principal=authz.SyntheticOwner())
        try:
            env.seed_leased()
            r = env.run("adopt", slot=SLOT, op_id="op1", reason=reason())
            self.assertEqual((r.outcome, r.code), (Outcome.REFUSED, Code.OWNER_REQUIRED))
        finally:
            env.close()

    def test_unmodified_library_refuses_owner_authenticated_documents(self):
        env = Env()
        try:
            env.seed_leased()
            harness.to_quarantine(env)
            w = env.world
            rec, _ = w.library.store._read(SLOT, "op1")
            doc = json.loads(w.authz.get(rec["gates"]["request"]))
            doc["trust"] = authz.TRUST_OWNER
            doc["transition"], doc["seq"] = "cancel", rec["seq"]
            with self.assertRaises(rs.Refused):
                w.library.store.cancel(SLOT, op_id="op1", authorization=doc, record=None)
        finally:
            env.close()


class QuarantineCrashRows(Base):
    """r4 §8.3 / A1 §A2.5: every crash row converges with the same token."""

    def advance(self):
        harness.to_quarantine(self.env)

    def finish(self):
        for _ in range(4):
            rid, f = self.row()
            if rid == "P11":
                break
            if rid == "P7":
                self.run_ok("quarantine", op_id="op1", **QUARANTINE)
            else:
                self.run_ok("resume-step", op_id="op1")
        self.assertEqual(self.row()[0], "P11")
        self.run_ok("quiescence", op_id="op1", **QUIESCENCE)
        self.assertEqual(self.facts().k, "BOUND")

    def test_library_fault_points(self):
        expect = {"library:before_intent": "P7a", "library:after_intent": "P8", "library:after_record": "P9",
                  "library:before_state_rename": "P9", "library:after_state": "P10", "library:after_complete": "P10a"}
        for point, row in expect.items():
            with self.subTest(point):
                self.env.close()
                self.env = Env()
                self.env.seed_leased()
                self.advance()
                self.crash(point, "quarantine", op_id="op1", **QUARANTINE)
                rid, f = self.row()
                self.assertEqual(rid, row)
                token_check = self.check_digest()
                self.assertIsNotNone(token_check)
                self.finish()
                self.assertEqual(self.check_digest(), token_check)

    def test_write_boundary_matrix(self):
        points = ["custody:persist:temp", "custody:persist:after", "authz:put:before", "authz:put:after",
                  "ledger:append:partial", "ledger:append:body", "ledger:append:after", "anchor:" + SLOT + ":append:after",
                  "tierc:write:after", "custody:mark_bound:before"]
        for point in points:
            with self.subTest(point):
                self.env.close()
                self.env = Env()
                self.env.seed_leased()
                self.advance()
                self.crash(point, "quarantine", op_id="op1", **QUARANTINE)
                first = self.check_digest()
                rid, _ = self.row()
                self.assertIn(rid, ("P7", "P7a", "P9", "P10a", "P11"))
                if point.startswith("ledger:append:partial"):
                    self.assertEqual(self.env.world.ledger.classify()["cls"], ledger.TAIL_PREFIX)
                if point.startswith("ledger:append:body"):
                    self.assertEqual(self.env.world.ledger.classify()["cls"], ledger.TAIL_UNTERMINATED_VALID)
                self.finish()
                if first is not None:
                    self.assertEqual(self.check_digest(), first)

    def test_ordinary_failure_after_the_burn_is_unfinished(self):
        self.advance()
        r = self.crash("library:after_intent", "quarantine", error=RuntimeError, op_id="op1", **QUARANTINE)
        kinds = [x.split(":")[0] for x in r.durable_ref]
        self.assertEqual(kinds[:4], ["custody", "authz", "ledger", "anchor"])
        self.assertTrue(all(self.lease_token not in x for x in r.durable_ref))
        self.assertEqual(self.row()[0], "P8")
        self.finish()

    def test_resume_pre_mode_rejects_a_non_custody_token(self):
        self.advance()
        self.crash("library:after_intent", "quarantine", op_id="op1", **QUARANTINE)
        w = self.env.world
        key = self.rkey()
        other = "f" * 32
        w.custody._store(key, {"receipt": custody.receipt(key, other), "token": other, "state": custody.PERSISTED,
                               "bound": [], "terminal_ref": {}})
        self.refused("resume-step", Code.CUSTODY_MISMATCH, op_id="op1")

    def test_resume_post_mode_checks_the_verifier(self):
        self.advance()
        self.crash("library:after_state", "quarantine", op_id="op1", **QUARANTINE)
        w = self.env.world
        key = self.rkey()
        other = "e" * 32
        w.custody._store(key, {"receipt": custody.receipt(key, other), "token": other, "state": custody.PERSISTED,
                               "bound": [], "terminal_ref": {}})
        self.refused("resume-step", Code.CUSTODY_MISMATCH, op_id="op1")

    def test_later_steps_check_the_verifier_before_the_burn(self):
        self.advance()
        self.run_ok("quarantine", op_id="op1", **QUARANTINE)
        w = self.env.world
        key = self.rkey()
        other = "d" * 32
        w.custody._store(key, {"receipt": custody.receipt(key, other), "token": other, "state": custody.BOUND,
                               "bound": w.custody.lookup(key).record["bound"], "terminal_ref": {}})
        self.refused("quiescence", Code.CUSTODY_MISMATCH, op_id="op1", **QUIESCENCE)

    def test_missing_custody_after_qi_is_absent_never_reminted(self):
        self.advance()
        self.crash("library:after_intent", "quarantine", op_id="op1", **QUARANTINE)
        self.env.world.custody.records.clear()
        self.refused("resume-step", Code.CUSTODY_ABSENT, op_id="op1")

    def test_no_remint_after_a_burned_quarantine_document(self):
        self.advance()
        self.crash("library:before_intent", "quarantine", op_id="op1", **QUARANTINE)
        self.run_ok("resume-step", op_id="op1")
        self.assertEqual(self.row()[0], "P7")
        self.env.world.custody.records.pop(canonical.digest(self.rkey()))
        self.refused("quarantine", Code.CUSTODY_ABSENT, op_id="op1", **QUARANTINE)

    def test_changed_evidence_refuses_original_unavailable(self):
        self.advance()
        self.crash("library:after_intent", "quarantine", op_id="op1", **QUARANTINE)
        w = self.env.world
        rec, pending = w.library.store._read(SLOT, "op1")
        w.authz.docs[pending["evidence"]] = w.authz.docs[pending["evidence"]].replace(b"orphaned", b"0rphaned")
        self.refused("resume-step", Code.ORIGINAL_AUTHZ_UNAVAILABLE, op_id="op1")

    def test_regenerated_document_is_refused_by_the_library(self):
        self.advance()
        self.crash("library:after_intent", "quarantine", op_id="op1", **QUARANTINE)
        w = self.env.world
        rec, pending = w.library.store._read(SLOT, "op1")
        doc = json.loads(w.authz.get(pending["evidence"]))
        doc["utc"] = "2026-11-12T00:00:00Z"
        before = w.library.snapshot_slot(SLOT)
        token = w.custody.lookup(self.rkey()).record["token"]
        with self.assertRaises(rs.Refused):
            w.library.store.quarantine(SLOT, op_id="op1", authorization=doc, recovery_token=token, record=None,
                                       replace_record=lambda a, b: b)
        self.assertEqual(w.library.snapshot_slot(SLOT), before)

    def test_mark_bound_only_at_m1_or_m2(self):
        self.advance()
        self.crash("library:after_complete", "quarantine", op_id="op1", **QUARANTINE)
        labels = [lab for lab in self.env.world.custody.writes]
        self.assertNotIn("mark_bound", [lab for _, lab in labels])
        self.run_ok("resume-step", op_id="op1")
        self.assertEqual([lab for _, lab in self.env.world.custody.writes].count("mark_bound"), 1)
        self.assertEqual(self.facts().k, "BOUND")


class CancellationAndCleanup(Base):
    def test_review_sequence_failed_start_thaw_cancel_destroy_readopt(self):
        harness.to_quarantine(self.env)
        self.crash("custody:persist:after", "quarantine", op_id="op1", **QUARANTINE)
        self.assertEqual((self.row()[0], self.facts().k), ("P7", "PERSISTED"))
        first = self.check_digest()
        self.crash("library:before_intent", "quarantine", op_id="op1", **QUARANTINE)
        self.assertEqual(self.check_digest(), first)
        self.assertEqual(self.row()[0], "P7a")
        self.run_ok("resume-step", op_id="op1")
        self.assertEqual(self.row()[0], "P7")
        custody_writes = len(self.env.world.custody.writes)
        self.run_ok("thaw", op_id="op1")
        self.assertEqual(self.row()[0], "P6")
        self.run_ok("cancel", op_id="op1")
        self.assertEqual(len(self.env.world.custody.writes), custody_writes)
        self.assertEqual((self.row()[0], self.facts().k), ("P2", "PERSISTED"))
        self.run_ok("custody-destroy", op_id="op1")
        self.assertEqual(self.facts().k, "DESTROYED")
        self.run_ok("adopt", op_id="op2")
        self.assertEqual(self.row()[0], "P5")

    def test_custody_destroy_crash_matrix(self):
        for point in ("custody:mark_terminal:after", "custody:tombstone:before", "custody:tombstone:after",
                      "custody:unlink:before"):
            with self.subTest(point):
                self.env.close()
                self.env = Env()
                self.env.seed_leased()
                harness.to_ready(self.env)
                self.crash(point, "custody-destroy", op_id="op1")
                self.assertIn(self.facts().k, ("BOUND", "TERMINAL(READY_FOR_REASSIGNMENT)", "DESTROYED*"))
                self.run_ok("custody-destroy", op_id="op1")
                self.assertEqual(self.facts().k, "DESTROYED")
                self.assertIs(self.env.run("custody-destroy", slot=SLOT, op_id="op1", reason=reason()).outcome,
                              Outcome.ALREADY_COMPLETE)

    def test_custody_destroy_refuses_while_stopped(self):
        harness.to_ready(self.env)
        self.assertIs(self.env.run("stop", reason=reason()).outcome, Outcome.COMMITTED)
        self.refused("custody-destroy", Code.STOPPED, op_id="op1")

    def test_custody_destroy_refuses_an_unclassifiable_anchor_log(self):
        harness.to_ready(self.env)
        self.env.world.anchors[SLOT].data += b"junk"
        self.refused("custody-destroy", Code.STATE_UNKNOWN, op_id="op1")
        self.assertEqual(self.facts().k, "BOUND")

    def test_custody_destroy_checks_a_terminal_record(self):
        harness.to_ready(self.env)
        w = self.env.world
        st = w.custody.lookup(self.rkey())
        w.custody._store(self.rkey(), dict(st.record, state=custody.TERMINAL, terminal_ref={
            "state": rs.READY, "marker_digest": "0" * 64, "journal_digest": "0" * 64}))
        self.refused("custody-destroy", Code.CUSTODY_CONFLICT, op_id="op1")
        w.custody._store(self.rkey(), dict(st.record, bound=["0" * 64, st.record["bound"][1]]))
        self.refused("custody-destroy", Code.CUSTODY_CONFLICT, op_id="op1")

    def test_custody_destroy_needs_a_backup_newer_than_the_marker(self):
        harness.to_ready(self.env)
        self.env.world.backups = lambda marker: None
        self.refused("custody-destroy", Code.VERB_NOT_ALLOWED_IN_STATE, op_id="op1")


class TerminalMarker(Base):
    def test_cancel_and_ready_marker_crashes_complete_terminal(self):
        cases = [("cancel", "library:after_complete"), ("cancel", "library:before_marker_rename"),
                 ("ready", "library:after_complete"), ("ready", "library:before_marker_rename")]
        for verb, point in cases:
            with self.subTest(verb=verb, point=point):
                self.env.close()
                self.env = Env()
                self.env.seed_leased()
                if verb == "cancel":
                    self.run_ok("adopt", op_id="op1")
                    self.run_ok("request", op_id="op1")
                else:
                    harness.to_ready(self.env)
                    self.env.close()
                    self.env = Env()
                    self.env.seed_leased()
                    harness.to_quarantine(self.env)
                    for v, extra in (("quarantine", QUARANTINE), ("quiescence", QUIESCENCE), ("snapshot", {}),
                                     ("verify-preservation", {}), ("preservation", {})):
                        self.run_ok(v, op_id="op1", **extra)
                self.crash(point, verb, op_id="op1")
                self.assertIn(self.row()[0], ("P17", "P20"))
                self.assertIsNone(self.env.world.library.store.terminal(SLOT, "op1"))
                self.run_ok("resume-step", op_id="op1")
                self.assertIsNotNone(self.env.world.library.store.terminal(SLOT, "op1"))
                before = self.env.snapshot()
                self.assertIs(self.env.run("resume-step", slot=SLOT, op_id="op1", reason=reason()).outcome,
                              Outcome.ALREADY_COMPLETE)
                self.assertEqual(self.env.snapshot(), before)


class FaultMatrixRows(Base):
    def test_every_library_fault_point_maps_to_a_listed_row(self):
        steps = [("request", {}), ("freeze-start", None), ("freeze-continue", None), ("quarantine", QUARANTINE),
                 ("quiescence", QUIESCENCE), ("snapshot", None), ("verify-preservation", None),
                 ("preservation", {}), ("ready", {})]
        points = ("before_intent", "after_intent", "after_record", "before_state_rename", "after_state",
                  "after_complete", "before_marker_rename")
        for i, (verb, extra) in enumerate(steps):
            if extra is None:
                continue
            for p in points:
                with self.subTest(verb=verb, point=p):
                    self.env.close()
                    self.env = Env()
                    self.env.seed_leased()
                    self.run_ok("adopt", op_id="op1")
                    for v, e in steps[:i]:
                        self.run_ok(v, op_id="op1", **(e or {}))
                    self.env.faults.point = "library:" + p
                    try:
                        self.env.run(verb, slot=SLOT, op_id="op1", reason=reason(), **extra)
                    except Crash:
                        pass
                    self.env.faults.point = None
                    rid, f = self.row()
                    self.assertIsNotNone(rid, (verb, p, f.r, f.a.kind, f.k))
                    st = self.env.world.library.store.state(SLOT, "op1")[0]
                    key = custody.recovery_key(SLOT, "op1", f.op.gen)
                    if st in rs.AFTER_QUARANTINE:
                        self.assertEqual(key["generation"], 1)


class Revalidation(unittest.TestCase):
    """A1 §A2.9 test_revalidation_detects_change: a change between C and R refuses with zero writes."""

    def test_changes_between_confirm_and_revalidate_refuse(self):
        def tierc(w):
            w.tierc.records[SLOT] = w.tierc.records[SLOT].replace("task=t1", "task=t2")

        def anchor_tail(w):
            w.anchors[SLOT].data += b"{"

        def ledger_tail(w):
            w.ledger.data += b"{"

        def principals(w):
            w.principals_ok = False

        def custody_change(w):
            w.custody.records["x" * 64] = b"{}"

        def stop(w):
            w.gates.entries.append({"kind": "stop", "auth_digest": "0" * 64})

        def authz_store(w):
            w.authz.docs["f" * 64] = b"{}"

        for name, mutate in (("tierc", tierc), ("anchor", anchor_tail), ("ledger", ledger_tail),
                             ("principals", principals), ("custody", custody_change), ("stop", stop),
                             ("authz", authz_store)):
            with self.subTest(name):
                env = Env()
                try:
                    env.seed_leased()
                    self.assertIs(env.run("adopt", slot=SLOT, op_id="op1", reason=reason()).outcome,
                                  Outcome.COMMITTED)
                    w = env.world
                    writes = len(w.writes())
                    orig = env.confirmer

                    def confirmer(text, line, mutate=mutate, w=w, orig=orig):
                        mutate(w)
                        return orig(text, line)
                    env.confirmer = confirmer
                    r = env.run("request", slot=SLOT, op_id="op1", reason=reason())
                    self.assertIs(r.outcome, Outcome.REFUSED, r)
                    self.assertEqual(len(w.writes()), writes)
                finally:
                    env.close()


class TierCVerbs(unittest.TestCase):
    def setUp(self):
        self.env = Env()
        self.env.seed_free()

    def tearDown(self):
        self.env.close()

    def run_ok(self, verb, **extra):
        r = self.env.run(verb, slot=SLOT, reason=reason(), **extra)
        self.assertIs(r.outcome, Outcome.COMMITTED, f"{verb}: {r}")

    def l(self):
        from broker import libview
        return libview.component(self.env.world.tierc.read(SLOT), SLOT)

    def test_acquire_release_and_suspect_cycle(self):
        from broker import libview
        self.run_ok("mark-suspect")
        self.assertEqual(self.l().kind, libview.SUSPECT_FREE)
        self.run_ok("clear-suspect")
        self.run_ok("acquire", task="t2")
        l = self.l()
        self.assertEqual((l.kind, l.g), (libview.LEASED, 1))
        st = self.env.world.custody.lookup(custody.lease_key(SLOT, l.lid, 1))
        self.assertEqual(st.k, custody.PERSISTED)
        self.assertNotIn(st.record["token"], self.env.world.tierc.read(SLOT))
        self.run_ok("mark-suspect")
        self.assertEqual(self.l().kind, libview.SUSPECT_LEASED)
        self.run_ok("adopt", op_id="op1")
        self.run_ok("cancel", op_id="op1")
        self.run_ok("clear-suspect")
        self.run_ok("release")
        self.assertEqual((self.l().kind, self.l().g), (libview.FREE, 2))
        a = self.env.world.anchor_log(SLOT).entries()
        self.assertEqual([e["kind"] for e in a].count("pending"), 6)

    def test_i_orphan_pins_anchor_entries(self):
        self.run_ok("mark-suspect")
        w = self.env.world
        lines = w.ledger.data.split(b"\n")
        w.ledger.data = b"\n".join(lines[:-2]) + b"\n"
        before = self.env.snapshot()
        r = self.env.run("clear-suspect", slot=SLOT, reason=reason())
        self.assertEqual((r.outcome, r.code), (Outcome.REFUSED, Code.LEDGER_UNAVAILABLE))
        self.assertEqual(self.env.snapshot(), before)

    def test_crash_between_anchor_and_tier_c_resumes_deterministically(self):
        from broker import libview
        for point, kind in (("tierc:write:before", libview.FREE), ("tierc:write:after", libview.LEASED)):
            with self.subTest(point):
                self.env.close()
                self.env = Env()
                self.env.seed_free()
                self.env.faults.point = point
                with self.assertRaises(Crash):
                    self.env.run("acquire", slot=SLOT, reason=reason(), task="t2")
                w = self.env.world
                f = invocation._read_slot(w, SLOT, w.ledger.entries())
                c = lifecycle.Components(invocation._g(w), f.l, f.a, f.f, f.r, f.k, f.op)
                self.assertEqual(lifecycle.row(c), "P21")
                self.run_ok("resume-step")
                self.assertEqual(self.l().kind, kind)
                self.assertEqual(w.anchor_log(SLOT).entries()[-1]["kind"],
                                 "abort" if kind == libview.FREE else "complete")
                self.assertIs(self.env.run("resume-step", slot=SLOT, reason=reason()).outcome,
                              Outcome.ALREADY_COMPLETE)


class RefusalSweep(Base):
    """A1 §A2.3 item 4: every refusal from every reached row writes nothing."""

    def test_refusal_writes_nothing(self):
        verbs = {"adopt": {}, "request": {}, "cancel": {}, "freeze-start": {}, "freeze-continue": {}, "thaw": {},
                 "quarantine": QUARANTINE, "quiescence": QUIESCENCE, "snapshot": {}, "verify-preservation": {},
                 "preservation": {}, "ready": {}, "resume-step": {}, "custody-destroy": {}, "acquire": None,
                 "release": None, "mark-suspect": None, "clear-suspect": None}
        stages = [("adopt", {}), ("request", {}), ("freeze-start", {}), ("freeze-continue", {}),
                  ("quarantine", QUARANTINE), ("quiescence", QUIESCENCE), ("snapshot", {}),
                  ("verify-preservation", {}), ("preservation", {}), ("ready", {})]
        checked = 0
        for verb_done, extra_done in [(None, None)] + stages:
            if verb_done:
                self.run_ok(verb_done, op_id="op1", **extra_done)
            for verb, extra in verbs.items():
                fields = dict(op_id="op1", **(extra or {})) if extra is not None else (
                    {"task": "t9"} if verb == "acquire" else {})
                if verb == "adopt" and verb_done is None:
                    continue
                before, writes = self.env.snapshot(), len(self.env.world.writes())
                self.env.declines = True
                r = self.env.run(verb, slot=SLOT, reason=reason(), **fields)
                self.env.declines = False
                self.assertIn(r.outcome, (Outcome.REFUSED, Outcome.ALREADY_COMPLETE), (verb_done, verb, r))
                self.assertEqual(self.env.snapshot(), before, (verb_done, verb))
                self.assertEqual(len(self.env.world.writes()), writes)
                checked += 1
        self.assertGreater(checked, 150)


class LedgerRepairInBand(Base):
    def test_torn_ledger_tail_is_trimmed_only_under_the_confirmed_plan(self):
        w = self.env.world
        w.ledger.data += b'{"auth_digest":"ab'
        self.env.declines = True
        self.refused("adopt", Code.CONFIRMATION_FAILED, op_id="op1")
        self.assertIn("UNPROVEN", self.env.last_render)
        self.env.declines = False
        self.run_ok("adopt", op_id="op1")
        res = w.ledger.classify()
        self.assertEqual(res["cls"], ledger.CLEAN)
        self.assertEqual(list(w.ledger.evidence.values()), [b'{"auth_digest":"ab'])
        self.assertTrue(res["entries"][-1]["repair"])

    def test_v11_truncated_burn_with_effects_is_refused_not_repaired(self):
        self.run_ok("adopt", op_id="op1")
        w = self.env.world
        lines = w.ledger.data.split(b"\n")
        w.ledger.data = b"\n".join(lines[:-2]) + b"\n" + lines[-2][:200]
        self.refused("request", Code.LEDGER_UNAVAILABLE, op_id="op1")

    def test_unclassifiable_ledger_refuses_every_owner_verb(self):
        w = self.env.world
        w.ledger.data = w.ledger.data[:-1] + b"*"
        self.refused("adopt", Code.LEDGER_UNAVAILABLE, op_id="op1")
        before = self.env.snapshot()
        r = self.env.run("stop", reason=reason())
        self.assertEqual((r.outcome, r.code, self.env.snapshot()), (Outcome.REFUSED, Code.LEDGER_UNAVAILABLE, before))

    def test_n5_3_evidence_checks_refuse_every_owner_verb(self):
        self.run_ok("adopt", op_id="op1")
        w = self.env.world
        size = len(w.ledger.data)
        w.ledger.evidence[(size, ledger.canonical.sha256(b"x"), 1)] = b"y"
        self.refused("request", Code.LEDGER_UNAVAILABLE, op_id="op1")
        w.ledger.evidence.clear()
        self.run_ok("request", op_id="op1")
        entries = w.ledger.entries()
        prev = entries[-1]["line_digest"]
        ghost = ledger.make_entry(ledger.LEDGER, len(entries), prev, {
            "auth_digest": "7" * 64, "auth_kind": "initiate", "binding_digest": "7" * 64, "confirmed_at": 1,
            "issued_at": 1, "nonce_digest": "8" * 64}, [{"offset": 1, "sha256": "9" * 64, "len": 1}])
        w.ledger.data += canonical.canon(ghost) + b"\n"
        self.refused("cancel", Code.LEDGER_UNAVAILABLE, op_id="op1")

    def test_i_orphan_pins_a_library_gate(self):
        self.run_ok("adopt", op_id="op1")
        self.run_ok("request", op_id="op1")
        w = self.env.world
        lines = w.ledger.data.split(b"\n")
        full = w.ledger.data
        for broken in (b"\n".join(lines[:-2]) + b"\n", b"\n".join(lines[:-2]) + b"\n" + lines[-2][:200]):
            w.ledger.data = broken
            self.refused("freeze-start", Code.LEDGER_UNAVAILABLE, op_id="op1")
        w.ledger.data = full
        self.run_ok("freeze-start", op_id="op1")

    def test_deleted_burn_with_effects_violates_i_orphan(self):
        self.run_ok("adopt", op_id="op1")
        w = self.env.world
        lines = w.ledger.data.split(b"\n")
        w.ledger.data = b"\n".join(lines[:-2]) + b"\n"
        self.refused("request", Code.LEDGER_UNAVAILABLE, op_id="op1")


class TokenScope(unittest.TestCase):
    def test_tokens_never_leave_custody(self):
        env = Env()
        try:
            _, lease = env.seed_leased()
            harness.to_quarantine(env)
            env.faults.point = "library:after_intent"
            try:
                env.run("quarantine", slot=SLOT, op_id="op1", reason=reason(), **QUARANTINE)
            except Crash:
                pass
            renders = [env.last_render]
            for verb, extra in (("resume-step", {}), ("quiescence", QUIESCENCE), ("snapshot", {}),
                                ("verify-preservation", {}), ("preservation", {}), ("ready", {})):
                r = env.run(verb, slot=SLOT, op_id="op1", reason=reason(), **extra)
                self.assertIs(r.outcome, Outcome.COMMITTED, verb)
                renders.append(env.last_render)
            w = env.world
            key = custody.recovery_key(SLOT, "op1", 1)
            recovery = w.custody.lookup(key).record["token"]
            public = b"".join([w.ledger.data, w.authz.snapshot(), w.tierc.snapshot(), w.fwal.snapshot(),
                               w.gates.snapshot()] + [a.data for a in w.anchors.values()]
                              + [r.encode() for r in renders] + [bytes.fromhex(v) for v in
                                                                  w.library.snapshot_slot(SLOT).values() if v])
            for token in (lease, recovery):
                self.assertNotIn(token.encode(), public)
            self.assertIn(recovery.encode(), w.custody.snapshot())
            r = env.run("request", slot=SLOT, op_id="op1", reason=reason())
            self.assertNotIn(lease, r.detail + repr(r))
        finally:
            env.close()


if __name__ == "__main__":
    unittest.main()

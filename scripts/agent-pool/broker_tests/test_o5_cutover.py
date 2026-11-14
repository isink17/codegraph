"""PR-3a cutover history (contract R2 §4, R3 §4, R4.1 §3.4/§3.7): the head rule
over ledger burns, documents, records, head entries, witnesses and the floor.
Synthetic stores only; the fakes give no rollback resistance (R2 §4.7 F5)."""

import json
import unittest

import harness
from harness import SLOT, Env
from broker import authz, canonical, cutover, freeze, ledger, o5contract as oc
from broker._testsupport import capability, o5
from broker.refusal import Code, Refused


def head(env):
    w = env.world
    entries = w.ledger.entries()
    return freeze.head_of(w, entries, freeze.burned_docs(w, entries))


def drop_ledger_line(env, auth):
    """Removes one burn and re-chains every later line (an actor able to write
    the unkeyed ledger, R3 §4.3): the ledger stays classifiable."""
    w = env.world
    keep = [e for e in w.ledger.entries() if e["auth_digest"] != auth]
    data, prev = b"", ledger.ZERO64
    for i, e in enumerate(keep):
        fields = {k: e[k] for k in ("auth_digest", "auth_kind", "binding_digest", "confirmed_at", "issued_at",
                                    "nonce_digest")}
        ne = ledger.make_entry(ledger.LEDGER, i, prev, fields)
        data += canonical.canon(ne) + b"\n"
        prev = ne["line_digest"]
    w.ledger.data = data


class HeadRule(unittest.TestCase):
    def setUp(self):
        self.env = Env(o5_fixtures=False)
        self.addCleanup(self.env.close)
        self.st = self.env.world.o5.stores

    def test_absent_is_model_a(self):
        self.assertEqual(head(self.env).state, cutover.ABSENT)

    def test_active_with_enumeration(self):
        o5.synthetic_model_b(self.env, [SLOT, "slot-y"])
        h = head(self.env)
        self.assertEqual((h.state, h.seq), (cutover.ACTIVE, 0))
        self.assertEqual([s["slot"] for s in h.enumeration["slots"]], sorted([SLOT, "slot-y"]))

    def test_enumeration_rules(self):
        for slots, reason in (([SLOT, "codegraph-worker-a"], "enumeration"), ([oc.CUTOVER_SUBJECT], "enumeration")):
            env = Env(o5_fixtures=False)
            self.addCleanup(env.close)
            o5.synthetic_model_b(env, slots)
            self.assertEqual((head(env).state, head(env).reason), (cutover.INVALID, reason))
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        enum = o5.enumeration(env, [SLOT], uid=777)
        o5.cutover_step(env, enum=enum)
        self.assertEqual(head(env).reason, "identity")

    def test_document_deletion_attack_is_auth_missing(self):
        """A6 / R1 PR3A-B2: record, head and document deleted; ledger line kept."""
        o5.synthetic_model_b(self.env, [SLOT])
        auth, _ = o5.cutover_step(self.env, "model-b-revoke")
        self.assertEqual(head(self.env).state, cutover.REVOKED)
        del self.st["cutovers"].items["cutover/0000000001"]
        del self.st["cutover_heads"].items["head/0000000001"]
        del self.env.world.authz.docs[auth]
        self.assertEqual((head(self.env).state, head(self.env).reason), (cutover.INVALID, "auth_missing"))

    def test_deletion_matrix_never_restores_an_earlier_trusted_state(self):
        """A6a in protected-style reconciliation (floor present): every subset of
        {document, record, head, floor-entry} deleted after a revoke is never
        ACTIVE; with the floor intact a removed record is rollback."""
        import itertools
        for subset in itertools.chain.from_iterable(itertools.combinations(("doc", "record", "head"), n)
                                                    for n in range(1, 4)):
            env = Env(o5_fixtures=False)
            self.addCleanup(env.close)
            o5.synthetic_model_b(env, [SLOT], floor=True)
            auth, _ = o5.cutover_step(env, "model-b-revoke", floor=True)
            st = env.world.o5.stores
            if "doc" in subset:
                del env.world.authz.docs[auth]
            if "record" in subset:
                del st["cutovers"].items["cutover/0000000001"]
            if "head" in subset:
                del st["cutover_heads"].items["head/0000000001"]
            h = head(env)
            self.assertNotIn(h.state, (cutover.ACTIVE, cutover.ABSENT), subset)
            if subset == ("head",):
                self.assertEqual((h.state, h.reason), (cutover.PENDING, "head"))

    def test_rechained_tail_removal_without_floor_is_the_documented_limit(self):
        """A6c / A62: ledger line, record and head removed (synthetic, no floor):
        ACTIVE. Asserted so the R2 §4.9 limit stays visible."""
        o5.synthetic_model_b(self.env, [SLOT])
        auth, _ = o5.cutover_step(self.env, "model-b-revoke")
        drop_ledger_line(self.env, auth)
        del self.st["cutovers"].items["cutover/0000000001"]
        del self.st["cutover_heads"].items["head/0000000001"]
        self.assertEqual(head(self.env).state, cutover.ACTIVE)

    def test_floor_detects_the_same_removal(self):
        """A6b / A62 protected side: the floor covers the removed record."""
        o5.synthetic_model_b(self.env, [SLOT], floor=True)
        auth, _ = o5.cutover_step(self.env, "model-b-revoke", floor=True)
        drop_ledger_line(self.env, auth)
        del self.st["cutovers"].items["cutover/0000000001"]
        del self.st["cutover_heads"].items["head/0000000001"]
        self.assertEqual((head(self.env).state, head(self.env).reason), (cutover.INVALID, "rollback"))

    def test_witness_binds_a_deleted_record(self):
        """A6d: a burned document binds the newest head; deleting it is rollback."""
        o5.synthetic_model_b(self.env, [SLOT])
        _, rec = o5.cutover_step(self.env, "model-b-rebaseline")
        o5.burn(self.env, {"v": 1, "doc": "freeze-start", "verb": "freeze-start", "slot": SLOT, "op_id": "op9",
                           "slot_generation": 1, "o5": {"cutover_head": rec}})
        drop_ledger_line(self.env, self.env.world.ledger.entries()[1]["auth_digest"])
        del self.st["cutovers"].items["cutover/0000000001"]
        del self.st["cutover_heads"].items["head/0000000001"]
        self.assertEqual((head(self.env).state, head(self.env).reason), (cutover.INVALID, "rollback"))

    def test_record_without_burn_is_auth_binding(self):
        """A7."""
        o5.synthetic_model_b(self.env, [SLOT])
        auth, _ = o5.cutover_step(self.env, "model-b-rebaseline")
        drop_ledger_line(self.env, auth)
        self.assertEqual((head(self.env).state, head(self.env).reason), (cutover.INVALID, "auth_binding"))
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        o5.synthetic_model_b(env, [SLOT])
        auth, _ = o5.cutover_step(env, "model-b-rebaseline")
        data = env.world.o5.stores["cutovers"].items["cutover/0000000001"]
        rec = json.loads(data)
        rec["owner_auth"] = "f" * 64
        env.world.o5.stores["cutovers"].items["cutover/0000000001"] = canonical.canon(rec)
        self.assertEqual((head(env).state, head(env).reason), (cutover.INVALID, "auth_binding"))

    def test_interrupted_cutover_is_pending_and_completes_forward(self):
        """A8 / A8b: burn without record; record without head; head without floor."""
        o5.synthetic_model_b(self.env, [SLOT], floor=True)
        o5.cutover_step(self.env, "model-b-rebaseline", record=False, head=False)
        self.assertEqual((head(self.env).state, head(self.env).reason), (cutover.PENDING, "record"))
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        o5.synthetic_model_b(env, [SLOT], floor=True)
        o5.cutover_step(env, "model-b-rebaseline", head=False)
        self.assertEqual((head(env).state, head(env).reason), (cutover.PENDING, "head"))
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        o5.synthetic_model_b(env, [SLOT], floor=True)
        o5.cutover_step(env, "model-b-rebaseline")
        self.assertEqual((head(env).state, head(env).reason), (cutover.PENDING, "floor"))
        o5.cutover_step(env, "model-b-rebaseline", floor=True)
        self.assertEqual(head(env).state, cutover.ACTIVE)

    def test_forks_and_floor_disagreement(self):
        """A8d: head replaced, floor disagrees, floor missing in protected reconciliation."""
        o5.synthetic_model_b(self.env, [SLOT])
        self.st["cutover_heads"].items["head/0000000000"] = canonical.canon({"schema": oc.S_HEAD, "seq": 0,
                                                                            "record": "e" * 64, "owner_auth": "e" * 64,
                                                                            "mode": oc.SYNTHETIC})
        self.assertEqual(head(self.env).reason, "fork")
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        o5.synthetic_model_b(env, [SLOT])
        env.world.o5.floor = (0, "d" * 64)
        self.assertEqual(head(env).reason, "rollback")
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        o5.synthetic_model_b(env, [SLOT])
        w = env.world
        entries = w.ledger.entries()
        h = cutover.head(entries, freeze.burned_docs(w, entries).get, w.o5.stores, set(), None, oc.PROTECTED)
        self.assertEqual((h.state, h.reason), (cutover.INVALID, "auth_binding"))
        h = cutover.head([], freeze.burned_docs(w, entries).get, w.o5.stores, set(), None, oc.PROTECTED)
        self.assertEqual(h.state, cutover.INVALID)

    def test_synthetic_records_under_protected_reconciliation_are_invalid(self):
        """A19."""
        o5.synthetic_model_b(self.env, [SLOT], floor=True)
        w = self.env.world
        entries = w.ledger.entries()
        h = cutover.head(entries, freeze.burned_docs(w, entries).get, w.o5.stores, set(), w.o5.floor, oc.PROTECTED)
        self.assertEqual(h.state, cutover.INVALID)

    def test_non_cutover_documents_are_not_cutover_burns(self):
        """A8e: a slot verb naming o5-cutover is not identified; a cutover verb with op_id is auth_binding."""
        o5.burn(self.env, {"v": 1, "doc": "freeze-start", "verb": "freeze-start", "slot": oc.CUTOVER_SUBJECT,
                           "slot_generation": 0})
        self.assertEqual(head(self.env).state, cutover.ABSENT)
        o5.burn(self.env, {"v": 1, "doc": "model-b-cutover", "verb": "model-b-cutover", "slot": oc.CUTOVER_SUBJECT,
                           "slot_generation": 0, "op_id": "x"})
        self.assertEqual(head(self.env).state, cutover.ABSENT)

    def test_replayed_burn_never_reaches_the_head_rule(self):
        """A replayed burn of an earlier document: the PR-1 ledger itself refuses
        it (R2 §4.6 last rows); the head rule's fork check is defence in depth."""
        o5.synthetic_model_b(self.env, [SLOT])
        w = self.env.world
        e = w.ledger.entries()[0]
        o5.burn(self.env, json.loads(w.authz.docs[e["auth_digest"]]))
        with self.assertRaises(Refused) as r:
            head(self.env)
        self.assertIs(r.exception.code, Code.LEDGER_UNAVAILABLE)
        entries = [dict(e, binding_digest=e["binding_digest"]), dict(e)]
        h = cutover.head(entries, freeze.burned_docs(w, [e]).get, w.o5.stores, set(), None, oc.SYNTHETIC)
        self.assertEqual((h.state, h.reason), (cutover.INVALID, "fork"))

    def test_rebaseline_and_revoke_then_new_cutover(self):
        """A8c and the F5 §5.2 transitions."""
        o5.synthetic_model_b(self.env, [SLOT])
        o5.cutover_step(self.env, "model-b-rebaseline")
        self.assertEqual(head(self.env).state, cutover.ACTIVE)
        o5.cutover_step(self.env, "model-b-revoke")
        self.assertEqual(head(self.env).state, cutover.REVOKED)
        o5.cutover_step(self.env, "model-b-cutover", slots=[SLOT])
        self.assertEqual((head(self.env).state, head(self.env).seq), (cutover.ACTIVE, 3))

    def test_illegal_transitions(self):
        o5.synthetic_model_b(self.env, [SLOT])
        o5.cutover_step(self.env, "model-b-cutover", slots=[SLOT])
        self.assertEqual((head(self.env).state, head(self.env).reason), (cutover.INVALID, "transition"))
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        o5.synthetic_model_b(env, [SLOT])
        o5.cutover_step(env, "model-b-rebaseline", agent={"writer_uids": [602], "account": "x", "primary_gid": 602,
                                                         "scope": "pool"})
        self.assertEqual(head(env).state, cutover.INVALID)
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        o5.cutover_step(env, slots=[SLOT], agent={"writer_uids": [501], "account": "ivan", "primary_gid": 20,
                                                  "scope": "pool"})
        self.assertEqual((head(env).state, head(env).reason), (cutover.INVALID, "identity"))


class CutoverAllowed(unittest.TestCase):
    """A35, A56, A61 and R4.1 §3.7 (rebaseline / revoke eligibility)."""

    def setUp(self):
        self.env = Env(o5_fixtures=False)
        self.addCleanup(self.env.close)
        o5.synthetic_model_b(self.env, [SLOT])
        self.h = head(self.env)

    def refuse(self, kind, chains, cycles, seq=1):
        with self.assertRaises(Refused) as e:
            cutover.cutover_allowed(kind, self.h, chains, cycles, seq)
        return e.exception.code, e.exception.detail

    def test_closed_cycles_never_block(self):
        for kind in ("rebaseline", "revoked"):
            cutover.cutover_allowed(kind, self.h, ["RELEASED"], ["none_no_move", "thaw_complete", "ready"], 1)

    def test_open_and_ready_pending_refuse(self):
        for kind in ("rebaseline", "revoked"):
            for c in ("open", "ready_pending"):
                self.assertEqual(self.refuse(kind, ["RELEASED"], [c]),
                                 (Code.VERB_NOT_ALLOWED_IN_STATE, "cycle_open"))

    def test_state_unknown_refuses_with_its_class(self):
        for c in ("STATE_UNKNOWN(marker)", "STATE_UNKNOWN(fwal)"):
            self.assertEqual(self.refuse("rebaseline", [], [c]), (Code.STATE_UNKNOWN, c))

    def test_live_chain_refuses(self):
        for st in ("ESTABLISHED", "INVALID", "RELEASING", "FAILED"):
            self.assertEqual(self.refuse("rebaseline", [st], []), (Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use"))

    def test_cutover_over_active_and_sequence_limits(self):
        self.assertEqual(self.refuse("cutover", [], [])[1], "cutover")
        self.assertEqual(self.refuse("rebaseline", [], [], seq=1022)[1], "sequence")
        cutover.cutover_allowed("revoked", self.h, [], [], oc.REVOKE_SEQ)

    def test_pending_or_invalid_head_refuses_everything(self):
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        o5.synthetic_model_b(env, [SLOT])
        o5.cutover_step(env, "model-b-rebaseline", record=False, head=False)
        h = head(env)
        for kind in ("cutover", "rebaseline", "revoked"):
            with self.assertRaises(Refused) as e:
                cutover.cutover_allowed(kind, h, [], [], 2)
            self.assertIs(e.exception.code, Code.STATE_UNKNOWN)


class ModelBGate(unittest.TestCase):
    """G7a (R3 §3.3) and A54: root-controlled drift refuses; a revert restores."""

    def test_drift_refuses_and_revert_restores(self):
        env = Env()
        self.addCleanup(env.close)
        env.seed_leased()
        for v in ("adopt", "request"):
            env.run(v, slot=SLOT, op_id="op1", reason=harness.reason())
        o5.observe(env, SLOT, overrides={"agent_identity_root": {"writer_uid": o5.AGENT_UID,
                                                                 "primary_gid": o5.AGENT_UID, "groups": ["admin"]}})
        snap = env.snapshot()
        raw = json.dumps({"v": 1, "verb": "freeze-start", "slot": SLOT, "op_id": "op1",
                          "reason": harness.reason()}).encode()
        r = harness.invocation.invoke(env.world, raw, env.confirmer)
        self.assertEqual((r.code, r.detail), (Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(baseline_drift)"))
        self.assertEqual(env.snapshot(), snap)
        r = env.run("freeze-start", slot=SLOT, op_id="op1", reason=harness.reason())
        self.assertEqual(r.outcome.value, "COMMITTED")


class Trust(unittest.TestCase):
    def test_mode_is_derived_from_trust(self):
        env = Env(store_kind=authz.PROTECTED, principal=capability.owner_capability(), o5_fixtures=False)
        self.addCleanup(env.close)
        self.assertEqual(oc.mode(env.world), oc.PROTECTED)
        env = Env(o5_fixtures=False)
        self.addCleanup(env.close)
        self.assertEqual(oc.mode(env.world), oc.SYNTHETIC)


if __name__ == "__main__":
    unittest.main()

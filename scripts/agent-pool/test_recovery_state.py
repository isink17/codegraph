#!/usr/bin/env python3
"""Tests for recovery_state.py. Every store and every slot record lives in a
fresh temporary directory; the live pool and its lease files are never read
or written.

    python3 -m unittest test_recovery_state      (from scripts/agent-pool)
"""

import errno
import hashlib
import itertools
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import recovery_state as rs  # noqa: E402

SLOT = "codegraph-worker-a"
OTHER = "codegraph-worker-b"
LEASE_ID = "1" * 32
TOKEN = "a" * 32
RTOKEN = "b" * 32
OP = "op-1"
HEAD = "c" * 40
LIVE_POOL = (Path.home() / "Projekti/codegraph-evidence/pool").resolve()
HOLDER = {"task": "t", "agent": "a", "pr": "", "owner": "coordinator:x", "host": "h", "pid": "",
          "utc": "2026-11-07T00:00:00Z"}
# The store seq each transition leaves when it is the legal next step.
SEQ = {"request": 0, "cancel": 1, "quarantine": 1, "quiescence": 2, "preservation": 3, "ready": 4}


def text(fields):
    return "".join(f"{k}={v}\n" for k, v in fields.items() if v is not None)


def v1(**over):
    return text({"version": "1", "lease_id": LEASE_ID, "slot": SLOT, **HOLDER, "token": TOKEN, **over})


def v2(state="leased", generation=1, **over):
    head = {"version": "2", "slot": SLOT, "generation": str(generation), "state": state}
    body = {"leased": {"lease_id": LEASE_ID, **HOLDER, "lease_token": TOKEN},
            "quarantined": {"lease_id": LEASE_ID, "recovery_op": OP, **HOLDER}, "free": {}}.get(state, {})
    return text({**head, **body, **over})


def legacy(**over):
    return text({"slot": SLOT, **HOLDER, "token": TOKEN, **over})


def cas_hook(live, fail=None):
    """Replaces the synthetic slot record with `post` only while it still
    equals `pre`, as the pool's own writer will. `fail` injects a failure or a
    hard crash before or after the replace."""
    def hook(pre, post):
        if fail == "crash-before":
            os._exit(70)
        if fail == "raise-before":
            raise RuntimeError("replace failed")
        if (live.read_text() if live.exists() else None) != pre:
            raise RuntimeError("live record changed")
        tmp = live.with_name(live.name + ".tmp")
        tmp.write_text(post)
        os.replace(tmp, live)
        if fail == "crash-after":
            os._exit(70)
        if fail == "raise-after":
            raise OSError(errno.ENOSPC, "No space left on device")
        return post
    return hook


class Store(unittest.TestCase):
    KIND = "v1"

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp()).resolve()
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.assertFalse(str(self.tmp).startswith(str(LIVE_POOL)))
        self.root = self.tmp / "recovery"
        self.live = self.tmp / "pool" / f"{SLOT}.lease"
        self.live.parent.mkdir()
        self.s = rs.RecoveryStore(self.root)
        self.set_lease(self.KIND)

    def set_lease(self, kind, generation=3, lease_id=LEASE_ID):
        self.kind = kind
        self.lease_text = {"v1": lambda: v1(lease_id=lease_id), "legacy": legacy,
                           "v2": lambda: v2(generation=generation, lease_id=lease_id)}[kind]()
        self.live.write_text(self.lease_text)
        parsed = rs.parse_record(self.lease_text, SLOT, legacy=kind == "legacy")
        self.identity, self.gen0 = parsed.identity, parsed.generation

    def record(self):
        return self.live.read_text() if self.live.exists() else None

    def hook(self, fail=None):
        return cas_hook(self.live, fail)

    def files(self):
        out = {p.relative_to(self.tmp).as_posix(): p.read_bytes()
               for p in sorted(self.tmp.rglob("*")) if p.is_file()}
        return out

    def assertRefusedUnchanged(self, call, exc=rs.Refused):
        before = self.files()
        with self.assertRaises(exc) as cm:
            call()
        self.assertEqual(self.files(), before, "a refused transition changed the store or the slot record")
        self.assertFalse((self.root / f".{SLOT}.lock").exists(), "a refused transition left its lock")
        return cm.exception

    def adopt(self, op=OP, store=None):
        return (store or self.s).adopt(SLOT, op, self.lease_text, legacy=self.kind == "legacy")

    def binding(self, transition, op=OP, **over):
        gen = self.gen0 + (transition in ("quiescence", "preservation", "ready"))
        b = {"slot": SLOT, "op_id": op, "transition": transition, "identity_kind": self.kind,
             "identity": self.identity, "slot_generation": gen, "seq": SEQ[transition]}
        b.update(over)
        return b

    def auth(self, step, op=OP, **over):
        a = {**self.binding(step, op), "actor": "owner", "reason": "orphaned",
             "utc": "2026-11-08T00:00:00Z", "trust": rs.TRUST}
        if step == "quarantine":
            a.update(assessment_digest="d" * 64, quiescence_method="owner-quiescence",
                     preservation_disposition="archive")
        a.update(over)
        return {k: v for k, v in a.items() if v is not None}

    def quiescence(self, op=OP, **over):
        q = {**self.binding("quiescence", op), "method": "owner-quiescence", "actor": "owner",
             "evidence_refs": ["evidence/reboot.md"], "accounted": {c: True for c in rs.QUIESCENCE_ACCOUNTED}}
        q.update(over)
        return {k: v for k, v in q.items() if v is not None}

    def preservation(self, op=OP, inventory=None, preserved=None, **over):
        p = {**self.binding("preservation", op), "head": HEAD, "complete": True,
             "inventory": inventory or {k: [] for k in rs.INVENTORY}, "preserved": preserved or {}}
        p.update(over)
        return p

    def final(self, op=OP, **over):
        f = {**self.binding("ready", op), "head": HEAD, "clean": True, "lease_absent": True,
             "preservation": rs.digest(self.preservation(op))}
        f.update(over)
        return f

    def attempts(self, token=RTOKEN, op=OP, store=None):
        """Every transition call, with otherwise valid evidence and the live record."""
        s = store or self.s
        return {
            "request": lambda: s.request(SLOT, op_id=op, authorization=self.auth("request", op), record=self.record()),
            "cancel": lambda: s.cancel(SLOT, op_id=op, authorization=self.auth("cancel", op), record=self.record()),
            "quarantine": lambda: s.quarantine(SLOT, op_id=op, authorization=self.auth("quarantine", op),
                                               recovery_token=RTOKEN, record=self.record(),
                                               replace_record=self.hook()),
            "quiescence": lambda: s.verify_quiescence(SLOT, op_id=op, recovery_token=token,
                                                      evidence=self.quiescence(op), record=self.record()),
            "preservation": lambda: s.record_preservation(SLOT, op_id=op, recovery_token=token,
                                                          evidence=self.preservation(op), record=self.record()),
            "ready": lambda: s.mark_ready(SLOT, op_id=op, recovery_token=token, final_check=self.final(op),
                                          record=self.record()),
        }

    PATH = {rs.ACTIVE: [], rs.REQUESTED: ["request"], rs.CANCELLED: ["request", "cancel"],
            rs.QUARANTINED: ["request", "quarantine"], rs.QUIESCENT: ["request", "quarantine", "quiescence"],
            rs.PRESERVED: ["request", "quarantine", "quiescence", "preservation"],
            rs.READY: ["request", "quarantine", "quiescence", "preservation", "ready"]}

    def to(self, state, op=OP):
        self.adopt(op)
        calls = self.attempts(op=op)
        for name in self.PATH[state]:
            calls[name]()
        self.assertEqual(self.s.state(SLOT, op)[0], state)

    def quarantined(self, op=OP):
        pre = rs.parse_record(self.lease_text, SLOT, legacy=self.kind == "legacy")
        return rs.quarantined_record(SLOT, op, self.gen0 + 1, pre)


class Transitions(Store):
    def test_only_path_reaches_ready(self):
        self.to(rs.READY)
        self.assertTrue(self.s.ready_for_reassignment(SLOT, OP))
        self.assertEqual(self.s.state(SLOT, OP), (rs.READY, self.gen0 + 1, False))
        self.assertEqual(self.s.terminal(SLOT, OP), rs.READY)
        self.assertEqual(self.record(), self.quarantined(), "READY must keep the slot record quarantined")

    def test_every_forbidden_transition_is_refused_unchanged(self):
        legal = {rs.ACTIVE: {"request"}, rs.REQUESTED: {"quarantine", "cancel"}, rs.QUARANTINED: {"quiescence"},
                 rs.QUIESCENT: {"preservation"}, rs.PRESERVED: {"ready"}, rs.READY: set(), rs.CANCELLED: set()}
        for state in rs.STATES:
            with self.subTest(state=state):
                self.setUp()
                self.to(state)
                done = self.PATH[state][-1:]
                for name, call in self.attempts().items():
                    if name in legal[state] or name in done:
                        continue  # the allowed steps, or an exact repeat (see Idempotency)
                    self.assertRefusedUnchanged(call)
                self.assertEqual(self.s.ready_for_reassignment(SLOT, OP), state == rs.READY)

    def test_an_adopted_operation_can_be_cancelled_once_its_lease_is_gone(self):
        """An ACTIVE operation whose lease was released has an exit, so it
        never blocks later recoveries of the slot."""
        self.adopt()
        if self.kind == "v2":
            self.live.write_text(v2("free", self.gen0))
        else:
            self.live.unlink()
        self.assertRefusedUnchanged(self.attempts()["request"])
        self.assertRefusedUnchanged(lambda: self.adopt("op-2"))
        self.assertRefusedUnchanged(lambda: self.s.cancel(SLOT, op_id=OP, authorization=self.auth("cancel"),
                                                          record=self.record()))  # bound to the wrong seq
        self.s.cancel(SLOT, op_id=OP, authorization=self.auth("cancel", seq=0), record=self.record())
        self.assertEqual(self.s.terminal(SLOT, OP), rs.CANCELLED)
        self.assertRefusedUnchanged(self.attempts()["request"])
        self.set_lease(self.kind, generation=self.gen0 + 1, lease_id="2" * 32)
        self.adopt("op-2")
        self.assertEqual(self.s.state(SLOT, "op-2")[0], rs.ACTIVE)

    def test_stale_holder_cannot_act_after_request(self):
        self.adopt()
        self.assertTrue(self.s.lease_action_allowed(SLOT, OP, self.identity, self.gen0, TOKEN))
        self.assertFalse(self.s.lease_action_allowed(SLOT, OP, self.identity, self.gen0, "f" * 32))
        self.assertFalse(self.s.lease_action_allowed(SLOT, OP, self.identity, self.gen0 + 1, TOKEN))
        self.assertFalse(self.s.lease_action_allowed(SLOT, OP, self.identity, float(self.gen0), TOKEN))
        self.assertFalse(self.s.lease_action_allowed(SLOT, OP, self.identity, bool(self.gen0), TOKEN))
        self.attempts()["request"]()
        self.assertFalse(self.s.lease_action_allowed(SLOT, OP, self.identity, self.gen0, TOKEN))
        self.attempts()["quarantine"]()
        for gen in (self.gen0, self.gen0 + 1):
            self.assertFalse(self.s.lease_action_allowed(SLOT, OP, self.identity, gen, TOKEN))
        self.assertRefusedUnchanged(self.attempts(token=TOKEN)["quiescence"])


class LegacyPath(Transitions):
    KIND = "legacy"

    def test_legacy_recovery_continues_after_quarantine(self):
        """T14: the legacy byte digest is never compared after the record is
        replaced; every later step and the finish-only retry use the
        quarantined record."""
        self.to(rs.REQUESTED)
        with self.assertRaises(rs.Refused):
            self.s.quarantine(SLOT, op_id=OP, authorization=self.auth("quarantine"), recovery_token=RTOKEN,
                              record=self.record(), replace_record=self.hook("raise-after"))
        self.assertEqual(self.record(), self.quarantined())
        self.assertNotEqual(rs.image_digest(self.record()), self.identity)
        self.assertEqual(self.s.reconcile(SLOT, OP, self.record()), "post")
        self.attempts()["quarantine"]()
        for name in ("quiescence", "preservation", "ready"):
            self.attempts()[name]()
        self.assertTrue(self.s.ready_for_reassignment(SLOT, OP))
        self.assertIn(f"legacy_digest={self.identity}\n", self.record())

    def test_a_reacquired_legacy_lease_with_the_same_other_bytes_is_not_the_adopted_one(self):
        self.to(rs.REQUESTED)
        self.live.write_text(legacy(token="e" * 32))
        self.assertEqual(rs.parse_record(self.record(), SLOT, legacy=True).identity, self.identity)
        self.assertRefusedUnchanged(self.attempts()["quarantine"])
        self.assertRefusedUnchanged(self.attempts()["cancel"])
        self.setUp()
        self.adopt()
        self.live.write_text(legacy(token="e" * 32))
        self.assertRefusedUnchanged(self.attempts()["request"])


class V2Path(Transitions):
    KIND = "v2"

    def test_slot_generation_comes_from_the_record(self):
        self.to(rs.QUARANTINED)
        self.assertEqual(self.s.state(SLOT, OP), (rs.QUARANTINED, 4, False))
        self.assertIn("generation=4\n", self.record())
        self.assertNotIn("lease_token", self.record())


class Records(unittest.TestCase):
    def test_valid_formats(self):
        r = rs.parse_record(v1(), SLOT)
        self.assertEqual((r.kind, r.state, r.generation, r.identity, r.token), ("v1", "leased", 0, LEASE_ID, TOKEN))
        r = rs.parse_record(v2(generation=5), SLOT)
        self.assertEqual((r.kind, r.state, r.generation, r.identity, r.token), ("v2", "leased", 5, LEASE_ID, TOKEN))
        r = rs.parse_record(v2("free", 0), SLOT)
        self.assertEqual((r.state, r.generation, r.identity, r.token), ("free", 0, None, None))
        r = rs.parse_record(v2("quarantined", 2), SLOT)
        self.assertEqual((r.state, r.recovery_op, r.identity, r.legacy), ("quarantined", OP, LEASE_ID, False))
        r = rs.parse_record(legacy(), SLOT, legacy=True)
        self.assertEqual((r.kind, r.generation, r.token, r.legacy), ("legacy", 0, TOKEN, True))
        self.assertEqual(r.identity, hashlib.sha256(legacy(token=None).encode()).hexdigest())

    def test_malformed_ambiguous_and_unsupported_records_are_refused(self):
        quarantined_legacy = v2("quarantined", 2, lease_id=None, legacy_digest="e" * 64)
        self.assertTrue(rs.parse_record(quarantined_legacy, SLOT).legacy)
        bad = {
            "v2 with token=": v2(token=TOKEN), "v2 free with token=": v2("free", 1, token=TOKEN),
            "v1 with lease_token": v1(lease_token=TOKEN), "v1 with state": v1(state="leased"),
            "v1 with generation": v1(generation="1"), "version 3": v1(version="3"), "empty version": v1(version=""),
            "v2 unknown state": v2("reserved"), "v2 no state": v2(state=None),
            "v2 free with lease_id": v2("free", 1, lease_id=LEASE_ID), "v2 free with owner": v2("free", 1, owner="x"),
            "v2 leased no lease_token": v2(lease_token=None), "v2 leased no lease_id": v2(lease_id=None),
            "v2 leased no task": v2(task=""), "v2 leased generation 0": v2(generation=0),
            "v2 generation 01": v2(generation="01"), "v2 generation -1": v2(generation="-1"),
            "v2 generation x": v2(generation="x"), "v2 no generation": v2(generation=None),
            "v2 leased with recovery_op": v2(recovery_op=OP),
            "quarantined with lease_token": v2("quarantined", 2, lease_token=TOKEN),
            "quarantined with both identities": v2("quarantined", 2, legacy_digest="e" * 64),
            "quarantined with no identity": v2("quarantined", 2, lease_id=None),
            "quarantined bad op": v2("quarantined", 2, recovery_op="../x"),
            "quarantined short digest": v2("quarantined", 2, lease_id=None, legacy_digest="e" * 32),
            "repeated field": v1() + "task=again\n", "control character": v1(task="t\r"),
            "line without =": v1() + "garbage\n", "empty line": v1() + "\n", "empty key": v1() + "=x\n",
            "uppercase key": v1() + "Owner=x\n", "unknown field": v1(color="blue"),
            "slot mismatch": v1(slot=OTHER), "bad lease_id": v1(lease_id="xyz"), "short token": v1(token="ab"),
            "legacy without the owner flag": legacy(), "legacy with lease_id": legacy(lease_id=LEASE_ID),
            "empty": "", "none": None, "bytes": v1().encode(),
        }
        for name, record in bad.items():
            with self.subTest(case=name):
                with self.assertRaises(rs.Refused) as cm:
                    rs.parse_record(record, SLOT, legacy=name.startswith("legacy with "))
                self.assertNotIn(TOKEN, str(cm.exception), "a diagnostic quoted a token")

    def test_legacy_identity_is_token_free_and_bound_to_the_bytes(self):
        a = rs.parse_record(legacy(), SLOT, legacy=True)
        self.assertEqual(rs.parse_record(legacy(token="d" * 32), SLOT, legacy=True).identity, a.identity)
        self.assertNotEqual(rs.parse_record(legacy(utc="2026-11-08T00:00:00Z"), SLOT, legacy=True).identity,
                            a.identity)
        self.assertNotEqual(a.identity, hashlib.sha256(legacy().encode()).hexdigest())


class Adoption(Store):
    def test_adoption_sources(self):
        """T15: v1, v2 leased and owner-flagged legacy leases are adoptable;
        v2 free and quarantined records never are."""
        for op, (record, flag) in enumerate([(v1(), False), (v2(generation=9), False), (legacy(), True)]):
            with self.subTest(record=record):
                self.setUp()
                self.s.adopt(SLOT, f"op-{op}", record, legacy=flag)
                self.assertEqual(self.s.state(SLOT, f"op-{op}")[0], rs.ACTIVE)
        for record, flag in [(v2("free", 3), False), (v2("quarantined", 3), False), (legacy(), False),
                             (v1(), True), (v2(), True), (v2("free", 3), True), (v1(), "yes")]:
            with self.subTest(record=record, legacy=flag):
                self.setUp()
                with self.assertRaises(rs.Refused):
                    self.s.adopt(SLOT, OP, record, legacy=flag)
                self.assertFalse(self.root.exists() and any(self.root.rglob("state.json")))

    def test_operation_cannot_be_adopted_twice_or_resumed(self):
        self.to(rs.CANCELLED)
        self.assertRefusedUnchanged(lambda: self.adopt(OP))
        self.adopt("op-2")
        self.assertEqual(self.s.state(SLOT, "op-2")[0], rs.ACTIVE)

    def test_adoption_waits_for_every_earlier_operation(self):
        self.to(rs.REQUESTED)
        self.assertRefusedUnchanged(lambda: self.adopt("op-2"))
        (self.root / SLOT / OP / "TERMINAL").write_bytes(b"state=CANCELLED\nseq=2\nop_id=op-1\n")
        os.chmod(self.root / SLOT / OP / "TERMINAL", 0o600)
        self.assertRefusedUnchanged(lambda: self.adopt("op-2"))  # a forged marker never finishes an operation
        (self.root / SLOT / OP / "TERMINAL").unlink()
        self.attempts()["cancel"]()
        self.adopt("op-2")
        (self.root / SLOT / ".adopt-op-5.0123456789abcdef").mkdir(mode=0o700)  # never published: ignored
        self.attempts(op="op-2")["request"]()
        self.assertRefusedUnchanged(lambda: self.adopt("op-4"))
        (self.root / SLOT / "op-9").mkdir(mode=0o700)  # a stray directory is never a finished operation
        self.assertRefusedUnchanged(lambda: self.adopt("op-4"))

    def test_a_corrupt_earlier_operation_stays_corrupt(self):
        self.to(rs.REQUESTED)
        (self.root / SLOT / OP / "state.json").write_text("{")
        self.assertRefusedUnchanged(lambda: self.adopt("op-2"), rs.Corrupt)

    def test_invalid_names(self):
        for slot, op in (("../x", OP), (SLOT, "../x"), (SLOT, ".hidden"), ("a/b", OP), (SLOT, "op\n"), (SLOT, ""),
                         (SLOT, None)):
            with self.subTest(slot=slot, op=op):
                with self.assertRaises(rs.Refused):
                    self.s.adopt(slot, op, v1(slot=slot) if isinstance(slot, str) and "\n" not in slot else v1())
                self.assertFalse(self.s.ready_for_reassignment(slot, op))


class Authorization(Store):
    def test_request_and_cancel_need_a_bound_authorization(self):
        for transition in ("request", "cancel"):
            fields = ("slot", "op_id", "transition", "identity_kind", "identity", "slot_generation", "seq")
            wrong = {"slot": OTHER, "op_id": "op-2", "transition": "quarantine", "identity_kind": "v2",
                     "identity": "2" * 32, "slot_generation": self.gen0 + 1, "seq": SEQ[transition] + 1}
            bad = [self.auth(transition, **{f: wrong[f]}) for f in fields]
            bad += [self.auth(transition, **{f: None}) for f in fields + rs.AUTHORIZATION_FIELDS + ("trust",)]
            bad += [self.auth(transition, trust="signed"), self.auth(transition, trust="tty-0600-file"),
                    self.auth(transition, seq=str(SEQ[transition])), self.auth(transition, slot_generation=False),
                    self.auth(transition, lease_token=TOKEN), None, "yes", []]
            with self.subTest(transition=transition):
                self.setUp()
                self.to(rs.ACTIVE if transition == "request" else rs.REQUESTED)
                call = {"request": self.s.request, "cancel": self.s.cancel}[transition]
                for a in bad:
                    with self.subTest(authorization=a):
                        self.assertRefusedUnchanged(lambda: call(SLOT, op_id=OP, authorization=a,
                                                                 record=self.record()))
                call(SLOT, op_id=OP, authorization=self.auth(transition), record=self.record())

    def test_quarantine_authorization_must_be_complete_and_bound(self):
        self.to(rs.REQUESTED)
        bad = [self.auth("quarantine", **{f: None}) for f in rs.QUARANTINE_FIELDS]
        bad += [self.auth("quarantine", quiescence_method="pid-absent"), self.auth("quarantine", seq=0),
                self.auth("quarantine", transition="cancel"), self.auth("quarantine", slot_generation=self.gen0 + 1)]
        for a in bad:
            with self.subTest(authorization=a):
                self.assertRefusedUnchanged(lambda: self.s.quarantine(
                    SLOT, op_id=OP, authorization=a, recovery_token=RTOKEN, record=self.record(),
                    replace_record=self.hook()))
        for token in ("short", TOKEN, None):
            self.assertRefusedUnchanged(lambda: self.s.quarantine(
                SLOT, op_id=OP, authorization=self.auth("quarantine"), recovery_token=token, record=self.record(),
                replace_record=self.hook()))
        self.assertRefusedUnchanged(lambda: self.s.quarantine(
            SLOT, op_id=OP, authorization=self.auth("quarantine"), recovery_token=RTOKEN, record=self.record(),
            replace_record=None))

    def test_authorization_replay_across_operations_slots_and_generations(self):
        """T13."""
        self.to(rs.CANCELLED)
        old = {t: self.auth(t) for t in ("request", "cancel", "quarantine")}
        self.adopt("op-2")
        self.assertRefusedUnchanged(lambda: self.s.request(SLOT, op_id="op-2", authorization=old["request"],
                                                           record=self.record()))
        self.s.request(SLOT, op_id="op-2", authorization=self.auth("request", "op-2"), record=self.record())
        self.assertRefusedUnchanged(lambda: self.s.cancel(SLOT, op_id="op-2", authorization=old["cancel"],
                                                          record=self.record()))
        self.assertRefusedUnchanged(lambda: self.s.quarantine(
            SLOT, op_id="op-2", authorization=old["quarantine"], recovery_token=RTOKEN, record=self.record(),
            replace_record=self.hook()))
        self.assertRefusedUnchanged(lambda: self.s.cancel(
            SLOT, op_id="op-2", authorization=self.auth("request", "op-2", seq=1), record=self.record()))
        self.assertRefusedUnchanged(lambda: self.s.cancel(
            SLOT, op_id="op-2", authorization=self.auth("cancel", "op-2", slot=OTHER), record=self.record()))

    def test_a_reacquired_lease_needs_a_fresh_authorization(self):
        self.set_lease("v2", generation=3)
        self.to(rs.CANCELLED)
        stale = self.auth("request")
        self.set_lease("v2", generation=5, lease_id="2" * 32)
        self.adopt("op-2")
        self.assertRefusedUnchanged(lambda: self.s.request(
            SLOT, op_id="op-2", authorization=dict(stale, op_id="op-2"), record=self.record()))
        self.s.request(SLOT, op_id="op-2", authorization=self.auth("request", "op-2"), record=self.record())


class LiveRecord(Store):
    def test_request_needs_the_adopted_lease(self):
        self.adopt()
        for record in (None, v1(lease_id="2" * 32), v2(generation=1), v2("quarantined", 1), "garbage"):
            with self.subTest(record=record):
                self.assertRefusedUnchanged(lambda: self.s.request(
                    SLOT, op_id=OP, authorization=self.auth("request"), record=record))

    def test_v2_request_needs_the_adopted_generation(self):
        self.set_lease("v2", generation=3)
        self.adopt()
        for record in (v2(generation=2), v2(generation=4), v2("free", 3)):
            self.assertRefusedUnchanged(lambda: self.s.request(
                SLOT, op_id=OP, authorization=self.auth("request"), record=record))

    def cancel_with(self, kind, record):
        self.setUp()
        self.set_lease(kind, generation=3)
        self.to(rs.REQUESTED)
        before = self.live.read_bytes()
        call = lambda: self.s.cancel(SLOT, op_id=OP, authorization=self.auth("cancel"), record=record)  # noqa: E731
        return call, before

    def test_cancel_needs_the_lease_unchanged_or_a_proven_release(self):
        allowed = [("v1", None), ("legacy", None), ("v1", v2("free", 0)), ("v2", v2("free", 3)), ("v2", v2("free", 6)),
                   ("v1", v1(lease_id="2" * 32)), ("v1", v2(generation=1, lease_id="2" * 32)),
                   ("v2", v2(generation=4, lease_id="2" * 32)), ("legacy", v1(lease_id="2" * 32)),
                   ("v1", "same"), ("v2", "same"), ("legacy", "same")]
        refused = [("v2", None), ("v2", v2("free", 2)), ("v2", v2(generation=3, lease_id="2" * 32)),
                   ("v2", v1(lease_id="2" * 32)), ("v2", v2(generation=4)), ("v1", v2("quarantined", 1)),
                   ("v2", v2("quarantined", 4)), ("legacy", legacy(task="other")), ("v1", legacy()),
                   ("v1", "garbage"), ("v1", v1(slot=OTHER))]
        for kind, record in allowed:
            with self.subTest(kind=kind, record=record, expect="cancelled"):
                call, before = self.cancel_with(kind, None)
                record = self.lease_text if record == "same" else record
                self.s.cancel(SLOT, op_id=OP, authorization=self.auth("cancel"), record=record)
                self.assertEqual(self.s.terminal(SLOT, OP), rs.CANCELLED)
                self.assertEqual(self.live.read_bytes(), before, "cancel wrote the slot record")
        for kind, record in refused:
            with self.subTest(kind=kind, record=record, expect="refused"):
                call, _ = self.cancel_with(kind, record)
                self.assertRefusedUnchanged(call)

    def test_later_steps_need_this_operations_quarantined_record(self):
        self.to(rs.QUARANTINED)
        good = self.record()
        for record in (self.lease_text, None, good.replace(f"recovery_op={OP}", "recovery_op=op-2"),
                       good.replace(f"generation={self.gen0 + 1}", f"generation={self.gen0 + 2}"),
                       good.replace(LEASE_ID, "2" * 32), v2("free", self.gen0 + 1)):
            with self.subTest(record=record):
                self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
                    SLOT, op_id=OP, recovery_token=RTOKEN, evidence=self.quiescence(), record=record))
        self.s.verify_quiescence(SLOT, op_id=OP, recovery_token=RTOKEN, evidence=self.quiescence(), record=good)


class Quiescence(Store):
    def test_missing_or_partial_quiescence_proof_is_refused(self):
        self.to(rs.QUARANTINED)
        bad = [self.quiescence(method=None), self.quiescence(method="pid-absent"),
               self.quiescence(method="heartbeat-expired"), self.quiescence(method="no-process-cwd"),
               self.quiescence(method="token-rotated"), self.quiescence(actor=None), self.quiescence(evidence_refs=[]),
               self.quiescence(evidence_refs="x"), self.quiescence(accounted=None),
               self.quiescence(slot_generation=self.gen0), self.quiescence(op_id="op-2"),
               self.quiescence(slot=OTHER), self.quiescence(seq=1), self.quiescence(transition="preservation"),
               self.quiescence(method="reboot-runbook"), self.quiescence(method="containment-teardown")]
        for c in rs.QUIESCENCE_ACCOUNTED:
            bad.append(self.quiescence(accounted={k: k != c for k in rs.QUIESCENCE_ACCOUNTED}))
            bad.append(self.quiescence(accounted={k: "yes" for k in rs.QUIESCENCE_ACCOUNTED}))
        for q in bad:
            with self.subTest(evidence=q):
                self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
                    SLOT, op_id=OP, recovery_token=RTOKEN, evidence=q, record=self.record()))

    def test_no_sequence_without_quiescence_reaches_ready(self):
        names = ["request", "cancel", "quarantine", "preservation", "ready"]
        for order in itertools.permutations(names):
            with self.subTest(order=order):
                self.setUp()
                self.to(rs.QUARANTINED)
                calls = self.attempts()
                for name in order:
                    if name == "quarantine":
                        calls[name]()  # an exact repeat of the completed step: no change
                        continue
                    with self.assertRaises(rs.Refused):
                        calls[name]()
                self.assertFalse(self.s.ready_for_reassignment(SLOT, OP))
                self.assertEqual(self.s.state(SLOT, OP)[0], rs.QUARANTINED)

    def test_quiescence_method_must_be_the_authorized_one(self):
        self.to(rs.REQUESTED)
        self.s.quarantine(SLOT, op_id=OP, recovery_token=RTOKEN, record=self.record(), replace_record=self.hook(),
                          authorization=self.auth("quarantine", quiescence_method="containment-teardown"))
        self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
            SLOT, op_id=OP, recovery_token=RTOKEN, evidence=self.quiescence(method="owner-quiescence"),
            record=self.record()))
        self.s.verify_quiescence(SLOT, op_id=OP, recovery_token=RTOKEN, record=self.record(),
                                 evidence=self.quiescence(method="containment-teardown"))


class Preservation(Store):
    def test_unpreserved_work_is_refused(self):
        self.to(rs.QUIESCENT)
        inv = {"dirty": ["src/a.go"], "untracked": ["notes.txt"], "ignored": [".env.local"],
               "unpublished_commits": ["d" * 40]}
        cases = [
            self.preservation(inventory=inv),
            self.preservation(inventory=inv, preserved={"src/a.go": {"to": "x", "verified": True}}),
            self.preservation(inventory=inv, preserved={i: {"to": "x", "verified": False} for k in inv for i in inv[k]}),
            self.preservation(inventory=inv, preserved={i: {"verified": True} for k in inv for i in inv[k]}),
            self.preservation(inventory={"dirty": []}), self.preservation(head="HEAD"),
            self.preservation(complete=False),
            self.preservation(inventory={**{k: [] for k in rs.INVENTORY}, "dirty": [["x"]]}),
        ]
        for p in cases:
            with self.subTest(evidence=p):
                self.assertRefusedUnchanged(lambda: self.s.record_preservation(
                    SLOT, op_id=OP, recovery_token=RTOKEN, evidence=p, record=self.record()))
        done = {i: {"to": f"archive/{i}", "verified": True} for k in inv for i in inv[k]}
        self.s.record_preservation(SLOT, op_id=OP, recovery_token=RTOKEN, record=self.record(),
                                   evidence=self.preservation(inventory=inv, preserved=done))
        self.assertEqual(self.s.state(SLOT, OP)[0], rs.PRESERVED)

    def test_final_check_must_show_clean_unleased_preserved_slot(self):
        self.to(rs.PRESERVED)
        for f in (self.final(clean=False), self.final(lease_absent=False), self.final(head=None),
                  self.final(head="d" * 40), self.final(preservation="0" * 64), self.final(seq=3)):
            with self.subTest(final=f):
                self.assertRefusedUnchanged(lambda: self.s.mark_ready(
                    SLOT, op_id=OP, recovery_token=RTOKEN, final_check=f, record=self.record()))
        self.assertFalse(self.s.ready_for_reassignment(SLOT, OP))


class UnfinishedQuarantine(Store):
    """B1: a trailing intent of the quarantine is unfinished whether or not
    the state was replaced, and is reconciled against the live record."""

    def failing(self, point):
        def fault(p):
            if p == point:
                raise OSError(errno.ENOSPC, "No space left on device")
        return rs.RecoveryStore(self.root, fault=fault)

    def assertBlocked(self):
        """Nothing but the exact retry may proceed."""
        self.assertTrue(self.s.state(SLOT, OP)[2])
        self.assertRefusedUnchanged(self.attempts()["cancel"])
        self.assertRefusedUnchanged(lambda: self.s.quarantine(
            SLOT, op_id=OP, authorization=self.auth("quarantine", reason="other"), recovery_token=RTOKEN,
            record=self.record(), replace_record=self.hook()))
        self.assertRefusedUnchanged(lambda: self.adopt("op-2"))
        self.assertIsNone(self.s.terminal(SLOT, OP))
        self.assertFalse((self.root / f".{SLOT}.lock").exists())

    def retry_and_finish(self):
        self.attempts()["quarantine"]()
        self.assertEqual(self.s.state(SLOT, OP), (rs.QUARANTINED, self.gen0 + 1, False))
        self.assertEqual(self.record(), self.quarantined())
        self.attempts()["quiescence"]()

    def test_hook_failure_before_the_replace(self):
        """T2: the record was not replaced, the state still reads REQUESTED,
        and the slot is unfinished, not requested."""
        self.to(rs.REQUESTED)
        with self.assertRaises(rs.Refused):
            self.s.quarantine(SLOT, op_id=OP, authorization=self.auth("quarantine"), recovery_token=RTOKEN,
                              record=self.record(), replace_record=self.hook("raise-before"))
        self.assertEqual(self.s.state(SLOT, OP), (rs.REQUESTED, self.gen0, True))
        self.assertEqual(self.s.reconcile(SLOT, OP, self.record()), "pre")
        self.assertBlocked()
        self.retry_and_finish()

    def test_enospc_after_the_record_replace(self):
        """T3: the record is quarantined but the state write failed; the lock
        is released and nothing but the finish-only retry proceeds."""
        self.to(rs.REQUESTED)
        with self.assertRaises(rs.Refused):
            self.failing("before_state_rename").quarantine(
                SLOT, op_id=OP, authorization=self.auth("quarantine"), recovery_token=RTOKEN, record=self.record(),
                replace_record=self.hook())
        self.assertEqual(self.s.state(SLOT, OP), (rs.REQUESTED, self.gen0, True))
        self.assertEqual(self.s.reconcile(SLOT, OP, self.record()), "post")
        self.assertBlocked()
        self.retry_and_finish()
        intents = [e for e in map(json.loads, (self.root / SLOT / OP / "journal.jsonl").read_text().splitlines())
                   if e["kind"] == "intent" and e["seq"] == 2]
        self.assertEqual(len(intents), 1, "a retry wrote a second intent")

    def test_every_failure_point(self):
        expect = {"before_intent": (rs.REQUESTED, False, "none"), "after_intent": (rs.REQUESTED, True, "pre"),
                  "after_record": (rs.REQUESTED, True, "post"), "before_state_rename": (rs.REQUESTED, True, "post"),
                  "after_state": (rs.QUARANTINED, True, "post"), "after_complete": (rs.QUARANTINED, False, "none")}
        for point, (state, incomplete, side) in expect.items():
            with self.subTest(point=point):
                self.setUp()
                self.to(rs.REQUESTED)
                with self.assertRaises(rs.Refused):
                    self.failing(point).quarantine(
                        SLOT, op_id=OP, authorization=self.auth("quarantine"), recovery_token=RTOKEN,
                        record=self.record(), replace_record=self.hook())
                self.assertEqual(self.s.state(SLOT, OP)[0::2], (state, incomplete))
                self.assertEqual(self.s.reconcile(SLOT, OP, self.record()), side)
                if incomplete:
                    self.assertBlocked()
                self.retry_and_finish()

    def test_hook_failure_after_the_replace(self):
        self.to(rs.REQUESTED)
        with self.assertRaises(rs.Refused):
            self.s.quarantine(SLOT, op_id=OP, authorization=self.auth("quarantine"), recovery_token=RTOKEN,
                              record=self.record(), replace_record=self.hook("raise-after"))
        self.assertEqual(self.s.reconcile(SLOT, OP, self.record()), "post")
        self.assertBlocked()
        self.retry_and_finish()

    def test_a_hook_that_does_not_report_the_write_leaves_it_unfinished(self):
        self.to(rs.REQUESTED)
        with self.assertRaises(rs.Refused):
            self.s.quarantine(SLOT, op_id=OP, authorization=self.auth("quarantine"), recovery_token=RTOKEN,
                              record=self.record(), replace_record=lambda pre, post: None)
        self.assertEqual(self.s.state(SLOT, OP), (rs.REQUESTED, self.gen0, True))
        self.assertEqual(self.s.reconcile(SLOT, OP, self.record()), "pre")
        self.assertBlocked()
        self.retry_and_finish()

    def test_unexpected_live_record_needs_the_owner(self):
        for live in (None, v1(lease_id="2" * 32), v2("free", 0), "garbage"):
            with self.subTest(live=live):
                self.setUp()
                self.to(rs.REQUESTED)
                with self.assertRaises(rs.Refused):
                    self.s.quarantine(SLOT, op_id=OP, authorization=self.auth("quarantine"), recovery_token=RTOKEN,
                                      record=self.record(), replace_record=self.hook("raise-before"))
                if live is None:
                    self.live.unlink()
                else:
                    self.live.write_text(live)
                with self.assertRaises(rs.Corrupt):
                    self.s.reconcile(SLOT, OP, self.record())
                self.assertRefusedUnchanged(self.attempts()["quarantine"], rs.Corrupt)
                self.assertRefusedUnchanged(self.attempts()["cancel"])

    def test_a_trailing_intent_of_a_plain_step_also_blocks(self):
        self.adopt()
        with self.assertRaises(rs.Refused):
            self.failing("before_state_rename").request(SLOT, op_id=OP, authorization=self.auth("request"),
                                                        record=self.record())
        self.assertEqual(self.s.state(SLOT, OP), (rs.ACTIVE, self.gen0, True))
        self.assertEqual(self.s.reconcile(SLOT, OP, self.record()), "none")
        self.assertFalse(self.s.lease_action_allowed(SLOT, OP, self.identity, self.gen0, TOKEN))
        self.assertRefusedUnchanged(lambda: self.s.request(
            SLOT, op_id=OP, authorization=self.auth("request", reason="other"), record=self.record()))
        self.attempts()["request"]()
        self.assertEqual(self.s.state(SLOT, OP), (rs.REQUESTED, self.gen0, False))


class TerminalMarker(Store):
    def test_marker_is_written_after_the_completion_only(self):
        for point in ("before_marker_rename", "after_complete"):
            with self.subTest(point=point):
                self.setUp()
                self.to(rs.REQUESTED)

                def fault(p, point=point):
                    if p == point:
                        raise OSError("crash")
                with self.assertRaises(rs.Refused):
                    rs.RecoveryStore(self.root, fault=fault).cancel(
                        SLOT, op_id=OP, authorization=self.auth("cancel"), record=self.record())
                self.assertEqual(self.s.state(SLOT, OP), (rs.CANCELLED, self.gen0, False))
                self.assertIsNone(self.s.terminal(SLOT, OP), "a terminal state without its marker is not terminal")
                self.assertRefusedUnchanged(lambda: self.adopt("op-2"))
                self.attempts()["cancel"]()  # the exact repeat writes the marker
                self.assertEqual(self.s.terminal(SLOT, OP), rs.CANCELLED)
                self.adopt("op-2")

    def test_marker_never_overrides_an_inconsistent_record(self):
        self.to(rs.REQUESTED)
        marker = self.root / SLOT / OP / "TERMINAL"
        marker.write_bytes(b"state=CANCELLED\nseq=2\nop_id=op-1\n")
        marker.chmod(0o600)
        with self.assertRaises(rs.Corrupt):
            self.s.terminal(SLOT, OP)
        marker.unlink()
        self.setUp()
        self.to(rs.READY)
        marker = self.root / SLOT / OP / "TERMINAL"
        good = marker.read_bytes()
        marker.write_bytes(good.replace(b"seq=5", b"seq=4"))
        with self.assertRaises(rs.Corrupt):
            self.s.terminal(SLOT, OP)
        self.assertFalse(self.s.ready_for_reassignment(SLOT, OP))
        marker.write_bytes(good)
        journal = self.root / SLOT / OP / "journal.jsonl"
        journal.write_bytes(journal.read_bytes().rsplit(b"\n", 2)[0] + b"\n")  # drop the final completion
        with self.assertRaises(rs.Corrupt):
            self.s.terminal(SLOT, OP)
        self.assertFalse(self.s.ready_for_reassignment(SLOT, OP))
        self.assertRefusedUnchanged(lambda: self.adopt("op-2"))


class SharedLock(Store):
    def setUp(self):
        super().setUp()
        self.pool_lock = self.live.parent / f".{SLOT}.lock"
        self.held = rs.RecoveryStore(self.root, held_lock=lambda slot: self.live.parent / f".{slot}.lock")

    def test_the_held_lock_is_used_and_never_taken_again(self):
        self.pool_lock.mkdir()
        self.adopt(store=self.held)
        calls = self.attempts(store=self.held)
        for name in self.PATH[rs.READY]:
            calls[name]()
        self.assertTrue(self.held.ready_for_reassignment(SLOT, OP))
        self.assertTrue(self.pool_lock.is_dir(), "the store removed the caller's lock")
        self.assertFalse((self.root / f".{SLOT}.lock").exists(), "the store took a second lock")
        self.assertEqual(list(self.pool_lock.iterdir()), [])

    def test_without_the_held_lock_nothing_changes(self):
        self.pool_lock.mkdir()
        self.adopt(store=self.held)
        self.pool_lock.rmdir()
        self.assertRefusedUnchanged(self.attempts(store=self.held)["request"])
        target = self.tmp / "elsewhere"
        target.mkdir()
        self.pool_lock.symlink_to(target)
        self.assertRefusedUnchanged(self.attempts(store=self.held)["request"])

    def test_operation_and_slot_names_do_not_mix(self):
        self.pool_lock.mkdir()
        self.adopt(OTHER, store=self.held)  # an op id that equals another slot's name
        self.assertTrue((self.root / SLOT / OTHER / "state.json").is_file())
        self.assertFalse((self.root / OTHER).exists())
        with self.assertRaises(rs.Refused):
            self.held.state(OTHER, OTHER)
        self.assertRefusedUnchanged(lambda: self.held.request(
            SLOT, op_id=OTHER, authorization=self.auth("request", OTHER, slot=OTHER), record=self.record()))
        self.held.request(SLOT, op_id=OTHER, authorization=self.auth("request", OTHER), record=self.record())


class Storage(Store):
    def modes(self):
        return {p.relative_to(self.root).as_posix(): stat.S_IMODE(os.lstat(p).st_mode)
                for p in [self.root, *self.root.rglob("*")]}

    def test_store_tree_is_private_regardless_of_umask(self):
        old = os.umask(0)
        try:
            self.to(rs.READY)
        finally:
            os.umask(old)
        for name, mode in self.modes().items():
            self.assertEqual(mode & 0o077, 0, f"{name} is accessible to others: {oct(mode)}")

    def test_symlinked_or_shared_paths_are_refused(self):
        self.to(rs.REQUESTED)
        d = self.root / SLOT / OP
        state = d / "state.json"
        good = state.read_bytes()
        cases = [
            ("state symlink", lambda: (state.unlink(), (self.tmp / "s.json").write_bytes(good),
                                       state.symlink_to(self.tmp / "s.json"))),
            ("journal readable by others", lambda: (d / "journal.jsonl").chmod(0o644)),
            ("op dir readable by others", lambda: d.chmod(0o755)),
            ("root readable by others", lambda: self.root.chmod(0o755)),
        ]
        for name, damage in cases:
            with self.subTest(case=name):
                self.setUp()
                self.to(rs.REQUESTED)
                d, state = self.root / SLOT / OP, self.root / SLOT / OP / "state.json"
                good = state.read_bytes()
                damage()
                with self.assertRaises(rs.Corrupt):
                    self.s.state(SLOT, OP)
                self.assertRefusedUnchanged(self.attempts()["cancel"], rs.Corrupt)
        self.setUp()
        self.to(rs.REQUESTED)
        moved = self.tmp / "moved"
        (self.root / SLOT / OP).rename(moved)
        (self.root / SLOT / OP).symlink_to(moved)
        with self.assertRaises(rs.Corrupt):
            self.s.state(SLOT, OP)

    def test_the_store_parent_must_exist(self):
        s = rs.RecoveryStore(self.tmp / "absent" / "recovery")
        with self.assertRaises(rs.Refused):
            self.adopt(store=s)
        self.assertFalse((self.tmp / "absent").exists())

    def test_unsupported_platform_refuses_everything(self):
        self.to(rs.READY)
        with mock.patch.object(rs, "SUPPORTED", False):
            self.assertFalse(self.s.ready_for_reassignment(SLOT, OP))
            for call in [lambda: self.adopt("op-2"), lambda: self.s.state(SLOT, OP), *self.attempts().values()]:
                self.assertRefusedUnchanged(call)

    def test_failed_directory_sync_never_completes_a_transition(self):
        class NoDirSync(rs.RecoveryStore):
            def _fsync_dir(self, path):
                raise OSError("directory sync unavailable")
        broken = NoDirSync(self.root)
        self.root.mkdir(mode=0o700)
        for _ in range(3):
            with self.assertRaises(rs.Refused):
                self.adopt(store=broken)
        self.assertFalse(self.s.lease_action_allowed(SLOT, OP, self.identity, self.gen0, TOKEN))
        self.adopt()  # the same operation finishes once sync works
        self.assertEqual(self.s.state(SLOT, OP), (rs.ACTIVE, self.gen0, False))
        for _ in range(3):
            with self.assertRaises(rs.Refused):
                self.attempts(store=broken)["request"]()
            self.assertNotEqual(self.s.state(SLOT, OP)[0::2], (rs.REQUESTED, False))

    def test_parent_sync_is_retried_for_an_existing_directory(self):
        synced = []

        class Recording(rs.RecoveryStore):
            def _fsync_dir(self, path):
                synced.append(Path(path))
                super()._fsync_dir(path)
        (self.root / SLOT).mkdir(mode=0o700, parents=True)
        self.root.chmod(0o700)
        self.adopt(store=Recording(self.root))
        for parent in (self.root.parent, self.root, self.root / SLOT):
            self.assertIn(parent, synced, f"{parent} was not synced although its child already existed")

    def test_failed_state_write_leaves_no_temporary_file(self):
        self.to(rs.REQUESTED)

        def fail(point):
            if point == "before_state_rename":
                raise OSError(errno.ENOSPC, "No space left on device")
        with self.assertRaises(rs.Refused):
            rs.RecoveryStore(self.root, fault=fail).cancel(SLOT, op_id=OP, authorization=self.auth("cancel"),
                                                           record=self.record())
        self.assertEqual(list((self.root / SLOT / OP).glob(".*.tmp")), [])
        self.assertFalse((self.root / f".{SLOT}.lock").exists())

    def test_unavailable_journal_refuses(self):
        self.to(rs.REQUESTED)
        journal = self.root / SLOT / OP / "journal.jsonl"
        journal.chmod(0o400)
        self.addCleanup(journal.chmod, 0o600)
        if os.access(journal, os.W_OK):
            self.skipTest("running as root")
        self.assertRefusedUnchanged(self.attempts()["cancel"])
        journal.chmod(0o600)
        self.assertEqual(self.s.state(SLOT, OP), (rs.REQUESTED, self.gen0, False))

    def test_a_lost_state_file_is_never_readopted(self):
        self.to(rs.QUARANTINED)
        (self.root / SLOT / OP / "state.json").unlink()
        self.live.write_text(self.lease_text)
        self.assertRefusedUnchanged(lambda: self.adopt())
        self.assertRefusedUnchanged(lambda: self.adopt("op-2"))


class Secrets(Store):
    def test_no_secret_in_any_stored_byte_name_or_diagnostic(self):
        """T10, for each lease kind."""
        for kind in ("v1", "v2", "legacy"):
            with self.subTest(kind=kind):
                self.setUp()
                self.set_lease(kind)
                self.to(rs.READY)
                self.adopt_errors = []
                for call in [lambda: self.adopt(OP), *self.attempts(token="e" * 32).values(),
                             lambda: self.s.adopt(SLOT, "op-9", self.lease_text + "token=x\n", legacy=True)]:
                    try:
                        call()
                    except rs.Refused as e:
                        self.adopt_errors.append(str(e))
                stored = b"".join(p.read_bytes() for p in self.root.rglob("*") if p.is_file())
                full = hashlib.sha256(self.lease_text.encode()).hexdigest()
                for secret in (TOKEN, RTOKEN, full):
                    self.assertNotIn(secret.encode(), stored)
                    self.assertFalse(any(secret in str(p) for p in self.root.rglob("*")))
                    self.assertFalse(any(secret in e for e in self.adopt_errors), self.adopt_errors)
                self.assertNotIn(TOKEN, self.record())
                for p in self.root.rglob("*"):
                    if p.is_file():
                        self.assertEqual(p.stat().st_mode & 0o077, 0, f"{p} is readable by others")

    def test_secret_bearing_evidence_is_refused(self):
        self.to(rs.QUARANTINED)
        for q in (self.quiescence(lease_token=TOKEN), self.quiescence(evidence_refs=[{"secret": "x"}]),
                  self.quiescence(actor=object())):
            self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
                SLOT, op_id=OP, recovery_token=RTOKEN, evidence=q, record=self.record()))


class JournalConsistency(Store):
    """The record is trusted only when the journal shows every transition from
    adoption, each completion matching the intent right before it."""

    def journal(self):
        return [json.loads(line) for line in (self.root / SLOT / OP / "journal.jsonl").read_text().splitlines()]

    def rewrite(self, entries):
        (self.root / SLOT / OP / "journal.jsonl").write_text("".join(json.dumps(e) + "\n" for e in entries))

    def assertCorrupt(self):
        self.assertFalse(self.s.ready_for_reassignment(SLOT, OP))
        self.assertFalse(self.s.lease_action_allowed(SLOT, OP, self.identity, self.gen0, TOKEN))
        with self.assertRaises(rs.Corrupt):
            self.s.state(SLOT, OP)
        with self.assertRaises(rs.Corrupt):
            self.attempts()["ready"]()

    def tampered(self, mutate):
        self.setUp()
        self.to(rs.READY)
        entries = self.journal()
        mutate(entries)
        self.rewrite(entries)
        self.assertCorrupt()

    def last(self, entries, kind, seq=None):
        return [e for e in entries if e["kind"] == kind and (seq is None or e["seq"] == seq)][-1]

    def test_a_journal_of_only_the_final_completion_is_corrupt(self):
        self.tampered(lambda es: es.__setitem__(slice(None), [self.last(es, "complete")]))

    def test_a_completion_must_match_its_intent(self):
        for field, value in (("op_id", "op-2"), ("evidence", "0" * 64), ("slot_generation", 9),
                             ("from", rs.ACTIVE), ("effect", {"pre": "0" * 64, "post": "0" * 64})):
            with self.subTest(field=field):
                self.tampered(lambda es: self.last(es, "complete", 2).__setitem__(field, value))

    def test_missing_earlier_completions_and_intents_are_corrupt(self):
        for seq in range(6):
            for kind in ("complete", "intent"):
                if (seq, kind) == (5, "complete"):
                    continue  # a trailing intent: unfinished, see TerminalMarker
                with self.subTest(seq=seq, kind=kind):
                    self.tampered(lambda es: es.remove(self.last(es, kind, seq)))

    def test_reordered_duplicated_or_unknown_entries_are_corrupt(self):
        self.tampered(lambda es: es.reverse())
        self.tampered(lambda es: es.append(dict(self.last(es, "complete"))))
        self.tampered(lambda es: es.insert(0, {"kind": "note", "seq": 0}))
        self.tampered(lambda es: es.insert(2, dict(self.last(es, "intent", 3))))
        self.tampered(lambda es: es.insert(3, dict(self.last(es, "intent", 1))))  # a second intent before completion

    def test_the_quarantine_must_name_its_effect(self):
        self.tampered(lambda es: [e.pop("effect") for e in es if e["seq"] == 2])
        self.tampered(lambda es: [e.__setitem__("effect", {"pre": "x"}) for e in es if e["seq"] == 2])
        self.tampered(lambda es: [e.__setitem__("effect", {"pre": "0" * 64, "post": "0" * 64}) for e in es
                                  if e["seq"] == 3])

    def test_state_gates_and_identity_must_match_the_journal(self):
        for field, value in (("gates", "quiescence"), ("identity", "2" * 32), ("identity_kind", "v2"),
                             ("slot_generation", 7), ("op_id", "op-2"), ("unknown", 1), ("schema", 1)):
            with self.subTest(field=field):
                self.setUp()
                self.to(rs.READY)
                state = self.root / SLOT / OP / "state.json"
                rec = json.loads(state.read_text())
                if field == "gates":
                    rec["gates"][value] = "0" * 64
                else:
                    rec[field] = value
                state.write_text(json.dumps(rec))
                self.assertCorrupt()

    def test_no_single_edit_of_the_journal_yields_ready(self):
        self.to(rs.READY)
        original = self.journal()
        edits = []
        for i in range(len(original)):
            edits.append(("delete", i, original[:i] + original[i + 1:]))
            edits.append(("duplicate", i, original[:i + 1] + [original[i]] + original[i + 1:]))
            if i + 1 < len(original):
                edits.append(("swap", i, original[:i] + [original[i + 1], original[i]] + original[i + 2:]))
            for key in rs.ENTRY_KEYS + ("kind",):
                for bad in (None, 7, True, [], "x"):
                    if original[i].get(key) != bad:
                        edits.append((f"{key}={bad!r}", i, original[:i] + [dict(original[i], **{key: bad})]
                                      + original[i + 1:]))
        for name, i, entries in edits:
            with self.subTest(edit=name, line=i):
                self.rewrite(entries)
                self.assertFalse(self.s.ready_for_reassignment(SLOT, OP))
        self.rewrite(original)
        self.assertTrue(self.s.ready_for_reassignment(SLOT, OP))

    def test_nothing_may_follow_a_terminal_state(self):
        for state in rs.TERMINAL_STATES:
            with self.subTest(state=state):
                self.setUp()
                self.to(state)
                original = self.journal()
                last = self.last(original, "complete")
                ghost = {"kind": "intent", "seq": last["seq"] + 1, "from": state, "slot_generation": last["slot_generation"],
                         "op_id": OP, "evidence": "x"}
                for extra in ([dict(ghost, to=None)], [dict(ghost, to=rs.ACTIVE)], [dict(ghost, to=state)],
                              [dict(ghost, to=rs.QUARANTINED)], [dict(ghost, to=rs.CANCELLED)]):
                    self.rewrite(original + extra)
                    self.assertCorrupt()

    def test_consistent_forgeries_of_generation_and_operation_are_corrupt(self):
        cases = {
            "generation never moves on": lambda e: e.__setitem__("slot_generation", self.gen0),
            "operation switches mid-chain": lambda e: e["seq"] >= 3 and e.__setitem__("op_id", "op-2"),
            "boolean generation": lambda e: e["seq"] == 0 and e.__setitem__("slot_generation", False),
        }
        for name, change in cases.items():
            with self.subTest(case=name):
                self.setUp()
                self.to(rs.READY)
                entries = self.journal()
                for e in entries:
                    change(e)
                self.rewrite(entries)
                self.assertCorrupt()


class MalformedInput(Store):
    def test_malformed_state_values(self):
        self.to(rs.READY)
        state = self.root / SLOT / OP / "state.json"
        good = json.loads(state.read_text())
        for bad in (dict(good, state=[]), dict(good, gates=[]), dict(good, op_id=[]), dict(good, identity={}),
                    dict(good, slot_generation=True), dict(good, seq="5")):
            with self.subTest(record=bad):
                state.write_text(json.dumps(bad))
                self.assertFalse(self.s.ready_for_reassignment(SLOT, OP))
                with self.assertRaises(rs.Corrupt):
                    self.s.state(SLOT, OP)
        deep = "[" * 100000 + "]" * 100000
        state.write_text(deep)
        self.assertFalse(self.s.ready_for_reassignment(SLOT, OP))
        state.write_text(json.dumps(good))
        (self.root / SLOT / OP / "journal.jsonl").write_text(deep + "\n")
        self.assertFalse(self.s.ready_for_reassignment(SLOT, OP))

    def test_malformed_tokens(self):
        self.to(rs.QUARANTINED)
        for token in ("\ud800", RTOKEN + "\n", RTOKEN.upper(), 7, b"b" * 32):
            with self.subTest(token=token):
                self.assertRefusedUnchanged(self.attempts(token=token)["quiescence"])


class Idempotency(Store):
    def test_repeating_the_last_step_changes_nothing(self):
        for state in rs.STATES[1:]:
            with self.subTest(state=state):
                self.setUp()
                self.to(state)
                before = self.files()
                calls = self.attempts()
                calls[self.PATH[state][-1]]()
                self.assertEqual(self.files(), before)
                for earlier in self.PATH[state][:-1]:
                    self.assertRefusedUnchanged(calls[earlier])

    def test_repeat_with_different_evidence_or_credentials_is_refused(self):
        self.to(rs.QUIESCENT)
        self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
            SLOT, op_id=OP, recovery_token=RTOKEN, evidence=self.quiescence(method="containment-teardown"),
            record=self.record()))
        self.assertRefusedUnchanged(self.attempts(token="e" * 32)["quiescence"])
        self.assertRefusedUnchanged(lambda: self.s.quarantine(
            SLOT, op_id=OP, authorization=self.auth("quarantine"), recovery_token="e" * 32, record=self.record(),
            replace_record=self.hook()))


class Concurrency(Store):
    def race(self, calls):
        barrier, results = threading.Barrier(len(calls)), []

        def go(i, call):
            barrier.wait()
            try:
                call()
                results.append(("ok", i))
            except rs.Refused as e:
                results.append((type(e).__name__, i))
            except Exception as e:  # noqa: BLE001 - any other type is a failure of the test
                results.append((f"unexpected {type(e).__name__}: {e}", i))
        threads = [threading.Thread(target=go, args=(i, c)) for i, c in enumerate(calls)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertFalse([r for r in results if r[0].startswith("unexpected")], results)
        return results

    def test_concurrent_adoptions_on_a_fresh_root_have_one_winner(self):
        results = self.race([lambda i=i: self.adopt(f"op-{i}", store=rs.RecoveryStore(self.root))
                             for i in range(12)])
        winners = [i for r, i in results if r == "ok"]
        self.assertEqual(len(winners), 1, results)
        self.assertEqual(sorted(p.name for p in (self.root / SLOT).iterdir()), [f"op-{winners[0]}"])
        self.assertFalse((self.root / f".{SLOT}.lock").exists())

    def test_cancel_and_quarantine_race_has_one_outcome(self):
        self.to(rs.REQUESTED)
        calls = []
        for i in range(10):
            s = rs.RecoveryStore(self.root)
            calls.append(self.attempts(store=s)["cancel" if i % 2 else "quarantine"])
        self.race(calls)
        state = self.s.state(SLOT, OP)
        self.assertIn(state, ((rs.CANCELLED, self.gen0, False), (rs.QUARANTINED, self.gen0 + 1, False)))
        self.assertEqual(self.record() == self.quarantined(), state[0] == rs.QUARANTINED)

    def test_held_lock_refuses_and_is_never_broken(self):
        self.to(rs.REQUESTED)
        (self.root / f".{SLOT}.lock").mkdir()
        before = self.files()
        with self.assertRaises(rs.Busy):
            self.attempts()["quarantine"]()
        self.assertEqual(self.files(), before)
        self.assertTrue((self.root / f".{SLOT}.lock").is_dir())


class Crashes(Store):
    """A real crash: a child process dies with os._exit at a crash point."""

    def crash(self, call, point=None):
        script = (f"import os, sys; sys.path.insert(0, {str(HERE)!r})\n"
                  f"import recovery_state as rs, test_recovery_state as t\nfrom pathlib import Path\n"
                  f"def fault(p):\n    if p == {point!r}: os._exit(70)\n"
                  f"s = rs.RecoveryStore({str(self.root)!r}, fault=fault)\n"
                  f"live = Path({str(self.live)!r})\nrecord = live.read_text() if live.exists() else None\n"
                  f"{call}\n")
        p = subprocess.run([sys.executable, "-c", script], cwd=HERE, capture_output=True, text=True)
        self.assertEqual(p.returncode, 70, p.stderr)
        lock = self.root / f".{SLOT}.lock"
        self.assertTrue(lock.is_dir(), "a crash left no lock")
        with self.assertRaises(rs.Busy):  # a crash-held lock blocks until the owner removes it
            self.attempts()["cancel"]()
        lock.rmdir()  # the owner's manual step after checking no transition is running

    def quarantine_call(self, hook="None"):
        return (f"s.quarantine(t.SLOT, op_id=t.OP, authorization={self.auth('quarantine')!r}, "
                f"recovery_token=t.RTOKEN, record=record, replace_record=t.cas_hook(live, {hook}))")

    def test_crash_at_every_quarantine_point(self):
        cases = {("point", "before_intent"): (rs.REQUESTED, False, "none"),
                 ("point", "after_intent"): (rs.REQUESTED, True, "pre"),
                 ("hook", "crash-before"): (rs.REQUESTED, True, "pre"),
                 ("hook", "crash-after"): (rs.REQUESTED, True, "post"),
                 ("point", "after_record"): (rs.REQUESTED, True, "post"),
                 ("point", "before_state_rename"): (rs.REQUESTED, True, "post"),
                 ("point", "after_state"): (rs.QUARANTINED, True, "post"),
                 ("point", "after_complete"): (rs.QUARANTINED, False, "none")}
        for (where, at), (state, incomplete, side) in cases.items():
            with self.subTest(at=at):
                self.setUp()
                self.to(rs.REQUESTED)
                if where == "point":
                    self.crash(self.quarantine_call(), at)
                else:
                    self.crash(self.quarantine_call(repr(at)))
                self.assertEqual(self.s.state(SLOT, OP)[0::2], (state, incomplete))
                self.assertEqual(self.s.reconcile(SLOT, OP, self.record()), side)
                self.assertFalse(self.s.lease_action_allowed(SLOT, OP, self.identity, self.gen0, TOKEN))
                if incomplete:
                    self.assertRefusedUnchanged(self.attempts()["cancel"])
                    self.assertRefusedUnchanged(self.attempts()["quiescence"])
                self.attempts()["quarantine"]()
                self.assertEqual(self.s.state(SLOT, OP), (rs.QUARANTINED, self.gen0 + 1, False))
                self.assertEqual(self.record(), self.quarantined())

    def test_crash_at_every_cancel_point(self):
        call = f"s.cancel(t.SLOT, op_id=t.OP, authorization={self.auth('cancel')!r}, record=record)"
        for point, (state, incomplete) in {"after_intent": (rs.REQUESTED, True),
                                           "before_state_rename": (rs.REQUESTED, True),
                                           "after_state": (rs.CANCELLED, True),
                                           "after_complete": (rs.CANCELLED, False),
                                           "before_marker_rename": (rs.CANCELLED, False)}.items():
            with self.subTest(point=point):
                self.setUp()
                self.to(rs.REQUESTED)
                self.crash(call, point)
                self.assertEqual(self.s.state(SLOT, OP)[0::2], (state, incomplete))
                self.assertIsNone(self.s.terminal(SLOT, OP))
                self.assertRefusedUnchanged(lambda: self.adopt("op-2"))
                self.attempts()["cancel"]()
                self.assertEqual(self.s.terminal(SLOT, OP), rs.CANCELLED)

    def test_an_interrupted_adoption_leaves_no_operation(self):
        other = v1(lease_id="2" * 32, token="d" * 32)
        for point in ("before_intent", "after_intent", "before_state_rename", "after_state", "after_complete",
                      "before_publish"):
            with self.subTest(point=point):
                self.setUp()
                self.root.mkdir(mode=0o700)
                self.crash("s.adopt(t.SLOT, t.OP, t.v1())", point)
                with self.assertRaises(rs.Refused):
                    self.s.state(SLOT, OP)
                self.assertFalse(self.s.lease_action_allowed(SLOT, OP, LEASE_ID, 0, TOKEN))
                self.s.adopt(SLOT, "op-2", other)  # nothing blocks a fresh operation, even of another lease
                self.assertTrue(self.s.lease_action_allowed(SLOT, "op-2", "2" * 32, 0, "d" * 32))


class NotWired(unittest.TestCase):
    def test_no_entry_point_and_no_caller(self):
        src = (HERE / "recovery_state.py").read_text()
        self.assertNotIn("__main__", src)
        self.assertNotIn("codegraph-evidence", src)
        for caller in (HERE / "pool.sh", HERE.parent / "agent-coordinator/coordinator.py",
                       HERE.parent / "agent-coordinator/campaign.py"):
            self.assertNotIn("recovery_state", caller.read_text(), f"{caller.name} calls the recovery state machine")


if __name__ == "__main__":
    unittest.main()

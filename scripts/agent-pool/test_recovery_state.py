#!/usr/bin/env python3
"""Tests for recovery_state.py. Every store lives in a fresh temporary
directory; the live pool and its lease files are never read or written.

    python3 -m unittest test_recovery_state      (from scripts/agent-pool)
"""

import itertools
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import recovery_state as rs  # noqa: E402

SLOT = "codegraph-worker-a"
LEASE_ID = "1" * 32
TOKEN = "a" * 32
RTOKEN = "b" * 32
OP = "op-1"
HEAD = "c" * 40
LIVE_POOL = (Path.home() / "Projekti/codegraph-evidence/pool").resolve()


def lease(**over):
    fields = {"version": "1", "lease_id": LEASE_ID, "slot": SLOT, "task": "t", "agent": "a", "pr": "",
              "owner": "coordinator:x", "host": "h", "pid": "", "utc": "2026-11-07T00:00:00Z", "token": TOKEN}
    fields.update(over)
    return "".join(f"{k}={v}\n" for k, v in fields.items() if v is not None)


def authorization(generation=1, **over):
    a = {"slot": SLOT, "lease_id": LEASE_ID, "generation": generation, "op_id": OP, "actor": "owner",
         "reason": "orphaned", "assessment_digest": "d" * 64, "quiescence_method": "owner-quiescence",
         "preservation_disposition": "archive", "utc": "2026-11-07T00:00:00Z"}
    a.update(over)
    return {k: v for k, v in a.items() if v is not None}


def quiescence(generation=2, **over):
    q = {"slot": SLOT, "op_id": OP, "generation": generation, "method": "owner-quiescence", "actor": "owner",
         "evidence_refs": ["evidence/reboot.md"], "accounted": {c: True for c in rs.QUIESCENCE_ACCOUNTED}}
    q.update(over)
    return {k: v for k, v in q.items() if v is not None}


def preservation(generation=2, inventory=None, preserved=None, **over):
    inv = inventory or {k: [] for k in rs.INVENTORY}
    p = {"slot": SLOT, "op_id": OP, "generation": generation, "head": HEAD, "complete": True,
         "inventory": inv, "preserved": preserved or {}}
    p.update(over)
    return p


class Store(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp()).resolve() / "recovery"
        self.addCleanup(shutil.rmtree, self.root.parent, True)
        self.assertFalse(str(self.root).startswith(str(LIVE_POOL)))
        self.s = rs.RecoveryStore(self.root)

    def files(self):
        return {p.relative_to(self.root).as_posix(): p.read_bytes()
                for p in sorted(self.root.rglob("*")) if p.is_file()}

    def assertRefusedUnchanged(self, call, exc=rs.Refused):
        before = self.files()
        with self.assertRaises(exc):
            call()
        self.assertEqual(self.files(), before, "a refused transition changed the record or journal")
        self.assertFalse((self.root / f".{SLOT}.lock").exists(), "a refused transition left its lock")

    def to(self, state):
        """Drives the slot along the only path up to `state`."""
        self.s.adopt(SLOT, lease())
        steps = [
            (rs.REQUESTED, lambda: self.s.request(SLOT, op_id=OP, lease_id=LEASE_ID, generation=1)),
            (rs.QUARANTINED, lambda: self.s.quarantine(SLOT, op_id=OP, authorization=authorization(),
                                                       recovery_token=RTOKEN)),
            (rs.QUIESCENT, lambda: self.s.verify_quiescence(SLOT, op_id=OP, generation=2, recovery_token=RTOKEN,
                                                            evidence=quiescence())),
            (rs.PRESERVED, lambda: self.s.record_preservation(SLOT, op_id=OP, generation=2,
                                                              recovery_token=RTOKEN, evidence=preservation())),
            (rs.READY, lambda: self.s.mark_ready(SLOT, op_id=OP, generation=2, recovery_token=RTOKEN,
                                                 final_check=self.final())),
        ]
        for name, step in steps:
            if rs.ORDER.index(name) > rs.ORDER.index(state):
                break
            step()
        self.assertEqual(self.s.state(SLOT)[0], state)

    def final(self, **over):
        f = {"slot": SLOT, "op_id": OP, "generation": 2, "head": HEAD, "clean": True, "lease_absent": True,
             "preservation": rs.digest(preservation())}
        f.update(over)
        return f

    def attempts(self, token=RTOKEN, generation=2):
        """Every transition call, with otherwise valid evidence."""
        return {
            "request": lambda: self.s.request(SLOT, op_id=OP, lease_id=LEASE_ID, generation=1),
            "quarantine": lambda: self.s.quarantine(SLOT, op_id=OP, authorization=authorization(),
                                                    recovery_token=RTOKEN),
            "quiescence": lambda: self.s.verify_quiescence(SLOT, op_id=OP, generation=generation,
                                                           recovery_token=token, evidence=quiescence(generation)),
            "preservation": lambda: self.s.record_preservation(SLOT, op_id=OP, generation=generation,
                                                               recovery_token=token,
                                                               evidence=preservation(generation)),
            "ready": lambda: self.s.mark_ready(SLOT, op_id=OP, generation=generation, recovery_token=token,
                                               final_check=self.final(generation=generation)),
        }


class Transitions(Store):
    def test_only_path_reaches_ready(self):
        self.to(rs.READY)
        self.assertTrue(self.s.ready_for_reassignment(SLOT))
        self.assertEqual(self.s.state(SLOT), (rs.READY, 2, False))

    def test_every_forbidden_transition_is_refused_unchanged(self):
        target = {"request": rs.REQUESTED, "quarantine": rs.QUARANTINED, "quiescence": rs.QUIESCENT,
                  "preservation": rs.PRESERVED, "ready": rs.READY}
        for state in rs.ORDER:
            with self.subTest(state=state):
                self.setUp()
                self.to(state)
                for name, call in self.attempts().items():
                    allowed = rs.ORDER.index(target[name]) == rs.ORDER.index(state) + 1
                    if allowed or target[name] == state:
                        continue  # the one allowed step, or an exact repeat (see Idempotency)
                    self.assertRefusedUnchanged(call)
                self.assertEqual(self.s.ready_for_reassignment(SLOT), state == rs.READY)

    def test_generation_and_token_mismatch_are_refused(self):
        self.to(rs.QUARANTINED)
        for token, gen in ((RTOKEN, 1), (RTOKEN, 3), ("e" * 32, 2), (TOKEN, 2), (None, 2)):
            with self.subTest(token=token, generation=gen):
                self.assertRefusedUnchanged(self.attempts(token, gen)["quiescence"])
        self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
            SLOT, op_id="op-2", generation=2, recovery_token=RTOKEN, evidence=quiescence(op_id="op-2")))

    def test_request_must_name_the_current_lease(self):
        self.s.adopt(SLOT, lease())
        self.assertRefusedUnchanged(lambda: self.s.request(SLOT, op_id=OP, lease_id="2" * 32, generation=1))
        self.assertRefusedUnchanged(lambda: self.s.request(SLOT, op_id=OP, lease_id=LEASE_ID, generation=2))
        self.assertRefusedUnchanged(lambda: self.s.request(SLOT, op_id="../x", lease_id=LEASE_ID, generation=1))

    def test_stale_owner_cannot_act_after_quarantine(self):
        self.s.adopt(SLOT, lease())
        self.assertTrue(self.s.lease_action_allowed(SLOT, LEASE_ID, 1, TOKEN))
        self.assertFalse(self.s.lease_action_allowed(SLOT, LEASE_ID, 1, "f" * 32))
        self.s.request(SLOT, op_id=OP, lease_id=LEASE_ID, generation=1)
        self.assertFalse(self.s.lease_action_allowed(SLOT, LEASE_ID, 1, TOKEN), "a requested recovery still lets owner act")
        self.s.quarantine(SLOT, op_id=OP, authorization=authorization(), recovery_token=RTOKEN)
        for gen in (1, 2):
            self.assertFalse(self.s.lease_action_allowed(SLOT, LEASE_ID, gen, TOKEN))
        self.assertRefusedUnchanged(self.attempts(TOKEN, 1)["quiescence"])
        self.assertRefusedUnchanged(self.attempts(TOKEN, 2)["quiescence"])

    def test_authorization_must_be_complete_and_bound(self):
        self.to(rs.REQUESTED)
        bad = [authorization(**{f: None}) for f in rs.AUTHORIZATION_FIELDS]
        bad += [authorization(slot="codegraph-worker-b"), authorization(lease_id="2" * 32),
                authorization(generation=2), authorization(op_id="op-2"),
                authorization(quiescence_method="pid-absent"), authorization(token=TOKEN), None, "yes"]
        for a in bad:
            with self.subTest(authorization=a):
                self.assertRefusedUnchanged(lambda: self.s.quarantine(SLOT, op_id=OP, authorization=a,
                                                                     recovery_token=RTOKEN))
        self.assertRefusedUnchanged(lambda: self.s.quarantine(SLOT, op_id="op-2",
                                                             authorization=authorization(op_id="op-2"),
                                                             recovery_token=RTOKEN))
        self.assertRefusedUnchanged(lambda: self.s.quarantine(SLOT, op_id=OP, authorization=authorization(),
                                                             recovery_token="short"))


class Quiescence(Store):
    def test_missing_or_partial_quiescence_proof_is_refused(self):
        self.to(rs.QUARANTINED)
        bad = [quiescence(method=None), quiescence(method="pid-absent"), quiescence(method="heartbeat-expired"),
               quiescence(method="no-process-cwd"), quiescence(method="token-rotated"), quiescence(actor=None),
               quiescence(evidence_refs=[]), quiescence(evidence_refs="x"), quiescence(accounted=None),
               quiescence(generation=1), quiescence(op_id="op-2"), quiescence(slot="codegraph-worker-b"),
               quiescence(method="reboot-runbook"), quiescence(method="containment-teardown")]
        for c in rs.QUIESCENCE_ACCOUNTED:
            bad.append(quiescence(accounted={k: k != c for k in rs.QUIESCENCE_ACCOUNTED}))
            bad.append(quiescence(accounted={k: "yes" for k in rs.QUIESCENCE_ACCOUNTED}))
        for q in bad:
            with self.subTest(evidence=q):
                self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
                    SLOT, op_id=OP, generation=2, recovery_token=RTOKEN, evidence=q))

    def test_no_sequence_without_quiescence_reaches_ready(self):
        """Every ordering of every non-quiescence call, from QUARANTINED, with
        valid credentials: none of them can reach READY_FOR_REASSIGNMENT."""
        names = ["request", "quarantine", "preservation", "ready"]
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
                self.assertFalse(self.s.ready_for_reassignment(SLOT))
                self.assertEqual(self.s.state(SLOT)[0], rs.QUARANTINED)

    def test_a_record_claiming_ready_without_its_gates_is_corrupt(self):
        self.to(rs.QUARANTINED)
        state = self.root / SLOT / "state.json"
        rec = json.loads(state.read_text())
        rec["state"] = rs.READY
        state.write_text(json.dumps(rec))
        self.assertFalse(self.s.ready_for_reassignment(SLOT))
        with self.assertRaises(rs.Corrupt):
            self.s.state(SLOT)


class Preservation(Store):
    def test_unpreserved_work_is_refused(self):
        self.to(rs.QUIESCENT)
        inv = {"dirty": ["src/a.go"], "untracked": ["notes.txt"], "ignored": [".env.local"],
               "unpublished_commits": ["d" * 40]}
        cases = [
            preservation(inventory=inv),
            preservation(inventory=inv, preserved={"src/a.go": {"to": "x", "verified": True}}),
            preservation(inventory=inv, preserved={i: {"to": "x", "verified": False} for k in inv for i in inv[k]}),
            preservation(inventory=inv, preserved={i: {"verified": True} for k in inv for i in inv[k]}),
            preservation(inventory={"dirty": []}),
            preservation(head="HEAD"),
            preservation(complete=False),  # interrupted preservation
            preservation(inventory={**{k: [] for k in rs.INVENTORY}, "dirty": [["x"]]}),
        ]
        for p in cases:
            with self.subTest(evidence=p):
                self.assertRefusedUnchanged(lambda: self.s.record_preservation(
                    SLOT, op_id=OP, generation=2, recovery_token=RTOKEN, evidence=p))
        done = {i: {"to": f"archive/{i}", "verified": True} for k in inv for i in inv[k]}
        self.s.record_preservation(SLOT, op_id=OP, generation=2, recovery_token=RTOKEN,
                                   evidence=preservation(inventory=inv, preserved=done))
        self.assertEqual(self.s.state(SLOT)[0], rs.PRESERVED)

    def test_final_check_must_show_clean_unleased_preserved_slot(self):
        self.to(rs.PRESERVED)
        for f in (self.final(clean=False), self.final(lease_absent=False), self.final(head=None),
                  self.final(head="d" * 40), self.final(preservation="0" * 64), self.final(generation=1)):
            with self.subTest(final=f):
                self.assertRefusedUnchanged(lambda: self.s.mark_ready(
                    SLOT, op_id=OP, generation=2, recovery_token=RTOKEN, final_check=f))


class Leases(Store):
    def test_legacy_unsupported_and_malformed_leases_are_refused(self):
        bad = [lease(version=None), lease(version="99"), lease(token=None), lease(lease_id=None),
               lease(lease_id="xyz"), lease(slot="codegraph-worker-b"), lease(task=""), lease() + "task=again\n",
               lease() + "garbage\n", "", None, b"version=1"]
        for text in bad:
            with self.subTest(lease=text):
                with self.assertRaises(rs.Refused):
                    self.s.adopt(SLOT, text)
                self.assertFalse((self.root / SLOT / "state.json").exists())
                self.assertFalse(self.s.lease_action_allowed(SLOT, LEASE_ID, 1, TOKEN))

    def test_adopt_once(self):
        self.s.adopt(SLOT, lease())
        self.assertRefusedUnchanged(lambda: self.s.adopt(SLOT, lease()))
        with self.assertRaises(rs.Refused):
            self.s.adopt("../escape", lease(slot="../escape"))


class Storage(Store):
    def test_corrupt_records_fail_closed(self):
        self.to(rs.READY)
        state, journal = self.root / SLOT / "state.json", self.root / SLOT / "journal.jsonl"
        good_state, good_journal = state.read_bytes(), journal.read_bytes()
        corruptions = [
            (state, b"{"), (state, b"[]"), (state, json.dumps({**json.loads(good_state), "schema": 2}).encode()),
            (state, json.dumps({**json.loads(good_state), "seq": 99}).encode()),
            (journal, good_journal + b"{torn"), (journal, b""), (journal, b"[1]\n"),
        ]
        for path, data in corruptions:
            with self.subTest(path=path.name, data=data[:20]):
                path.write_bytes(data)
                self.assertFalse(self.s.ready_for_reassignment(SLOT))
                with self.assertRaises(rs.Corrupt):
                    self.s.state(SLOT)
                state.write_bytes(good_state)
                journal.write_bytes(good_journal)
        journal.unlink()
        self.assertFalse(self.s.ready_for_reassignment(SLOT))
        self.assertFalse(self.s.ready_for_reassignment("codegraph-worker-b"))  # no record at all

    def test_unavailable_audit_storage_refuses(self):
        self.to(rs.REQUESTED)
        journal = self.root / SLOT / "journal.jsonl"
        journal.chmod(0o400)
        self.addCleanup(journal.chmod, 0o600)
        if os.access(journal, os.W_OK):
            self.skipTest("running as root")
        self.assertRefusedUnchanged(self.attempts()["quarantine"])
        journal.chmod(0o600)
        self.assertEqual(self.s.state(SLOT), (rs.REQUESTED, 1, False))

    def test_failed_state_write_leaves_record_intact(self):
        self.to(rs.REQUESTED)

        def fail(point):
            if point == "before_state_rename":
                raise OSError("disk full")
        before = (self.root / SLOT / "state.json").read_bytes()
        s = rs.RecoveryStore(self.root, fault=fail)
        with self.assertRaises(rs.Refused):
            s.quarantine(SLOT, op_id=OP, authorization=authorization(), recovery_token=RTOKEN)
        self.assertEqual((self.root / SLOT / "state.json").read_bytes(), before)
        self.assertEqual(list((self.root / SLOT).glob(".state.*")), [], "temporary state file left behind")
        self.assertFalse((self.root / f".{SLOT}.lock").exists())
        # The dangling intent leaves the old state readable; the same operation can retry.
        self.assertEqual(self.s.state(SLOT), (rs.REQUESTED, 1, False))
        self.attempts()["quarantine"]()
        self.assertEqual(self.s.state(SLOT), (rs.QUARANTINED, 2, False))

    def test_no_secret_in_any_stored_byte(self):
        self.to(rs.READY)
        blob = b"".join(self.files().values())
        for secret in (TOKEN, RTOKEN):
            self.assertNotIn(secret.encode(), blob)
        for p in self.root.rglob("*"):
            self.assertNotIn(TOKEN, p.name)
            self.assertNotIn(RTOKEN, p.name)
            if p.is_file():
                self.assertEqual(p.stat().st_mode & 0o077, 0, f"{p} is readable by others")

    def test_secret_bearing_evidence_is_refused(self):
        self.to(rs.QUARANTINED)
        self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
            SLOT, op_id=OP, generation=2, recovery_token=RTOKEN, evidence=quiescence(lease_token=TOKEN)))
        self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
            SLOT, op_id=OP, generation=2, recovery_token=RTOKEN,
            evidence=quiescence(evidence_refs=[{"secret": "x"}])))
        self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
            SLOT, op_id=OP, generation=2, recovery_token=RTOKEN, evidence=quiescence(actor=object())))


class ReviewRegressions(Store):
    def test_lease_token_cannot_become_the_recovery_token(self):
        self.to(rs.REQUESTED)
        self.assertRefusedUnchanged(lambda: self.s.quarantine(SLOT, op_id=OP, authorization=authorization(),
                                                             recovery_token=TOKEN))

    def test_quiescence_method_must_be_the_authorized_one(self):
        self.to(rs.REQUESTED)
        self.s.quarantine(SLOT, op_id=OP, recovery_token=RTOKEN,
                          authorization=authorization(quiescence_method="containment-teardown"))
        self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
            SLOT, op_id=OP, generation=2, recovery_token=RTOKEN, evidence=quiescence(method="owner-quiescence")))
        self.s.verify_quiescence(SLOT, op_id=OP, generation=2, recovery_token=RTOKEN,
                                 evidence=quiescence(method="containment-teardown"))

    def test_a_commit_after_preservation_never_reaches_ready(self):
        self.to(rs.PRESERVED)
        self.assertRefusedUnchanged(lambda: self.s.mark_ready(
            SLOT, op_id=OP, generation=2, recovery_token=RTOKEN, final_check=self.final(head="d" * 40)))
        self.assertFalse(self.s.ready_for_reassignment(SLOT))

    def test_failed_directory_sync_never_completes_a_transition(self):
        class NoDirSync(rs.RecoveryStore):
            def _fsync_dir(self, path):
                raise OSError("directory sync unavailable")
        broken = NoDirSync(self.root)
        self.root.mkdir(parents=True)
        for _ in range(3):
            with self.assertRaises(rs.Refused):
                broken.adopt(SLOT, lease())
        self.assertFalse(self.s.lease_action_allowed(SLOT, LEASE_ID, 1, TOKEN))
        self.s.adopt(SLOT, lease())  # the same operation finishes once sync works
        self.assertEqual(self.s.state(SLOT), (rs.ACTIVE, 1, False))
        for _ in range(3):
            with self.assertRaises(rs.Refused):
                broken.request(SLOT, op_id=OP, lease_id=LEASE_ID, generation=1)
            self.assertNotEqual(self.s.state(SLOT), (rs.REQUESTED, 1, False))

    def test_a_dangling_intent_does_not_wedge_a_later_operation(self):
        self.s.adopt(SLOT, lease())

        def at(point):
            def fault(p):
                if p == point:
                    raise OSError("crash")
            return rs.RecoveryStore(self.root, fault=fault)
        with self.assertRaises(rs.Refused):
            at("before_state_rename").request(SLOT, op_id="op-1", lease_id=LEASE_ID, generation=1)
        with self.assertRaises(rs.Refused):
            at("after_state").request(SLOT, op_id="op-2", lease_id=LEASE_ID, generation=1)
        self.assertEqual(self.s.state(SLOT), (rs.REQUESTED, 1, True))
        self.s.request(SLOT, op_id="op-2", lease_id=LEASE_ID, generation=1)
        self.assertEqual(self.s.state(SLOT), (rs.REQUESTED, 1, False))

    def test_a_lost_state_file_is_never_readopted(self):
        self.to(rs.QUARANTINED)
        (self.root / SLOT / "state.json").unlink()
        with self.assertRaises(rs.Corrupt):
            self.s.adopt(SLOT, lease())
        self.assertFalse(self.s.lease_action_allowed(SLOT, LEASE_ID, 1, TOKEN))


class Idempotency(Store):
    def test_repeating_the_last_step_changes_nothing(self):
        """Only the step that produced the current state is repeatable; an
        earlier step is superseded and refused."""
        steps = list(self.attempts())
        for i, state in enumerate(rs.ORDER[1:]):
            with self.subTest(state=state):
                self.setUp()
                self.to(state)
                before = self.files()
                calls = self.attempts()
                calls[steps[i]]()
                self.assertEqual(self.files(), before)
                for earlier in steps[:i]:
                    self.assertRefusedUnchanged(calls[earlier])

    def test_repeat_with_different_evidence_or_credentials_is_refused(self):
        self.to(rs.QUIESCENT)
        self.assertRefusedUnchanged(lambda: self.s.verify_quiescence(
            SLOT, op_id=OP, generation=2, recovery_token=RTOKEN,
            evidence=quiescence(method="containment-teardown")))
        self.assertRefusedUnchanged(self.attempts(token="e" * 32)["quiescence"])
        self.assertRefusedUnchanged(lambda: self.s.quarantine(
            SLOT, op_id=OP, authorization=authorization(), recovery_token="e" * 32))
        self.assertRefusedUnchanged(lambda: self.s.request(SLOT, op_id="op-2", lease_id=LEASE_ID, generation=1))


class Concurrency(Store):
    def test_concurrent_requests_have_one_winner(self):
        self.s.adopt(SLOT, lease())
        n = 12
        barrier, results = threading.Barrier(n), []

        def go(i):
            barrier.wait()
            try:
                rs.RecoveryStore(self.root).request(SLOT, op_id=f"op-{i}", lease_id=LEASE_ID, generation=1)
                results.append(("ok", i))
            except rs.Refused as e:
                results.append((type(e).__name__, i))
        threads = [threading.Thread(target=go, args=(i,)) for i in range(n)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        winners = [i for r, i in results if r == "ok"]
        self.assertEqual(len(winners), 1, results)
        self.assertEqual(self.s.state(SLOT), (rs.REQUESTED, 1, False))
        self.assertFalse((self.root / f".{SLOT}.lock").exists())

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

    def crash(self, call, point):
        script = (f"import os, sys; sys.path.insert(0, {str(HERE)!r}); import recovery_state as rs, test_recovery_state as t\n"
                  f"def fault(p):\n    if p == {point!r}: os._exit(70)\n"
                  f"s = rs.RecoveryStore({str(self.root)!r}, fault=fault)\n"
                  f"{call}\n")
        p = subprocess.run([sys.executable, "-c", script], cwd=HERE, capture_output=True, text=True)
        self.assertEqual(p.returncode, 70, p.stderr)
        lock = self.root / f".{SLOT}.lock"
        self.assertTrue(lock.is_dir(), "a crash left no lock")
        with self.assertRaises(rs.Busy):  # a crash-held lock blocks until the owner removes it
            self.attempts()["ready"]()
        lock.rmdir()  # the owner's manual step after checking no transition is running

    READY_CALL = ("s.mark_ready(t.SLOT, op_id=t.OP, generation=2, recovery_token=t.RTOKEN, "
                  "final_check=dict(slot=t.SLOT, op_id=t.OP, generation=2, head=t.HEAD, clean=True, "
                  "lease_absent=True, preservation=rs.digest(t.preservation())))")

    def test_crash_before_journal_leaves_old_state(self):
        self.to(rs.PRESERVED)
        before = self.files()
        self.crash(self.READY_CALL, "before_intent")
        self.assertEqual(self.files(), before)
        self.assertEqual(self.s.state(SLOT), (rs.PRESERVED, 2, False))
        self.assertFalse(self.s.ready_for_reassignment(SLOT))

    def test_crash_after_intent_before_state_keeps_old_state_and_allows_retry(self):
        self.to(rs.PRESERVED)
        self.crash(self.READY_CALL, "after_intent")
        self.assertEqual(self.s.state(SLOT), (rs.PRESERVED, 2, False))
        self.assertFalse(self.s.ready_for_reassignment(SLOT))
        self.attempts()["ready"]()
        self.assertTrue(self.s.ready_for_reassignment(SLOT))

    def test_crash_after_state_before_completion_is_not_ready(self):
        self.to(rs.PRESERVED)
        self.crash(self.READY_CALL, "after_state")
        self.assertEqual(self.s.state(SLOT), (rs.READY, 2, True))
        self.assertFalse(self.s.ready_for_reassignment(SLOT), "an unfinished transition authorized reassignment")
        # Nobody but the same operation, with the same evidence, can finish it.
        self.assertRefusedUnchanged(self.attempts(token="e" * 32)["ready"])
        self.assertRefusedUnchanged(lambda: self.s.mark_ready(
            SLOT, op_id=OP, generation=2, recovery_token=RTOKEN, final_check=self.final(head="d" * 40)))
        self.attempts()["ready"]()
        self.assertTrue(self.s.ready_for_reassignment(SLOT))

    def test_crash_inside_quarantine_never_reopens_the_old_lease(self):
        self.to(rs.REQUESTED)
        call = ("s.quarantine(t.SLOT, op_id=t.OP, authorization=t.authorization(), recovery_token=t.RTOKEN)")
        self.crash(call, "after_state")
        self.assertEqual(self.s.state(SLOT), (rs.QUARANTINED, 2, True))
        self.assertFalse(self.s.lease_action_allowed(SLOT, LEASE_ID, 1, TOKEN))
        self.assertRefusedUnchanged(self.attempts()["quiescence"])  # unfinished: no next step yet
        self.attempts()["quarantine"]()
        self.assertEqual(self.s.state(SLOT), (rs.QUARANTINED, 2, False))

    def test_crash_while_adopting_can_be_finished(self):
        call = "s.adopt(t.SLOT, t.lease())"
        self.root.mkdir(parents=True)
        self.crash(call, "after_state")
        self.assertEqual(self.s.state(SLOT), (rs.ACTIVE, 1, True))
        self.assertFalse(self.s.lease_action_allowed(SLOT, LEASE_ID, 1, TOKEN))
        self.s.adopt(SLOT, lease())
        self.assertTrue(self.s.lease_action_allowed(SLOT, LEASE_ID, 1, TOKEN))


class NotWired(unittest.TestCase):
    def test_no_entry_point_and_no_caller(self):
        src = (HERE / "recovery_state.py").read_text()
        self.assertNotIn("__main__", src)
        for caller in (HERE / "pool.sh", HERE.parent / "agent-coordinator/coordinator.py",
                       HERE.parent / "agent-coordinator/campaign.py"):
            self.assertNotIn("recovery_state", caller.read_text(), f"{caller.name} calls the recovery state machine")


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""The recovery gate of a migrated pool: pool_record.py gate, the pool.sh
mutators and status that use it, old pool.sh copies and the coordinator and
campaign readers. Operations are built with recovery_state.py directly, as a
test fixture: neither pool.sh nor pool_record.py can create or advance one.
Every pool is a throwaway: HOME and every CG_POOL_* point into a temporary
directory.

    python3 -m unittest test_recovery_gate      (from scripts/agent-pool)
"""

import ast
import importlib.util
import os
import secrets
import shutil
import stat
import subprocess
import threading
import unittest
from contextlib import contextmanager
from unittest import mock

from test_pool_v2 import A, B, HISTORY, OWNER, POOL, R, TXN, HERE, Pool, blocks, record, v1_lease
from test_pool_v2_writes import SLOTS, Migrated
import pool_record  # noqa: E402
import recovery_state as rs  # noqa: E402

GATE_KEYS = ["slot", "recovery", "gate", "recovery_operation", "recovery_state", "reason"]
# The pool.sh of PR #539 (2b-2b) with its helpers: a presence fence on recovery state.
PRESENCE_FENCE = "3d34491e"
# A coordinator older than the strict status parser, as on the primary checkout.
OLD_COORDINATOR = "cdc2cfe9"
TRANSITIONS = ("adopt", "request", "cancel", "quarantine", "quiescence", "preservation", "ready")


def fault_at(point):
    def fault(p):
        if p == point:
            raise OSError(5, f"injected at {point}")
    return fault


class Recovery(Migrated):
    """A migrated pool plus a synthetic recovery fixture driven through the library."""

    def setUp(self):
        super().setUp()
        self.rtok = {}
        self.auths = {}
        self.migrate()

    def store(self, fault=None):
        return rs.RecoveryStore(self.leases / "recovery", fault=fault,
                                held_lock=lambda s: self.leases / f".{s}.lock")

    @contextmanager
    def locked(self, slot):
        lock = self.leases / f".{slot}.lock"
        lock.mkdir()
        try:
            yield
        finally:
            lock.rmdir()

    def live(self, slot):
        return pool_record.read_record(self.leases / f"{slot}.lease")

    def auth(self, rec, transition, **extra):
        return {"slot": rec["slot"], "op_id": rec["op_id"], "transition": transition,
                "identity_kind": rec["identity_kind"], "identity": rec["identity"],
                "slot_generation": rec["slot_generation"], "seq": rec["seq"], "trust": rs.TRUST,
                "actor": "synthetic", "reason": "test fixture", "utc": "2026-11-09T00:00:00Z", **extra}

    def hook(self, slot):
        def replace(pre, post):
            path = self.leases / f"{slot}.lease"
            if pool_record.read_record(path) != pre:
                raise rs.Refused("record changed")
            pool_record.durable_replace(path, post)
            return post
        return replace

    def drive(self, slot, op, *steps, fault=None):
        """Runs `steps` (names from TRANSITIONS) on operation `op` of `slot`
        under its slot lock, with fabricated same-uid authorizations; returns
        the last record."""
        st = self.store(fault)
        with self.locked(slot):
            rec = None
            for step in steps:
                if step != "adopt":
                    rec = st._read(slot, op)[0]
                rec = self.step(st, slot, op, step, rec)
        return rec

    def step(self, st, slot, op, step, rec):
        live = self.live(slot)
        if step == "adopt":
            return st.adopt(slot, op, live)
        if step in ("request", "cancel"):
            a = self.auths.setdefault((slot, op, step), self.auth(rec, step))
            return getattr(st, step)(slot, op_id=op, authorization=a, record=live)
        if step == "quarantine":
            tok = self.rtok.setdefault((slot, op), secrets.token_hex(16))
            a = self.auths.setdefault((slot, op, step), self.auth(
                rec, step, assessment_digest="0" * 64, quiescence_method="owner-quiescence",
                preservation_disposition="test"))
            return st.quarantine(slot, op_id=op, authorization=a, recovery_token=tok, record=live,
                                 replace_record=self.hook(slot))
        tok = self.rtok[(slot, op)]
        bind = {k: v for k, v in self.auth(rec, step).items()
                if k in ("slot", "op_id", "transition", "identity_kind", "identity", "slot_generation", "seq")}
        if step == "quiescence":
            return st.verify_quiescence(slot, op_id=op, recovery_token=tok, record=live, evidence={
                **bind, "method": "owner-quiescence", "actor": "synthetic", "evidence_refs": ["test"],
                "accounted": {k: True for k in rs.QUIESCENCE_ACCOUNTED}})
        if step == "preservation":
            return st.record_preservation(slot, op_id=op, recovery_token=tok, record=live, evidence={
                **bind, "head": "a" * 40, "complete": True, "inventory": {k: [] for k in rs.INVENTORY},
                "preserved": {}})
        return st.mark_ready(slot, op_id=op, recovery_token=tok, record=live, final_check={
            **bind, "clean": True, "lease_absent": True, "head": "a" * 40,
            "preservation": rec["gates"]["preservation"]})

    def gate(self, slot):
        p = subprocess.run(["python3", "-I", "-B", str(HERE / "pool_record.py"), "gate", str(self.leases), slot],
                           capture_output=True, text=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        lines = p.stdout.splitlines()
        self.assertEqual([ln.split("=", 1)[0] for ln in lines], GATE_KEYS, p.stdout)
        out = dict(ln.split("=", 1) for ln in lines)
        self.assertEqual(out["slot"], slot)
        self.assertEqual(out["gate"], "open" if out["recovery"] in pool_record.GATE_OPEN else "blocked")
        return out

    def tree(self):
        """Every path under the lease directory with its type, mode, bytes and mtime."""
        out = {}
        for p in sorted(self.leases.rglob("*")):
            st = os.lstat(p)
            body = os.readlink(p) if stat.S_ISLNK(st.st_mode) else p.read_bytes() if stat.S_ISREG(st.st_mode) else None
            out[p.relative_to(self.leases).as_posix()] = (stat.S_IFMT(st.st_mode), stat.S_IMODE(st.st_mode), body,
                                                          st.st_mtime_ns)
        return out

    def mutators(self, slot, token):
        runs = [("acquire", "--slot", slot, "--task", "t", "--agent", "ag", *(["--review"] if slot == R else [])),
                ("release", "--slot", slot, "--token", token), ("prepare", "--slot", slot)]
        if slot == R:
            runs.append(("review-checkout", "--slot", R, "--token", token, "--branch", "v2.0", "--head", self.head(R)))
        return runs

    def assert_blocked(self, slot, token, cls=None):
        if cls:
            self.assertEqual(self.gate(slot)["recovery"], cls)
        before = self.tree(), [self.head(s) for s in SLOTS]
        for args in self.mutators(slot, token):
            p = self.pool(*args)
            self.assertEqual(p.returncode, 1, f"{args}: {p.stdout}{p.stderr}")
            if args[0] == "prepare" and "recovery state" in p.stderr:
                # The coordinator ignores prepare refusals that say "is leased".
                self.assertNotIn("is leased", p.stderr)
        self.assertEqual((self.tree(), [self.head(s) for s in SLOTS]), before, "a refused mutator changed the pool")
        header = self.status(slot)[0]
        self.assertNotEqual(header, f"{slot}: free")

    def status_of(self, slot):
        lines = self.status(slot)
        return lines[0].split(": ", 1)[1], dict(ln.strip().split("=", 1) for ln in lines[1:]
                                                if ln.startswith("  ") and "=" in ln)


class GateClasses(Recovery):
    def test_no_recovery_directory_keeps_the_existing_behavior(self):
        self.assertEqual(self.gate(A)["recovery"], "none")
        self.assertEqual(self.status(A), [f"{A}: free", "  lease=absent", "  record_state=free", "  generation=0"])
        slot, token = self.acquire(A)
        self.assertEqual(self.status_of(A)[0], "busy")
        self.assertNotIn("recovery", self.status_of(A)[1])
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)
        self.assertIn("recovery: none", self.pool("assess", A).stdout)

    def test_empty_unexpected_and_malformed_recovery_directories(self):
        root = self.leases / "recovery"

        def private(*parts):
            p = root.joinpath(*parts)
            p.mkdir(parents=True, exist_ok=True)
            for q in [root, *[root.joinpath(*parts[:i + 1]) for i in range(len(parts))]]:
                q.chmod(0o700)
            return p

        cases = {
            "empty recovery directory": (lambda: private(), "none", "none"),
            "empty slot directory": (lambda: private(A), "none", "none"),
            "recovery directory not private": (lambda: (private(), root.chmod(0o755)), "corrupt", "corrupt"),
            "recovery is a file": (lambda: root.write_text("x"), "corrupt", "corrupt"),
            "recovery is a symlink": (lambda: (private(), root.rename(self.t / "elsewhere"),
                                               root.symlink_to(self.t / "elsewhere")), "corrupt", "corrupt"),
            "unexpected top-level file": (lambda: (private(), (root / "notes.txt").write_text("x")),
                                          "corrupt", "corrupt"),
            "directory of an unknown slot": (lambda: private("codegraph-worker-c"), "corrupt", "corrupt"),
            "store lock of the slot": (lambda: private(f".{A}.lock"), "incomplete", "none"),
            "slot directory not private": (lambda: (private(A), (root / A).chmod(0o755)), "corrupt", "none"),
            "slot entry is a file": (lambda: (private(), (root / A).write_text("x")), "corrupt", "none"),
            "unexpected file in the slot directory": (lambda: (private(A), (root / A / "junk").write_text("x")),
                                                      "corrupt", "none"),
            "entry name with a newline": (lambda: (private(A), (root / A / "op\nfree").write_text("x")),
                                          "corrupt", "none"),
            "unreadable slot directory": (lambda: (private(A), (root / A / "op-1").mkdir(mode=0o700),
                                                   (root / A).chmod(0o000)), "unknown", "none"),
            "unreadable recovery directory": (lambda: (private(), root.chmod(0o000)), "unknown", "unknown"),
            "recovery directory listable, not searchable": (lambda: (private(A, "op-1"), root.chmod(0o600)),
                                                            "unknown", "unknown"),
            "recovery directory searchable, not listable": (lambda: (private(A, "op-1"), root.chmod(0o300)),
                                                            "unknown", "unknown"),
            "slot directory listable, not searchable": (lambda: (private(A, "op-1"), (root / A).chmod(0o600)),
                                                        "unknown", "none"),
            "empty slot directory, not searchable": (lambda: (private(A), (root / A).chmod(0o600)), "unknown", "none"),
            "operation directory listable, not searchable": (lambda: (private(A, "op-1"),
                                                                      (root / A / "op-1").chmod(0o600)),
                                                             "unknown", "none"),
            "operation directory without state": (lambda: private(A, "op-1"), "corrupt", "none"),
        }
        for name, (arrange, cls_a, cls_b) in cases.items():
            with self.subTest(case=name):
                self.setUp()
                arrange()
                try:
                    before = self.tree() if cls_a != "unknown" and cls_b != "unknown" else None
                    self.assertEqual(self.gate(A)["recovery"], cls_a)
                    self.assertEqual(self.gate(B)["recovery"], cls_b)
                    if before is not None:
                        self.assertEqual(self.tree(), before, "the gate changed the recovery state")
                    st = self.pool("status")
                    self.assertEqual(st.returncode, 0)
                    self.assertNotIn("op\nfree", st.stdout)
                    a = blocks(st.stdout)[A]
                    if cls_a == "none":
                        self.assertEqual(a[0], f"{A}: free")
                    else:
                        self.assertEqual(a[0], f"{A}: inconsistent")
                        self.assertIn(f"  recovery={cls_a}", a)
                        for args in self.mutators(A, "d" * 32):
                            self.assertEqual(self.pool(*args).returncode, 1, args)
                finally:
                    for p in (root, root / A, root / A / "op-1"):
                        if p.is_dir() and not p.is_symlink():
                            p.chmod(0o700)
                    if root.is_symlink():
                        root.unlink()
                    shutil.rmtree(self.t / "elsewhere", True)

    def test_an_unsearchable_lease_directory_is_never_no_recovery(self):
        for mode in (0o600, 0o000):
            with self.subTest(mode=oct(mode)):
                self.leases.chmod(mode)
                try:
                    p = subprocess.run(["python3", "-I", "-B", str(HERE / "pool_record.py"), "gate", str(self.leases), A],
                                       capture_output=True, text=True)
                    a = self.pool("assess", A)
                    st = self.pool("status", A)
                finally:
                    self.leases.chmod(0o755)
                self.assertEqual(p.returncode, 0)
                self.assertIn("recovery=unknown\ngate=blocked\n", p.stdout)
                self.assertIn("recovery: unknown;", a.stdout)
                self.assertNotEqual(st.returncode, 0)
                self.assertNotIn(f"{A}: free", st.stdout)

    def test_the_gate_reads_only(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "request")
        before = self.tree()
        for _ in range(3):
            self.gate(A)
            self.pool("status")
            self.pool("assess", A)
            self.pool("release", "--slot", A, "--token", token)
        self.assertEqual(self.tree(), before)


class Active(Recovery):
    def test_active_lets_the_original_holder_act(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt")
        self.assertEqual(self.gate(A)["recovery"], "active")
        state, d = self.status_of(A)
        self.assertEqual((state, d["owner"], d["recovery"], d["recovery_operation"], d["recovery_state"]),
                         ("busy", OWNER, "active", "op-1", "ACTIVE"))
        self.assertEqual(self.pool("acquire", "--slot", A, "--task", "t", "--agent", "ag").returncode, 1)
        self.assertEqual(self.pool("prepare", "--slot", A).returncode, 1)
        self.assertEqual(self.pool("release", "--slot", A, "--token", "d" * 32).returncode, 1)
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)
        # ACTIVE never makes a slot free: the adopted lease is gone, so the record no longer fits.
        self.assertEqual(self.gate(A)["recovery"], "blocked")
        self.assert_blocked(A, token)

    def test_active_lets_the_review_holder_check_out(self):
        _, token = self.acquire(R)
        self.drive(R, "op-1", "adopt")
        p = self.pool("review-checkout", "--slot", R, "--token", token, "--branch", "v2.0", "--head", self.head(R))
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.pool("release", "--slot", R, "--token", token).returncode, 0)

    def test_active_with_another_identity_generation_or_token_refuses(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt")
        adopted = self.live(A)
        r = rs.parse_record(adopted, A)
        other = {"identity": adopted.replace(r.identity, "2" * 32),
                 "generation": adopted.replace(f"generation={r.generation}\n", f"generation={r.generation + 1}\n"),
                 "token": adopted.replace(token, "e" * 32)}
        for name, text in other.items():
            with self.subTest(changed=name):
                self.put(A, text)
                self.assertEqual(self.gate(A)["recovery"], "blocked")
                tok = "e" * 32 if name == "token" else token
                self.assert_blocked(A, tok)
        self.put(A, adopted)
        self.assertEqual(self.gate(A)["recovery"], "active")
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)


class Requested(Recovery):
    def test_a_request_blocks_the_holder(self):
        _, ta = self.acquire(A)
        _, tr = self.acquire(R)
        self.drive(A, "op-1", "adopt", "request")
        self.drive(R, "op-1", "adopt", "request")
        for slot, tok in ((A, ta), (R, tr)):
            self.assert_blocked(slot, tok, "blocked")
            state, d = self.status_of(slot)
            self.assertEqual((state, d["recovery_state"], d["owner"]), ("busy", rs.REQUESTED, OWNER))
        p = self.pool("release", "--slot", A, "--token", ta)
        self.assertIn("recovery state is blocked (RECOVERY_REQUESTED)", p.stderr)
        # Other slots keep working.
        _, tb = self.acquire(B)
        self.assertEqual(self.pool("release", "--slot", B, "--token", tb).returncode, 0)

    def test_quarantined_through_preserved_block_every_mutator(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "request", "quarantine")
        for step, state in ((None, rs.QUARANTINED), ("quiescence", rs.QUIESCENT), ("preservation", rs.PRESERVED)):
            with self.subTest(state=state):
                if step:
                    self.drive(A, "op-1", step)
                self.assert_blocked(A, token, "blocked")
                st, d = self.status_of(A)
                self.assertEqual((st, d["record_state"], d["recovery_op"], d["recovery_state"]),
                                 ("inconsistent", "quarantined", "op-1", state))
        self.assertEqual(self.pool("unmigrate").returncode, 1)


class Incomplete(Recovery):
    def test_unfinished_transitions_block_until_the_exact_retry(self):
        cases = {"request after intent": ("request", "after_intent"),
                 "quarantine after the record": ("quarantine", "after_record"),
                 "quarantine before the state": ("quarantine", "before_state_rename"),
                 "cancel after completion, before the marker": ("cancel", "before_marker_rename"),
                 "cancel before the completion": ("cancel", "after_state")}
        for name, (step, point) in cases.items():
            with self.subTest(case=name):
                self.setUp()
                _, token = self.acquire(A)
                pre = ["adopt"] + (["request"] if step == "quarantine" else [])
                self.drive(A, "op-1", *pre)
                with self.assertRaises(rs.Refused):
                    self.drive(A, "op-1", step, fault=fault_at(point))
                self.assert_blocked(A, token, "incomplete")
                self.drive(A, "op-1", step)
                want = {"request": "blocked", "quarantine": "blocked", "cancel": "cancelled"}[step]
                self.assertEqual(self.gate(A)["recovery"], want)

    def test_an_interrupted_adoption_blocks(self):
        _, token = self.acquire(A)
        with self.assertRaises(rs.Refused):
            self.drive(A, "op-1", "adopt", fault=fault_at("before_publish"))
        self.assertEqual([p.name.startswith(".adopt-op-1.") for p in (self.leases / "recovery" / A).iterdir()], [True])
        self.assert_blocked(A, token, "incomplete")
        self.assertEqual(self.gate(A)["recovery_operation"], "")

    def test_a_leftover_temporary_file_blocks(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "cancel")
        tmp = self.leases / "recovery" / A / "op-1" / f".state.json.{'0' * 16}.tmp"
        tmp.write_text("{}")
        tmp.chmod(0o600)
        self.assert_blocked(A, token, "incomplete")
        tmp.unlink()
        self.assertEqual(self.gate(A)["recovery"], "cancelled")


class Cancelled(Recovery):
    def test_cancelled_with_the_adopted_lease_unchanged(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "request", "cancel")
        self.assertEqual(self.gate(A)["recovery"], "cancelled")
        state, d = self.status_of(A)
        self.assertEqual((state, d["owner"], d["recovery"], d["recovery_state"]), ("busy", OWNER, "cancelled",
                                                                                   rs.CANCELLED))
        # Cancellation never frees a lease: the record still decides.
        self.assertEqual(self.pool("acquire", "--slot", A, "--task", "t", "--agent", "ag").returncode, 1)
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)
        self.assertEqual(self.status_of(A)[0], "free")
        _, t2 = self.acquire(A)
        self.assertEqual(self.pool("release", "--slot", A, "--token", t2).returncode, 0)
        self.assertEqual(self.gate(A)["recovery"], "cancelled")

    def test_cancelled_after_a_proven_release(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt")
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)
        self.drive(A, "op-1", "cancel")
        state, d = self.status_of(A)
        self.assertEqual((state, d["recovery"], d["generation"]), ("free", "cancelled", "1"))
        _, t2 = self.acquire(A)
        self.assertEqual(self.rec(A).generation, 2)
        self.assertEqual(self.pool("release", "--slot", A, "--token", t2).returncode, 0)

    def test_cancelled_with_a_record_that_does_not_fit_stays_blocked(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "cancel")
        for name, text in {"free at an older generation": record("free", generation=0),
                           "same lease, other token": self.live(A).replace(token, "e" * 32),
                           "other lease, same generation": record(generation=1, lease_id="2" * 32),
                           "quarantined": record("quarantined", generation=2)}.items():
            with self.subTest(record=name):
                self.put(A, text)
                self.assert_blocked(A, token, "blocked")

    def test_missing_contradictory_or_forged_markers(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "request", "cancel")
        d = self.leases / "recovery" / A / "op-1"
        good = {n: (d / n).read_bytes() for n in ("TERMINAL", "state.json", "journal.jsonl")}

        def restore():
            for n, b in good.items():
                (d / n).write_bytes(b)
                (d / n).chmod(0o600)

        cases = {
            "marker missing": (lambda: (d / "TERMINAL").unlink(), "incomplete"),
            "marker of another seq": (lambda: (d / "TERMINAL").write_bytes(good["TERMINAL"].replace(b"seq=2", b"seq=1")),
                                      "corrupt"),
            "marker of another operation": (lambda: (d / "TERMINAL").write_bytes(
                good["TERMINAL"].replace(b"op_id=op-1", b"op_id=op-2")), "corrupt"),
            "marker naming READY": (lambda: (d / "TERMINAL").write_bytes(
                good["TERMINAL"].replace(b"CANCELLED", rs.READY.encode())), "corrupt"),
            "marker bytes corrupted": (lambda: (d / "TERMINAL").write_bytes(b"\x00garbage"), "corrupt"),
            "journal truncated, marker intact": (lambda: (d / "journal.jsonl").write_bytes(
                b"".join(good["journal.jsonl"].splitlines(keepends=True)[:-1])), "corrupt"),
            "state rewritten, marker intact": (lambda: (d / "state.json").write_bytes(
                good["state.json"].replace(b'"seq": 2', b'"seq": 3')), "corrupt"),
            "journal missing": (lambda: (d / "journal.jsonl").unlink(), "corrupt"),
            "state missing": (lambda: (d / "state.json").unlink(), "corrupt"),
            "marker is a symlink": (lambda: ((d / "TERMINAL").unlink(), (d / "TERMINAL").symlink_to(self.t / "x")),
                                    "corrupt"),
            "state readable by others": (lambda: (d / "state.json").chmod(0o644), "corrupt"),
        }
        for name, (arrange, cls) in cases.items():
            with self.subTest(case=name):
                arrange()
                self.assert_blocked(A, token, cls)
                if (d / "TERMINAL").is_symlink():
                    (d / "TERMINAL").unlink()
                restore()
                self.assertEqual(self.gate(A)["recovery"], "cancelled")

    def test_a_forged_marker_on_an_unfinished_operation_is_corrupt(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "request")
        m = self.leases / "recovery" / A / "op-1" / "TERMINAL"
        m.write_bytes(f"state={rs.CANCELLED}\nseq=1\nop_id=op-1\n".encode())
        m.chmod(0o600)
        self.assert_blocked(A, token, "corrupt")

    def test_a_consistent_forgery_opens_the_gate_by_design(self):
        """Documented limitation: any process of the owner's uid can build a
        consistent CANCELLED operation with the library, or delete one. The gate
        proves consistency, not owner approval."""
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "request", "cancel")
        self.assertEqual(self.gate(A)["recovery"], "cancelled")
        self.assertEqual(rs.TRUST, "same-uid-attestation")
        doc = (HERE.parent.parent / "docs" / "agent-pool.md").read_text()
        self.assertIn("proves internal consistency, not owner approval", doc)
        self.assertIn("does not make live recovery secure", doc)


class Ready(Recovery):
    def test_ready_stays_unavailable(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", *TRANSITIONS[:2], *TRANSITIONS[3:])
        self.assertEqual(self.store().terminal(A, "op-1"), rs.READY)
        self.assert_blocked(A, token, "blocked")
        state, d = self.status_of(A)
        self.assertEqual((state, d["record_state"], d["recovery_state"]), ("inconsistent", "quarantined", rs.READY))
        p = self.pool("acquire", "--slot", "auto", "--task", "t", "--agent", "ag")
        self.assertEqual(p.returncode, 0)
        self.tokens.append(p.stdout.split("token=")[1].strip())
        self.assertIn(B, p.stdout)

    def test_ready_with_a_hand_written_record_stays_unavailable(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", *TRANSITIONS[:2], *TRANSITIONS[3:])
        for name, text in {"free": record("free", generation=2), "leased": record(generation=3, lease_token=token)}.items():
            with self.subTest(record=name):
                self.put(A, text)
                self.assert_blocked(A, token, "blocked")
                self.assertEqual(self.status_of(A)[1]["recovery_state"], rs.READY)


class Histories(Recovery):
    def copy_operation(self, slot, op, into):
        shutil.copytree(self.leases / "recovery" / slot / op, self.leases / "recovery" / slot / into, symlinks=True)

    def test_multiple_operations_with_different_terminal_states(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "cancel")
        self.drive(A, "op-2", "adopt", "request", "cancel")
        self.assertEqual(self.gate(A)["recovery"], "cancelled")
        self.drive(A, "op-3", "adopt")
        g = self.gate(A)
        self.assertEqual((g["recovery"], g["recovery_operation"]), ("active", "op-3"))
        self.drive(A, "op-3", "request")
        g = self.gate(A)
        self.assertEqual((g["recovery"], g["recovery_operation"], g["recovery_state"]), ("blocked", "op-3", rs.REQUESTED))
        self.drive(A, "op-3", "quarantine", "quiescence", "preservation", "ready")
        self.assert_blocked(A, token, "blocked")
        self.assertEqual(self.gate(A)["recovery_state"], rs.READY)

    def test_two_unfinished_operations_are_corrupt(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt")
        # A second ACTIVE operation the library would never adopt next to the first.
        other = self.t / "other"
        shutil.rmtree(other, True)
        st = rs.RecoveryStore(other)
        st.adopt(A, "op-2", self.live(A))
        shutil.copytree(other / A / "op-2", self.leases / "recovery" / A / "op-2")
        self.assert_blocked(A, token, "corrupt")

    def test_a_corrupt_earlier_operation_blocks_a_valid_later_one(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "cancel")
        self.drive(A, "op-2", "adopt", "cancel")
        self.assertEqual(self.gate(A)["recovery"], "cancelled")
        (self.leases / "recovery" / A / "op-1" / "journal.jsonl").write_text("{}\n")
        self.assert_blocked(A, token, "corrupt")

    def test_unexpected_entries_inside_an_operation_are_corrupt(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "cancel")
        junk = self.leases / "recovery" / A / "op-1" / "notes"
        junk.write_text("x")
        junk.chmod(0o600)
        self.assert_blocked(A, token, "corrupt")
        junk.unlink()
        self.assertEqual(self.gate(A)["recovery"], "cancelled")

    def test_an_operation_on_a_lease_that_is_not_version_2_is_corrupt(self):
        _, token = self.acquire(A)
        other = self.t / "other-v1"
        shutil.rmtree(other, True)
        rs.RecoveryStore(other).adopt(A, "op-1", v1_lease())
        (self.leases / "recovery").mkdir(mode=0o700)
        shutil.copytree(other / A, self.leases / "recovery" / A)
        self.assert_blocked(A, token, "corrupt")

    def test_symlinked_operation_or_slot_directories_are_corrupt(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "cancel")
        root = self.leases / "recovery"
        (root / A / "op-1").rename(self.t / "op-moved")
        (root / A / "op-1").symlink_to(self.t / "op-moved")
        self.assert_blocked(A, token, "corrupt")
        (root / A / "op-1").unlink()
        (self.t / "op-moved").rename(root / A / "op-1")
        (root / A).rename(self.t / "slot-moved")
        (root / A).symlink_to(self.t / "slot-moved")
        self.assert_blocked(A, token, "corrupt")
        (root / A).unlink()
        (self.t / "slot-moved").rename(root / A)
        self.assertEqual(self.gate(A)["recovery"], "cancelled")


class Interpreter(Recovery):
    def wrapper(self, name, body):
        """A python3 that runs the real one except for the gate command."""
        d = self.t / "fakegate"
        d.mkdir(exist_ok=True)
        real = shutil.which("python3")
        p = d / name
        p.write_text(f'#!/bin/sh\ncase " $* " in *" gate "*) {body} ;; esac\nexec "{real}" "$@"\n')
        p.chmod(0o755)
        return str(p)

    def test_a_failing_or_lying_gate_is_never_open(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "cancel")
        good = "slot={s}\\nrecovery=none\\ngate=open\\nrecovery_operation=\\nrecovery_state=\\nreason="
        fakes = {
            "exits 1": "exit 1",
            "exits 3": "exit 3",
            "killed": "kill -9 $$",
            "prints nothing": "exit 0",
            "open, but for another slot": f"printf '{good.format(s=B)}\\n'; exit 0",
            "open, but gate and class disagree": "printf 'slot=%s\\nrecovery=blocked\\ngate=open\\n"
                                                 "recovery_operation=\\nrecovery_state=\\nreason=\\n' \"$6\"; exit 0",
            "open, with a line missing": "printf 'slot=%s\\nrecovery=none\\ngate=open\\n' \"$6\"; exit 0",
            "open, with an extra line": f"printf 'slot=%s\\nrecovery=none\\ngate=open\\nrecovery_operation=\\n"
                                        f"recovery_state=\\nreason=\\nextra=1\\n' \"$6\"; exit 0",
            "open, but exit 4": "printf 'slot=%s\\nrecovery=none\\ngate=open\\nrecovery_operation=\\n"
                                "recovery_state=\\nreason=\\n' \"$6\"; exit 4",
        }
        before = self.tree()
        for name, body in fakes.items():
            with self.subTest(fake=name):
                py = self.wrapper(name.replace(" ", "-").replace(",", ""), body)
                for args in self.mutators(A, token):
                    p = self.pool(*args, CG_POOL_PYTHON=py)
                    self.assertEqual(p.returncode, 1, f"{args}: {p.stdout}{p.stderr}")
                st = self.pool("status", A, CG_POOL_PYTHON=py)
                self.assertEqual(st.returncode, 0)
                lines = blocks(st.stdout)[A]
                self.assertNotEqual(lines[0], f"{A}: free")
                self.assertIn("  recovery=unknown", lines)
                self.assertEqual(self.tree(), before)
        # The real gate opens it: the holder may release.
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)

    def test_an_unsupported_platform_never_reads_as_no_recovery(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "cancel")
        with mock.patch.object(rs, "SUPPORTED", False):
            self.assertEqual(pool_record.recovery_gate(self.leases, A)[0], "unknown")
            self.assertEqual(pool_record.recovery_gate(self.leases, B)[0], "unknown")
            shutil.rmtree(self.leases / "recovery")
            self.assertEqual(pool_record.recovery_gate(self.leases, A)[0], "none")


class Locks(Recovery):
    def template(self, *steps):
        """A recovery directory for slot A of the current record, built in another pool directory."""
        self.drive(A, "op-1", *steps)
        tpl = self.t / "template"
        shutil.rmtree(tpl, True)
        shutil.copytree(self.leases / "recovery", tpl, symlinks=True)
        shutil.rmtree(self.leases / "recovery")
        return tpl

    def shim(self, tpl):
        d = self.t / "shim"
        d.mkdir(exist_ok=True)
        (d / "mkdir").write_text(
            '#!/bin/sh\n/bin/mkdir "$@" || exit $?\n'
            'for a in "$@"; do case $a in *.lock) [ -e "$REC" ] || /bin/cp -Rp "$TPL" "$REC" ;; esac; done\n')
        (d / "mkdir").chmod(0o755)
        return {"PATH": f"{d}:{os.environ['PATH']}", "TPL": str(tpl), "REC": str(self.leases / "recovery")}

    def test_a_held_slot_lock_refuses(self):
        _, token = self.acquire(A)
        with self.locked(A):
            for args in self.mutators(A, token):
                p = self.pool(*args)
                self.assertEqual(p.returncode, 1)
                self.assertIn("lock held", p.stderr)

    def test_recovery_state_that_appears_after_the_lock_is_taken_is_seen(self):
        for steps in (("adopt", "request"), ("adopt", "request", "quarantine")):
            with self.subTest(state=steps[-1]):
                self.setUp()
                _, token = self.acquire(A)
                before = self.live(A)
                tpl = self.template(*steps)
                if steps[-1] == "quarantine":
                    quarantined = self.live(A)
                    self.put(A, before)
                env = self.shim(tpl)
                self.assertFalse((self.leases / "recovery").exists())
                if steps[-1] == "quarantine":
                    # The record is replaced together with the recovery state, as a quarantine would.
                    (self.t / "shim" / "mkdir").write_text(
                        (self.t / "shim" / "mkdir").read_text() + f"printf '%s' '{quarantined}' >'{self.leases / A}.lease'\n")
                p = self.pool("release", "--slot", A, "--token", token, **env)
                self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
                self.assertIn("recovery state is", p.stderr)
                self.assertNotEqual(rs.parse_record(self.live(A), A).state, "free")

    def test_the_helper_rechecks_the_gate_under_the_lock(self):
        _, token = self.acquire(A)
        r = self.rec(A)
        self.drive(A, "op-1", "adopt", "request")
        with self.locked(A):
            for call in (lambda: pool_record.owns(A, self.leases / f"{A}.lease", token),
                         lambda: pool_record.release(A, self.leases / f"{A}.lease", r.generation, r.identity, token)):
                with self.assertRaisesRegex(rs.Refused, "recovery state is blocked"):
                    call()
            p = subprocess.run(["python3", "-I", "-B", str(HERE / "pool_record.py"), "owns", A,
                                str(self.leases / f"{A}.lease")], input=token + "\n", capture_output=True, text=True)
            self.assertEqual(p.returncode, 1)
            self.assertNotIn(token, p.stdout + p.stderr)

    def test_a_request_and_a_release_are_serialised_by_the_slot_lock(self):
        """pool.sh never waits for a lock: while a recovery transition holds
        the slot lock the release refuses, and once the request is in place
        the release is refused by the gate. In the other order the released
        record is no adoption source."""
        _, token = self.acquire(A)
        holding, done = threading.Event(), threading.Event()

        def request():
            with self.locked(A):
                holding.set()
                st = self.store()
                rec = st.adopt(A, "op-1", self.live(A))
                done.wait(60)
                st.request(A, op_id="op-1", authorization=self.auth(rec, "request"), record=self.live(A))

        t = threading.Thread(target=request)
        t.start()
        self.assertTrue(holding.wait(60))
        p = self.pool("release", "--slot", A, "--token", token)
        done.set()
        t.join()
        self.assertEqual(p.returncode, 1)
        self.assertIn("lock held", p.stderr)
        p = self.pool("release", "--slot", A, "--token", token)
        self.assertEqual(p.returncode, 1)
        self.assertIn("recovery state is blocked (RECOVERY_REQUESTED)", p.stderr)
        self.assertEqual(rs.parse_record(self.live(A), A).state, "leased")

        self.setUp()
        _, token = self.acquire(A)
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)
        with self.locked(A), self.assertRaisesRegex(rs.Refused, "never an adoption source"):
            self.store().adopt(A, "op-1", self.live(A))
        self.assertEqual(self.gate(A)["recovery"], "none")


class Rollback(Recovery):
    def test_unmigrate_refuses_while_any_recovery_entry_exists(self):
        _, token = self.acquire(A)
        self.drive(A, "op-1", "adopt", "cancel")
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)
        self.put(A, pool_record.free_record(A, 0))
        before = self.tree()
        p = self.pool("unmigrate")
        self.assertEqual(p.returncode, 1)
        self.assertIn(f"{A}: refused: recovery state present", p.stdout)
        self.assertEqual(self.tree(), before)


class NoRecoveryCommand(Recovery):
    def test_no_command_creates_or_advances_an_operation(self):
        for cmd in ("adopt", "request", "cancel", "quarantine", "quiescence", "preservation", "ready", "recover",
                    "reconcile"):
            p = subprocess.run(["python3", "-I", "-B", str(HERE / "pool_record.py"), cmd, str(self.leases), A],
                               capture_output=True, text=True)
            self.assertEqual(p.returncode, 2, cmd)
            self.assertEqual(self.pool(cmd, "--slot", A).returncode, 2, cmd)
            self.assertEqual(self.pool("recover", cmd, "--slot", A).returncode, 2, cmd)
        self.assertFalse((self.leases / "recovery").exists())
        writers = {"adopt", "request", "cancel", "quarantine", "verify_quiescence", "record_preservation",
                   "mark_ready", "reconcile", "_step", "_commit", "_complete", "_append", "_replace", "_ensure_dir",
                   "_ensure_marker", "_lock"}
        tree = ast.parse((HERE / "pool_record.py").read_text())
        called = {n.func.attr for n in ast.walk(tree) if isinstance(n, ast.Call) and isinstance(n.func, ast.Attribute)}
        self.assertEqual(called & writers, set())
        self.assertNotIn("held_lock", (HERE / "pool_record.py").read_text())
        self.assertNotIn("recovery_state.py", (HERE / "pool.sh").read_text())


class OldCopies(Recovery):
    """Old pool.sh copies against a migrated pool with recovery state."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.copies = {}
        for commit, blob in HISTORY:
            p = subprocess.run(["git", "-C", str(HERE), "show", f"{commit}:scripts/agent-pool/pool.sh"],
                               capture_output=True, text=True)
            if p.returncode != 0:
                raise unittest.SkipTest(f"git history for {commit} is not available")
            path = cls.t / f"old-{blob}.sh"
            path.write_text(p.stdout)
            cls.copies[blob] = path
        bundle = cls.t / "bundle-2b2b"
        bundle.mkdir()
        for name in ("pool.sh", "pool_record.py", "recovery_state.py"):
            p = subprocess.run(["git", "-C", str(HERE), "show", f"{PRESENCE_FENCE}:scripts/agent-pool/{name}"],
                               capture_output=True, text=True)
            if p.returncode != 0:
                raise unittest.SkipTest(f"git history for {PRESENCE_FENCE} is not available")
            (bundle / name).write_text(p.stdout)
        cls.copies["2b-2b"] = bundle / "pool.sh"

    def test_old_copies_never_change_a_slot_with_recovery_state(self):
        scenarios = {"ACTIVE": ("adopt",), "REQUESTED": ("adopt", "request"), "CANCELLED": ("adopt", "cancel"),
                     "READY": TRANSITIONS[:2] + TRANSITIONS[3:]}
        for name, steps in scenarios.items():
            self.setUp()
            _, ta = self.acquire(A)
            _, tb = self.acquire(B)
            _, tr = self.acquire(R)
            self.drive(A, "op-1", *steps)
            for blob, script in self.copies.items():
                with self.subTest(state=name, copy=blob):
                    before = self.tree(), [self.head(s) for s in SLOTS]
                    for args in (("acquire", "--slot", A, "--task", "x", "--agent", "y"),
                                 ("acquire", "--slot", "auto", "--task", "x", "--agent", "y"),
                                 ("release", "--slot", A, "--token", ta),
                                 ("reclaim", "--slot", A, "--token", ta, "--reason", "t"),
                                 ("prepare", "--slot", A)):
                        p = Pool.pool(self, *args, script=script)
                        self.assertNotEqual(p.returncode, 0, f"{blob} {args} under {name}: {p.stdout}{p.stderr}")
                        for t in (ta, tb, tr):
                            self.assertNotIn(t, p.stdout + p.stderr)
                    st = Pool.pool(self, "status", script=script)
                    for t in (ta, tb, tr):
                        self.assertNotIn(t, st.stdout + st.stderr)
                    if A in blocks(st.stdout):
                        self.assertNotEqual(blocks(st.stdout)[A][0], f"{A}: free")
                    self.assertEqual((self.tree(), [self.head(s) for s in SLOTS]), before)


class Consumers(Recovery):
    """Unchanged coordinator and campaign readers, current and older, against every status shape."""

    def readers(self):
        import campaign
        import coordinator as co
        out = [("current", co)]
        src = subprocess.run(["git", "-C", str(HERE), "show", f"{OLD_COORDINATOR}:scripts/agent-coordinator/coordinator.py"],
                             capture_output=True, text=True)
        if src.returncode == 0:
            path = self.t / f"coordinator_{OLD_COORDINATOR}.py"
            path.write_text(src.stdout)
            with mock.patch.dict(os.environ, self.env, clear=True):
                spec = importlib.util.spec_from_file_location(f"coordinator_{OLD_COORDINATOR}", path)
                old = importlib.util.module_from_spec(spec)
                spec.loader.exec_module(old)
            out.append((OLD_COORDINATOR, old))
        return campaign, out

    def parse(self, co, *slots):
        with mock.patch.dict(os.environ, self.env, clear=True), mock.patch.object(co, "POOL", POOL), \
                mock.patch.object(co.Coordinator, "pool", lambda c, *a: subprocess.run(
                    ["/bin/bash", str(POOL), *a, *slots], capture_output=True, text=True, env=self.env)):
            c = object.__new__(co.Coordinator)
            c.dir = self.t / "coord"
            return c, c.pool_status()

    def test_every_recovery_shape_keeps_slots_owners_and_tokens(self):
        _, ta = self.acquire(A)
        _, tb = self.acquire(B)
        _, tr = self.acquire(R)
        self.drive(A, "op-1", "adopt", "request")                       # busy, blocked
        self.drive(B, "op-1", "adopt", "cancel")                        # busy, cancelled
        self.assertEqual(self.pool("release", "--slot", B, "--token", tb).returncode, 0)   # free, cancelled
        self.drive(R, "op-1", *TRANSITIONS[:2], *TRANSITIONS[3:])       # quarantined, READY
        campaign, readers = self.readers()
        self.assertGreater(len(readers), 1, "the older coordinator could not be extracted")
        for order in ((A, B, R), (R, B, A), (B,), (R, A)):
            for name, co in readers:
                with self.subTest(reader=name, order=order):
                    c, status = self.parse(co, *order)
                    self.assertEqual(sorted(status), sorted(order))
                    if A in order:
                        v = status[A]
                        self.assertEqual((v["state"], v["owner"], v["slot"], v["recovery"], v["recovery_state"]),
                                         ("busy", OWNER, A, "blocked", rs.REQUESTED))
                        if hasattr(c, "lease_matches"):
                            self.assertTrue(c.lease_matches({"lease_txn": TXN, "id": "t"}, v))
                        self.assertFalse(campaign.ci_lease_absent(A, v, OWNER))
                    if B in order:
                        v = status[B]
                        self.assertEqual((v["state"], v["lease"], v["recovery"], v["recovery_state"]),
                                         ("free", "absent", "cancelled", rs.CANCELLED))
                        self.assertNotIn("owner", v)
                    if R in order:
                        v = status[R]
                        self.assertEqual((v["state"], v["record_state"], v["recovery_op"], v["recovery_state"]),
                                         ("inconsistent", "quarantined", "op-1", rs.READY))
                        self.assertFalse(campaign.ci_lease_absent(R, v, OWNER))
                        if hasattr(c, "lease_matches"):
                            self.assertTrue(c.lease_matches({"lease_txn": TXN, "id": "t"}, v))
        # A refused release leaves the lease and its token where they were.
        p = self.pool("release", "--slot", A, "--token", ta)
        self.assertEqual(p.returncode, 1)
        self.assertEqual(rs.parse_record(self.live(A), A).state, "leased")

    def test_an_unknown_gate_is_never_free_for_any_reader(self):
        (self.leases / "recovery").mkdir(mode=0o755)
        campaign, readers = self.readers()
        for name, co in readers:
            with self.subTest(reader=name):
                _, status = self.parse(co)
                for s in SLOTS:
                    self.assertEqual((status[s]["state"], status[s]["recovery"]), ("inconsistent", "corrupt"))
                    self.assertFalse(campaign.ci_lease_absent(s, status[s], OWNER))


if __name__ == "__main__":
    unittest.main()

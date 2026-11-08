#!/usr/bin/env python3
"""Version 2 writes behind the migration marker: migrate, unmigrate, acquire,
release, prepare and review-checkout in a migrated pool, durable-write
failures, races, every historical pool.sh against the records this pool.sh
writes, and the coordinator and campaign readers. Every pool is a throwaway:
HOME and every CG_POOL_* point into a temporary directory.

    python3 -m unittest test_pool_v2_writes      (from scripts/agent-pool)
"""

import errno
import os
import re
import secrets
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from test_pool_v2 import (A, B, HISTORY, LEASE_ID, LIVE_POOL, OWNER, POOL, R, TOKEN, TXN, HERE,
                          Pool, blocks, git, record, v1_lease)
import pool_record  # noqa: E402
import recovery_state as rs  # noqa: E402

SLOTS = (A, B, R)
LEASED = re.compile(r"^leased (\S+) token=([0-9a-f]{32})$", re.M)
SOURCES = {HERE.parent / "agent-coordinator" / "coordinator.py", HERE.parent / "agent-coordinator" / "campaign.py"}


def initial(slot):
    return pool_record.free_record(slot, 0)


class Migrated(Pool):
    """A pool this pool.sh has migrated, plus helpers to drive it."""

    def pool(self, *args, **env):
        p = super().pool(*args, **env)
        self.outputs.append(p.stdout + p.stderr)
        return p

    def setUp(self):
        super().setUp()
        self.outputs = []
        for s in SLOTS:
            git("-C", str(self.t / "slots" / s), "checkout", "-q", "--detach", "origin/v2.0")

    def tearDown(self):
        # No token ever reaches an output other than acquire's own line.
        for out in self.outputs:
            for tok in LEASED.findall(out):
                out = out.replace(f"leased {tok[0]} token={tok[1]}", "")
            for tok in getattr(self, "tokens", []):
                self.assertNotIn(tok, out)
        for p in self.leases.rglob("*"):
            for tok in getattr(self, "tokens", []):
                self.assertNotIn(tok, p.name)
        self.assertEqual([p.name for p in self.leases.iterdir() if p.name.endswith(".lock")], [],
                         "a slot lock was left behind")
        self.assertEqual([p.name for p in self.leases.iterdir() if p.name.endswith(".tmp")], [],
                         "a temporary record was left behind")

    def migrate(self):
        p = self.pool("migrate")
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        return p

    def acquire(self, slot="auto", *extra, **env):
        args = ["acquire", "--slot", slot, "--task", "t", "--agent", "ag", "--owner", OWNER, *extra]
        if slot == R:
            args.append("--review")
        p = self.pool(*args, **env)
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        got = LEASED.fullmatch(p.stdout.strip())
        self.assertTrue(got, p.stdout)
        self.tokens = getattr(self, "tokens", []) + [got.group(2)]
        return got.group(1), got.group(2)

    def rec(self, slot):
        return rs.parse_record((self.leases / f"{slot}.lease").read_text(), slot)

    def snapshot(self):
        files = {}
        for p in sorted(self.leases.rglob("*")):
            rel = p.relative_to(self.leases).as_posix()
            files[rel] = os.readlink(p) if p.is_symlink() else p.read_bytes() if p.is_file() else "dir"
        return files, [self.head(s) for s in SLOTS]


class Migrate(Migrated):
    def test_dry_run_reports_and_writes_nothing(self):
        before = self.snapshot()
        p = self.pool("migrate", "--dry-run")
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        for s in SLOTS:
            self.assertIn(f"{s}: would write a free record of generation 0", p.stdout)
        self.assertIn("migration would proceed", p.stdout)
        self.assertEqual(self.snapshot(), before)

    def test_migrate_writes_the_initial_records_then_the_marker(self):
        p = self.migrate()
        self.assertIn("migrated: marker written", p.stdout)
        self.assertEqual((self.leases / ".schema").read_text(), "2\n")
        for s in SLOTS:
            path = self.leases / f"{s}.lease"
            self.assertEqual(path.read_text(), initial(s))
            self.assertTrue(stat.S_ISREG(os.lstat(path).st_mode))
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertEqual(self.status(s), [f"{s}: free", "  lease=absent", "  record_state=free",
                                              "  generation=0"])
        self.assertEqual(self.pool("migrate").returncode, 1, "a second migrate must refuse")

    def test_any_unsafe_slot_refuses_the_whole_migration(self):
        dirt = self.t / "slots" / B / "dirt.txt"
        cases = {
            "v1 lease": lambda: self.put(A, v1_lease()),
            "legacy lease": lambda: self.put(A, v1_lease(version=None, lease_id=None)),
            "v2 leased": lambda: self.put(A, record()),
            "v2 quarantined": lambda: self.put(A, record("quarantined")),
            "v2 free, later generation": lambda: self.put(A, record("free", generation=1)),
            "invalid record": lambda: self.put(A, record(token=TOKEN)),
            "dangling link": lambda: (self.leases / f"{A}.lease").symlink_to(self.t / "nowhere"),
            "dirty slot": lambda: dirt.write_text("x"),
            "on a branch": lambda: git("-C", str(self.t / "slots" / R), "checkout", "-q", "-B", "work"),
            "recovery state": lambda: (self.leases / "recovery" / R).mkdir(parents=True),
            "process in slot": lambda: (self.t / "cwds").write_text(str(self.t / "slots" / B) + "\n"),
            "slot lock held": lambda: (self.leases / f".{R}.lock").mkdir(),
            "marker present": lambda: (self.leases / ".schema").write_text("2\n"),
            "unknown marker": lambda: (self.leases / ".schema").write_text("unmigrating\n"),
        }
        for name, arrange in cases.items():
            with self.subTest(case=name):
                self.setUp()
                arrange()
                before = self.snapshot()
                for args in (("migrate", "--dry-run"), ("migrate",)):
                    p = self.pool(*args)
                    self.assertEqual(p.returncode, 1, f"{name} {args}: {p.stdout}{p.stderr}")
                    self.assertNotIn(TOKEN, p.stdout + p.stderr)
                    self.assertEqual(self.snapshot(), before, f"{name} {args} changed the pool")
                if dirt.exists():
                    dirt.unlink()
                (self.t / "cwds").write_text("")
                shutil.rmtree(self.leases / f".{R}.lock", True)
        self.setUp()

    def test_an_interrupted_migration_is_fenced_and_resumes(self):
        (self.leases / f"{A}.lease").write_text(initial(A))
        (self.leases / f"{A}.lease").chmod(0o600)
        # No marker yet: the written record fences A, the other slots stay version 1.
        self.assertEqual(self.pool("acquire", "--slot", A, "--task", "t", "--agent", "ag").returncode, 1)
        self.assertEqual(self.pool("prepare", "--slot", A).returncode, 1)
        p = self.pool("migrate", "--dry-run")
        self.assertIn(f"{A}: already holds the free record of generation 0", p.stdout)
        self.migrate()
        for s in SLOTS:
            self.assertEqual((self.leases / f"{s}.lease").read_text(), initial(s))


class Lifecycle(Migrated):
    def test_acquire_release_and_reacquire_move_the_generation(self):
        self.migrate()
        slot, token = self.acquire()
        self.assertEqual(slot, A)
        r = self.rec(A)
        self.assertEqual((r.state, r.generation, r.token, r.holder["owner"], r.holder["task"]),
                         ("leased", 1, token, OWNER, "t"))
        text = (self.leases / f"{A}.lease").read_text()
        self.assertNotIn("\ntoken=", "\n" + text)
        lines = self.status(A)
        self.assertEqual(lines[0], f"{A}: busy")
        for want in ("  record_state=leased", f"  owner={OWNER}", "  generation=1", f"  lease_id={r.identity}"):
            self.assertIn(want, lines)
        first_id = r.identity
        self.assertEqual(self.pool("release", "--slot", A, "--token", "b" * 32).returncode, 1)
        self.assertEqual(self.rec(A).state, "leased")
        p = self.pool("release", "--slot", A, "--token", token)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual((self.leases / f"{A}.lease").read_text(), pool_record.free_record(A, 1))
        self.assertEqual(self.status(A), [f"{A}: free", "  lease=absent", "  record_state=free", "  generation=1"])
        # A released token never works again, not even on the next lease.
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 1)
        slot, token2 = self.acquire(A)
        r2 = self.rec(A)
        self.assertEqual(r2.generation, 2)
        self.assertNotEqual(r2.identity, first_id)
        self.assertNotEqual(token2, token)
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 1)
        self.assertEqual(self.rec(A).token, token2)
        self.assertEqual(self.pool("release", "--slot", A, "--token", token2).returncode, 0)
        self.assertEqual(self.rec(A).generation, 2)

    def test_auto_acquire_takes_free_workers_then_reports_busy(self):
        self.migrate()
        self.assertEqual(self.acquire()[0], A)
        self.assertEqual(self.acquire()[0], B)
        p = self.pool("acquire", "--slot", "auto", "--task", "t", "--agent", "ag")
        self.assertEqual(p.returncode, 3)
        self.assertIn("BLOCKED_POOL_BUSY", p.stderr)

    def test_release_keeps_the_lease_when_the_slot_is_not_releasable(self):
        self.migrate()
        _, token = self.acquire(A)
        dirt = self.t / "slots" / A / "wip.txt"
        dirt.write_text("wip")
        before = self.snapshot()
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 1)
        self.assertEqual(self.snapshot(), before)
        dirt.unlink()
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)

    def test_review_slot_lease_checkout_and_release(self):
        self.migrate()
        _, token = self.acquire(R)
        self.assertEqual(self.rec(R).generation, 1)
        head = self.head(R)
        self.assertEqual(self.pool("review-checkout", "--slot", R, "--token", "c" * 32, "--branch", "v2.0",
                                   "--head", head).returncode, 1)
        p = self.pool("review-checkout", "--slot", R, "--token", token, "--branch", "v2.0", "--head", head)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(self.rec(R).state, "leased", "review-checkout must not change the record")
        self.assertEqual(self.pool("release", "--slot", R, "--token", token).returncode, 0)

    def test_prepare_moves_free_slots_and_refuses_leased_ones(self):
        self.migrate()
        _, token = self.acquire(A)
        seed = self.t / "seed"
        git("-C", str(seed), "commit", "-q", "--allow-empty", "-m", "next")
        git("-C", str(seed), "push", "-q", "origin", "HEAD:v2.0")
        self.addCleanup(self.reset_base)
        before = (self.leases / f"{A}.lease").read_bytes()
        p = self.pool("prepare", "--all")
        self.assertEqual(p.returncode, 1)
        self.assertIn(f"{A} is leased; not prepared", p.stderr)
        self.assertIn(f"prepared {B}", p.stdout)
        self.assertEqual((self.leases / f"{A}.lease").read_bytes(), before)
        self.assertEqual((self.leases / f"{B}.lease").read_text(), initial(B), "prepare must not write a record")
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)

    def reset_base(self):
        seed = self.t / "seed"
        git("-C", str(seed), "push", "-q", "-f", "origin", f"{self.base}:v2.0")
        for s in SLOTS:
            git("-C", str(self.t / "slots" / s), "fetch", "-q", "--prune", "origin")
            git("-C", str(self.t / "slots" / s), "checkout", "-q", "--detach", "origin/v2.0")

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.base = git("-C", str(cls.t / "seed"), "rev-parse", "origin/v2.0").stdout.strip()

    def test_a_missing_record_after_migration_is_never_free(self):
        self.migrate()
        (self.leases / f"{B}.lease").unlink()
        lines = self.status(B)
        self.assertEqual(lines[0], f"{B}: inconsistent")
        self.assertIn("  record_state=missing", lines)
        before = self.snapshot()
        for args in (("acquire", "--slot", B, "--task", "t", "--agent", "ag"), ("prepare", "--slot", B),
                     ("release", "--slot", B, "--token", TOKEN)):
            p = self.pool(*args)
            self.assertEqual(p.returncode, 1, f"{args}: {p.stdout}{p.stderr}")
            self.assertIn("missing", p.stderr)
        self.assertEqual(self.snapshot(), before)
        self.assertEqual(self.acquire()[0], A)
        self.assertEqual(self.pool("acquire", "--slot", "auto", "--task", "t", "--agent", "ag").returncode, 3)
        self.assertFalse((self.leases / f"{B}.lease").exists(), "no command may recreate a missing record")

    def test_a_version_1_lease_in_a_migrated_pool_is_refused(self):
        self.migrate()
        self.put(A, v1_lease())
        self.assertEqual(self.status(A)[0], f"{A}: busy")
        before = self.snapshot()
        for args in (("release", "--slot", A, "--token", TOKEN), ("prepare", "--slot", A),
                     ("acquire", "--slot", A, "--task", "t", "--agent", "ag")):
            self.assertEqual(self.pool(*args).returncode, 1, args)
        self.assertEqual(self.snapshot(), before)

    def test_quarantined_and_invalid_records_are_never_changed(self):
        self.migrate()
        for text in (record("quarantined"), record(token=TOKEN), record("free", generation="01")):
            with self.subTest(record=text):
                self.put(A, text)
                before = self.snapshot()
                for args in (("acquire", "--slot", A, "--task", "t", "--agent", "ag"),
                             ("release", "--slot", A, "--token", TOKEN), ("prepare", "--slot", A)):
                    self.assertEqual(self.pool(*args).returncode, 1, args)
                self.assertEqual(self.pool("acquire", "--slot", "auto", "--task", "t", "--agent", "ag").returncode, 0)
                self.assertEqual((self.leases / f"{A}.lease").read_bytes(), before[0][f"{A}.lease"])
                self.setUp()
                self.migrate()

    def test_recovery_state_fences_a_migrated_slot(self):
        self.migrate()
        _, token = self.acquire(A)
        (self.leases / "recovery" / A).mkdir(parents=True)
        before = self.snapshot()
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 1)
        (self.leases / "recovery" / B).mkdir()
        self.assertEqual(self.pool("acquire", "--slot", B, "--task", "t", "--agent", "ag").returncode, 1)
        self.assertEqual(self.pool("prepare", "--slot", B).returncode, 1)
        self.assertEqual(self.snapshot()[0][f"{A}.lease"], before[0][f"{A}.lease"])
        self.assertEqual((self.leases / f"{B}.lease").read_text(), initial(B))

    def test_unknown_markers_refuse_every_mutator(self):
        self.migrate()
        _, token = self.acquire(A)
        for marker in ("garbage\n", "unmigrating\n", "2\n2\n", None):
            with self.subTest(marker=marker):
                m = self.leases / ".schema"
                m.unlink()
                if marker is None:
                    m.symlink_to(self.t / "elsewhere-schema")
                    (self.t / "elsewhere-schema").write_text("2\n")
                else:
                    m.write_text(marker)
                before = self.snapshot()
                for args in (("acquire", "--slot", B, "--task", "t", "--agent", "ag"),
                             ("acquire", "--slot", "auto", "--task", "t", "--agent", "ag"),
                             ("release", "--slot", A, "--token", token), ("prepare", "--all"),
                             ("review-checkout", "--slot", R, "--token", token, "--branch", "v2.0",
                              "--head", self.head(R)), ("migrate",)):
                    self.assertEqual(self.pool(*args).returncode, 1, f"{marker!r} {args}")
                self.assertEqual(self.snapshot(), before)
                m.unlink()
                m.write_text("2\n")

    def test_reclaim_is_disabled_in_both_modes(self):
        for migrated in (False, True):
            with self.subTest(migrated=migrated):
                self.setUp()
                if migrated:
                    self.migrate()
                _, token = self.acquire(A, "--pid", "999999")
                before = self.snapshot()
                p = self.pool("reclaim", "--slot", A, "--token", token, "--reason", "test")
                self.assertEqual(p.returncode, 1)
                self.assertIn("reclaim is disabled", p.stderr)
                self.assertEqual(self.snapshot(), before)
                self.assertFalse((self.leases / "archive").exists())
                self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)


class Unmigrate(Migrated):
    def test_unmigrate_returns_an_untouched_pool_to_version_1(self):
        self.migrate()
        p = self.pool("unmigrate")
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        self.assertEqual(sorted(x.name for x in self.leases.iterdir()), [])
        _, token = self.acquire(A)
        self.assertIn(f"token={token}", (self.leases / f"{A}.lease").read_text(), "version 1 lease expected")
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)

    def test_unmigrate_refuses_any_changed_record_and_changes_nothing(self):
        def leased():
            self.acquire(A)

        def released():
            _, token = self.acquire(A)
            self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)

        cases = {
            "leased": leased, "released (generation 1)": released,
            "quarantined": lambda: self.put(A, record("quarantined")),
            "free record with other content": lambda: self.put(A, initial(A) + "\n"),
            "missing record": lambda: (self.leases / f"{A}.lease").unlink(),
            "version 1 lease": lambda: self.put(A, v1_lease()),
            "invalid": lambda: self.put(A, record(token=TOKEN)),
            "recovery state": lambda: (self.leases / "recovery" / A).mkdir(parents=True),
            "unknown marker": lambda: (self.leases / ".schema").write_text("3\n"),
            "slot lock held": lambda: (self.leases / f".{B}.lock").mkdir(),
        }
        for name, arrange in cases.items():
            with self.subTest(case=name):
                self.setUp()
                self.migrate()
                arrange()
                before = self.snapshot()
                p = self.pool("unmigrate")
                self.assertEqual(p.returncode, 1, f"{name}: {p.stdout}{p.stderr}")
                self.assertEqual(self.snapshot(), before, name)
                shutil.rmtree(self.leases / f".{B}.lock", True)
        self.setUp()

    def test_an_interrupted_unmigrate_leaves_the_pool_unavailable_until_it_completes(self):
        self.migrate()
        (self.leases / ".schema").write_text("unmigrating\n")
        (self.leases / f"{A}.lease").unlink()
        self.assertEqual(self.status(A)[0], f"{A}: inconsistent")
        before = self.snapshot()
        for args in (("acquire", "--slot", "auto", "--task", "t", "--agent", "ag"),
                     ("acquire", "--slot", A, "--task", "t", "--agent", "ag"), ("prepare", "--all")):
            self.assertEqual(self.pool(*args).returncode, 1, args)
        self.assertEqual(self.snapshot(), before)
        p = self.pool("unmigrate")
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        self.assertEqual(list(self.leases.iterdir()), [])
        self.assertEqual(self.status(A)[0], f"{A}: free")

    def test_unmigrate_rolls_back_an_interrupted_migration(self):
        (self.leases / f"{A}.lease").write_text(initial(A))
        self.put(B, v1_lease(slot=B))
        p = self.pool("unmigrate")
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        self.assertEqual(sorted(x.name for x in self.leases.iterdir()), [f"{B}.lease"])


class MarkerRaces(Migrated):
    """The mode that acts is the one read under the slot lock, and the marker
    changes only while every slot lock is held."""

    def shim(self):
        d = self.t / "shim"
        d.mkdir(exist_ok=True)
        (d / "mkdir").write_text(
            '#!/bin/sh\nfor a in "$@"; do case $a in *.lock) [ -e "$MARK_AT" ] || printf \'2\\n\' >"$MARK_AT" ;; esac; done\n'
            'exec /bin/mkdir "$@"\n')
        (d / "mkdir").chmod(0o755)
        return {"PATH": f"{d}:{os.environ['PATH']}", "MARK_AT": str(self.leases / ".schema")}

    def test_release_and_prepare_also_act_on_the_marker_under_the_lock(self):
        env = self.shim()
        self.put(A, v1_lease())
        before = (self.leases / f"{A}.lease").read_bytes()
        p = self.pool("release", "--slot", A, "--token", TOKEN, **env)
        self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
        self.assertEqual((self.leases / f"{A}.lease").read_bytes(), before, "a migrated pool released a v1 lease")
        (self.leases / ".schema").unlink()
        heads = [self.head(s) for s in SLOTS]
        git("-C", str(self.t / "seed"), "commit", "-q", "--allow-empty", "-m", "next")
        git("-C", str(self.t / "seed"), "push", "-q", "origin", "HEAD:v2.0")
        try:
            p = self.pool("prepare", "--slot", B, **env)
            self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
            self.assertIn("missing", p.stderr)
            self.assertEqual([self.head(s) for s in SLOTS], heads, "prepare moved a slot of a migrated pool")
        finally:
            git("-C", str(self.t / "seed"), "push", "-q", "-f", "origin", f"{heads[0]}:v2.0")
            for s in SLOTS:
                git("-C", str(self.t / "slots" / s), "fetch", "-q", "--prune", "origin")
        (self.leases / ".schema").unlink()
        (self.leases / f"{A}.lease").unlink()

    def test_a_marker_written_before_the_lock_is_taken_wins(self):
        env = self.shim()
        before = self.snapshot()
        p = self.pool("acquire", "--slot", A, "--task", "t", "--agent", "ag", **env)
        self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
        self.assertIn("missing", p.stderr)
        self.assertEqual(sorted(x.name for x in self.leases.iterdir()), [".schema"], "no lease may be written")
        (self.leases / ".schema").unlink()
        self.assertEqual(self.pool("acquire", "--slot", "auto", "--task", "t", "--agent", "ag", **env).returncode, 3)
        self.assertEqual(sorted(x.name for x in self.leases.iterdir()), [".schema"])
        (self.leases / ".schema").unlink()
        self.assertEqual(self.snapshot(), before)

    def test_a_held_slot_lock_blocks_migrate_and_unmigrate(self):
        for held in SLOTS:
            with self.subTest(lock=held):
                self.setUp()
                lock = self.leases / f".{held}.lock"
                lock.mkdir()
                before = self.snapshot()
                p = self.pool("migrate")
                self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
                self.assertEqual(self.snapshot(), before)
                self.assertTrue(lock.is_dir(), "migrate removed a lock it did not take")
                lock.rmdir()
                self.migrate()
                lock.mkdir()
                before = self.snapshot()
                self.assertEqual(self.pool("unmigrate").returncode, 1)
                self.assertEqual(self.snapshot(), before)
                self.assertTrue(lock.is_dir())
                lock.rmdir()

    def test_migrate_against_concurrent_acquires(self):
        procs = [subprocess.Popen(["/bin/bash", str(POOL), "acquire", "--slot", "auto", "--task", "t", "--agent",
                                   f"ag{i}"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                 for i in range(4)]
        mig = subprocess.run(["/bin/bash", str(POOL), "migrate"], env=self.env, capture_output=True, text=True)
        outs = [p.communicate(timeout=120) for p in procs]
        self.tokens = [t for out, _ in outs for _, t in LEASED.findall(out)]
        leased = [s for out, _ in outs for s, _ in LEASED.findall(out)]
        self.assertEqual(len(leased), len(set(leased)), "one slot leased twice")
        if mig.returncode == 0:
            # Migration saw no lease, so every acquire ran in version 2 mode or found its slot taken.
            self.assertEqual((self.leases / ".schema").read_text(), "2\n")
            for s in SLOTS:
                state = self.rec(s).state
                self.assertEqual(state, "leased" if s in leased else "free")
        else:
            self.assertFalse((self.leases / ".schema").exists())
            for s in SLOTS:
                path = self.leases / f"{s}.lease"
                if s in leased:
                    self.assertIn("\ntoken=", "\n" + path.read_text())
                else:
                    self.assertFalse(path.exists(), "a refused migration wrote a record")


class Races(Migrated):
    def test_concurrent_acquires_of_one_slot_have_one_winner(self):
        self.migrate()
        procs = [subprocess.Popen(["/bin/bash", str(POOL), "acquire", "--slot", A, "--task", "t", "--agent",
                                   f"ag{i}"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                 for i in range(8)]
        outs = [p.communicate(timeout=120)[0] for p in procs]
        wins = [m for out in outs for m in LEASED.findall(out)]
        self.tokens = [t for _, t in wins]
        self.assertEqual(len(wins), 1, outs)
        r = self.rec(A)
        self.assertEqual((r.generation, r.token), (1, wins[0][1]))

    def test_release_against_acquire_never_issues_one_generation_twice(self):
        self.migrate()
        _, token = self.acquire(A)
        rel = subprocess.Popen(["/bin/bash", str(POOL), "release", "--slot", A, "--token", token], env=self.env,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        acq = [subprocess.Popen(["/bin/bash", str(POOL), "acquire", "--slot", A, "--task", "t", "--agent", "x"],
                                env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
               for _ in range(3)]
        out, err = rel.communicate(timeout=120)
        self.assertEqual(rel.returncode, 0, out + err)
        wins = [m for p in acq for m in LEASED.findall(p.communicate(timeout=120)[0])]
        self.tokens += [t for _, t in wins]
        r = self.rec(A)
        self.assertLessEqual(len(wins), 1)
        if wins:
            self.assertEqual((r.state, r.generation, r.token), ("leased", 2, wins[0][1]))
        else:
            self.assertEqual((r.state, r.generation), ("free", 1))

    def test_stale_generation_and_identity_are_refused_by_the_writer(self):
        self.migrate()
        _, token = self.acquire(A)
        r = self.rec(A)
        f = str(self.leases / f"{A}.lease")
        before = (self.leases / f"{A}.lease").read_bytes()
        with self.assertRaises(rs.Refused):
            pool_record.release(A, f, 1, r.identity, token)
        with self.assertRaises(rs.Refused):
            pool_record.acquire(B, str(self.leases / f"{B}.lease"), 0, {"task": "t"})
        (self.leases / f".{A}.lock").mkdir()
        (self.leases / f".{B}.lock").mkdir()
        try:
            for gen, lid, tok in ((0, r.identity, token), (2, r.identity, token), (1, LEASE_ID, token),
                                  (1, r.identity, TOKEN), (1, r.identity, token.upper()), (1, r.identity, "")):
                with self.subTest(gen=gen, lid=lid == r.identity, tok=tok == token):
                    with self.assertRaises(rs.Refused):
                        pool_record.release(A, f, gen, lid, tok)
                    self.assertEqual((self.leases / f"{A}.lease").read_bytes(), before)
            with self.assertRaises(rs.Refused):
                pool_record.acquire(B, str(self.leases / f"{B}.lease"), 1, {"task": "t"})
            with self.assertRaises(rs.Refused):
                pool_record.acquire(A, f, 1, {"task": "t"})
            self.assertEqual((self.leases / f"{B}.lease").read_text(), initial(B))
            pool_record.release(A, f, 1, r.identity, token)
        finally:
            (self.leases / f".{A}.lock").rmdir()
            (self.leases / f".{B}.lock").rmdir()


class Holders(Migrated):
    def test_metadata_must_be_printable_ascii_in_both_modes(self):
        bad = ["t x", "t\x85x", "café", "t\tx", "t\x1bx", "t\x7fx"]
        for migrated in (False, True):
            self.setUp()
            if migrated:
                self.migrate()
            before = self.snapshot()
            for value in bad:
                for flag in ("--task", "--agent", "--pr", "--owner", "--pid"):
                    args = {"--task": "t", "--agent": "ag", flag: value}
                    p = self.pool("acquire", "--slot", A, *[x for kv in args.items() for x in kv])
                    with self.subTest(migrated=migrated, flag=flag, value=value):
                        self.assertEqual(p.returncode, 2, p.stdout + p.stderr)
            self.assertEqual(self.snapshot(), before)

    def test_records_with_non_ascii_values_are_refused_and_never_written(self):
        for value in ("t x", "t\x85x", "café", "t\tx"):
            with self.subTest(value=value):
                with self.assertRaises(rs.Refused):
                    rs.parse_record(record(task=value), A)
                self.put(A, record(task=value))
                lines = self.status(A)
                self.assertIn("  record_state=invalid", lines)
                self.assertFalse([ln for ln in lines if re.search(r"[^\x20-\x7e]", ln)], lines)
                with self.assertRaises(rs.Refused):
                    pool_record.durable_replace(self.t / "x" / f"{A}.lease", record(task=value))

    def test_the_writer_refuses_non_ascii_holder_values(self):
        self.migrate()
        (self.leases / f".{A}.lock").mkdir()
        try:
            for value in ("t x", "café", "t\x1bx"):
                with self.assertRaises(rs.Refused):
                    pool_record.acquire(A, str(self.leases / f"{A}.lease"), 0, {"task": "t", "owner": value})
            self.assertEqual((self.leases / f"{A}.lease").read_text(), initial(A))
        finally:
            (self.leases / f".{A}.lock").rmdir()


class Interpreter(Migrated):
    def test_a_missing_or_failing_interpreter_refuses_every_migrated_mutation(self):
        self.migrate()
        _, token = self.acquire(A)
        fake = self.t / "fakepy"
        fake.mkdir(exist_ok=True)
        for name, body in (("fails", "exit 1"), ("lies", "echo free"), ("old", "exit 3"),
                           ("crashes", "kill -9 $$")):
            (fake / name).write_text(f"#!/bin/sh\n{body}\n")
            (fake / name).chmod(0o755)
        before = self.snapshot()
        for py in ("/nonexistent/python3", *(str(p) for p in sorted(fake.iterdir()))):
            for args in (("acquire", "--slot", B, "--task", "t", "--agent", "ag"),
                         ("acquire", "--slot", "auto", "--task", "t", "--agent", "ag"),
                         ("release", "--slot", A, "--token", token), ("prepare", "--slot", B),
                         ("review-checkout", "--slot", R, "--token", token, "--branch", "v2.0", "--head", self.head(R)),
                         ("unmigrate",)):
                with self.subTest(python=py, args=args):
                    p = self.pool(*args, CG_POOL_PYTHON=py)
                    self.assertNotEqual(p.returncode, 0, p.stdout + p.stderr)
                    self.assertNotIn("d" * 32, p.stdout)
            self.assertEqual(self.snapshot(), before, py)
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)

    def test_the_write_path_runs_on_the_oldest_supported_python(self):
        old = Path("/usr/bin/python3")
        if not old.exists():
            self.skipTest("no system python3")
        v = subprocess.run([str(old), "-c", "import sys; print(sys.version_info[:2])"], capture_output=True, text=True)
        p = self.pool("migrate", CG_POOL_PYTHON=str(old))
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        _, token = self.acquire(A, CG_POOL_PYTHON=str(old))
        self.assertEqual(self.pool("release", "--slot", A, "--token", token, CG_POOL_PYTHON=str(old)).returncode, 0)
        self.assertEqual(self.pool("unmigrate", CG_POOL_PYTHON=str(old)).returncode, 1, v.stdout)

    def test_tokens_never_reach_helper_arguments(self):
        log = self.t / "argv.log"
        wrapper = self.t / "py-argv"
        wrapper.write_text(f'#!/bin/sh\necho "$@" >> "{log}"\nexec "{sys.executable}" "$@"\n')
        wrapper.chmod(0o755)
        self.pool("migrate", CG_POOL_PYTHON=str(wrapper))
        _, token = self.acquire(A, CG_POOL_PYTHON=str(wrapper))
        head = self.head(R)
        _, rtoken = self.acquire(R, CG_POOL_PYTHON=str(wrapper))
        self.pool("review-checkout", "--slot", R, "--token", rtoken, "--branch", "v2.0", "--head", head,
                  CG_POOL_PYTHON=str(wrapper))
        for slot, tok in ((A, token), (R, rtoken)):
            self.assertEqual(self.pool("release", "--slot", slot, "--token", tok, CG_POOL_PYTHON=str(wrapper))
                             .returncode, 0)
        calls = log.read_text()
        self.assertIn(" owns ", calls)
        self.assertIn(" release ", calls)
        for tok in (token, rtoken):
            self.assertNotIn(tok, calls)

    def test_an_unmigrated_pool_only_ever_asks_the_helper_to_inspect(self):
        log = self.t / "py.log"
        wrapper = self.t / "py-wrapper"
        wrapper.write_text(f'#!/bin/sh\necho "$@" >> "{log}"\nexec "{sys.executable}" "$@"\n')
        wrapper.chmod(0o755)
        for text in (record("free"), record(), record("quarantined"), record(token=TOKEN), v1_lease()):
            self.setUp()
            self.put(A, text)
            for args in (("status",), ("assess", A), ("acquire", "--slot", A, "--task", "x", "--agent", "y"),
                         ("acquire", "--slot", "auto", "--task", "x", "--agent", "y"),
                         ("release", "--slot", A, "--token", TOKEN), ("prepare", "--all")):
                self.pool(*args, CG_POOL_PYTHON=str(wrapper))
        for call in log.read_text().splitlines():
            self.assertEqual(call.split()[3], "inspect", call)

    def test_helper_usage_errors(self):
        for args in ([], ["write", A, "x"], ["inspect", A], ["owns", A], ["acquire", A, "x", "1"],
                     ["release", A, "x", "1"], ["migrate"], ["migrate", "--dry-run"], ["unmigrate", "x"]):
            p = subprocess.run([sys.executable, "-I", "-B", str(HERE / "pool_record.py"), *args],
                               capture_output=True, text=True, input="")
            self.assertEqual(p.returncode, 2, args)

    def test_write_commands_refuse_outside_a_migrated_pool_or_without_the_lock(self):
        f = self.leases / f"{A}.lease"
        f.write_text(initial(A))
        run = lambda *a: subprocess.run([sys.executable, "-I", "-B", str(HERE / "pool_record.py"), *a],  # noqa: E731
                                        capture_output=True, text=True, input=TOKEN + "\n")
        (self.leases / f".{A}.lock").mkdir()
        try:
            for marker in (None, "garbage\n", "unmigrating\n"):
                if marker:
                    (self.leases / ".schema").write_text(marker)
                p = run("acquire", A, str(f), "0", "task=t")
                self.assertEqual(p.returncode, 1, f"marker {marker!r}")
                self.assertIn("not migrated", p.stderr)
        finally:
            (self.leases / f".{A}.lock").rmdir()
        (self.leases / ".schema").write_text("2\n")
        self.assertEqual(run("acquire", A, str(f), "0", "task=t").returncode, 1, "no lock")
        self.assertEqual(run("unmigrate", str(self.leases), A).returncode, 1, "no lock")
        (self.leases / f".{A}.lock").mkdir()
        try:
            (self.leases / "recovery" / A).mkdir(parents=True)
            self.assertEqual(run("acquire", A, str(f), "0", "task=t").returncode, 1, "recovery state")
            self.assertEqual(run("migrate", str(self.leases), A).returncode, 1, "marker present")
        finally:
            (self.leases / f".{A}.lock").rmdir()
        self.assertEqual(f.read_text(), initial(A))

    def test_oversized_metadata_is_refused_before_any_token(self):
        self.migrate()
        before = self.snapshot()
        for flag in ("--task", "--owner"):
            p = self.pool("acquire", "--slot", "auto", "--task", "t", "--agent", "ag", flag, "x" * 70000)
            self.assertNotEqual(p.returncode, 0, p.stdout + p.stderr)
            self.assertNotIn("token=", p.stdout)
        self.assertEqual(self.snapshot(), before)
        long_value = "x" * 70000 + "\u00e9"
        p = self.pool("acquire", "--slot", "auto", "--task", long_value, "--agent", "ag")
        self.assertEqual(p.returncode, 2, "the ASCII check must hold for long values too")

    def test_a_helper_killed_after_writing_stops_auto(self):
        self.migrate()
        wrapper = self.t / "py-killed"
        wrapper.write_text(f'#!/bin/sh\n"{sys.executable}" "$@" >/dev/null || exit $?\n'
                           'case $4 in acquire) kill -9 $$ ;; esac\n'
                           f'exec "{sys.executable}" "$@"\n')
        wrapper.chmod(0o755)
        p = self.pool("acquire", "--slot", "auto", "--task", "t", "--agent", "ag", CG_POOL_PYTHON=str(wrapper))
        self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
        self.assertEqual(self.rec(A).state, "leased")
        self.assertEqual((self.leases / f"{B}.lease").read_text(), initial(B), "auto moved on to the next worker")
        self.setUp()
        self.migrate()
        p = self.pool("acquire", "--slot", A, "--task", "t", "--agent", "ag", CG_POOL_PYTHON=str(wrapper))
        self.assertEqual(p.returncode, 1)
        self.assertIn("not confirmed", p.stderr)

    def test_an_unconfirmed_acquire_stops_auto_and_keeps_the_slot_busy(self):
        self.migrate()
        wrapper = self.t / "py-unconfirmed"
        wrapper.write_text(f'#!/bin/sh\n"{sys.executable}" "$@" >/dev/null || exit $?\n'
                           'case $4 in acquire) exit 4 ;; esac\n'
                           f'exec "{sys.executable}" "$@"\n')
        wrapper.chmod(0o755)
        p = self.pool("acquire", "--slot", "auto", "--task", "t", "--agent", "ag", CG_POOL_PYTHON=str(wrapper))
        self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
        self.assertNotIn("token=", p.stdout)
        self.assertIn("not confirmed", p.stderr)
        self.assertEqual(self.rec(A).state, "leased")
        self.assertEqual((self.leases / f"{B}.lease").read_text(), initial(B), "auto moved on to the next worker")
        self.assertEqual(self.status(A)[0], f"{A}: busy")


class Durability(unittest.TestCase):
    """Fault injection at every persistence point of the writer, in process."""

    def setUp(self):
        self.d = Path(tempfile.mkdtemp()).resolve()
        self.addCleanup(shutil.rmtree, self.d, True)
        self.assertFalse(str(self.d).startswith(str(LIVE_POOL)))
        (self.d / ".schema").write_text("2\n")
        for s in SLOTS:
            (self.d / f".{s}.lock").mkdir()
            (self.d / f"{s}.lease").write_text(initial(s))
        self.path = self.d / f"{A}.lease"

    def leftovers(self):
        return sorted(p.name for p in self.d.iterdir() if p.name.endswith(".tmp"))

    FAULTS = {
        "ENOSPC on write": ("os.write", OSError(errno.ENOSPC, "No space left on device")),
        "file flush": ("recovery_state.RecoveryStore._sync", OSError(errno.EIO, "I/O error")),
        "interrupted rename": ("os.replace", OSError(errno.EINTR, "Interrupted system call")),
        "crash before rename": ("os.replace", KeyboardInterrupt()),
    }

    def test_a_failure_before_publication_leaves_the_record_and_returns_no_token(self):
        for name, (target, err) in self.FAULTS.items():
            for op in ("acquire", "release"):
                with self.subTest(fault=name, op=op):
                    self.setUp()
                    token = None
                    if op == "release":
                        token = pool_record.acquire(A, self.path, 0, {"task": "t"})
                    before = self.path.read_bytes()
                    got = None
                    with mock.patch(target, side_effect=err), self.assertRaises(type(err)):
                        if op == "acquire":
                            got = pool_record.acquire(A, self.path, 0, {"task": "t"})
                        else:
                            r = rs.parse_record(before.decode(), A)
                            pool_record.release(A, self.path, r.generation, r.identity, token)
                    self.assertIsNone(got)
                    self.assertEqual(self.path.read_bytes(), before)
                    self.assertEqual(self.leftovers(), [])

    def test_a_failed_directory_flush_is_reported_and_returns_no_token(self):
        before = self.path.read_bytes()
        with mock.patch("pool_record._sync_dir", side_effect=OSError(errno.EIO, "I/O error")):
            with self.assertRaises(pool_record.Unconfirmed):
                pool_record.acquire(A, self.path, 0, {"task": "t"})
        r = rs.parse_record(self.path.read_text(), A)
        self.assertEqual((r.state, r.generation), ("leased", 1), "the record was published")
        # Power loss: the unflushed rename is lost and the old record comes back.
        self.path.write_bytes(before)
        token = pool_record.acquire(A, self.path, 0, {"task": "t"})
        r2 = rs.parse_record(self.path.read_text(), A)
        self.assertEqual((r2.generation, r2.token), (1, token))
        self.assertNotEqual(r2.identity, r.identity)

    def test_the_token_is_returned_only_after_the_directory_flush(self):
        events = []
        real_sync, real_replace, real_dir = rs.RecoveryStore._sync, os.replace, pool_record._sync_dir
        with mock.patch("recovery_state.RecoveryStore._sync", side_effect=lambda fd: (events.append("sync"),
                                                                                      real_sync(fd))), \
                mock.patch("os.replace", side_effect=lambda a, b: (events.append("rename"), real_replace(a, b))), \
                mock.patch("pool_record._sync_dir", side_effect=lambda p: (events.append("dir"), real_dir(p))):
            token = pool_record.acquire(A, self.path, 0, {"task": "t"})
            events.append("returned")
        self.assertEqual(events[-1], "returned")
        self.assertLess(events.index("rename"), events.index("dir"))
        self.assertEqual(events[events.index("rename") - 1], "sync")
        self.assertTrue(rs.HEX32.match(token))

    def test_power_loss_at_each_point_never_issues_one_generation_to_two_holders(self):
        """A crash at any point before the directory flush returns no token.
        Whatever the disk holds afterwards (the old record, or the unflushed
        new one), generation 1 has at most one token holder: the slot is
        free at generation 0, or leased at 1 to nobody (busy, never free)."""
        points = {"write": "os.write", "file sync": "recovery_state.RecoveryStore._sync",
                  "rename": "os.replace", "dir sync": "pool_record._sync_dir"}
        for point, target in points.items():
            with self.subTest(crash=point):
                self.setUp()
                before = self.path.read_bytes()
                with mock.patch(target, side_effect=KeyboardInterrupt(point)), self.assertRaises(KeyboardInterrupt):
                    pool_record.acquire(A, self.path, 0, {"task": "t"})
                self.assertEqual(self.leftovers(), [])
                for image in {before, self.path.read_bytes()}:
                    self.path.write_bytes(image)
                    r = rs.parse_record(image.decode(), A)
                    if r.state == "free":
                        self.assertEqual(r.generation, 0)
                        token = pool_record.acquire(A, self.path, 0, {"task": "t"})
                        now = rs.parse_record(self.path.read_text(), A)
                        self.assertEqual((now.generation, now.token), (1, token))
                    else:
                        self.assertEqual((r.state, r.generation), ("leased", 1))
                        with self.assertRaises(rs.Refused):
                            pool_record.acquire(A, self.path, 0, {"task": "t"})

    def test_migration_interrupted_at_each_write_never_leaves_a_marker_without_records(self):
        for s in SLOTS:
            (self.d / f"{s}.lease").unlink()
        (self.d / ".schema").unlink()
        real = pool_record._durable_write
        for fail_at in range(4):
            with self.subTest(fail_at=fail_at):
                for p in self.d.glob("*.lease"):
                    p.unlink()
                calls = []

                def flaky(path, data, *, create=False):
                    calls.append(Path(path).name)
                    if len(calls) - 1 == fail_at:
                        raise OSError(errno.ENOSPC, "No space left on device")
                    return real(path, data, create=create)

                with mock.patch("pool_record._durable_write", side_effect=flaky), self.assertRaises(OSError):
                    pool_record.migrate(self.d, SLOTS)
                self.assertFalse((self.d / ".schema").exists(), "marker written before every record")
                for s in SLOTS[:fail_at]:
                    self.assertEqual((self.d / f"{s}.lease").read_text(), initial(s))
                # A re-run completes the migration.
                pool_record.migrate(self.d, SLOTS)
                self.assertEqual(pool_record.marker_state(self.d), "2")
                (self.d / ".schema").unlink()

    def test_unmigrate_interrupted_at_each_step_leaves_the_pool_unavailable_then_completes(self):
        real = pool_record._durable_unlink
        for fail_at in range(4):
            with self.subTest(fail_at=fail_at):
                self.setUp()
                calls = []

                def flaky(path):
                    calls.append(Path(path).name)
                    if len(calls) - 1 == fail_at:
                        raise OSError(errno.EIO, "I/O error")
                    return real(path)

                with mock.patch("pool_record._durable_unlink", side_effect=flaky), self.assertRaises(OSError):
                    pool_record.unmigrate(self.d, SLOTS)
                self.assertEqual(pool_record.marker_state(self.d), "unmigrating")
                pool_record.unmigrate(self.d, SLOTS)
                self.assertEqual(sorted(p.name for p in self.d.iterdir()), sorted(f".{s}.lock" for s in SLOTS))

    def test_migrate_verifies_records_before_writing_the_marker(self):
        for s in SLOTS:
            (self.d / f"{s}.lease").unlink()
        (self.d / ".schema").unlink()
        with mock.patch("pool_record.durable_replace"), self.assertRaises(rs.Refused):
            pool_record.migrate(self.d, SLOTS)
        self.assertEqual(pool_record.marker_state(self.d), "absent")

    def test_unmigrate_rechecks_each_record_before_removing_it(self):
        real = pool_record._durable_write

        def and_change_b(path, data, *, create=False):
            real(path, data, create=create)
            (self.d / f"{B}.lease").write_text(pool_record.free_record(B, 1))

        with mock.patch("pool_record._durable_write", side_effect=and_change_b), self.assertRaises(rs.Refused):
            pool_record.unmigrate(self.d, SLOTS)
        self.assertEqual((self.d / f"{B}.lease").read_text(), pool_record.free_record(B, 1), "a changed record was removed")
        self.assertEqual(pool_record.marker_state(self.d), "unmigrating", "the pool must stay unavailable")

    def test_an_unconfirmed_marker_flush_is_reported(self):
        for s in SLOTS:
            (self.d / f"{s}.lease").unlink()
        (self.d / ".schema").unlink()
        real = pool_record._sync_dir
        n = []

        def flaky(p):
            n.append(p)
            if len(n) == 4:
                raise OSError(errno.EIO, "I/O error")
            return real(p)

        with mock.patch("pool_record._sync_dir", side_effect=flaky), self.assertRaises(pool_record.Unconfirmed):
            pool_record.migrate(self.d, SLOTS)


class History(Migrated):
    """Every historical pool.sh against every record this pool.sh writes, in
    the migrated pool it writes them to."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.blobs = {}
        for commit, blob in HISTORY:
            p = subprocess.run(["git", "-C", str(HERE), "show", f"{commit}:scripts/agent-pool/pool.sh"],
                               capture_output=True, text=True)
            if p.returncode != 0:
                raise unittest.SkipTest(f"git history for {commit} is not available")
            path = cls.t / f"pool-{blob}.sh"
            path.write_text(p.stdout)
            cls.blobs[blob] = path

    def generated(self):
        """{name: {slot: bytes}} of records produced by this pool.sh."""
        out = {}
        self.migrate()
        out["migrated free (generation 0)"] = self.files()
        _, ta = self.acquire(A)
        _, tb = self.acquire(B)
        _, tr = self.acquire(R)
        out["leased (generation 1)"] = self.files()
        for s, t in ((A, ta), (B, tb), (R, tr)):
            self.assertEqual(self.pool("release", "--slot", s, "--token", t).returncode, 0)
        out["released free (generation 1)"] = self.files()
        self.gen_tokens = [ta, tb, tr]
        return out

    def files(self):
        return {s: (self.leases / f"{s}.lease").read_bytes() for s in SLOTS}

    def test_every_historical_mutator_refuses_every_generated_record(self):
        images = self.generated()
        tokens = self.gen_tokens
        for blob, script in self.blobs.items():
            for name, files in images.items():
                with self.subTest(blob=blob, records=name):
                    self.setUp()
                    (self.leases / ".schema").write_text("2\n")
                    for s, data in files.items():
                        (self.leases / f"{s}.lease").write_bytes(data)
                        (self.leases / f"{s}.lease").chmod(0o600)
                    heads = [self.head(s) for s in SLOTS]
                    tok = tokens[0] if "leased" in name else TOKEN
                    for args in (("acquire", "--slot", A, "--task", "x", "--agent", "y"),
                                 ("acquire", "--slot", "auto", "--task", "x", "--agent", "y"),
                                 ("acquire", "--slot", R, "--task", "x", "--agent", "y", "--review"),
                                 ("release", "--slot", A, "--token", tok),
                                 ("reclaim", "--slot", A, "--token", tok, "--reason", "t"),
                                 ("review-checkout", "--slot", R, "--token", tokens[2], "--branch", "v2.0",
                                  "--head", heads[2]),
                                 ("prepare", "--all")):
                        p = Pool.pool(self, *args, script=script)
                        self.assertNotEqual(p.returncode, 0, f"{blob} {args} accepted {name}")
                        for t in tokens:
                            self.assertNotIn(t, p.stdout + p.stderr)
                    st = Pool.pool(self, "status", script=script)
                    for t in tokens:
                        self.assertNotIn(t, st.stdout + st.stderr)
                    self.assertEqual(self.files(), files)
                    for s in SLOTS:
                        self.assertTrue(stat.S_ISREG(os.lstat(self.leases / f"{s}.lease").st_mode))
                    self.assertEqual([self.head(s) for s in SLOTS], heads)

    def test_known_residual_old_copies_take_a_missing_record(self):
        """Why a missing record must stay unavailable and old copies must not
        run against a migrated pool: pre-schema copies acquire an absent path
        regardless of the marker. This pool.sh then refuses that lease."""
        self.migrate()
        (self.leases / f"{A}.lease").unlink()
        for commit, blob in HISTORY[:-1]:
            with self.subTest(blob=blob):
                p = Pool.pool(self, "acquire", "--slot", A, "--task", "x", "--agent", "y", script=self.blobs[blob])
                self.assertEqual(p.returncode, 0, f"{blob}: {p.stdout}{p.stderr}")
                self.assertEqual(self.pool("release", "--slot", A,
                                           "--token", p.stdout.split("token=")[1].strip()).returncode, 1)
                (self.leases / f"{A}.lease").unlink()
        p = Pool.pool(self, "acquire", "--slot", A, "--task", "x", "--agent", "y", script=self.blobs[HISTORY[-1][1]])
        self.assertEqual(p.returncode, 1, "the 2b-2a pool.sh refuses every change to a marked pool")
        self.assertFalse((self.leases / f"{A}.lease").exists())


class Consumers(Migrated):
    """The unchanged coordinator and campaign against records this pool.sh writes."""

    def parsed(self):
        import campaign
        import coordinator as co
        with mock.patch.dict(os.environ, self.env, clear=True), mock.patch.object(co, "POOL", POOL):
            c = object.__new__(co.Coordinator)
            c.dir = self.t / "coord"
            return co, campaign, c, c.pool_status()

    def test_the_acquire_line_is_the_one_both_consumers_parse(self):
        for src in SOURCES:
            self.assertIn(r're.search(r"^leased (\S+) token=([0-9a-f]{32})$"', src.read_text(), src)
        self.migrate()
        p = self.pool("acquire", "--slot", "auto", "--task", "ci/x", "--agent", "campaign/ci-local",
                      "--owner", f"campaign:{TXN}")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(p.stdout.count("\n"), 1)
        got = re.search(r"^leased (\S+) token=([0-9a-f]{32})$", p.stdout, re.M)
        self.tokens = [got.group(2)]
        self.assertEqual(got.group(1), A)

    def test_coordinator_and_campaign_read_written_records(self):
        self.migrate()
        slot, token = self.acquire(A)
        co, campaign, c, status = self.parsed()
        v = status[A]
        self.assertEqual((v["state"], v["owner"], v["record_state"], v["generation"]), ("busy", OWNER, "leased", "1"))
        self.assertNotIn("lease_token", v)
        self.assertTrue(c.lease_matches({"lease_txn": TXN, "id": "t"}, v))
        self.assertFalse(co.lease_unreadable(v))
        self.assertFalse(campaign.ci_lease_absent(A, v, OWNER))
        self.assertTrue(campaign.ci_lease_absent(A, v, "campaign:" + "0" * 32))
        self.assertEqual(status[B], {"state": "free", "lease": "absent", "record_state": "free", "generation": "0"})
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)
        _, _, _, status = self.parsed()
        v = status[A]
        self.assertEqual(v, {"state": "free", "lease": "absent", "record_state": "free", "generation": "1"})
        self.assertFalse(co.lease_unreadable(v))
        self.assertTrue(campaign.ci_lease_absent(A, v, OWNER))

    def test_a_missing_record_keeps_consumer_tokens(self):
        self.migrate()
        (self.leases / f"{B}.lease").unlink()
        co, campaign, c, status = self.parsed()
        v = status[B]
        self.assertEqual((v["state"], v["lease"], v["record_state"]), ("inconsistent", "absent", "missing"))
        self.assertFalse(campaign.ci_lease_absent(B, v, OWNER))


if __name__ == "__main__":
    unittest.main()

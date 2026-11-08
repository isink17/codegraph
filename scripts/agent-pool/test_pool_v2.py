#!/usr/bin/env python3
"""Version 2 slot records against pool.sh, every historical pool.sh, the
strict reader and the coordinator and campaign consumers. Every pool here is
a throwaway: HOME and every CG_POOL_* point into a temporary directory, so
neither the current nor any historical pool.sh can reach the live pool.

    python3 -m unittest test_pool_v2      (from scripts/agent-pool)
"""

import hashlib
import json
import os
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent / "agent-coordinator"))
import pool_record  # noqa: E402
import recovery_state as rs  # noqa: E402

POOL = HERE / "pool.sh"
LIVE_POOL = (Path.home() / "Projekti/codegraph-evidence/pool").resolve()
A, B, R = "codegraph-worker-a", "codegraph-worker-b", "codegraph-review"
TOKEN = "a" * 32
LEASE_ID = "1" * 32
TXN = "f" * 32
OWNER = f"coordinator:{TXN}"
# Every pool.sh that ever shipped: (commit, blob).
HISTORY = [("7c581a0d", "49556991ad"), ("3f9cebb4", "b07f261e2e"), ("3042dfd2", "abb16fb08a"),
           ("c324a362", "8f1e829a11"), ("fff20693", "b7ffd7056a"), ("0962d226", "081b10cd4b")]
PRE_V1 = HISTORY[:4]


def record(state="leased", slot=A, generation=3, **over):
    head = {"version": "2", "slot": slot, "generation": str(generation), "state": state}
    holder = {"task": "t", "agent": "ag", "pr": "", "owner": OWNER, "host": "elsewhere", "pid": "",
              "utc": "2026-11-08T00:00:00Z"}
    body = {"free": {}, "leased": {"lease_id": LEASE_ID, **holder, "lease_token": TOKEN},
            "quarantined": {"lease_id": LEASE_ID, "recovery_op": "op-1", **holder}}.get(state, {})
    fields = {**head, **body, **over}
    return "".join(f"{k}={v}\n" for k, v in fields.items() if v is not None)


def v1_lease(slot=A, **over):
    fields = {"version": "1", "lease_id": LEASE_ID, "slot": slot, "task": "t", "agent": "ag", "pr": "",
              "owner": OWNER, "host": "elsewhere", "pid": "", "utc": "2026-11-08T00:00:00Z", "token": TOKEN,
              **over}
    return "".join(f"{k}={v}\n" for k, v in fields.items() if v is not None)


def git(*args, **kw):
    return subprocess.run(["git", "-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main",
                           *args], check=True, capture_output=True, text=True, **kw)


def blocks(out):
    """{slot: [lines]} of a status output."""
    res, cur = {}, None
    for line in out.splitlines():
        if line.startswith("codegraph-") and ": " in line:
            cur = line.split(":", 1)[0]
            res[cur] = [line]
        elif cur:
            res[cur].append(line)
    return res


class Pool(unittest.TestCase):
    """A synthetic pool with three slot worktrees of a throwaway repository."""

    @classmethod
    def setUpClass(cls):
        cls.t = Path(tempfile.mkdtemp()).resolve()
        assert not str(cls.t).startswith(str(LIVE_POOL))
        (cls.t / "home").mkdir()
        git("init", "-q", "--bare", str(cls.t / "origin.git"))
        git("clone", "-q", str(cls.t / "origin.git"), str(cls.t / "seed"))
        git("-C", str(cls.t / "seed"), "commit", "-q", "--allow-empty", "-m", "base")
        git("-C", str(cls.t / "seed"), "push", "-q", "origin", "HEAD:v2.0")
        git("-C", str(cls.t / "seed"), "fetch", "-q", "origin")
        for s in (A, B, R):
            git("-C", str(cls.t / "seed"), "worktree", "add", "-q", "--detach", str(cls.t / "slots" / s),
                "origin/v2.0")
        (cls.t / "cwds").write_text("")
        cls.env = {**os.environ, "HOME": str(cls.t / "home"), "CG_POOL_ROOT": str(cls.t / "slots"),
                   "CG_POOL_LEASES": str(cls.t / "leases"), "CG_POOL_CWD_FILE": str(cls.t / "cwds"),
                   "CG_POOL_REPO": str(cls.t / "seed")}
        cls.env.pop("CG_POOL_PYTHON", None)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.t, True)

    def setUp(self):
        self.leases = self.t / "leases"
        shutil.rmtree(self.leases, True)
        self.leases.mkdir()

    def put(self, slot, text):
        p = self.leases / f"{slot}.lease"
        p.write_text(text)
        p.chmod(0o600)
        return p

    def pool(self, *args, script=POOL, **env):
        return subprocess.run(["/bin/bash", str(script), *args], capture_output=True, text=True,
                              env={**self.env, **env}, timeout=120)

    def status(self, slot, **env):
        p = self.pool("status", slot, **env)
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        self.assertNotIn(TOKEN, p.stdout + p.stderr)
        lines = blocks(p.stdout)[slot]
        keys = [ln.strip().split("=", 1)[0] for ln in lines[1:] if ln.startswith("  ") and "=" in ln]
        self.assertEqual(len(keys), len(set(keys)), f"a detail key repeats: {lines}")
        return lines

    def head(self, slot):
        return git("-C", str(self.t / "slots" / slot), "rev-parse", "HEAD").stdout.strip()


class Status(Pool):
    def test_v2_free_renders_free_with_no_active_lease(self):
        self.put(A, record("free"))
        self.assertEqual(self.status(A), [f"{A}: free", "  lease=absent", "  record_state=free", "  generation=3"])

    def test_v2_free_on_a_slot_that_is_not_idle_is_inconsistent(self):
        dirt = self.t / "slots" / A / "dirt.txt"
        dirt.write_text("x")
        self.addCleanup(dirt.unlink)
        self.put(A, record("free"))
        lines = self.status(A)
        self.assertEqual(lines[0], f"{A}: inconsistent")
        self.assertIn("unleased: dirty", lines)
        self.assertIn("  lease=absent", lines)

    def test_v2_leased_renders_busy_with_public_fields_only(self):
        self.put(A, record())
        lines = self.status(A)
        self.assertEqual(lines[0], f"{A}: busy")
        for want in ("  lease=present", "  record_state=leased", "  version=2", f"  slot={A}",
                     f"  lease_id={LEASE_ID}", f"  owner={OWNER}", "  task=t", "  generation=3"):
            self.assertIn(want, lines)
        self.assertFalse([ln for ln in lines if "lease_token" in ln])

    def test_v2_leased_with_a_dead_local_process_is_stale_suspected(self):
        self.put(A, record(host=socket.gethostname(), pid="999999"))
        self.assertEqual(self.status(A)[0], f"{A}: stale-suspected")

    def test_v2_quarantined_keeps_the_inconsistent_header_and_the_holder(self):
        self.put(A, record("quarantined"))
        lines = self.status(A)
        self.assertEqual(lines[0], f"{A}: inconsistent")
        for want in ("  lease=present", "  record_state=quarantined", "  recovery_op=op-1", f"  owner={OWNER}",
                     f"  lease_id={LEASE_ID}", f"  slot={A}", "  generation=3"):
            self.assertIn(want, lines)
        self.put(A, record("quarantined", lease_id=None, legacy_digest="e" * 64))
        self.assertIn("  legacy_digest=" + "e" * 64, self.status(A))

    def test_records_the_strict_parser_refuses_are_never_free(self):
        big = record() + "".join(f"x{i}=1\n" for i in range(9000))
        cases = {
            "token= in v2": record(token=TOKEN), "token= in v2 free": record("free", token=TOKEN),
            "repeated owner": record() + f"owner={OWNER}\n", "unknown v2 state": record("reserved"),
            "two versions": v1_lease() + "version=2\n", "v1 with lease_token": v1_lease(lease_token=TOKEN),
            "v1 with state": v1_lease(state="free"), "wrong slot": record(slot=B),
            "generation 01": record(generation="01"), "both identities": record("quarantined", legacy_digest="e" * 64),
            "control character": record(task="t\r"), "oversized": big,
        }
        for name, text in cases.items():
            with self.subTest(case=name):
                self.put(A, text)
                lines = self.status(A)
                self.assertEqual(lines[0], f"{A}: inconsistent")
                self.assertIn("  lease=present", lines)
                self.assertIn("  record_state=invalid", lines)
        (self.leases / f"{A}.lease").write_bytes(record("free").encode() + b"task=\xff\n")
        self.assertIn("  record_state=invalid", self.status(A))
        target = self.t / "elsewhere.lease"
        target.write_text(record("free"))
        (self.leases / f"{A}.lease").unlink()
        (self.leases / f"{A}.lease").symlink_to(target)
        lines = self.status(A)
        self.assertEqual(lines[0], f"{A}: inconsistent")
        self.assertIn("  record_state=invalid", lines)

    def test_a_missing_or_failing_strict_parser_never_falls_back(self):
        fake = self.t / "fakepy"
        fake.mkdir(exist_ok=True)
        (fake / "fails").write_text("#!/bin/sh\nexit 1\n")
        (fake / "lies").write_text("#!/bin/sh\necho free\n")
        (fake / "old").write_text("#!/bin/sh\nexit 3\n")
        for f in fake.iterdir():
            f.chmod(0o755)
        for py in ("/nonexistent/python3", fake / "fails", fake / "lies", fake / "old"):
            for text in (record("free"), record()):
                with self.subTest(python=str(py), record=text.splitlines()[3]):
                    self.put(A, text)
                    lines = self.status(A, CG_POOL_PYTHON=str(py))
                    self.assertEqual(lines[0], f"{A}: inconsistent")
                    self.assertIn("  record_state=unknown", lines)
                    self.assertIn("  lease=present", lines)
                    p = self.pool("release", "--slot", A, "--token", TOKEN, CG_POOL_PYTHON=str(py))
                    self.assertEqual(p.returncode, 1)
        # Legacy and version 1 leases never need the interpreter.
        self.put(A, v1_lease())
        self.assertEqual(self.status(A, CG_POOL_PYTHON="/nonexistent/python3")[0], f"{A}: busy")

    def test_the_schema_marker_makes_a_missing_record_inconsistent(self):
        for content in ("2\n", "garbage\n"):
            with self.subTest(marker=content):
                (self.leases / ".schema").write_text(content)
                lines = self.status(B)
                self.assertEqual(lines, [f"{B}: inconsistent", lines[1], "  lease=absent", "  record_state=missing"])
        (self.leases / ".schema").unlink()
        self.assertEqual(self.status(B)[0], f"{B}: free")

    def test_recovery_state_is_never_read_as_free(self):
        (self.leases / "recovery" / B).mkdir(parents=True)
        lines = self.status(B)
        self.assertEqual(lines[0], f"{B}: inconsistent")
        self.assertIn("  recovery=unknown", lines)
        self.assertEqual(self.status(A)[0], f"{A}: free")
        self.put(A, record("free"))
        (self.leases / "recovery" / A).mkdir()
        lines = self.status(A)
        self.assertEqual(lines[0], f"{A}: inconsistent")
        self.assertIn("  recovery=unknown", lines)

    def test_assess_reports_strict_records_without_secrets(self):
        self.put(A, record("quarantined"))
        p = self.pool("assess", A)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertNotIn(TOKEN, p.stdout + p.stderr)
        for want in ("state: inconsistent", "lease: present", "pool-schema-marker: absent", "recovery: none",
                     "  record_state=quarantined", f"  owner={OWNER}"):
            self.assertIn(want, p.stdout.splitlines())
        self.put(A, record())
        p = self.pool("assess", A)
        self.assertNotIn(TOKEN, p.stdout + p.stderr)
        self.assertIn("lease-schema: strict (version 2 or ambiguous; read by pool_record.py)", p.stdout)

    def test_other_slots_keep_their_own_fields(self):
        self.put(A, record("quarantined"))
        self.put(B, record("free", slot=B))
        p = self.pool("status")
        self.assertEqual(p.returncode, 0)
        b = blocks(p.stdout)
        self.assertEqual(b[B], [f"{B}: free", "  lease=absent", "  record_state=free", "  generation=3"])
        self.assertEqual(b[R], [f"{R}: free", "  lease=absent"])


class Fencing(Pool):
    """Every mutator of this pool.sh refuses every version 2 record, a marked
    pool and recovery state, and changes nothing."""

    def snapshot(self):
        files = {p.relative_to(self.leases).as_posix(): (p.read_bytes() if p.is_file() and not p.is_symlink()
                                                         else os.readlink(p) if p.is_symlink() else None)
                 for p in sorted(self.leases.rglob("*"))}
        return files, [self.head(s) for s in (A, B, R)]

    def mutators(self, slot=A):
        return [("acquire", "--slot", slot, "--task", "x", "--agent", "y"),
                ("release", "--slot", slot, "--token", TOKEN),
                ("reclaim", "--slot", slot, "--token", TOKEN, "--reason", "test"),
                ("prepare", "--slot", slot),
                ("review-checkout", "--slot", R, "--token", TOKEN, "--branch", "v2.0", "--head", self.head(R))]

    def test_every_mutator_refuses_every_v2_record(self):
        for text in (record("free"), record(), record("quarantined"), record(token=TOKEN)):
            with self.subTest(record=text.splitlines()[3]):
                self.setUp()
                self.put(A, text)
                self.put(R, text.replace(f"slot={A}", f"slot={R}"))
                before = self.snapshot()
                for args in self.mutators():
                    p = self.pool(*args)
                    self.assertEqual(p.returncode, 1, f"{args}: {p.stdout}{p.stderr}")
                    self.assertNotIn(TOKEN, p.stdout + p.stderr)
                self.assertEqual(self.snapshot(), before)

    def test_a_marked_pool_refuses_every_mutator(self):
        self.put(A, v1_lease())
        (self.leases / ".schema").write_text("2\n")
        before = self.snapshot()
        for args in self.mutators() + [("acquire", "--slot", "auto", "--task", "x", "--agent", "y"),
                                       ("prepare", "--all")]:
            p = self.pool(*args)
            self.assertEqual(p.returncode, 1, f"{args}: {p.stdout}{p.stderr}")
        self.assertEqual(self.snapshot(), before)

    def test_recovery_state_refuses_the_slot_only(self):
        self.put(A, v1_lease())
        (self.leases / "recovery" / A).mkdir(parents=True)
        before = self.snapshot()
        for args in self.mutators()[:4]:
            p = self.pool(*args)
            self.assertEqual(p.returncode, 1, f"{args}: {p.stdout}{p.stderr}")
        self.assertEqual(self.snapshot(), before)
        (self.leases / f"{A}.lease").unlink()
        p = self.pool("acquire", "--slot", A, "--task", "x", "--agent", "y")
        self.assertEqual(p.returncode, 1)
        p = self.pool("acquire", "--slot", "auto", "--task", "x", "--agent", "y")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn(f"leased {B} token=", p.stdout)
        self.assertFalse((self.leases / f"{A}.lease").exists())

    def test_without_marker_or_v2_records_version_1_still_works(self):
        p = self.pool("acquire", "--slot", A, "--task", "x", "--agent", "y")
        self.assertEqual(p.returncode, 0, p.stderr)
        token = p.stdout.split("token=")[1].strip()
        self.assertEqual(self.pool("status", A).stdout.splitlines()[0], f"{A}: busy")
        p = self.pool("release", "--slot", A, "--token", token)
        self.assertEqual(p.returncode, 0, p.stderr)


class History(Pool):
    """Every historical pool.sh, extracted from git, refuses every version 2
    record and leaves it untouched."""

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.blobs = {}
        for commit, blob in HISTORY:
            p = subprocess.run(["git", "-C", str(HERE), "show", f"{commit}:scripts/agent-pool/pool.sh"],
                               capture_output=True, text=True)
            if p.returncode != 0:
                raise unittest.SkipTest(f"git history for {commit} is not available")
            got = hashlib.sha1(f"blob {len(p.stdout.encode())}\0".encode() + p.stdout.encode()).hexdigest()
            assert got.startswith(blob), (commit, got)
            path = cls.t / f"pool-{blob}.sh"
            path.write_text(p.stdout)
            cls.blobs[blob] = path

    def test_every_historical_mutator_refuses_every_v2_record(self):
        matrix = {}
        for commit, blob in HISTORY:
            for state in ("free", "leased", "quarantined"):
                with self.subTest(blob=blob, state=state):
                    self.setUp()
                    for s in (A, B, R):
                        self.put(s, record(state, slot=s))
                    before = {s: (self.leases / f"{s}.lease").read_bytes() for s in (A, B, R)}
                    heads = [self.head(s) for s in (A, B, R)]
                    runs = [("acquire", "--slot", A, "--task", "x", "--agent", "y"),
                            ("acquire", "--slot", "auto", "--task", "x", "--agent", "y"),
                            ("acquire", "--slot", R, "--task", "x", "--agent", "y", "--review"),
                            ("release", "--slot", A, "--token", TOKEN),
                            ("reclaim", "--slot", A, "--token", TOKEN, "--reason", "test"),
                            ("review-checkout", "--slot", R, "--token", TOKEN, "--branch", "v2.0", "--head", heads[2]),
                            ("prepare", "--all")]
                    codes = []
                    for args in runs:
                        p = self.pool(*args, script=self.blobs[blob])
                        codes.append(p.returncode)
                        self.assertNotEqual(p.returncode, 0, f"{blob} {args} accepted a v2 {state} record")
                        self.assertNotIn(TOKEN, p.stdout + p.stderr)
                    st = self.pool("status", script=self.blobs[blob])
                    self.assertNotIn(TOKEN, st.stdout + st.stderr)
                    for s in (A, B, R):
                        path = self.leases / f"{s}.lease"
                        self.assertTrue(stat.S_ISREG(os.lstat(path).st_mode))
                        self.assertEqual(path.read_bytes(), before[s])
                        self.assertNotIn(b"\ntoken=", b"\n" + path.read_bytes())
                    self.assertEqual([self.head(s) for s in (A, B, R)], heads)
                    matrix[f"{blob} {state}"] = codes
        out = os.environ.get("CG_POOL_V2_MATRIX")
        if out:
            Path(out).write_text(json.dumps(matrix, indent=1, sort_keys=True))

    def test_negative_control_a_stray_token_line_breaks_the_fence(self):
        """Pre-v1 pool.sh validates a lease by its token= line alone, so a v2
        record carrying one is released by them. This is why no v2 record may
        ever contain token=."""
        for commit, blob in PRE_V1:
            with self.subTest(blob=blob):
                self.setUp()
                self.put(A, record() + f"token={TOKEN}\n")
                p = self.pool("release", "--slot", A, "--token", TOKEN, script=self.blobs[blob])
                self.assertEqual(p.returncode, 0, f"{blob}: {p.stdout}{p.stderr}")
                self.assertFalse((self.leases / f"{A}.lease").exists(), f"{blob} kept the record")
        self.put(A, record() + f"token={TOKEN}\n")
        p = self.pool("release", "--slot", A, "--token", TOKEN)
        self.assertEqual(p.returncode, 1, "the current pool.sh released a v2 record with a token= line")
        self.assertTrue((self.leases / f"{A}.lease").exists())


class Consumers(Pool):
    """The unchanged coordinator and campaign read the new status format."""

    def parsed(self):
        import campaign
        import coordinator as co
        state = self.t / "coord"
        with mock.patch.dict(os.environ, self.env, clear=True), mock.patch.object(co, "POOL", POOL):
            c = object.__new__(co.Coordinator)
            c.dir = state
            return co, campaign, c.pool_status()

    def test_consumers_keep_tokens_unless_the_record_proves_absence(self):
        cases = {
            "free": (record("free"), True, False),
            "leased by another owner": (record(owner="coordinator:" + "0" * 32), True, False),
            "quarantined": (record("quarantined"), False, False),
            "invalid": (record(token=TOKEN), False, False),
            "unparseable owner-less": (record("free") + "junk\n", False, True),
        }
        for name, (text, absent, unreadable) in cases.items():
            with self.subTest(case=name):
                self.put(A, text)
                co, campaign, status = self.parsed()
                v = status[A]
                self.assertEqual(campaign.ci_lease_absent(A, v, OWNER), absent, v)
                self.assertEqual(co.lease_unreadable(v), unreadable, v)
                self.assertEqual(status[B], {"state": "free", "lease": "absent"})

    def test_free_v2_record_parses_exactly(self):
        self.put(A, record("free"))
        _, _, status = self.parsed()
        self.assertEqual(status[A], {"state": "free", "lease": "absent", "record_state": "free", "generation": "3"})

    def test_quarantined_record_keeps_the_owner_on_its_own_slot(self):
        self.put(A, record("quarantined"))
        co, _, status = self.parsed()
        c = object.__new__(co.Coordinator)
        self.assertTrue(c.lease_matches({"lease_txn": TXN, "id": "t"}, status[A]))
        self.assertEqual(status[A]["state"], "inconsistent")
        self.assertNotIn("owner", status[B])


class DormantWriter(Pool):
    def test_pool_sh_only_ever_asks_the_helper_to_inspect(self):
        log = self.t / "py.log"
        wrapper = self.t / "py-wrapper"
        wrapper.write_text(f'#!/bin/sh\necho "$@" >> "{log}"\nexec "{sys.executable}" "$@"\n')
        wrapper.chmod(0o755)
        for text in (record("free"), record(), record("quarantined"), record(token=TOKEN)):
            self.setUp()
            self.put(A, text)
            for args in (("status",), ("assess", A), ("acquire", "--slot", A, "--task", "x", "--agent", "y"),
                         ("release", "--slot", A, "--token", TOKEN), ("prepare", "--all")):
                self.pool(*args, CG_POOL_PYTHON=str(wrapper))
        calls = log.read_text().splitlines()
        self.assertTrue(calls)
        for call in calls:
            argv = call.split()
            self.assertEqual(argv[:2], ["-I", "-B"], call)
            self.assertTrue(argv[2].endswith("/pool_record.py"), call)
            self.assertEqual(argv[3], "inspect", call)
        src = POOL.read_text()
        self.assertEqual([ln.strip() for ln in src.splitlines() if '"$PYTHON"' in ln],
                         ['out=$("$PYTHON" -I -B "$HERE/pool_record.py" inspect "$1" "$2" 2>/dev/null) || return 1'],
                         "pool.sh runs the helper from somewhere new")
        self.assertNotIn("durable_replace", src)

    def test_the_helper_has_no_write_command(self):
        for args in ([], ["write", A, "x"], ["replace", A, "x"], ["durable_replace", A, "x"], ["inspect", A]):
            p = subprocess.run([sys.executable, "-I", "-B", str(HERE / "pool_record.py"), *args],
                               capture_output=True, text=True)
            self.assertEqual(p.returncode, 2, args)
        src = (HERE / "pool_record.py").read_text()
        self.assertNotIn("RecoveryStore(", src)

    def test_the_helper_runs_on_the_oldest_supported_python(self):
        old = Path("/usr/bin/python3")
        if not old.exists():
            self.skipTest("no system python3")
        self.put(A, record("quarantined"))
        p = subprocess.run([str(old), "-I", "-B", str(HERE / "pool_record.py"), "inspect", A,
                            str(self.leases / f"{A}.lease")], capture_output=True, text=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(p.stdout.splitlines()[:3], ["record=v2", "state=quarantined", "generation=3"])


class DurableReplace(unittest.TestCase):
    def setUp(self):
        self.d = Path(tempfile.mkdtemp()).resolve()
        self.addCleanup(shutil.rmtree, self.d, True)
        self.path = self.d / f"{A}.lease"

    def leftovers(self):
        return [p.name for p in self.d.iterdir() if p.name.endswith(".tmp")]

    def test_writes_and_replaces_atomically(self):
        pool_record.durable_replace(self.path, record("free"))
        self.assertEqual(self.path.read_text(), record("free"))
        self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), 0o600)
        pool_record.durable_replace(self.path, record())
        self.assertEqual(self.path.read_text(), record())
        self.assertEqual(self.leftovers(), [])

    def test_refuses_what_must_never_be_written(self):
        self.path.write_text(record("free"))
        for text in (v1_lease(), record(token=TOKEN), record("free").rstrip("\n"), record(slot=B), "", None,
                     record("reserved")):
            with self.subTest(text=text):
                with self.assertRaises(rs.Refused):
                    pool_record.durable_replace(self.path, text)
                self.assertEqual(self.path.read_text(), record("free"))
        with self.assertRaises(rs.Refused):
            pool_record.durable_replace(self.d / "x.txt", record("free"))
        self.path.unlink()
        target = self.d / "target"
        target.write_text("keep")
        self.path.symlink_to(target)
        with self.assertRaises(rs.Refused):
            pool_record.durable_replace(self.path, record("free"))
        self.assertEqual(target.read_text(), "keep")
        self.assertEqual(self.leftovers(), [])

    def test_a_failed_flush_or_rename_keeps_the_old_record(self):
        self.path.write_text(record("free"))
        for target, err in (("os.replace", OSError("rename failed")),
                            ("recovery_state.RecoveryStore._sync", OSError("flush failed"))):
            with self.subTest(failure=target):
                with mock.patch(target, side_effect=err):
                    with self.assertRaises(OSError):
                        pool_record.durable_replace(self.path, record())
                self.assertEqual(self.path.read_text(), record("free"))
                self.assertEqual(self.leftovers(), [])

    def test_unsupported_platform_is_refused(self):
        with mock.patch.object(rs, "SUPPORTED", False):
            with self.assertRaises(rs.Refused):
                pool_record.durable_replace(self.path, record("free"))
        self.assertFalse(self.path.exists())


if __name__ == "__main__":
    unittest.main()

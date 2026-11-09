#!/usr/bin/env python3
"""The recovery command line, pool.sh recover, against synthetic pools only.
End-to-end runs drive pool.sh through a pseudo-terminal made the controlling
terminal of a new session; crash points run the same code in process with a
scripted terminal and the library's fault hook. Every pool is a throwaway:
HOME and every CG_POOL_* point into a temporary directory. The live pool is
only ever named, to prove it is refused, and its tree is compared by stat
before and after; no file in it is read.

    python3 -m unittest test_recovery_cli      (from scripts/agent-pool)
"""

import ast
import contextlib
import fcntl
import json
import os
import re
import select
import shutil
import signal
import stat
import subprocess
import termios
import time
import unittest
from pathlib import Path
from unittest import mock

from test_pool_v2 import A, B, OWNER, POOL, R, HERE, blocks, record, v1_lease
from test_pool_v2_writes import SLOTS
from test_recovery_gate import TRANSITIONS, Recovery, fault_at
import pool_record  # noqa: E402
import recovery_cli as cli  # noqa: E402
import recovery_state as rs  # noqa: E402

MARK = cli.SYNTHETIC_HEAD + "\nslots=" + " ".join(SLOTS) + "\n"
RETYPES = ["slot", "operation", "transition", "first 12 characters of the adopted identity", "slot generation",
           "journal sequence", "first 12 characters of the digest"]
HEAD = "a" * 40
TOKEN_SHOWN = re.compile(r"quarantined for good\.\r?\n([0-9a-f]{32})\r?\n")


def display(text):
    """The fields of the last confirmation display in a terminal transcript."""
    block = text[text.rindex("pool: "):].replace("\r", "")
    get = {k: re.search(rf"^{re.escape(k)}: (.*)$", block, re.M) for k in
           ("slot", "operation", "transition", "adopted identity", "slot generation", "journal sequence", "digest")}
    out = {k: m.group(1) for k, m in get.items() if m}
    out["identity"] = out.pop("adopted identity").split(" ")[1]
    m = re.search(r'"utc":"([^"]*)"', block)
    if m:
        out["utc"] = m.group(1)
    return out


def answer(prompt, transcript, ctx):
    """What the owner types at `prompt`; ctx overrides it per test."""
    if prompt.startswith("Retype the recovery token"):
        return ctx.get("confirm_token") or TOKEN_SHOWN.findall(transcript)[-1]
    if prompt.startswith("Retype the "):
        label = prompt[len("Retype the "):-2]
        d = display(transcript)
        want = {"slot": d["slot"], "operation": d["operation"], "transition": d["transition"],
                RETYPES[3]: d["identity"][:12], "slot generation": d["slot generation"],
                "journal sequence": d["journal sequence"], RETYPES[6]: d.get("digest", "")[:12]}[label]
        return "wrong" if ctx.get("wrong") == label else want
    if prompt.startswith("Original "):
        return ctx["original"][prompt[len("Original "):-2]]
    if "token" in prompt:
        return ctx["token"]
    for start, value in (("Reason", "synthetic test"), ("Quiescence method", "owner-quiescence"),
                         ("Preservation disposition", "synthetic"), ("Assessment digest", "0" * 64),
                         ("Evidence references", "ref-1"), ("HEAD observed", HEAD)):
        if prompt.startswith(start):
            return ctx.get(start, value)
    if prompt.endswith("(type yes): "):
        return ctx.get("yes", "yes")
    raise AssertionError(f"unexpected prompt {prompt!r}")


PROMPT = re.compile(r"(?:^|\n)([^\n]*: )$")


class Script:
    """A scripted terminal for in-process runs."""

    def __init__(self, ctx):
        self.ctx, self.text, self.secret = ctx, "", []

    def write(self, text):
        self.text += text

    def ask(self, prompt, secret=False):
        self.text += prompt
        a = answer(prompt, self.text, self.ctx)
        self.secret.append((prompt, secret))
        if not secret:
            self.text += a + "\n"
        return a


class Session:
    """pool.sh recover with a pseudo-terminal as its controlling terminal and
    stdin, stdout and stderr kept apart from it."""

    def __init__(self, test, args, env, ctx):
        self.ctx, self.text, self.ps = ctx, "", []
        self.master, slave = os.openpty()

        def ctty():
            fcntl.ioctl(0, termios.TIOCSCTTY, 0)
        self.p = subprocess.Popen(["/bin/bash", str(POOL), "recover", *args], stdin=slave, stdout=subprocess.PIPE,
                                  stderr=subprocess.PIPE, env=env, start_new_session=True, preexec_fn=ctty)
        os.close(slave)

    def _read(self, timeout):
        r, _, _ = select.select([self.master], [], [], timeout)
        if not r:
            return False
        try:
            data = os.read(self.master, 65536)
        except OSError:
            data = b""
        if not data:
            return None
        self.text += data.decode("utf-8", "replace")
        return True

    def prompt(self, timeout=60):
        """The next prompt, or None once the terminal closed."""
        end = time.monotonic() + timeout
        while True:
            m = PROMPT.search(self.text.replace("\r", ""))
            if m and self.text.replace("\r", "").endswith(m.group(1)):
                self.text += "\x00"   # consumed
                return m.group(1)
            got = self._read(max(0.0, end - time.monotonic()))
            if got is None:
                return None
            if got is False:
                if self.p.poll() is not None:
                    return None
                raise AssertionError(f"no prompt within {timeout}s: {self.text[-400:]!r}")

    def run(self, until=None):
        """Answers every prompt; stops early at a prompt starting with `until`."""
        while (p := self.prompt()) is not None:
            if until and p.startswith(until):
                return p
            if "token" in p:
                self.ps.append(subprocess.run(["ps", "axeww"], capture_output=True, text=True).stdout)
            os.write(self.master, (answer(p, self.text.replace("\x00", ""), self.ctx) + "\n").encode())
        return None

    def finish(self):
        out, err = self.p.communicate(timeout=120)
        while self._read(0.2):
            pass
        os.close(self.master)
        return self.p.returncode, out.decode(), err.decode(), self.text.replace("\x00", "")


class Cli(Recovery):
    def setUp(self):
        super().setUp()
        self.mark()
        self.runs = []

    def mark(self, text=MARK, mode=0o600, pool=None):
        p = (pool or self.leases) / cli.SYNTHETIC
        p.write_text(text)
        p.chmod(mode)

    def args(self, action, slot, op, pool, *extra):
        a = [action, "--pool", str(pool or self.leases), "--slot", slot]
        if op:
            a += ["--op", op]
        return a + list(extra)

    def recover(self, action, slot=A, op=None, *extra, pool=None, stdin=subprocess.DEVNULL, **env):
        """pool.sh recover in a new session with no controlling terminal."""
        p = subprocess.run(["/bin/bash", str(POOL), "recover", *self.args(action, slot, op, pool, *extra)],
                           stdin=stdin, capture_output=True, text=True, env={**self.env, **env},
                           start_new_session=True, timeout=120)
        self.outputs.append(p.stdout + p.stderr)
        return p

    def tty(self, action, slot=A, op=None, *extra, ctx=None, env=None):
        """pool.sh recover through a pseudo-terminal: (exit, stdout, stderr, transcript)."""
        s = Session(self, self.args(action, slot, op, None, *extra), {**self.env, **(env or {})}, ctx or {})
        s.run()
        res = s.finish()
        self.runs.append((res, s.ps))
        self.outputs.append(res[1] + res[2])
        return res

    def ok(self, action, slot=A, op=None, *extra, ctx=None):
        code, out, err, text = self.tty(action, slot, op, *extra, ctx=ctx)
        self.assertEqual(code, 0, f"{action}: {out}{err}{text[-600:]}")
        self.assertRegex(out, rf"^recover {action}: slot={slot} operation=\S+ ")
        return out, text

    def op_of(self, out):
        return re.search(r"operation=(\S+)", out).group(1)

    def evidence(self, data=None):
        p = self.t / "evidence.json"
        p.write_text(json.dumps(data or {"head": HEAD, "inventory": {k: [] for k in rs.INVENTORY}, "preserved": {}}))
        p.chmod(0o600)
        return str(p)

    def path_to(self, slot, upto, ctx=None):
        """Adopts the lease of `slot` and drives it through the terminal to `upto`; (op, token)."""
        out, _ = self.ok("adopt", slot, ctx=ctx)
        op, token = self.op_of(out), None
        for step in ("request", "quarantine", "quiescence", "preservation", "ready"):
            if TRANSITIONS.index(step) > TRANSITIONS.index(upto):
                break
            extra = ("--evidence", self.evidence()) if step == "preservation" else ()
            _, text = self.ok(step, slot, op, *extra, ctx={**(ctx or {}), "token": token})
            if step == "quarantine":
                token = TOKEN_SHOWN.findall(text)[-1]
        return op, token

    def inproc(self, action, slot=A, op=None, evidence=None, ctx=None, fault=None):
        """recovery_cli.run under the slot lock with a scripted terminal; (lines or exception, script)."""
        term = Script(ctx or {})
        with self.locked(slot):
            try:
                return cli.run(action, str(self.leases), slot, op, evidence, term=term, fault=fault), term
            except rs.Refused as e:
                return e, term

    def store_state(self, slot, op):
        return rs.RecoveryStore(self.leases / "recovery").state(slot, op)


def live_tree():
    """stat of every path in the live pool, without reading a file."""
    live = Path(cli.live_pool())
    if not os.path.lexists(live):
        return None
    out = {}
    for root, dirs, files in os.walk(live):
        for n in sorted(dirs + files):
            st = os.lstat(os.path.join(root, n))
            out[os.path.relpath(os.path.join(root, n), live)] = (st.st_mode, st.st_size, st.st_mtime_ns, st.st_ino)
    st = os.lstat(live)
    out["."] = (st.st_mode, st.st_mtime_ns, st.st_ino)
    return out


class Interlock(Cli):
    def every_action(self, pool, **env):
        for action in cli.ACTIONS:
            op = None if action in ("adopt", "inspect") else "op-1"
            extra = ("--evidence", self.evidence()) if action == "preservation" else ()
            p = subprocess.run(["/bin/bash", str(POOL), "recover", action, "--pool", pool, "--slot", A,
                                *(["--op", op] if op else []), *extra], stdin=subprocess.DEVNULL,
                               capture_output=True, text=True, env={**self.env, **env}, start_new_session=True,
                               timeout=120)
            yield action, p

    def test_the_live_pool_and_its_aliases_are_refused_before_anything_happens(self):
        live = cli.live_pool()
        aliases = {"live": live, "inside": live + "/recovery", "parent": os.path.dirname(live),
                   "home": os.path.dirname(os.path.dirname(os.path.dirname(live))), "root": "/"}
        (self.t / "alias").unlink(missing_ok=True)
        (self.t / "alias").symlink_to(live)
        aliases["symlink"] = str(self.t / "alias")
        if Path("/System/Volumes/Data" + live).parent.exists():
            aliases["firmlink"] = "/System/Volumes/Data" + live
        for name, sub in (("firmlink-inside", live + "/archive"), ("firmlink-parent", os.path.dirname(live))):
            if os.path.isdir(sub) and os.path.isdir("/System/Volumes/Data" + sub):
                aliases[name] = "/System/Volumes/Data" + sub
        before = live_tree()
        for name, path in aliases.items():
            for env in ({}, {"CG_POOL_LEASES": live}, {"HOME": str(self.t / "home"), "CG_POOL_LEASES": path}):
                for action, p in self.every_action(path, **env):
                    with self.subTest(alias=name, action=action, env=sorted(env)):
                        self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
                        self.assertIn("nothing changed", p.stderr)
                        self.assertNotIn("lock held", p.stderr)
                        self.assertEqual(p.stdout, "")
                        if name != "symlink":
                            self.assertIn("live pool", p.stderr)
                        else:
                            self.assertIn("canonical", p.stderr)
        self.assertEqual(live_tree(), before, "the live pool changed")

    def test_a_perfect_synthetic_pool_at_the_live_path_is_refused(self):
        """The live pool, played by a marked and migrated synthetic pool."""
        for name, live in (("is the live pool", self.leases), ("inside the live pool", self.t),
                           ("a parent of the live pool", self.leases / "recovery" / "x")):
            with self.subTest(case=name), mock.patch.object(cli, "live_pool", lambda live=live: str(live)):
                with self.assertRaisesRegex(rs.Refused, "live pool"):
                    cli.synthetic_pool(str(self.leases), A)
        # The same pool is accepted once it is not the live one.
        cli.synthetic_pool(str(self.leases), A)

    def test_inode_aliases_of_the_live_pool_are_refused(self):
        """Paths that name the live pool's inodes under other names, as a
        firmlink or bind mount would, played by a stat that resolves them."""
        (self.t / "elsewhere-live").mkdir(exist_ok=True)
        real_stat = os.stat
        cases = {
            # The live pool is the pool's parent under another name: the pool lies inside it.
            "inside": (self.t / "elsewhere-live", {str(self.t / "elsewhere-live"): self.t}, "a path inside it"),
            # A parent of the live pool is the pool under another name: the pool lies above it.
            "above": (self.t / "elsewhere" / "pool", {str(self.t / "elsewhere"): self.leases}, "one of its parents"),
        }
        for name, (live, alias, msg) in cases.items():
            def fake_stat(p, *a, alias=alias, **k):
                return real_stat(alias[str(p)]) if str(p) in alias else real_stat(p, *a, **k)
            with self.subTest(case=name), mock.patch.object(cli, "live_pool", lambda live=live: str(live)), \
                    mock.patch.object(cli.os, "stat", fake_stat):
                with self.assertRaisesRegex(rs.Refused, msg):
                    cli.synthetic_pool(str(self.leases), A)

    def test_the_pool_path_must_be_absolute_and_canonical(self):
        (self.t / "pool-link").unlink(missing_ok=True)
        (self.t / "pool-link").symlink_to(self.leases)
        before = self.tree()
        for path in ("relative/pool", str(self.leases) + "/", f"{self.leases}/.", f"{self.t}/x/..",
                     f"{self.leases.parent}/./{self.leases.name}", f"{self.leases}/../{self.leases.name}", str(self.t / "pool-link"), "",
                     str(self.t / "missing")):
            with self.subTest(path=path):
                for action, p in self.every_action(path):
                    self.assertIn(p.returncode, (1, 2), f"{action}: {p.stderr}")
                    self.assertNotEqual(p.returncode, 0)
        self.assertEqual(self.tree(), before)
        (self.t / "afile").write_text("x")
        for path, why in (("relative/pool", "explicit absolute pool path"),
                          (str(self.t / "afile"), "not a directory of this user")):
            for action, p in self.every_action(path):
                self.assertIn(why, p.stderr, action)

    def test_the_synthetic_marker_must_be_present_well_formed_and_private(self):
        _, token = self.acquire(A)
        cases = {
            "missing": lambda: (self.leases / cli.SYNTHETIC).unlink(),
            "empty": lambda: self.mark(""),
            "wrong head": lambda: self.mark(MARK.replace("=1", "=2")),
            "extra line": lambda: self.mark(MARK + "extra=1\n"),
            "no newline": lambda: self.mark(MARK.rstrip("\n")),
            "slot not listed": lambda: self.mark(cli.SYNTHETIC_HEAD + f"\nslots={B} {R}\n"),
            "duplicate slot": lambda: self.mark(cli.SYNTHETIC_HEAD + f"\nslots={A} {A}\n"),
            "listed slot without a record": lambda: self.mark(MARK.replace("\n", " codegraph-worker-c\n", 2)
                                                              .replace(" codegraph-worker-c\n", "\n", 1)),
            "readable by others": lambda: self.mark(mode=0o644),
            "symlink": lambda: ((self.t / "m").write_text(MARK), (self.t / "m").chmod(0o600),
                                (self.leases / cli.SYNTHETIC).unlink(), (self.leases / cli.SYNTHETIC).symlink_to(self.t / "m")),
            "directory": lambda: ((self.leases / cli.SYNTHETIC).unlink(), (self.leases / cli.SYNTHETIC).mkdir()),
            "not ascii": lambda: self.mark(MARK + "é"),
        }
        for name, arrange in cases.items():
            with self.subTest(case=name):
                arrange()
                before = self.tree()
                for action, p in self.every_action(str(self.leases)):
                    self.assertEqual(p.returncode, 1, f"{action}: {p.stdout}{p.stderr}")
                    self.assertIn("nothing changed", p.stderr)
                self.assertEqual(self.tree(), before)
                m = self.leases / cli.SYNTHETIC
                if m.is_dir() and not m.is_symlink():
                    m.rmdir()
                elif os.path.lexists(m):
                    m.unlink()
                self.mark()
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)

    def test_an_unmigrated_pool_refuses_every_action(self):
        """G15: no recovery in an unmigrated pool; the presence fence there is unchanged."""
        self.setUp()
        self.assertEqual(self.pool("unmigrate").returncode, 0)
        self.mark()
        for s in SLOTS:
            self.put(s, pool_record.free_record(s, 0))
        before = self.tree()
        for action, p in self.every_action(str(self.leases)):
            self.assertEqual(p.returncode, 1, action)
            self.assertIn("not migrated", p.stderr)
        self.assertEqual(self.tree(), before)

    def test_unsupported_platforms_old_interpreters_and_bad_helper_output(self):
        with mock.patch.object(rs, "SUPPORTED", False):
            with self.assertRaisesRegex(rs.Refused, "not supported on this platform"):
                cli.synthetic_pool(str(self.leases), A)
        fakes = {"too old": "exit 3", "crashes": "kill -9 $$", "says nothing": "exit 0",
                 "names another pool": "echo synthetic-pool=/elsewhere; exit 0",
                 "truncated": "printf 'synthetic-pool='; exit 0",
                 "extra line": "printf 'synthetic-pool=%s\\nmore\\n' \"$5\"; exit 0"}
        d = self.t / "fakecli"
        d.mkdir(exist_ok=True)
        before = self.tree()
        for name, body in fakes.items():
            with self.subTest(fake=name):
                py = d / name.replace(" ", "-")
                py.write_text(f"#!/bin/sh\n{body}\n")
                py.chmod(0o755)
                for action, p in self.every_action(str(self.leases), CG_POOL_PYTHON=str(py)):
                    self.assertEqual(p.returncode, 1, f"{action}: {p.stdout}{p.stderr}")
                    self.assertIn("nothing changed", p.stderr)
        self.assertEqual(self.tree(), before)

    def test_mutating_actions_need_a_terminal_and_never_read_stdin(self):
        _, token = self.acquire(A)
        before = self.tree()
        for action, p in self.every_action(str(self.leases)):
            if action in ("inspect", "reconcile"):
                continue
            self.assertEqual(p.returncode, 1, f"{action}: {p.stdout}{p.stderr}")
        p = self.recover("adopt", stdin=open(os.devnull, "rb"), CG_RECOVERY_CONFIRM="yes")
        piped = subprocess.run(["/bin/bash", str(POOL), "recover", "adopt", "--pool", str(self.leases), "--slot", A],
                               input="\n".join([A, "x", "adopt"] * 5), capture_output=True, text=True, env=self.env,
                               start_new_session=True, timeout=60)
        for q in (p, piped):
            self.assertEqual(q.returncode, 1)
            self.assertIn("needs an interactive terminal", q.stderr)
        self.assertEqual(self.tree(), before)
        self.assertEqual(self.recover("inspect").returncode, 0)

    def test_no_bypass_option_and_no_approval_from_flags(self):
        before = self.tree()
        for extra in (["--force-live"], ["--unsafe"], ["--skip-interlock"], ["--yes"], ["--authorization", "x"],
                      ["--token", "a" * 32], ["--utc", "x"], ["--live"]):
            p = self.recover("adopt", A, None, *extra)
            self.assertEqual(p.returncode, 2, extra)
        for action in ("reassign", "rotate", "rotate-token", "eval", "hook", "free"):
            self.assertEqual(self.recover(action).returncode, 2, action)
        self.assertEqual(self.pool("recover").returncode, 2)
        self.assertEqual(self.pool("recover", "adopt", "--slot", A).returncode, 2)   # no default pool
        self.assertEqual(self.tree(), before)

    def test_the_helper_alone_refuses_without_the_slot_lock(self):
        _, token = self.acquire(A)
        before = self.tree()
        p = subprocess.run(["python3", "-I", "-B", str(HERE / "recovery_cli.py"), "adopt", str(self.leases), A],
                           capture_output=True, text=True, start_new_session=True, stdin=subprocess.DEVNULL)
        self.assertEqual(p.returncode, 1)
        self.assertIn("does not hold the slot lock", p.stderr)
        self.assertEqual(self.tree(), before)


class Lifecycle(Cli):
    def test_the_full_path_through_the_terminal(self):
        _, lease_token = self.acquire(A)
        op, token = self.path_to(A, "adopt")
        self.assertRegex(op, r"^rec-\d{8}T\d{6}Z-[0-9a-f]{8}$")
        self.assertEqual(self.gate(A)["recovery"], "active")
        self.ok("request", A, op)
        self.assertEqual(self.gate(A)["recovery_state"], rs.REQUESTED)
        out, text = self.ok("quarantine", A, op)
        token = TOKEN_SHOWN.findall(text)[-1]
        self.assertEqual(text.count(token), 1, "the token was shown more than once")
        self.assertIn("A lost token leaves the operation quarantined for good", text)
        self.assertEqual((self.rec(A).state, self.rec(A).recovery_op), ("quarantined", op))
        self.ok("quiescence", A, op, ctx={"token": token})
        self.ok("preservation", A, op, "--evidence", self.evidence(), ctx={"token": token})
        out, _ = self.ok("ready", A, op, ctx={"token": token})
        self.assertIn(f"state={rs.READY}", out)
        self.assertEqual(rs.RecoveryStore(self.leases / "recovery").terminal(A, op), rs.READY)
        # READY is not a free slot, and nothing in recover reassigns it.
        self.assert_blocked(A, lease_token, "blocked")
        _, tb = self.acquire()
        self.assertEqual(self.rec(B).state, "leased")
        self.assertEqual(self.pool("release", "--slot", B, "--token", tb).returncode, 0)
        self.assertFalse((self.leases / "recovery" / f".{A}.lock").exists())
        # The token stays out of every output, file, file name and process listing.
        for (code, so, se, transcript), ps in self.runs:
            self.assertNotIn(token, so + se)
            for snap in ps:
                self.assertNotIn(token, snap)
        for p in self.leases.rglob("*"):
            self.assertNotIn(token, p.name)
            if p.is_file():
                self.assertNotIn(token.encode(), p.read_bytes())
        for args in (("status",), ("assess", A)):
            p = self.pool(*args)
            self.assertNotIn(token, p.stdout + p.stderr)
        ins = self.recover("inspect", A, op)
        self.assertNotIn(token, ins.stdout + ins.stderr)
        self.assertIn("next=none; reassignment is not available in this phase", ins.stdout)

    def test_active_lets_the_holder_release_then_only_cancel_remains(self):
        """G6."""
        _, token = self.acquire(A)
        op, _ = self.path_to(A, "adopt")
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)
        self.assertEqual(self.gate(A)["recovery"], "blocked")
        self.assertEqual(self.pool("acquire", "--slot", A, "--task", "t", "--agent", "ag").returncode, 1)
        code, _, err, _ = self.tty("request", A, op)
        self.assertEqual(code, 1, err)
        self.assertIn("not the adopted lease", err)
        self.ok("cancel", A, op)
        self.assertEqual(self.gate(A)["recovery"], "cancelled")
        _, t2 = self.acquire(A)
        self.assertEqual(self.pool("release", "--slot", A, "--token", t2).returncode, 0)

    def test_a_request_blocks_the_holder_and_keeps_the_lease(self):
        """G7."""
        _, ta = self.acquire(A)
        _, tr = self.acquire(R)
        for slot in (A, R):
            op, _ = self.path_to(slot, "request")
        self.assert_blocked(A, ta, "blocked")
        self.assert_blocked(R, tr, "blocked")
        for slot in (A, R):
            state, d = self.status_of(slot)
            self.assertEqual((state, d["owner"], d["recovery_state"]), ("busy", OWNER, rs.REQUESTED))
            self.assertEqual(self.rec(slot).state, "leased")

    def test_cancel_is_terminal_never_frees_and_is_refused_after_quarantine(self):
        _, token = self.acquire(A)
        op, _ = self.path_to(A, "request")
        before = self.live(A)
        self.ok("cancel", A, op)
        self.assertEqual(self.live(A), before)
        self.assertEqual(self.gate(A)["recovery"], "cancelled")
        self.assertEqual(self.status_of(A)[0], "busy")
        out, _ = self.ok("cancel", A, op)
        self.assertIn("already CANCELLED; nothing changed", out)
        code, _, err, _ = self.tty("request", A, op)
        self.assertEqual(code, 1)
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)

        self.setUp()
        self.acquire(A)
        op, _ = self.path_to(A, "quarantine")
        before = self.tree()
        code, _, err, _ = self.tty("cancel", A, op)
        self.assertEqual(code, 1)
        self.assertIn("not a legal step", err)
        self.assertEqual(self.tree(), before)

    def test_a_wrong_retyped_value_changes_nothing(self):
        _, token = self.acquire(A)
        for label in RETYPES[:6]:
            with self.subTest(adopt=label):
                before = self.tree()
                code, out, err, _ = self.tty("adopt", ctx={"wrong": label})
                self.assertEqual(code, 1, out + err)
                self.assertIn("does not match; nothing changed", err)
                self.assertEqual(self.tree(), before)
        op, _ = self.path_to(A, "adopt")
        for step in ("request", "cancel"):
            for label in RETYPES:
                with self.subTest(step=step, retype=label):
                    before = self.tree()
                    code, _, err, _ = self.tty(step, A, op, ctx={"wrong": label})
                    self.assertEqual(code, 1, err)
                    self.assertEqual(self.tree(), before)
        self.ok("request", A, op)
        before = self.tree()
        code, _, err, text = self.tty("quarantine", A, op, ctx={"confirm_token": "b" * 32})
        self.assertEqual(code, 1)
        self.assertIn("recovery token was not confirmed; quarantine not started", err)
        self.assertEqual(self.tree(), before)

    def test_a_lost_or_wrong_token_strands_the_operation(self):
        self.acquire(A)
        op, token = self.path_to(A, "quarantine")
        before = self.tree()
        for wrong in ("0" * 32, token.upper(), "", token[:-1]):
            code, _, err, _ = self.tty("quiescence", A, op, ctx={"token": wrong})
            self.assertEqual(code, 1, err)
            self.assertIn("recovery token does not match", err)
            self.assertEqual(self.tree(), before)
        self.assertEqual(self.store_state(A, op)[0], rs.QUARANTINED)
        self.assertEqual(self.recover("rotate", A, op).returncode, 2)

    def test_adoption_takes_only_a_clean_v2_lease(self):
        for name, text in (("free", None), ("version 1", v1_lease()), ("quarantined", record("quarantined", generation=2))):
            with self.subTest(record=name):
                if text:
                    self.put(A, text)
                before = self.tree()
                code, _, err, _ = self.tty("adopt")
                self.assertEqual(code, 1, err)
                self.assertEqual(self.tree(), before)
        self.put(A, pool_record.free_record(A, 0))
        _, token = self.acquire(A)
        op, _ = self.path_to(A, "adopt")
        code, _, err, _ = self.tty("adopt")
        self.assertEqual(code, 1)
        self.assertIn(f"{op} is active", err)
        self.ok("cancel", A, op)
        lock = self.leases / "recovery" / A / ".adopt-op-9.0000000000000000"
        lock.mkdir(mode=0o700)
        code, _, err, _ = self.tty("adopt")
        self.assertEqual(code, 1)
        self.assertIn("is incomplete", err)
        lock.rmdir()
        out, _ = self.ok("adopt")
        self.assertNotEqual(self.op_of(out), op)


class Crash(Cli):
    def test_an_interrupted_request_needs_its_exact_authorization(self):
        self.acquire(A)
        op, _ = self.path_to(A, "adopt")
        err, term = self.inproc("request", A, op, ctx={"Reason": "first"}, fault=fault_at("after_intent"))
        self.assertIsInstance(err, cli.Unfinished)
        utc = display(term.text)["utc"]
        self.assertEqual(self.store_state(A, op), (rs.ACTIVE, self.rec(A).generation, True))
        self.assertIn("next=exact retry of request", self.recover("inspect", A, op).stdout)
        for original in ({"utc": "2026-01-01T00:00:00Z", "reason": "first"}, {"utc": utc, "reason": "second"}):
            before = self.tree()
            err, term = self.inproc("request", A, op, ctx={"original": original})
            self.assertIsInstance(err, rs.Refused)
            self.assertIn("differs from the interrupted step", str(err))
            self.assertNotIn("pool: ", term.text, "a changed authorization was displayed for confirmation")
            self.assertEqual(self.tree(), before)
        lines, _ = self.inproc("request", A, op, ctx={"original": {"utc": utc, "reason": "first"}})
        self.assertIn(f"state={rs.REQUESTED}", lines[0])

    def quarantine_interrupted(self, point, **patches):
        _, lease_token = self.acquire(A)
        op, _ = self.path_to(A, "request")
        pre = self.live(A)
        with mock.patch.multiple(pool_record, **patches) if patches else contextlib.nullcontext():
            err, term = self.inproc("quarantine", A, op, fault=fault_at(point) if point else None)
        self.assertIsInstance(err, cli.Unfinished, err)
        d = display(term.text)
        original = {"utc": d["utc"], "reason": "synthetic test", "quiescence_method": "owner-quiescence",
                    "preservation_disposition": "synthetic", "assessment_digest": "0" * 64}
        return op, pre, lease_token, TOKEN_SHOWN.findall(term.text)[-1], original

    def finish(self, op, original, token=None):
        lines, term = self.inproc("quarantine", A, op, ctx={"original": original, "token": token})
        self.assertNotIsInstance(lines, Exception, lines)
        self.assertIn(f"state={rs.QUARANTINED}", lines[0])
        self.assertEqual((self.rec(A).state, self.rec(A).recovery_op), ("quarantined", op))
        return term

    def test_a_quarantine_interrupted_before_the_record_replace(self):
        op, pre, lease_token, first, original = self.quarantine_interrupted("after_intent")
        self.assertEqual(self.live(A), pre)
        self.assertIn("reconcile=pre", self.recover("reconcile", A, op).stdout)
        self.assert_blocked(A, lease_token, "incomplete")
        term = self.finish(op, original)
        # The state never took the first token: a new one was issued and confirmed.
        self.assertIn("never recorded a token", term.text)
        second = TOKEN_SHOWN.findall(term.text)[-1]
        self.assertNotEqual(first, second)
        code, _, err, _ = self.tty("quiescence", A, op, ctx={"token": first})
        self.assertIn("recovery token does not match", err)
        self.ok("quiescence", A, op, ctx={"token": second})

    def test_a_quarantine_interrupted_after_the_record_replace(self):
        op, pre, lease_token, _, original = self.quarantine_interrupted("after_record")
        self.assertEqual(rs.parse_record(self.live(A), A).state, "quarantined")
        r = self.recover("reconcile", A, op)
        self.assertIn("reconcile=post", r.stdout)
        ins = self.recover("inspect", A, op).stdout
        self.assertIn("record=replaced", ins)
        self.assertIn("next=exact retry of quarantine", ins)
        changed = dict(original, reason="other")
        err, _ = self.inproc("quarantine", A, op, ctx={"original": changed})
        self.assertIn("differs from the interrupted step", str(err))
        self.finish(op, original)

    def test_a_quarantine_interrupted_after_the_state_needs_the_retained_token(self):
        op, pre, lease_token, token, original = self.quarantine_interrupted("after_state")
        before = self.tree()
        err, _ = self.inproc("quarantine", A, op, ctx={"original": original, "token": "c" * 32})
        self.assertIn("recovery token does not match", str(err))
        self.assertEqual(self.tree(), before)
        term = self.finish(op, original, token)
        self.assertEqual(TOKEN_SHOWN.findall(term.text), [], "a retry after the state showed a new token")

    def test_an_unconfirmed_record_flush_is_reconciled_against_the_record(self):
        real = pool_record._sync_dir

        def failing(path):
            if Path(path) == self.leases:
                raise OSError(5, "injected directory flush failure")
            return real(path)
        op, pre, lease_token, _, original = self.quarantine_interrupted(None, _sync_dir=failing)
        self.assertIn("reconcile=post", self.recover("reconcile", A, op).stdout)
        self.finish(op, original)

    def test_the_hook_refuses_a_changed_or_missing_record(self):
        """G9: the hook's own compare under the lock, with the library's check passed."""
        for name, change in (("changed", lambda p: p.write_text(p.read_text().replace("task=t", "task=u"))),
                             ("missing", lambda p: p.unlink())):
            with self.subTest(record=name):
                self.setUp()
                self.acquire(A)
                op, _ = self.path_to(A, "request")
                real = cli._replace_hook

                def hook(pool, slot, op_):
                    inner = real(pool, slot, op_)

                    def run(pre, post):
                        change(self.leases / f"{A}.lease")
                        return inner(pre, post)
                    return run
                with mock.patch.object(cli, "_replace_hook", hook):
                    err, _ = self.inproc("quarantine", A, op)
                self.assertIsInstance(err, cli.Unfinished)
                self.assertIn("slot record replace failed", str(err))
                if name == "changed":
                    self.assertNotEqual(rs.parse_record(self.live(A), A).state, "quarantined")
                    self.assertIn("reconcile=corrupt", self.recover("reconcile", A, op).stdout)
                    self.assertIn("owner must inspect", self.recover("inspect", A, op).stdout)
                else:
                    self.assertFalse((self.leases / f"{A}.lease").exists())
                    # A synthetic pool missing a slot record fails its own fixture check.
                    self.assertIn("lacks a regular slot record", self.recover("inspect", A, op).stderr)
                self.put(A, pool_record.free_record(A, 9))   # leaves no missing record for tearDown's status

    def test_a_mismatched_identity_or_generation_is_refused(self):
        _, token = self.acquire(A)
        op, _ = self.path_to(A, "request")
        adopted = self.live(A)
        r = rs.parse_record(adopted, A)
        for name, text in {"identity": adopted.replace(r.identity, "2" * 32),
                           "generation": adopted.replace(f"generation={r.generation}\n",
                                                         f"generation={r.generation + 1}\n"),
                           "token": adopted.replace(token, "e" * 32)}.items():
            with self.subTest(changed=name):
                self.put(A, text)
                before = self.tree()
                for step in ("quarantine", "cancel"):
                    err, _ = self.inproc(step, A, op)
                    self.assertIsInstance(err, rs.Refused)
                    self.assertEqual(self.tree(), before, step)
        self.put(A, adopted)

    def test_a_missing_terminal_marker_is_written_by_the_exact_repeat(self):
        _, token = self.acquire(A)
        op, _ = self.path_to(A, "request")
        err, term = self.inproc("cancel", A, op, fault=fault_at("before_marker_rename"))
        self.assertIsInstance(err, rs.Refused)
        self.assertEqual(self.gate(A)["recovery"], "incomplete")
        self.assertIn("terminal_marker=missing", self.recover("inspect", A, op).stdout)
        utc = display(term.text)["utc"]
        err, _ = self.inproc("cancel", A, op, ctx={"original": {"utc": "2026-01-01T00:00:00Z",
                                                                  "reason": "synthetic test"}})
        self.assertIn("differs from the interrupted step", str(err))
        lines, _ = self.inproc("cancel", A, op, ctx={"original": {"utc": utc, "reason": "synthetic test"}})
        self.assertIn(f"state={rs.CANCELLED}", lines[0])
        self.assertEqual(self.gate(A)["recovery"], "cancelled")
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)

    def test_the_cli_never_takes_a_second_lock(self):
        self.acquire(A)
        op, _ = self.path_to(A, "adopt")
        made = []
        real = os.mkdir

        def spy(p, *a, **k):
            made.append(str(p))
            return real(p, *a, **k)
        with self.locked(A), mock.patch.object(os, "mkdir", spy):
            lines = cli.run("request", str(self.leases), A, op, term=Script({}))
        self.assertIn(f"state={rs.REQUESTED}", lines[0])
        self.assertEqual([m for m in made if m.endswith(".lock")], [])
        self.assertFalse((self.leases / "recovery" / f".{A}.lock").exists())


class Hook(Cli):
    def prepared(self):
        self.acquire(A)
        op, _ = self.path_to(A, "request")
        pre = self.live(A)
        r = rs.parse_record(pre, A)
        return op, pre, r, rs.quarantined_record(A, op, r.generation + 1, r)

    def test_the_hook_checks_its_lock_and_the_post_image(self):
        op, pre, r, good = self.prepared()
        hook = cli._replace_hook(str(self.leases), A, op)
        with self.assertRaisesRegex(rs.Refused, "does not hold the slot lock"):
            hook(pre, good)
        bad = {"another operation": rs.quarantined_record(A, "op-other", r.generation + 1, r),
               "the same generation": rs.quarantined_record(A, op, r.generation, r),
               "another identity": good.replace(r.identity, "2" * 32),
               "a lease": pre}
        with self.locked(A):
            for name, post in bad.items():
                with self.subTest(post=name):
                    with self.assertRaisesRegex(rs.Refused, "does not follow from the adopted lease"):
                        hook(pre, post)
                    self.assertEqual(self.live(A), pre)
            self.assertEqual(hook(pre, good), good)
        self.put(A, pre)

    def test_the_hook_returns_what_it_reads_back(self):
        op, pre, r, good = self.prepared()
        hook = cli._replace_hook(str(self.leases), A, op)
        with self.locked(A), mock.patch.object(pool_record, "durable_replace",
                                               lambda path, text: Path(path).write_text(text.replace("task=t", "task=x"))):
            got = hook(pre, good)
        self.assertNotEqual(got, good)
        self.assertEqual(got, self.live(A))
        self.put(A, pre)

    def test_a_lying_hook_never_counts_as_the_replace(self):
        op, pre, r, good = self.prepared()
        with mock.patch.object(cli, "_replace_hook", lambda pool, slot, op_: (lambda p, post: post)):
            err, _ = self.inproc("quarantine", A, op)
        self.assertIsInstance(err, cli.Unfinished)
        self.assertIn("does not show it", str(err))
        self.assertEqual(self.live(A), pre)

    def test_adopt_refuses_an_unsound_recovery_directory(self):
        self.acquire(A)
        root = self.leases / "recovery"
        root.mkdir(mode=0o700)
        cases = (("an unexpected entry", lambda: (root / "notes").write_text("x"), lambda: (root / "notes").unlink()),
                 ("the store lock", lambda: (root / f".{A}.lock").mkdir(mode=0o700),
                  lambda: (root / f".{A}.lock").rmdir()))
        for name, arrange, cleanup in cases:
            with self.subTest(case=name):
                arrange()
                before = self.tree()
                err, _ = self.inproc("adopt", A)
                self.assertIsInstance(err, rs.Refused)
                self.assertIn("recovery state is", str(err))
                self.assertEqual(self.tree(), before)
                cleanup()

    def test_an_operation_on_a_lease_that_is_not_version_2_is_refused(self):
        self.acquire(A)
        other = self.t / "other-v1"
        shutil.rmtree(other, True)
        rs.RecoveryStore(other).adopt(A, "op-1", v1_lease())
        (self.leases / "recovery").mkdir(mode=0o700)
        shutil.copytree(other / A, self.leases / "recovery" / A)
        before = self.tree()
        for step in ("cancel", "request"):
            err, _ = self.inproc(step, A, "op-1")
            self.assertIn("version 2 leases", str(err), step)
            self.assertEqual(self.tree(), before)


class Locks(Cli):
    def test_lock_contenders_are_refused_while_the_owner_confirms(self):
        _, token = self.acquire(A)
        _, tr = self.acquire(R)
        s = Session(self, self.args("adopt", A, None, None), self.env, {})
        self.assertTrue(s.run(until="Retype the slot"))
        try:
            lock = self.leases / f".{A}.lock"
            self.assertTrue(lock.is_dir())
            for args in self.mutators(A, token) + [("migrate",), ("unmigrate",)]:
                p = self.pool(*args)
                self.assertEqual(p.returncode, 1, args)
                self.assertIn("lock held", p.stdout + p.stderr)
            for action in ("adopt", "request", "cancel"):
                p = self.recover(action, A, None if action == "adopt" else "op-1")
                self.assertEqual(p.returncode, 1)
                self.assertIn("lock held", p.stderr)
            self.assertNotEqual(self.status_of(A)[0], "free")
            self.assertEqual(self.recover("inspect").returncode, 0)
            self.assertFalse((self.leases / "recovery").exists())
        finally:
            os.write(s.master, (answer("Retype the slot: ", s.text.replace("\x00", ""), {}) + "\n").encode())
            s.run()
            code, out, err, _ = s.finish()
        self.assertEqual(code, 0, out + err)
        self.assertFalse((self.leases / f".{A}.lock").exists())
        self.assertEqual(self.pool("release", "--slot", R, "--token", tr).returncode, 0)

    def test_a_crash_leaves_the_lock_and_every_command_refuses(self):
        _, token = self.acquire(A)
        op, _ = self.path_to(A, "adopt")
        s = Session(self, self.args("request", A, op, None), self.env, {})
        self.assertTrue(s.run(until="Retype the slot"))
        os.killpg(s.p.pid, signal.SIGKILL)
        s.finish()
        lock = self.leases / f".{A}.lock"
        self.assertTrue(lock.is_dir(), "a killed run left no lock")
        before = self.tree()
        for args in self.mutators(A, token):
            self.assertIn("lock held", self.pool(*args).stderr)
        self.assertIn("lock held", self.recover("request", A, op).stderr)
        self.assertEqual(self.tree(), before)
        # The documented procedure: inspect read-only, see nothing unfinished, then remove the lock by hand.
        ins = self.recover("inspect", A, op)
        self.assertIn("pending=none", ins.stdout)
        lock.rmdir()
        self.ok("request", A, op)


class ReadOnly(Cli):
    def test_inspect_and_reconcile_change_nothing(self):
        _, token = self.acquire(A)
        self.assertEqual(self.recover("inspect").stdout.splitlines()[2], "gate=none")
        op, rtok = self.path_to(A, "quarantine")
        for _ in range(2):
            before = self.tree()
            for args in (("inspect", A, None), ("inspect", A, op), ("reconcile", A, op), ("inspect", B, None)):
                p = self.recover(*args)
                self.assertEqual(p.returncode, 0, p.stderr)
                self.assertNotIn(rtok, p.stdout + p.stderr)
                self.assertNotIn(token, p.stdout + p.stderr)
            self.assertEqual(self.tree(), before)
        out = self.recover("inspect", A, op).stdout
        for key in ("slot=", f"operation={op}", f"state={rs.QUARANTINED}", "seq=2", "pending=none",
                    "terminal_marker=none", "expected_record=v2 quarantined", "observed_record=v2 quarantined",
                    "record=replaced", "next=quiescence", "class=blocked"):
            self.assertIn(key, out)
        self.assertEqual(self.recover("reconcile", A, "op-none").returncode, 1)

    def test_inspect_reports_leftovers_and_unknown_entries(self):
        self.acquire(A)
        op, _ = self.path_to(A, "adopt")
        left = self.leases / "recovery" / A / op / ".state.json.0000000000000000.tmp"
        left.write_text("{}")
        left.chmod(0o600)
        out = self.recover("inspect", A).stdout
        self.assertIn("class=incomplete", out)
        code, _, err, _ = self.tty("request", A, op)
        self.assertEqual(code, 1)
        self.assertIn("temporary file", err)
        left.unlink()


class Boundaries(Cli):
    def test_no_evaluation_hook_option_or_reassignment(self):
        src = (HERE / "recovery_cli.py").read_text()
        tree = ast.parse(src)
        names = {n.id for n in ast.walk(tree) if isinstance(n, ast.Name)}
        attrs = {n.attr for n in ast.walk(tree) if isinstance(n, ast.Attribute)}
        imports = {a.name for n in ast.walk(tree) if isinstance(n, (ast.Import, ast.ImportFrom)) for a in n.names}
        self.assertEqual(names & {"eval", "exec", "compile", "__import__"}, set())
        self.assertEqual(imports & {"subprocess", "importlib", "pickle", "shlex", "runpy"}, set())
        self.assertEqual(attrs & {"system", "popen", "execv", "execvp", "spawnv"}, set())
        self.assertEqual(cli.ACTIONS, ("adopt", "request", "cancel", "quarantine", "quiescence", "preservation",
                                       "ready", "reconcile", "inspect"))
        defs = {n.name for n in ast.walk(tree) if isinstance(n, (ast.FunctionDef, ast.ClassDef))}
        self.assertEqual({d for d in defs if "reassign" in d or "rotate" in d}, set())
        self.assertNotIn("environ", src, "the command line reads no environment variable")
        pool = (HERE / "pool.sh").read_text()
        self.assertNotIn("recovery_state.py", pool)
        doc = (HERE.parent.parent / "docs" / "agent-pool.md").read_text()
        for claim in ("It authenticates nobody", "A lost token leaves the operation quarantined for",
                      "says nothing about the\nsecurity of a live recovery", "There is no rotation"):
            self.assertIn(claim, doc)
        self.assertEqual(pool.count("\"$HERE/recovery_cli.py\""), 3)

    def test_the_gate_still_decides_ordinary_commands(self):
        _, token = self.acquire(A)
        op, _ = self.path_to(A, "adopt")
        self.assertEqual(self.gate(A)["recovery"], "active")
        self.ok("request", A, op)
        self.assert_blocked(A, token, "blocked")
        self.ok("cancel", A, op)
        self.assertEqual(self.gate(A)["recovery"], "cancelled")
        self.assertEqual(self.pool("release", "--slot", A, "--token", token).returncode, 0)
        self.assertEqual(self.pool("unmigrate").returncode, 1)


if __name__ == "__main__":
    unittest.main()

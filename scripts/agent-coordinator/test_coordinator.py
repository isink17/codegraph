#!/usr/bin/env python3
"""Tests for coordinator.py against a throwaway pool, fake remote, fake gh,
fake merge gate and fake agents. Never touches the live pool.

    python3 scripts/agent-coordinator/test_coordinator.py
"""

import fcntl
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import textwrap
import time
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
# CG_TEST_COORD runs the suite against another coordinator, e.g. an older
# version, to reproduce the failures the newer tests guard against.
COORD = Path(os.environ.get("CG_TEST_COORD", HERE / "coordinator.py"))
POOL = HERE.parent / "agent-pool" / "pool.sh"
A, B, R = "codegraph-worker-a", "codegraph-worker-b", "codegraph-review"

# gh stand-in: PRs live in a JSON file; a PR's head is read from the fake remote.
FAKE_GH = textwrap.dedent('''\
    #!/usr/bin/env python3
    import json, os, subprocess, sys
    db_path = os.environ["FAKE_GH_DB"]
    db = json.load(open(db_path))
    a = sys.argv[1:]
    def head(branch):
        return subprocess.run(["git", "--git-dir", os.environ["FAKE_ORIGIN"], "rev-parse", "refs/heads/" + branch],
                              capture_output=True, text=True).stdout.strip()
    def opt(name):
        return a[a.index(name) + 1]
    if a[:2] == ["pr", "list"]:
        print(json.dumps([{"number": p["number"], "state": p["state"]} for p in db["prs"] if p["head"] == opt("--head")]))
    elif a[:2] == ["pr", "create"]:
        n = len(db["prs"]) + 1
        db["prs"].append({"number": n, "head": opt("--head"), "base": opt("--base"), "state": "OPEN", "checks": []})
        json.dump(db, open(db_path, "w"))
        print(f"https://example.invalid/pull/{n}")
    elif a[:2] == ["pr", "view"]:
        p = next(p for p in db["prs"] if p["number"] == int(a[2]))
        h = p.get("merged_head") or head(p["head"])
        print(json.dumps({"number": p["number"], "state": p["state"], "headRefOid": h,
                          "headRefName": p["head"], "baseRefName": p["base"], "statusCheckRollup": p["checks"],
                          "mergeCommit": {"oid": p["squash"]} if p.get("squash") else None}))
    elif a[:1] == ["api"] and a[1].endswith("/rulesets/24420032"):
        if os.path.exists(os.environ.get("FAKE_GH_DOWN", "/nonexistent")):
            sys.exit("fake gh: api unavailable")
        print(open(os.environ["FAKE_RULESET"]).read())
    elif a[:2] == ["pr", "update-branch"]:
        import tempfile
        p = next(p for p in db["prs"] if p["number"] == int(a[2]))
        w = tempfile.mkdtemp()
        g = lambda *x: subprocess.run(["git", "-C", w, *x], check=True, capture_output=True)
        subprocess.run(["git", "clone", "-q", "-b", p["head"], os.environ["FAKE_ORIGIN"], w], check=True)
        g("merge", "-q", "--no-edit", "origin/" + p["base"])
        g("push", "-q", "origin", "HEAD:" + p["head"])
        print("updated")
    else:
        sys.exit("fake gh: unsupported " + " ".join(a))
''')

# Merge gate stand-in: checks the review file names the PR and head; can be told to refuse.
FAKE_GATE = textwrap.dedent('''\
    #!/bin/sh
    [ $# -eq 0 ] && { echo "usage: gate [--base BASE] PR HEAD_SHA REVIEW_FILE [--merge]" >&2; exit 2; }
    [ "$1" = --base ] && [ "$2" = v2.0 ] && shift 2
    [ -e "$FAKE_GATE_REFUSE" ] && { echo "merge-gate: BLOCKED: test"; exit 1; }
    grep -qx "PR: #$1" "$3" && grep -qx "Reviewed-Head: $2" "$3" && grep -qx "Verdict: APPROVE" "$3" &&
        grep -qx "Blocking: 0" "$3" || { echo "merge-gate: BLOCKED: review"; exit 1; }
    echo "merge-gate: PASS: PR #$1 head $2"
''')

GREEN = [{"name": "ci", "conclusion": "SUCCESS"}, {"name": "race", "conclusion": "SUCCESS"}]

# codex stand-in. Per task, log/codex-plan.json lists one action per
# execution (the last repeats); every call is recorded in log/codex-calls.jsonl
# with what the sandbox let it see and do.
FAKE_CODEX = textwrap.dedent('''\
    #!/usr/bin/env python3
    import json, os, re, subprocess, sys
    from pathlib import Path
    log = Path(os.environ["CG_TEST_LOG"]).parent
    prompt = sys.argv[-1]
    if prompt.startswith("You are the project manager"):
        with open(log / "codex-calls.jsonl", "a") as f:
            canary = (log / "pm-canary").read_text() if (log / "pm-canary").exists() else ""
            try:
                open(canary).read()
                readable = True
            except OSError:
                readable = False
            f.write(json.dumps({"key": "pm", "role": "pm", "cwd": os.getcwd(), "canary_readable": readable}) + "\\n")
        print(json.dumps({"type": "item.completed", "item": {"type": "agent_message",
                                                             "text": (log / "pm-answer.json").read_text()}}))
        sys.exit(0)
    role = "review" if prompt.startswith("Independent review") else "implement"
    tid = re.search(r"[Tt]ask ([A-Za-z0-9._-]+):", prompt).group(1)
    key = tid if role == "implement" else "review:" + tid
    calls = log / "codex-calls.jsonl"
    n = sum(1 for l in calls.read_text().splitlines() if json.loads(l)["key"] == key) if calls.exists() else 0
    plan = json.loads((log / "codex-plan.json").read_text()) if (log / "codex-plan.json").exists() else {}
    acts = plan.get(key) or ["commit" if role == "implement" else "approve"]
    act = acts[min(n, len(acts) - 1)]
    home = Path(os.environ.get("CODEX_HOME", ""))
    def can(f):
        try:
            f()
            return True
        except Exception:
            return False
    def touch_slot():
        Path("probe-write").write_text("x")
        Path("probe-write").unlink()
    def make_ref():
        subprocess.run(["git", "branch", "probe-ref"], check=True, capture_output=True)
        subprocess.run(["git", "branch", "-D", "probe-ref"], capture_output=True)
    rec = {"key": key, "role": role, "act": act, "argv": sys.argv[1:-1], "prompt": prompt,
           "codex_home": str(home), "auth_copied": (home / "auth.json").is_file(),
           "keychain_readable": can(lambda: (Path.home() / "Library/Keychains/probe").read_text()),
           "state_readable": can(lambda: (Path(os.environ["CG_COORD_STATE"]) / "state.json").read_text()),
           "slot_write_ok": can(touch_slot), "ref_write_ok": can(make_ref)}
    with open(calls, "a") as f:
        f.write(json.dumps(rec) + "\\n")
    with open(os.environ["CG_TEST_LOG"], "a") as f:
        f.write(f"start {key}\\n")
    def emit(kind, **item):
        print(json.dumps({"type": kind, "item": item} if item else {"type": kind}), flush=True)
    def say(text):
        emit("item.completed", type="agent_message", text=text)
    emit("thread.started")
    if role == "review":
        verdict = "REQUEST_CHANGES\\nBlocking: 1" if act == "reject" else "APPROVE\\nBlocking: 0"
        say("reviewed\\nVerdict: " + verdict)
        sys.exit(0)
    if act == "claim-done":
        say("DONE. All acceptance criteria are met and tests pass.")
        sys.exit(0)
    if act == "exit1":
        sys.exit(1)
    if act == "sleep-commit":
        import time
        time.sleep(4)
    os.makedirs("work", exist_ok=True)
    with open(f"work/{tid}.txt", "a") as f:
        f.write(f"{tid} {n}\\n")
    for cmd in (["git", "add", "work"], ["git", "commit", "-qm", tid]):
        r = subprocess.run(cmd, capture_output=True, text=True)
        emit("item.completed", type="command_execution", command=" ".join(cmd), exit_code=r.returncode)
    if act in ("refresh-commit", "foreign-commit"):
        a = json.loads((home / "auth.json").read_text())
        if act == "refresh-commit":
            a["tokens"]["access_token"] = "refreshed"
        else:
            a["tokens"]["account_id"] = "acct-2"
        (home / "auth.json").write_text(json.dumps(a))
    say("DONE")
''')


def install_fake_codex(t, env):
    """Puts the fake codex first on PATH, with a fake login and a keychain
    file the sandbox must hide."""
    (t / "bin/codex").write_text(FAKE_CODEX)
    (t / "bin/codex").chmod(0o755)
    (t / "home/.codex").mkdir(parents=True, exist_ok=True)
    (t / "home/.codex/auth.json").write_text(json.dumps(
        {"auth_mode": "chatgpt", "tokens": {"account_id": "acct-1", "access_token": "a"}}))
    (t / "home/Library/Keychains").mkdir(parents=True, exist_ok=True)
    (t / "home/Library/Keychains/probe").write_text("secret")
    env["PATH"] = f"{t / 'bin'}:{env['PATH']}"


def sh(script):
    return ["/bin/sh", "-c", script]


def commit(name, sleep=0):
    """Fake implementer: optionally sleeps, logs its start/end, commits a file."""
    return sh(f'echo "start $(date +%s.%N 2>/dev/null || date +%s) {name}" >>"$CG_TEST_LOG"; sleep {sleep}; '
              f'mkdir -p work && echo {name}-$RANDOM >>work/{name}.txt && git add work && '
              f'git commit -qm {name} && echo "end {name}" >>"$CG_TEST_LOG"')


APPROVE = sh('git diff --stat "origin/v2.0...HEAD" >/dev/null && printf "looks fine\\nVerdict: APPROVE\\nBlocking: 0\\n"')


class Fixture(unittest.TestCase):
    """Throwaway remote, slots, leases and fakes; shared with test_campaign.py."""

    def setUp(self):
        self.t = Path(tempfile.mkdtemp()).resolve()
        self.all_out = ""
        # Agents may write /private/tmp, so fixtures there would make every
        # sandbox assertion pass or fail for the wrong reason.
        if str(self.t).startswith("/private/tmp/"):
            shutil.rmtree(self.t, True)
            self.fail(f"TMPDIR {tempfile.gettempdir()} is under /private/tmp, which the agent sandbox leaves "
                      "writable; run the tests with the default TMPDIR")
        self.addCleanup(shutil.rmtree, self.t, True)
        t = self.t
        self.origin = t / "origin.git"
        self.seed = t / "seed"
        self.slots = t / "slots"
        self.leases = t / "leases"
        self.state = t / "state"
        # Agents run sandboxed: they may write only under log/ (and their slot).
        (t / "log").mkdir()
        (t / "home").mkdir()
        self.cwds = t / "log/cwds"
        self.cwds.write_text("")
        self.db = t / "gh.json"
        self.db.write_text(json.dumps({"prs": []}))
        self.log = t / "log/agents.log"
        (t / "bin").mkdir()
        for name, body in (("gh", FAKE_GH), ("gate", FAKE_GATE)):
            (t / "bin" / name).write_text(body)
            (t / "bin" / name).chmod(0o755)
        self.env = {**os.environ,
                    "CG_POOL_ROOT": str(self.slots), "CG_POOL_LEASES": str(self.leases),
                    "CG_POOL_CWD_FILE": str(self.cwds), "CG_POOL_REPO": str(self.seed),
                    "CG_COORD_STATE": str(self.state), "CG_COORD_POOL": str(POOL),
                    "CG_COORD_GH": str(t / "bin/gh"), "CG_COORD_GATE": str(t / "bin/gate"),
                    "FAKE_GH_DB": str(self.db), "FAKE_ORIGIN": str(self.origin),
                    "FAKE_GATE_REFUSE": str(t / "gate-refuse"), "CG_TEST_LOG": str(self.log),
                    "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t",
                    "GIT_COMMITTER_EMAIL": "t@t", "GIT_CONFIG_GLOBAL": "/dev/null",
                    "HOME": str(t / "home"), "CG_COORD_SANDBOX_WRITABLE": str(t / "log"),
                    # Agents inherit only these (and the basics): see ENV_KEEP.
                    "CG_COORD_SANDBOX_ENV": "CG_TEST_LOG:CG_COORD_STATE:CTL:FAKE_ORIGIN:GIT_AUTHOR_NAME:"
                                            "GIT_AUTHOR_EMAIL:GIT_COMMITTER_NAME:GIT_COMMITTER_EMAIL"}
        self.git("init", "-q", "--bare", str(self.origin))
        self.git("clone", "-q", str(self.origin), str(self.seed))
        self.git("-C", str(self.seed), "commit", "-q", "--allow-empty", "-m", "base")
        self.git("-C", str(self.seed), "push", "-q", "origin", "HEAD:v2.0")
        self.git("-C", str(self.seed), "fetch", "-q")
        for s in (A, B, R):
            self.git("-C", str(self.seed), "worktree", "add", "-q", "--detach", str(self.slots / s), "origin/v2.0")

    # ------------------------------------------------------------ helpers

    def git(self, *args):
        return subprocess.run(["git", *args], env=self.env, check=True, capture_output=True, text=True).stdout.strip()

    def coord(self, *args, ok=None, **env):
        p = subprocess.run([sys.executable, str(COORD), *args], env={**self.env, **env},
                           capture_output=True, text=True)
        self.out = p.stdout + p.stderr
        self.all_out += self.out
        if ok is not None:
            self.assertEqual(p.returncode == 0, ok, self.out)
        return p

    def plan(self, *tasks):
        f = self.t / "tasks.json"
        f.write_text(json.dumps({"tasks": list(tasks)}))
        self.coord("plan", str(f), ok=True)

    def task(self, tid, agent_cmd, scope=None, review=APPROVE, **kw):
        return {"id": tid, "description": f"task {tid}", "agent": "command", "command": agent_cmd,
                "scope": scope or [f"work/{tid}*"], "acceptance": ["works"], "validate": ["true"],
                "review": {"agent": "command", "command": review}, **kw}

    def st(self):
        return json.loads((self.state / "state.json").read_text())["tasks"]

    def status(self, tid):
        return self.st()[tid]["status"]

    def lease(self, slot):
        f = self.leases / f"{slot}.lease"
        return f.read_text() if f.exists() else None

    def pool_state(self, slot):
        p = subprocess.run([str(POOL), "status", slot], env=self.env, capture_output=True, text=True)
        return p.stdout.split("\n")[0].split(": ")[1]

    def run_until(self, done, limit=30):
        for _ in range(int(limit / 0.2)):
            self.coord("run", "--once")
            if done():
                return
            time.sleep(0.2)
        self.fail(f"condition not reached; status: {self.st()}\n{self.out}")

    def pr(self, n):
        return next(p for p in json.loads(self.db.read_text())["prs"] if p["number"] == n)

    def set_pr(self, n, **kw):
        db = json.loads(self.db.read_text())
        next(p for p in db["prs"] if p["number"] == n).update(kw)
        self.db.write_text(json.dumps(db))

    def to_ready(self, tid):
        self.run_until(lambda: self.status(tid) == "awaiting_ci")
        self.set_pr(self.st()[tid]["pr"], checks=GREEN)
        self.run_until(lambda: self.status(tid) == "ready_for_owner")

    def assert_no_token_leak(self):
        tokens = [l.split("=", 1)[1] for f in self.leases.glob("*.lease") for l in f.read_text().splitlines()
                  if l.startswith("token=")]
        for f in [self.state / "state.json", self.state / "events.jsonl", *self.state.glob("runs/*/*")]:
            if f.exists():
                for tok in tokens:
                    self.assertNotIn(tok, f.read_text(errors="replace"), f)
        for tok in tokens:
            self.assertNotIn(tok, self.out)


class Pool(Fixture):
    # ------------------------------------------------------------ scheduling

    def test_two_concurrent_third_queued_then_full_flow(self):
        self.plan(self.task("t1", commit("t1", 2)), self.task("t2", commit("t2", 2)), self.task("t3", commit("t3")))
        self.coord("run", "--once", ok=True)
        st = self.st()
        self.assertEqual(sorted(t["status"] for t in st.values()), ["pending", "running", "running"])
        self.assertIn("BLOCKED_POOL_BUSY", self.out)
        self.assertEqual({st["t1"]["slot"], st["t2"]["slot"]}, {A, B})
        self.assertIsNone(self.lease(R))
        self.assert_no_token_leak()
        self.run_until(lambda: all(t["status"] == "awaiting_ci" for t in self.st().values()), 60)
        starts = [l for l in self.log.read_text().splitlines()]
        # t1 and t2 both started before either ended: they overlapped.
        self.assertEqual([l.split()[0] for l in starts[:2]], ["start", "start"], starts)
        for tid in ("t1", "t2", "t3"):
            t = self.st()[tid]
            self.assertTrue(t["review_file"])
            art = Path(t["review_file"]).read_text()
            self.assertIn(f"Reviewed-Head: {t['approved_head']}", art)
            self.assertEqual(t["approved_head"], self.git("--git-dir", str(self.origin), "rev-parse", f"task/{tid}"))
            self.set_pr(t["pr"], checks=GREEN)
        self.run_until(lambda: all(t["status"] == "ready_for_owner" for t in self.st().values()))
        self.assertIn("MERGE_READY", self.out)
        for s in (A, B, R):
            self.assertEqual(self.pool_state(s), "free")
        self.assert_no_token_leak()

    def test_dependency_and_scope_overlap_wait(self):
        self.plan(self.task("a", commit("a")), self.task("b", commit("b"), depends_on=["a"]),
                  self.task("c", commit("c"), scope=["work/a*"]))
        self.coord("run", "--once", ok=True)
        self.assertEqual(self.status("a"), "running")
        self.assertEqual(self.status("b"), "pending")  # dependency not completed
        self.assertEqual(self.status("c"), "pending")  # overlaps a's scope; a worker is still free
        self.assertEqual(self.pool_state(B), "free")
        self.to_ready("a")
        self.assertEqual(self.status("b"), "pending")  # ready is not completed
        self.assertEqual(self.status("c"), "pending")  # a's PR is still open
        self.set_pr(self.st()["a"]["pr"], state="MERGED")
        self.run_until(lambda: self.status("b") != "pending" and self.status("c") != "pending")
        self.assertEqual(self.status("a"), "completed")

    def test_plan_rejects_bad_specs(self):
        f = self.t / "bad.json"
        for tasks in ([self.task("x", commit("x"), depends_on=["y"])],
                      [self.task("x", commit("x"), depends_on=["y"]), self.task("y", commit("y"), depends_on=["x"])],
                      [{"id": "x", "description": "d"}],
                      [{**self.task("x", commit("x")), "agent": "nope"}]):
            f.write_text(json.dumps({"tasks": tasks}))
            self.coord("plan", str(f), ok=False)
        self.assertFalse((self.state / "state.json").exists())

    # ------------------------------------------------------------ leases

    def test_same_slot_race_between_two_coordinators(self):
        # Two coordinators with separate state race for one free worker.
        self.git("-C", str(self.slots / B), "switch", "-q", "-c", "occupied")  # B not idle
        envs = []
        for i in (1, 2):
            e = {**self.env, "CG_COORD_STATE": str(self.t / f"state{i}")}
            f = self.t / f"tasks{i}.json"
            f.write_text(json.dumps({"tasks": [self.task(f"r{i}", commit(f"r{i}", 1))]}))
            subprocess.run([sys.executable, str(COORD), "plan", str(f)], env=e, check=True, capture_output=True)
            envs.append(e)
        procs = [subprocess.Popen([sys.executable, str(COORD), "run", "--once"], env=e,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True) for e in envs]
        for p in procs:
            p.communicate()
        running = [json.loads((self.t / f"state{i}/state.json").read_text())["tasks"][f"r{i}"]["status"]
                   for i in (1, 2)]
        self.assertEqual(sorted(running), ["pending", "running"])

    def test_second_coordinator_on_same_state_refused(self):
        self.plan(self.task("x", commit("x")))
        fd = os.open(self.state / "coordinator.lock", os.O_RDWR)
        fcntl.flock(fd, fcntl.LOCK_EX)
        try:
            self.coord("run", "--once", ok=False)
            self.assertIn("another coordinator owns", self.out)
        finally:
            os.close(fd)
        self.assertEqual(self.status("x"), "pending")

    def test_crash_during_active_task_and_restart_with_existing_lease(self):
        self.plan(self.task("x", commit("x", 2)))
        p = subprocess.Popen([sys.executable, str(COORD), "run", "--poll", "0.2"], env=self.env,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(50):
            if (self.state / "state.json").exists() and self.status("x") == "running":
                break
            time.sleep(0.1)
        p.send_signal(signal.SIGKILL)
        p.wait()
        slot = self.st()["x"]["slot"]
        lease = self.lease(slot)
        self.assertIsNotNone(lease)  # the lease survives the crash
        self.coord("run", "--once", ok=True)  # agent still running: no new action, no second slot
        self.assertEqual(self.status("x"), "running")
        self.assertEqual(self.lease(slot), lease)
        other = B if slot == A else A
        self.assertIsNone(self.lease(other))
        self.run_until(lambda: self.status("x") == "awaiting_ci")

    def test_run_on_an_empty_queue_leaves_a_usable_state(self):
        (self.slots / R / "dirty.txt").write_text("x")  # makes prepare log an event
        self.coord("run", "--once", ok=True)
        self.assertTrue((self.state / "events.jsonl").exists())
        self.coord("status", ok=True)
        self.coord("run", "--once", ok=True)

    def test_missing_or_malformed_state(self):
        self.plan(self.task("x", commit("x", 1)))
        self.coord("run", "--once", ok=True)
        good = (self.state / "state.json").read_text()
        (self.state / "state.json").write_text("{not json")
        self.coord("status", ok=False)
        self.assertIn("malformed", self.out)
        self.assertEqual((self.state / "state.json").read_text(), "{not json")
        (self.state / "state.json").unlink()
        self.coord("run", "--once", ok=False)
        self.assertIn("missing", self.out)
        self.assertIsNotNone(self.lease(A))  # nothing was released or re-acquired
        (self.state / "state.json").write_text(good)
        self.run_until(lambda: self.status("x") == "awaiting_ci")

    # ------------------------------------------------------------ agent failures

    def test_failed_agent_keeps_lease_and_work(self):
        self.plan(self.task("x", sh("mkdir -p work && echo half > work/x.txt && exit 7")))
        self.run_until(lambda: self.status("x") == "failed")
        t = self.st()["x"]
        self.assertIn("exited 7", t["reason"])
        self.assertIsNotNone(self.lease(t["slot"]))
        self.assertEqual((self.slots / t["slot"] / "work/x.txt").read_text(), "half\n")  # dirty work preserved
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "failed")  # never retried or released automatically

    def test_exit_zero_is_not_success(self):
        self.plan(self.task("nocommit", sh("true")),
                  self.task("outside", sh("echo z > other.txt && git add other.txt && git commit -qm z")),
                  self.task("badvalidate", commit("badvalidate"), validate=["false"]))
        self.run_until(lambda: sum(t["status"] == "failed" for t in self.st().values()) == 2, 40)
        # Failed tasks keep both workers; the third stays queued.
        self.coord("run", "--once")
        self.assertIn("BLOCKED_POOL_BUSY", self.out)
        reasons = {t["id"]: t.get("reason", "") for t in self.st().values() if t["status"] == "failed"}
        queued = [t["id"] for t in self.st().values() if t["status"] == "pending"]
        self.assertEqual(len(queued), 1)
        expect = {"nocommit": "no new commit", "outside": "outside scope", "badvalidate": "validation failed"}
        for tid, why in reasons.items():
            self.assertIn(expect[tid], why)
        self.assertEqual(json.loads(self.db.read_text())["prs"], [])

    def test_rename_from_outside_scope_is_refused(self):
        self.git("-C", str(self.seed), "switch", "-q", "--detach", "origin/v2.0")
        (self.seed / "secret.txt").write_text("keep\n")
        self.git("-C", str(self.seed), "add", "secret.txt")
        self.git("-C", str(self.seed), "commit", "-q", "-m", "secret")
        self.git("-C", str(self.seed), "push", "-q", "origin", "HEAD:v2.0")
        self.plan(self.task("x", sh("mkdir -p work && git mv secret.txt work/x.txt && git commit -qm mv")))
        self.run_until(lambda: self.status("x") == "failed")
        self.assertIn("secret.txt", self.st()["x"]["reason"])

    def crash_after_acquire(self, status, review=False):
        """Simulates a coordinator killed after pool.sh leased a slot but
        before the slot was saved: only the starting marker is on disk."""
        st = json.loads((self.state / "state.json").read_text())
        slot = R if review else "auto"
        st["tasks"]["x"].update(status=status, starting=slot, slot=None)
        (self.state / "state.json").write_text(json.dumps(st))
        args = ["acquire", "--slot", slot, "--task", "x", "--agent", "coordinator/command"]
        subprocess.run([str(POOL), *args, *(["--review"] if review else [])], env=self.env, check=True,
                       capture_output=True)
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "interrupted")
        self.assertIn("did not complete", self.st()["x"]["reason"])
        leased = [s for s in (A, B, R) if self.lease(s)]
        self.assertEqual(len(leased), 1, leased)  # no second slot taken

    def test_crash_between_acquire_and_save_never_takes_a_second_slot(self):
        self.plan(self.task("x", commit("x")))
        self.crash_after_acquire("pending")

    def test_crash_between_review_acquire_and_save(self):
        out = subprocess.run([str(POOL), "acquire", "--slot", R, "--task", "hold", "--agent", "t", "--review"],
                             env=self.env, capture_output=True, text=True, check=True).stdout
        self.plan(self.task("x", commit("x")))
        self.run_until(lambda: self.status("x") == "awaiting_review")
        subprocess.run([str(POOL), "release", "--slot", R, "--token", out.split("token=")[1].strip()],
                       env=self.env, check=True, capture_output=True)
        self.crash_after_acquire("awaiting_review", review=True)

    def test_failed_git_fetch_leaves_task_pending(self):
        self.git("-C", str(self.seed), "remote", "set-url", "origin", str(self.t / "missing.git"))
        self.plan(self.task("x", commit("x")))
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "pending")
        self.assertIsNone(self.lease(A))
        self.assertIsNone(self.lease(B))
        self.git("-C", str(self.seed), "remote", "set-url", "origin", str(self.origin))
        self.run_until(lambda: self.status("x") == "running" or self.status("x") == "awaiting_ci")

    def test_failed_prepare_of_free_slot(self):
        self.plan(self.task("x", commit("x")))
        (self.slots / A / "stray").write_text("x")  # A cannot be prepared or leased
        self.git("-C", str(self.seed), "commit", "-q", "--allow-empty", "-m", "advance")
        self.git("-C", str(self.seed), "push", "-q", "origin", "HEAD:v2.0")
        self.coord("run", "--once", ok=True)
        self.assertEqual(self.st()["x"]["slot"], B)
        self.assertTrue((self.slots / A / "stray").exists())
        self.assertIn("prepare_refused", (self.state / "events.jsonl").read_text())

    def test_stale_process_information(self):
        self.plan(self.task("x", commit("x", 5)))
        self.coord("run", "--once", ok=True)
        st = json.loads((self.state / "state.json").read_text())
        info = st["tasks"]["x"]["process"]
        os.killpg(info["pid"], signal.SIGKILL)  # agent dies without an exit status
        time.sleep(0.3)
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "interrupted")
        self.assertIsNotNone(self.lease(A))
        # A recorded pid whose start time does not match is not "our" process.
        st = json.loads((self.state / "state.json").read_text())
        st["tasks"]["x"].update(status="running", process={**info, "pid": os.getpid(), "start": "Mon Jan  1 00:00:00 1990"})
        (self.state / "state.json").write_text(json.dumps(st))
        self.coord("cancel", "x")  # must not signal this test process
        self.assertEqual(self.status("x"), "interrupted")

    def test_cancel_preserves_uncommitted_work(self):
        self.plan(self.task("x", sh("mkdir -p work && echo wip > work/x.txt && sleep 30")))
        self.coord("run", "--once", ok=True)
        time.sleep(0.5)
        self.coord("cancel", "x", ok=True)
        t = self.st()["x"]
        self.assertEqual(t["status"], "interrupted")
        self.assertEqual((self.slots / A / "work/x.txt").read_text(), "wip\n")
        self.assertIsNotNone(self.lease(A))
        self.assertEqual(self.git("-C", str(self.slots / A), "symbolic-ref", "--short", "HEAD"), "task/x")

    # ------------------------------------------------------------ review and GitHub

    def test_review_blocked_by_busy_review_slot(self):
        out = subprocess.run([str(POOL), "acquire", "--slot", R, "--task", "other", "--agent", "human", "--review"],
                             env=self.env, capture_output=True, text=True, check=True).stdout
        self.plan(self.task("x", commit("x")))
        self.run_until(lambda: self.status("x") == "awaiting_review")
        self.coord("run", "--once")
        self.assertIn("BLOCKED_REVIEW_BUSY", self.out)
        self.assertEqual(self.status("x"), "awaiting_review")
        self.assertIn(out.split("token=")[1].strip(), self.lease(R))  # foreign lease untouched

    def test_review_rejection_and_rework(self):
        marker = self.t / "log/reviewed-once"
        review = sh(f'if [ -e {marker} ]; then printf "Verdict: APPROVE\\nBlocking: 0\\n"; '
                    f'else touch {marker}; printf "fix the thing\\nVerdict: REQUEST_CHANGES\\nBlocking: 1\\n"; fi')
        self.plan(self.task("x", commit("x"), review=review))
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        events = [json.loads(l) for l in (self.state / "events.jsonl").read_text().splitlines()]
        reviews = [e for e in events if e["event"] == "reviewed"]
        self.assertEqual([e["verdict"] for e in reviews], ["REQUEST_CHANGES", "APPROVE"])
        self.assertNotEqual(reviews[0]["head"], reviews[1]["head"])
        self.assertIn("Verdict: REQUEST_CHANGES", Path(reviews[0]["artifact"]).read_text())
        self.assertIn("review_changes_required", [e.get("new") for e in events])
        t = self.st()["x"]
        self.assertEqual(t["approved_head"], reviews[1]["head"])
        self.assertEqual(t["attempt"], 2)
        self.assertEqual(t["pr"], 1)
        self.assertEqual(len(json.loads(self.db.read_text())["prs"]), 1)
        self.assertEqual(self.git("-C", str(self.slots / R), "status", "--porcelain"), "")

    def test_head_change_after_approval_invalidates_review(self):
        self.plan(self.task("x", commit("x")))
        self.to_ready("x")
        clone = self.t / "c"
        self.git("clone", "-q", "-b", "task/x", str(self.origin), str(clone))
        self.git("-C", str(clone), "commit", "-q", "--allow-empty", "-m", "late")
        self.git("-C", str(clone), "push", "-q", "origin", "task/x")
        self.coord("run", "--once")
        self.assertIn(self.status("x"), ("awaiting_review", "reviewing"))
        self.assertIn("head_changed", (self.state / "events.jsonl").read_text())

    def test_ci_failure_and_cancellation_block_then_recover(self):
        self.plan(self.task("x", commit("x")))
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        n = self.st()["x"]["pr"]
        for bad in ("FAILURE", "CANCELLED"):
            self.set_pr(n, checks=[{"name": "ci", "conclusion": bad}])
            self.coord("run", "--once")
            self.assertEqual(self.status("x"), "blocked")
            self.assertIn(bad, self.st()["x"]["reason"])
        self.set_pr(n, checks=[{"name": "ci", "status": "IN_PROGRESS"}])
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "awaiting_ci")
        self.set_pr(n, checks=GREEN)
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "ready_for_owner")
        (self.t / "gate-refuse").write_text("")
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "blocked")
        self.assertIn("merge gate refused", self.st()["x"]["reason"])
        (self.t / "gate-refuse").unlink()  # e.g. mergeable was UNKNOWN: rechecked on the next pass
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "ready_for_owner")
        self.set_pr(n, checks=[{"name": "ci", "conclusion": "SUCCESS"}, {"name": "ci", "conclusion": "FAILURE"}])
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "blocked")  # a duplicate failing check is not hidden

    def test_base_advance_after_review_blocks_until_new_review(self):
        self.plan(self.task("x", commit("x")))
        self.to_ready("x")
        self.git("-C", str(self.seed), "fetch", "-q")
        self.git("-C", str(self.seed), "switch", "-q", "--detach", "origin/v2.0")
        self.git("-C", str(self.seed), "commit", "-q", "--allow-empty", "-m", "advance")
        self.git("-C", str(self.seed), "push", "-q", "origin", "HEAD:v2.0")
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "blocked")
        self.assertIn("stale base", self.st()["x"]["reason"])
        # Owner updates the branch: new head, so a new review and CI are required.
        clone = self.t / "c"
        self.git("clone", "-q", "-b", "task/x", str(self.origin), str(clone))
        self.git("-C", str(clone), "merge", "-q", "--no-edit", "origin/v2.0")
        self.git("-C", str(clone), "push", "-q", "origin", "task/x")
        self.coord("run", "--once")
        self.assertIn(self.status("x"), ("awaiting_review", "reviewing"))
        self.run_until(lambda: self.status("x") == "ready_for_owner")

    def test_interrupted_release_keeps_lease(self):
        self.plan(self.task("x", sh(f'echo "$PWD" >{self.cwds}; mkdir -p work; echo 1 >work/x.txt; '
                                    'git add work; git commit -qm x')))
        self.run_until(lambda: self.status("x") == "blocked")
        t = self.st()["x"]
        self.assertIn("release", t["reason"])
        self.assertIsNotNone(self.lease(A))  # pool.sh refused: a process is in the slot
        self.assertEqual(self.git("--git-dir", str(self.origin), "rev-parse", "task/x"), t["head"])  # pushed
        self.cwds.write_text("")
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "blocked")  # owner decides; nothing retried behind their back
        self.assertIsNotNone(self.lease(A))

    # ------------------------------------------------------------ v1.1 helpers

    def events(self):
        return [json.loads(l) for l in (self.state / "events.jsonl").read_text().splitlines()]

    def lease_field(self, slot, key):
        text = self.lease(slot) or ""
        return next((l.split("=", 1)[1] for l in text.splitlines() if l.startswith(key + "=")), None)

    def foreign_lease(self, slot, task="other"):
        out = subprocess.run([str(POOL), "acquire", "--slot", slot, "--task", task, "--agent", "human",
                              *(["--review"] if slot == R else [])],
                             env=self.env, capture_output=True, text=True, check=True).stdout
        return out.split("token=")[1].strip()

    def pool_release(self, slot, token):
        subprocess.run([str(POOL), "release", "--slot", slot, "--token", token], env=self.env, check=True,
                       capture_output=True)

    def set_task(self, tid, **kw):
        st = json.loads((self.state / "state.json").read_text())
        st["tasks"][tid].update(kw)
        (self.state / "state.json").write_text(json.dumps(st))

    def crash_at(self, point, limit=30):
        """Runs passes with the fault armed until the coordinator dies there."""
        for _ in range(int(limit / 0.2)):
            if self.coord("run", "--once", CG_COORD_FAULT=point).returncode == 70:
                return
            time.sleep(0.2)
        self.fail(f"fault {point} never reached; status: {self.st()}\n{self.out}")

    def c_start(self, pid):
        p = subprocess.run(["ps", "-o", "lstart=", "-p", str(pid)], capture_output=True, text=True)
        return p.stdout.strip() or None

    def starts(self, tid):
        return self.log.read_text().split().count(tid) // 2 if self.log.exists() else 0

    # ------------------------------------------------------------ merge gate

    def install_gate(self, body):
        """Commits body as the merge gate on origin/v2.0 and returns the main
        checkout's copy of it."""
        self.git("-C", str(self.seed), "switch", "-q", "--detach", "origin/v2.0")
        g = self.seed / ".github/scripts/merge-gate.sh"
        g.parent.mkdir(parents=True, exist_ok=True)
        g.write_text(body)
        g.chmod(0o755)
        self.git("-C", str(self.seed), "add", str(g))
        self.git("-C", str(self.seed), "commit", "-qm", "gate")
        self.git("-C", str(self.seed), "push", "-q", "origin", "HEAD:v2.0")
        return g

    def test_default_gate_is_the_base_version_not_the_main_checkout(self):
        del self.env["CG_COORD_GATE"]
        g = self.install_gate(FAKE_GATE.replace("merge-gate: PASS", "merge-gate: PASS base-gate"))
        obsolete = "#!/bin/sh\necho 'merge-gate: unknown option --base' >&2; exit 1\n"
        g.write_text(obsolete)  # the main checkout holds an older, uncommitted gate
        self.plan(self.task("x", commit("x")))
        self.to_ready("x")
        verify = [e for e in self.events() if e["event"] == "merge_gate_verify"]
        self.assertIn("base-gate", verify[-1]["output"])
        self.assertEqual(g.read_text(), obsolete)  # main checkout untouched

    def test_default_gate_ignores_a_moved_remote_tracking_ref(self):
        del self.env["CG_COORD_GATE"]
        self.install_gate(FAKE_GATE.replace("merge-gate: PASS", "merge-gate: PASS base-gate"))
        self.plan(self.task("x", commit("x")))
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        self.set_pr(self.st()["x"]["pr"], checks=GREEN)
        # Something local points origin/v2.0 elsewhere and blocks the fetch.
        ref = self.seed / ".git/refs/remotes/origin/v2.0"
        ref.write_text(self.git("-C", str(self.seed), "rev-parse", "HEAD~1") + "\n")
        Path(f"{ref}.lock").write_text("")
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "ready_for_owner")
        verify = [e for e in self.events() if e["event"] == "merge_gate_verify"]
        self.assertIn("base-gate", verify[-1]["output"])  # read from the remote, not the moved ref

    def test_default_gate_ignores_forged_objects_in_the_shared_store(self):
        del self.env["CG_COORD_GATE"]
        self.install_gate(FAKE_GATE.replace("merge-gate: PASS", "merge-gate: PASS base-gate"))
        self.plan(self.task("x", commit("x")))
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        # Replace the gate blob's loose object in the shared store with one
        # holding other content under the same name (as an agent could).
        blob = self.git("-C", str(self.seed), "rev-parse", "origin/v2.0:.github/scripts/merge-gate.sh")
        obj = self.seed / ".git/objects" / blob[:2] / blob[2:]
        forged = self.t / "forged"
        forged.write_text("#!/bin/sh\necho FORGED; exit 2 # --base\n")
        fake = self.git("-C", str(self.seed), "hash-object", "-w", str(forged))
        obj.chmod(0o644)
        obj.write_bytes((self.seed / ".git/objects" / fake[:2] / fake[2:]).read_bytes())
        self.assertIn("FORGED", self.git("-C", str(self.seed), "cat-file", "blob", blob))
        self.set_pr(self.st()["x"]["pr"], checks=GREEN)
        self.run_until(lambda: self.status("x") == "ready_for_owner")
        verify = [e for e in self.events() if e["event"] == "merge_gate_verify"]
        self.assertIn("base-gate", verify[-1]["output"])

    def test_incompatible_gate_override_blocks_until_fixed(self):
        old = self.t / "bin/oldgate"
        old.write_text("#!/bin/sh\necho 'usage: oldgate PR HEAD REVIEW [--merge]' >&2; exit 2\n")
        old.chmod(0o755)
        self.plan(self.task("x", commit("x")))
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        self.set_pr(self.st()["x"]["pr"], checks=GREEN)
        for gate, why in ((old, "does not support --base"), (self.t / "bin/missing", "not an executable")):
            self.coord("run", "--once", CG_COORD_GATE=str(gate))
            self.assertEqual(self.status("x"), "blocked")
            self.assertIn(why, self.st()["x"]["reason"])
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "ready_for_owner")

    # ------------------------------------------------------------ acquire transactions

    def mangling_pool(self):
        """pool.sh stand-in whose acquire really leases but prints garbage."""
        f = self.t / "bin/pool-mangle"
        f.write_text(f'#!/bin/sh\nif [ "$1" = acquire ]; then "{POOL}" "$@" >/dev/null; rc=$?; '
                     f'echo garbage; exit $rc; fi\nexec "{POOL}" "$@"\n')
        f.chmod(0o755)
        return str(f)

    def test_acquire_with_malformed_output_fails_closed(self):
        self.plan(self.task("x", commit("x")))
        self.coord("run", "--once", CG_COORD_POOL=self.mangling_pool())
        t = self.st()["x"]
        self.assertEqual(t["status"], "interrupted")
        self.assertIn("did not yield its token", t["reason"])
        txn = t["starting"]["txn"]  # the marker is kept
        self.assertEqual(self.lease_field(A, "owner"), f"coordinator:{txn}")
        lease = self.lease(A)
        for _ in range(2):
            self.coord("run", "--once")
            self.assertEqual(self.status("x"), "interrupted")
            self.assertEqual(self.lease(A), lease)  # never deleted or overwritten
            self.assertIsNone(self.lease(B))  # never a second slot
        self.assert_no_token_leak()
        self.assertIn(txn, (self.state / "events.jsonl").read_text())
        self.assertIn(f"owner=coordinator:{txn}", self.out)  # the owner is told what to inspect
        # The owner inspects and releases the lease: the transaction then
        # provably holds nothing and the task is queued again.
        self.pool_release(A, self.lease_field(A, "token"))
        self.run_until(lambda: self.status("x") == "awaiting_ci")

    def crash_in_acquire(self, point):
        self.plan(self.task("x", commit("x")))
        self.assertEqual(self.coord("run", "--once", CG_COORD_FAULT=point).returncode, 70)
        self.assertIn("starting", self.st()["x"])
        self.assertIsNotNone(self.lease(A))
        self.assertFalse(list((self.state / "tokens").glob("x.*.token")))
        self.coord("run", "--once")
        self.assertNotIn(self.status("x"), ("pending", "interrupted"))
        self.assertIsNone(self.lease(B))
        rec = [e for e in self.events() if e["event"] in ("acquire_begin", "acquire_recovered")]
        self.assertEqual([(e["event"], e["slot"]) for e in rec][:2], [("acquire_begin", "auto"), ("acquire_recovered", A)])
        self.assert_no_token_leak()
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        self.assertEqual(self.starts("x"), 1)

    def test_crash_before_parsing_acquire_output(self):
        self.crash_in_acquire("acquire_before_parse")

    def test_crash_before_persisting_token(self):
        self.crash_in_acquire("acquire_before_token")

    def test_restart_with_acquire_marker_and_occupied_slots(self):
        self.plan(self.task("x", commit("x")))
        tok_a, tok_b = self.foreign_lease(A), self.foreign_lease(B)
        marker = {"slot": "auto", "txn": "0" * 32, "from": "pending"}  # crashed before pool.sh ran
        self.set_task("x", starting=marker)
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "pending")
        self.assertNotIn("starting", self.st()["x"])
        self.assertIn("BLOCKED_POOL_BUSY", self.out)
        self.assertIn(tok_a, self.lease(A))
        self.assertIn(tok_b, self.lease(B))
        # A slot lock left by a killed pool.sh may hide a lease: stays open.
        self.set_task("x", starting=marker)
        (self.leases / f".{A}.lock").mkdir()
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "interrupted")
        self.assertIn("slot lock", self.st()["x"]["reason"])
        (self.leases / f".{A}.lock").rmdir()
        self.pool_release(A, tok_a)
        self.run_until(lambda: self.status("x") == "running")
        t = self.st()["x"]
        self.assertEqual(self.lease_field(t["slot"], "owner"), f"coordinator:{t['lease_txn']}")
        self.assertIn(tok_b, self.lease(B))

    def test_stale_or_missing_token_files(self):
        self.plan(self.task("x", commit("x", 1)), self.task("y", commit("y", 1), scope=["work/y*"]))
        self.coord("run", "--once", ok=True)
        st = self.st()
        (self.state / f"tokens/x.{st['x']['slot']}.token").write_text("0" * 32 + "\n")  # stale
        (self.state / f"tokens/y.{st['y']['slot']}.token").unlink()  # missing
        self.run_until(lambda: self.status("x") == "blocked" and self.status("y") == "interrupted")
        self.assertIn("release", self.st()["x"]["reason"])
        self.assertIn("no recorded slot or token", self.st()["y"]["reason"])
        self.assertIsNotNone(self.lease(st["x"]["slot"]))
        self.assertIsNotNone(self.lease(st["y"]["slot"]))

    def test_concurrent_acquire_by_two_coordinators(self):
        envs = []
        for i in (1, 2):
            e = {**self.env, "CG_COORD_STATE": str(self.t / f"state{i}")}
            f = self.t / f"tasks{i}.json"
            f.write_text(json.dumps({"tasks": [self.task(f"r{i}", commit(f"r{i}", 1))]}))
            subprocess.run([sys.executable, str(COORD), "plan", str(f)], env=e, check=True, capture_output=True)
            envs.append(e)
        procs = [subprocess.Popen([sys.executable, str(COORD), "run", "--once"], env=e,
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) for e in envs]
        for p in procs:
            p.wait()
        slots = set()
        for i in (1, 2):
            t = json.loads((self.t / f"state{i}/state.json").read_text())["tasks"][f"r{i}"]
            self.assertEqual(t["status"], "running")
            self.assertEqual(self.lease_field(t["slot"], "owner"), f"coordinator:{t['lease_txn']}")
            slots.add(t["slot"])
        self.assertEqual(slots, {A, B})

    # ------------------------------------------------------------ launch handshake

    def crash_in_launch(self, point, sleep=2):
        self.plan(self.task("x", commit("x", sleep)))
        self.assertEqual(self.coord("run", "--once", CG_COORD_FAULT=point).returncode, 70)
        self.assertEqual(self.status("x"), "running")

    def test_crash_before_process_creation(self):
        self.crash_in_launch("launch_before_popen")
        self.assertIsNone(self.st()["x"]["process"]["pid"])
        for _ in range(2):
            self.coord("run", "--once")
            self.assertEqual(self.status("x"), "interrupted")
            self.assertIn("never started", self.st()["x"]["reason"])
        self.assertIsNotNone(self.lease(A))  # lease and slot kept for the owner
        self.assertIsNone(self.lease(B))
        time.sleep(0.5)
        self.assertEqual(self.starts("x"), 0)  # nothing was launched, nothing relaunched

    def adopt_after(self, point):
        self.crash_in_launch(point)
        self.coord("run", "--once")
        self.assertIn("process_adopted", [e["event"] for e in self.events()])
        self.assertEqual(self.status("x"), "running")
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        self.assertEqual(self.starts("x"), 1)

    def test_crash_immediately_after_process_creation(self):
        self.adopt_after("launch_after_popen")

    def test_crash_before_recording_process_identity(self):
        self.adopt_after("launch_before_save")

    def test_crash_after_identity_before_launch_event(self):
        self.crash_in_launch("launch_after_save")
        self.assertIsNotNone(self.st()["x"]["process"]["pid"])
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        self.assertEqual(self.starts("x"), 1)

    def test_crash_after_exit_before_reconciliation(self):
        self.plan(self.task("x", commit("x")))
        self.coord("run", "--once", ok=True)
        self.crash_at("finish_before_checks")
        self.assertEqual(self.status("x"), "running")
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        self.assertEqual(self.starts("x"), 1)
        self.assertEqual(len(json.loads(self.db.read_text())["prs"]), 1)

    # ------------------------------------------------------------ cancellation

    def test_cancel_reports_incomplete_when_agent_ignores_sigterm(self):
        self.plan(self.task("x", sh('trap "" TERM; mkdir -p work; echo wip > work/x.txt; sleep 30')))
        self.coord("run", "--once", ok=True)
        time.sleep(0.5)
        pid = self.st()["x"]["process"]["pid"]
        self.addCleanup(lambda: subprocess.run(["kill", "-9", f"-{pid}"], capture_output=True))
        p = self.coord("cancel", "x", CG_COORD_CANCEL_WAIT="1")
        self.assertEqual(p.returncode, 1)
        self.assertIn("cancellation incomplete", self.out)
        t = self.st()["x"]
        self.assertEqual(t["status"], "running")  # not claimed stopped
        self.assertTrue(t["cancel_requested"])
        lease = self.lease(A)
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "running")
        self.assertEqual(self.lease(A), lease)  # slot never reused
        # The wrapper (group leader) is gone; the group is still verified
        # and signalled, and still reported as surviving.
        self.assertIsNone(self.c_start(pid))
        self.assertEqual(self.coord("cancel", "x", CG_COORD_CANCEL_WAIT="0.5").returncode, 1)
        self.assertIn("cancellation incomplete", self.out)
        os.killpg(pid, signal.SIGKILL)
        for _ in range(50):
            try:
                os.killpg(pid, 0)
            except ProcessLookupError:
                break
            time.sleep(0.1)
        self.coord("run", "--once")
        t = self.st()["x"]
        self.assertEqual(t["status"], "interrupted")
        self.assertIn("cancelled by owner", t["reason"])
        self.assertEqual(self.lease(A), lease)
        self.assertEqual((self.slots / A / "work/x.txt").read_text(), "wip\n")

    def test_cancel_when_agent_exits_during_cancellation(self):
        self.plan(self.task("x", sh('trap "mkdir -p work; echo t > work/x.txt; git add work; git commit -qm t; '
                                    'exit 0" TERM; sleep 30 & wait')))
        self.coord("run", "--once", ok=True)
        time.sleep(0.5)
        self.coord("cancel", "x", ok=True)
        self.assertEqual(self.status("x"), "interrupted")
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "interrupted")  # exit 0 after cancel is never published
        self.assertEqual(json.loads(self.db.read_text())["prs"], [])
        self.assertIsNotNone(self.lease(A))

    # ------------------------------------------------------------ release transactions

    def test_crash_after_release_before_state_save(self):
        self.plan(self.task("x", commit("x")))
        self.crash_at("release_before_save")
        t = self.st()["x"]
        self.assertEqual(t["status"], "running")
        self.assertIn("releasing", t)
        self.assertIsNone(self.lease(A))  # pool.sh did release
        self.coord("run", "--once")
        ev = [e["event"] for e in self.events()]
        self.assertIn("release_recovered", ev)
        self.assertEqual(ev.count("released"), 1)
        self.assertIn(self.status("x"), ("awaiting_review", "reviewing"))
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        acquires = [e for e in self.events() if e["event"] == "acquire_begin"]
        self.assertEqual([e["slot"] for e in acquires], ["auto", R])  # worker never reacquired

    def test_release_recovery_refuses_unproven_pr_head(self):
        self.plan(self.task("x", commit("x")))
        self.crash_at("release_before_save")
        clone = self.t / "c"
        self.git("clone", "-q", "-b", "task/x", str(self.origin), str(clone))
        self.git("-C", str(clone), "commit", "-q", "--allow-empty", "-m", "foreign")
        self.git("-C", str(clone), "push", "-q", "origin", "task/x")
        for _ in range(2):
            self.coord("run", "--once")
            self.assertEqual(self.status("x"), "interrupted")
            self.assertIn("not the published", self.st()["x"]["reason"])
        self.assertTrue(all(self.lease(s) is None for s in (A, B, R)))  # nothing reacquired

    def test_crash_after_review_release(self):
        self.plan(self.task("x", commit("x")))
        self.run_until(lambda: self.status("x") == "reviewing")
        self.crash_at("release_before_save")
        self.assertEqual(self.status("x"), "reviewing")
        self.assertIsNone(self.lease(R))
        self.coord("run", "--once")
        self.assertEqual(self.status("x"), "awaiting_ci")
        self.assertTrue(self.st()["x"]["review_file"])

    # ------------------------------------------------------------ isolation

    @unittest.skipUnless(sys.platform == "darwin", "sandbox-exec is macOS only")
    def test_agents_cannot_read_tokens_or_leases(self):
        probe = ('cat "$CG_COORD_STATE"/tokens/* "$CG_POOL_LEASES"/*.lease 2>&1; '
                 'ls "$CG_COORD_STATE"/tokens 2>&1; echo leak >>"$CG_COORD_STATE"/state.json 2>&1; ')
        self.plan(self.task("x", sh(probe + 'mkdir -p work && echo x > work/x.txt && git add work && '
                                            'git commit -qm x'),
                            validate=[probe + "true"], review=sh(probe + 'printf "Verdict: APPROVE\\nBlocking: 0\\n"')))
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        logs = [f for f in self.state.glob("runs/*/*") if f.name in ("output.log", "validate-0.log")]
        self.assertEqual(len(logs), 3)
        for f in logs:
            text = f.read_text()
            self.assertIn("Operation not permitted", text, f)
            self.assertNotIn("token=", text, f)
            self.assertIsNone(re.search(r"(?<![0-9a-f])[0-9a-f]{32}(?![0-9a-f])", text), f)
        json.loads((self.state / "state.json").read_text())  # the appends were refused

    @unittest.skipUnless(sys.platform == "darwin", "sandbox-exec is macOS only")
    def test_agents_cannot_plant_code_the_coordinator_runs(self):
        """Every file that unsandboxed code (the coordinator, pool.sh, git,
        gh) later executes or reads configuration from stays unwritable."""
        wt = self.seed / ".git/worktrees" / A
        targets = {"hook": self.seed / ".git/hooks/pre-push", "config": self.seed / ".git/config",
                   "commondir": wt / "commondir", "gitdir": wt / "gitdir", "pointer": self.slots / A / ".git",
                   "pool": POOL, "coordinator": COORD, "gitconfig": self.t / "home/.gitconfig",
                   "path-shim": self.t / "bin/gh", "claude-settings": self.t / "home/.claude/settings.json",
                   "claude-json": self.t / "home/.claude.json",
                   "shell-snapshot": self.t / "home/.claude/shell-snapshots/snapshot-zsh-1.sh",
                   "session-env": self.t / "home/.claude/session-env/s/env.sh",
                   "memory": self.t / "home/.claude/projects/p/memory/MEMORY.md",
                   "ide-lock": self.t / "home/.claude/ide/1.lock",
                   "slot-claude": self.slots / A / ".claude/settings.local.json",
                   "slot-mcp": self.slots / A / ".mcp.json",
                   "nested-claude": self.slots / A / "work/.claude/settings.json", "go-bin": self.t / "home/go/bin/codegraph"}
        for d in ("home/.claude/shell-snapshots", "home/.claude/session-env/s", "home/.claude/projects/p/memory",
                  "home/.claude/ide", "home/go/bin", f"slots/{A}/.claude", f"slots/{A}/work/.claude"):
            (self.t / d).mkdir(parents=True)
        before = {k: f.read_bytes() if f.exists() else None for k, f in targets.items()}
        plant = "".join(f'(echo "#!/bin/sh" >>{f}) 2>/dev/null && echo PLANTED-{k}; ' for k, f in targets.items())
        plant += f'git config core.hooksPath {self.t} 2>/dev/null && echo PLANTED-hookspath; '
        remote_ref = self.seed / ".git/refs/remotes/origin/v2.0"
        before["remote-ref"] = remote_ref.read_bytes() if remote_ref.exists() else None
        plant += (f'(git rev-parse HEAD >{remote_ref}) 2>/dev/null && echo PLANTED-remote-ref; '
                  f'(: >{remote_ref}.lock) 2>/dev/null && echo PLANTED-ref-lock; ')
        targets["remote-ref"] = remote_ref
        alternates = self.seed / ".git/objects/info/alternates"
        before["alternates"] = alternates.read_bytes() if alternates.exists() else None
        targets["alternates"] = alternates
        plant += f'(echo {self.t} >>{alternates}) 2>/dev/null && echo PLANTED-alternates; '
        self.plan(self.task("x", sh(plant + 'mkdir -p work && echo x > work/x.txt && git add work && '
                                            'git commit -qm x')))
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        log = next(self.state.glob("runs/x-implement-*/output.log")).read_text()
        self.assertNotIn("PLANTED", log)
        self.assertFalse(Path(f"{remote_ref}.lock").exists())
        for k, f in targets.items():
            self.assertEqual(f.read_bytes() if f.exists() else None, before[k], k)

    @unittest.skipUnless(sys.platform == "darwin", "sandbox-exec is macOS only")
    def test_owner_writable_dirs_cannot_cover_protected_paths(self):
        self.plan(self.task("x", commit("x")))
        (self.t / "home/.config/git").mkdir(parents=True)
        self.coord("run", "--once", CG_COORD_SANDBOX_WRITABLE=f"{self.t / 'log'}:{self.t / 'home/.config'}")
        self.assertEqual(self.status("x"), "blocked")
        self.assertIn("contains protected", self.st()["x"]["reason"])
        self.assertFalse(list(Path(tempfile.gettempdir()).glob("cg-agent-*/x")))

    def test_ignored_tool_configuration_in_slot_fails_the_task(self):
        # Unsandboxed command agents (or a sandbox gap) could leave it.
        self.plan(self.task("x", commit("x")))
        self.coord("run", "--once", ok=True)
        exclude = Path(self.git("-C", str(self.slots / A), "rev-parse", "--git-path", "info/exclude"))
        if not exclude.is_absolute():
            exclude = self.slots / A / exclude
        exclude.parent.mkdir(parents=True, exist_ok=True)
        exclude.write_text(".claude/\n")
        (self.slots / A / ".claude").mkdir()
        (self.slots / A / ".claude/settings.local.json").write_text("{}")
        self.run_until(lambda: self.status("x") == "failed")
        self.assertIn("ignored tool configuration", self.st()["x"]["reason"])
        self.assertIsNotNone(self.lease(A))

    def test_plan_rejects_bad_max_attempts(self):
        f = self.t / "attempts.json"
        for bad in (0, 6, "2", True):
            f.write_text(json.dumps({"tasks": [self.task("x", commit("x"), max_attempts=bad)]}))
            self.coord("plan", str(f), ok=False)
            self.assertIn("max_attempts", self.out)

    # ------------------------------------------------------------ continuation and STOP

    def test_agent_that_stops_short_is_continued_in_place(self):
        # First execution exits 0 without a commit, the second leaves dirty
        # work, the third commits it: one slot, one lease, one PR.
        n = self.t / "log/n"
        agent = sh(f'echo x >>{n}; c=$(wc -l <{n}); echo "start $c" >>"$CG_TEST_LOG"; '
                   f'[ $c -eq 1 ] && exit 0; mkdir -p work; echo $c >work/x.txt; [ $c -eq 2 ] && exit 0; '
                   f'git add work && git commit -qm x')
        self.plan(self.task("x", agent, max_attempts=3))
        self.run_until(lambda: self.status("x") == "awaiting_ci", 60)
        cont = [e for e in self.events() if e["event"] == "continue"]
        self.assertEqual([e["reason"].split(";")[0] for e in cont],
                         ["agent produced no new commit", f"agent left {A} dirty"])
        self.assertEqual(len([e for e in self.events() if e["event"] == "acquired" and e["slot"] != R]), 1)
        self.assertEqual(self.st()["x"]["tries"], 3)
        self.assertEqual(len(json.loads(self.db.read_text())["prs"]), 1)

    def test_continuation_is_bounded(self):
        self.plan(self.task("x", commit("x"), validate=["false"], max_attempts=2))
        self.run_until(lambda: self.status("x") == "failed", 40)
        self.assertEqual(len(self.starts_of("x")), 2)
        self.assertIn("validation failed", self.st()["x"]["reason"])
        self.assertIsNotNone(self.lease(self.st()["x"]["slot"]))
        self.coord("run", "--once")
        self.assertEqual(len(self.starts_of("x")), 2)

    def test_stop_file_prevents_launch_and_continuation(self):
        stop = self.state / "STOP"
        self.state.mkdir(exist_ok=True)
        self.plan(self.task("a", sh("sleep 1; true"), max_attempts=3))
        self.coord("run", "--once", ok=True)
        self.assertEqual(self.status("a"), "running")
        stop.write_text("")
        self.plan(self.task("b", commit("b")))
        self.run_until(lambda: self.status("a") == "interrupted")
        self.assertIn("not continued", self.st()["a"]["reason"])
        self.assertEqual(self.status("b"), "pending")
        self.assertIn("STOP", self.out)
        stop.unlink()
        self.run_until(lambda: self.status("b") == "awaiting_ci")

    # ------------------------------------------------------------ authorization gate

    def gate(self, *ids, raw=None):
        g = self.t / "gate.json"
        g.write_text(raw if raw is not None else json.dumps({"tasks": list(ids)}))
        self.env["CG_COORD_AUTHORIZED"] = str(g)

    def test_gate_holds_unauthorized_tasks_and_fails_closed(self):
        self.gate("a")
        self.plan(self.task("a", commit("a")), self.task("b", commit("b")))
        self.coord("run", "--once", ok=True)
        self.assertEqual((self.status("a"), self.status("b")), ("running", "pending"))
        self.assertIn(("b", "implement"), [(e["task"], e.get("action")) for e in self.events()
                                            if e["event"] == "deauthorized"])
        self.gate(raw="{not json")
        self.run_until(lambda: self.status("a") == "awaiting_review")
        self.coord("run", "--once")
        self.assertEqual((self.status("a"), self.status("b")), ("awaiting_review", "pending"))  # nothing reviewed
        self.gate("a", "b")
        self.run_until(lambda: self.status("a") == "awaiting_ci" and self.status("b") == "awaiting_ci")

    def test_revoked_task_is_not_continued_and_resumes_when_reauthorized(self):
        n = self.t / "log/n"
        agent = sh(f'echo x >>{n}; [ $(wc -l <{n}) -eq 1 ] && exit 0; '
                   'mkdir -p work && echo x >work/x.txt && git add work && git commit -qm x')
        self.gate("x")
        self.plan(self.task("x", agent, max_attempts=3))
        self.coord("run", "--once", ok=True)
        self.gate()
        self.run_until(lambda: self.status("x") == "blocked")
        t = self.st()["x"]
        self.assertTrue(t["reason"].startswith("deauthorized"))
        self.assertIsNotNone(self.lease(t["slot"]))  # work and lease kept
        self.gate("x")
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        self.assertIn("reauthorized", [e["event"] for e in self.events()])

    def test_claude_agents_need_an_explicit_opt_in(self):
        self.plan({**self.task("x", None), "agent": "claude", "command": None})
        self.run_until(lambda: self.status("x") == "blocked")
        self.assertIn("CG_COORD_ALLOW_CLAUDE", self.st()["x"]["reason"])

    def test_reauthorized_task_without_its_lease_is_not_launched(self):
        self.gate("x")
        self.plan(self.task("x", sh("true"), max_attempts=3))
        self.coord("run", "--once", ok=True)
        self.gate()
        self.run_until(lambda: self.status("x") == "blocked")
        t = self.st()["x"]
        token = (self.state / "tokens" / f"x.{t['slot']}.token").read_text().strip()
        subprocess.run([str(POOL), "release", "--slot", t["slot"], "--token", token], env=self.env, check=True,
                       capture_output=True)
        self.foreign_lease(t["slot"])
        self.gate("x")
        self.run_until(lambda: self.status("x") == "interrupted")
        self.assertIn("lease", self.st()["x"]["reason"])
        self.assertEqual(len(self.starts_of("x")), 1)

    def test_review_refuses_a_checkout_that_differs_from_the_remote(self):
        """An agent rewrote a loose object in the shared store: the review
        slot would show content the remote does not have."""
        import zlib
        self.gate("x")
        self.plan(self.task("x", commit("x")))
        self.coord("run", "--once", ok=True)
        self.gate()  # hold the review until the store is tampered with
        self.run_until(lambda: self.status("x") == "awaiting_review", 30)
        self.gate("x")
        blob = self.git("-C", str(self.seed), "rev-parse", "task/x:work/x.txt")
        obj = self.seed / ".git/objects" / blob[:2] / blob[2:]
        forged = b"forged\n"
        obj.chmod(0o644)
        obj.write_bytes(zlib.compress(b"blob %d\0" % len(forged) + forged))
        self.run_until(lambda: self.status("x") == "failed")
        self.assertIn("differs from the remote", self.st()["x"]["reason"])
        self.assertIn("checkout_mismatch", [e["event"] for e in self.events()])

    def test_missing_review_slot_waits_without_losing_work(self):
        hidden = self.t / "review-moved"
        (self.slots / R).rename(hidden)
        self.plan(self.task("x", commit("x")))
        self.run_until(lambda: self.status("x") == "awaiting_review")
        for _ in range(3):
            self.coord("run", "--once")
        self.assertEqual(self.status("x"), "awaiting_review")
        hidden.rename(self.slots / R)
        self.run_until(lambda: self.status("x") == "awaiting_ci")

    def test_validation_has_only_localhost_network(self):
        import http.server
        import threading
        srv = http.server.HTTPServer(("0.0.0.0", 0), http.server.SimpleHTTPRequestHandler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)
        port = srv.server_address[1]
        # Another host the machine itself can reach, so a refusal is the
        # sandbox's. Not this host's own LAN address: seatbelt counts every
        # local interface address as localhost.
        gw = subprocess.run(["route", "-n", "get", "default"], capture_output=True, text=True).stdout
        gw = next((l.split()[-1] for l in gw.splitlines() if "gateway:" in l), "")
        remote = next((u for u in ([f"http://{gw}/"] if gw else []) + ["https://example.com/"]
                       if subprocess.run(["curl", "-s", "-m", "5", "-o", "/dev/null", u]).returncode == 0), None)
        if not remote:
            self.skipTest("no non-localhost target reachable from this host")
        probe = (f'(curl -s -m 5 -o /dev/null http://127.0.0.1:{port}/ && echo LOCAL-OK; '
                 f'curl -s -m 5 -o /dev/null {remote} && echo REMOTE-REACHED) >>"$CG_TEST_LOG"; true')
        self.plan(self.task("x", commit("x"), validate=[probe]))
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        log = self.log.read_text()
        self.assertIn("LOCAL-OK", log)
        self.assertNotIn("REMOTE-REACHED", log)

    def starts_of(self, tid):
        return [e for e in self.events() if e["event"] == "launched" and e["task"] == tid and e["kind"] == "implement"]

    # ------------------------------------------------------------ codex adapter

    def install_codex(self):
        install_fake_codex(self.t, self.env)

    def test_codex_implements_and_reviews_inside_the_sandbox(self):
        self.install_codex()
        (self.t / "log/codex-plan.json").write_text(json.dumps(
            {"x": ["claim-done", "commit"], "review:x": ["write-then-approve"]}))
        self.plan({**self.task("x", None, max_attempts=2), "agent": "codex", "command": None,
                   "review": {"agent": "codex"}})
        self.run_until(lambda: self.status("x") in ("awaiting_ci", "failed"), 60)
        self.assertEqual(self.status("x"), "awaiting_ci", self.st()["x"].get("reason"))
        calls = [json.loads(l) for l in (self.t / "log/codex-calls.jsonl").read_text().splitlines()]
        self.assertEqual([c["role"] for c in calls], ["implement", "implement", "review"])
        for c in calls:
            self.assertEqual(c["argv"][:4], ["exec", "--json", "--ephemeral",
                                             "--dangerously-bypass-approvals-and-sandbox"])
            self.assertTrue(c["codex_home"].startswith(tempfile.gettempdir().rstrip("/")) or
                            "/cg-agent-" in c["codex_home"], c)
            self.assertNotEqual(c["codex_home"], str(self.t / "home/.codex"))
            self.assertTrue(c["auth_copied"])
            self.assertFalse(c["keychain_readable"])
            self.assertFalse(c["state_readable"])
        self.assertIn("previous attempt ended", calls[1]["prompt"])
        self.assertIn("no new commit", calls[1]["prompt"])
        self.assertFalse(calls[2]["slot_write_ok"])  # reviewer cannot write the review slot
        self.assertFalse(calls[2]["ref_write_ok"])   # nor create a branch
        self.assertEqual(self.git("-C", str(self.slots / R), "status", "--porcelain", "--untracked-files=all"), "")
        launched = [e for e in self.events() if e["event"] == "launched"]
        self.assertTrue(all(e["argv0"] == "/usr/bin/sandbox-exec" for e in launched))
        self.assert_no_token_leak()

    def test_codex_login_copy_is_never_written_back(self):
        self.install_codex()
        auth = self.t / "home/.codex/auth.json"
        before = auth.read_bytes()
        (self.t / "log/codex-plan.json").write_text(json.dumps({"x": ["refresh-commit"]}))
        self.plan({**self.task("x", None), "agent": "codex", "command": None})
        self.run_until(lambda: self.status("x") == "awaiting_ci", 60)
        self.assertEqual(auth.read_bytes(), before)
        self.assertIn("not copied back", self.all_out)

    def test_validation_and_agents_cannot_read_owner_secrets(self):
        """Agent-written code runs in validation too: it must see no keychain,
        no other coordinator's state under the evidence root, no ssh keys."""
        probes = {"keychain": self.t / "home/Library/Keychains/probe", "ssh": self.t / "home/.ssh/id_probe",
                  "evidence": self.t / "home/Projekti/codegraph-evidence/coordinator/tokens/x.token",
                  "gh": self.t / "home/.config/gh/hosts.yml", "codex": self.t / "home/.codex/auth.json",
                  "aws": self.t / "home/.aws/credentials", "history": self.t / "home/.zsh_history",
                  "other": self.t / "home/Documents/canary.txt", "gitconfig": self.t / "home/.gitconfig"}
        for f in probes.values():
            f.parent.mkdir(parents=True, exist_ok=True)
            f.write_text("secret")
        user_tmp = os.path.realpath(subprocess.run(["getconf", "DARWIN_USER_TEMP_DIR"], capture_output=True,
                                                   text=True).stdout.strip())
        tmp_canary = Path(tempfile.mkdtemp(dir="/private/tmp")) / "canary"
        tmp_canary.write_text("secret")
        self.addCleanup(shutil.rmtree, tmp_canary.parent, True)
        probes["private-tmp"] = tmp_canary
        reads = "; ".join(f'cat {f} >/dev/null 2>&1 && echo LEAK-{k}' for k, f in probes.items())
        reads += (f'; echo x >/private/tmp/cg-probe-$$ 2>/dev/null && echo LEAK-tmp-write'
                  f'; (: >{user_tmp}/cg-sandbox-probe-$$) 2>/dev/null && echo LEAK-usertmp-write'
                  f'; cat {user_tmp}/xcrun_db >/dev/null 2>&1 && echo LEAK-xcrun-read'
                  '; [ -n "$CG_ENV_CANARY" ] && echo LEAK-env'
                  '; cat work/x.txt >/dev/null 2>&1 && echo READ-OK')  # positive control: the slot
        self.env["CG_ENV_CANARY"] = "token"
        self.plan(self.task("x", sh(f'mkdir -p work && echo x >work/x.txt && ({reads}) >>"$CG_TEST_LOG"; '
                                    'git add work && git commit -qm x'),
                            validate=[f'({reads}) >>"$CG_TEST_LOG"; true']))
        self.run_until(lambda: self.status("x") == "awaiting_ci")
        log = self.log.read_text()
        self.assertNotIn("LEAK", log)
        self.assertEqual(log.count("READ-OK"), 2)


    # ------------------------------------------------------------ overlapping adopted PRs

    def test_overlapping_adopted_prs_rework_one_at_a_time(self):
        db = {"prs": []}
        for n, tid in ((1, "p1"), (2, "p2")):
            clone = self.t / f"c{tid}"
            self.git("clone", "-q", "-b", "v2.0", str(self.origin), str(clone))
            (clone / "work").mkdir()
            (clone / "work" / f"{tid}-init.txt").write_text(tid)
            self.git("-C", str(clone), "add", "work")
            self.git("-C", str(clone), "commit", "-qm", tid)
            self.git("-C", str(clone), "push", "-q", "origin", f"HEAD:task/{tid}")
            db["prs"].append({"number": n, "head": f"task/{tid}", "base": "v2.0", "state": "OPEN", "checks": []})
        self.db.write_text(json.dumps(db))
        tasks = []
        for n, tid in ((1, "p1"), (2, "p2")):
            m = self.t / f"log/reviewed-{tid}"
            review = sh(f'if [ -e {m} ]; then printf "Verdict: APPROVE\\nBlocking: 0\\n"; '
                        f'else touch {m}; printf "Verdict: REQUEST_CHANGES\\nBlocking: 1\\n"; fi')
            tasks.append(self.task(tid, commit(tid, 1), scope=["work/*"], review=review, pr=n,
                                   branch=f"task/{tid}"))
        self.plan(*tasks)
        self.run_until(lambda: all(t["status"] == "awaiting_ci" for t in self.st().values()), 90)
        words = [l.split()[0] for l in self.log.read_text().splitlines()]
        self.assertEqual(words, ["start", "end", "start", "end"])  # rework never overlapped
        self.assertTrue([e for e in self.events() if e["event"] == "waiting_on" and e.get("other")])
        for tid in ("p1", "p2"):
            self.assertIn(f"{tid}-init.txt", self.git("--git-dir", str(self.origin), "ls-tree", "-r",
                                                      "--name-only", f"task/{tid}"))


class Units(unittest.TestCase):
    def setUp(self):
        sys.dont_write_bytecode = True
        sys.path.insert(0, str(HERE))
        import coordinator
        self.c = coordinator

    def test_agent_text(self):
        claude = 'warning\n{"type":"result","result":"ok\\nVerdict: APPROVE\\nBlocking: 0"}\n'
        self.assertEqual(self.c.agent_text("claude", claude), "ok\nVerdict: APPROVE\nBlocking: 0")
        codex = ('{"type":"item.completed","item":{"type":"agent_message","text":"first"}}\n'
                 '{"type":"item.completed","item":{"type":"agent_message","text":"Verdict: APPROVE"}}\n')
        self.assertEqual(self.c.agent_text("codex", codex), "Verdict: APPROVE")
        self.assertEqual(self.c.agent_text("claude", "Verdict: APPROVE\n"), "")  # unstructured: no verdict

    def test_scope_overlap(self):
        o = self.c.scopes_overlap
        self.assertTrue(o(["internal/store/*"], ["internal/store/x.go"]))
        self.assertTrue(o(["internal/*"], ["internal/store/*"]))
        self.assertFalse(o(["internal/store/*"], ["docs/*"]))
        self.assertTrue(o([], ["docs/*"]))

    def test_adapters_never_bypass_permissions(self):
        for review in (False, True):
            argv = self.c.ADAPTERS["claude"]("p", "s", ["Read"], review)
            self.assertFalse([a for a in argv if "dangerously" in a or a == "bypassPermissions"], argv)

    def test_codex_never_runs_outside_the_coordinator_sandbox(self):
        argv = self.c.ADAPTERS["codex"]("p", "s", ["Read"], False)
        self.assertIn("--dangerously-bypass-approvals-and-sandbox", argv)
        real = self.c.sandbox_available
        self.c.sandbox_available = lambda: False
        try:
            with self.assertRaises(self.c.Refused):
                self.c.isolate("codex", argv)
        finally:
            self.c.sandbox_available = real


if __name__ == "__main__":
    unittest.main(verbosity=2)

#!/usr/bin/env python3
"""Tests for coordinator.py against a throwaway pool, fake remote, fake gh,
fake merge gate and fake agents. Never touches the live pool.

    python3 scripts/agent-coordinator/test_coordinator.py
"""

import fcntl
import json
import os
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
COORD = HERE / "coordinator.py"
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
        print(json.dumps({"number": p["number"], "state": p["state"], "headRefOid": head(p["head"]),
                          "headRefName": p["head"], "baseRefName": p["base"], "statusCheckRollup": p["checks"]}))
    else:
        sys.exit("fake gh: unsupported " + " ".join(a))
''')

# Merge gate stand-in: checks the review file names the PR and head; can be told to refuse.
FAKE_GATE = textwrap.dedent('''\
    #!/bin/sh
    [ -e "$FAKE_GATE_REFUSE" ] && { echo "merge-gate: BLOCKED: test"; exit 1; }
    grep -qx "PR: #$1" "$3" && grep -qx "Reviewed-Head: $2" "$3" && grep -qx "Verdict: APPROVE" "$3" &&
        grep -qx "Blocking: 0" "$3" || { echo "merge-gate: BLOCKED: review"; exit 1; }
    echo "merge-gate: PASS: PR #$1 head $2"
''')

GREEN = [{"name": "ci", "conclusion": "SUCCESS"}, {"name": "race", "conclusion": "SUCCESS"}]


def sh(script):
    return ["/bin/sh", "-c", script]


def commit(name, sleep=0):
    """Fake implementer: optionally sleeps, logs its start/end, commits a file."""
    return sh(f'echo "start $(date +%s.%N 2>/dev/null || date +%s) {name}" >>"$CG_TEST_LOG"; sleep {sleep}; '
              f'mkdir -p work && echo {name}-$RANDOM >>work/{name}.txt && git add work && '
              f'git commit -qm {name} && echo "end {name}" >>"$CG_TEST_LOG"')


APPROVE = sh('git diff --stat "origin/v2.0...HEAD" >/dev/null && printf "looks fine\\nVerdict: APPROVE\\nBlocking: 0\\n"')


class Pool(unittest.TestCase):
    def setUp(self):
        self.t = Path(tempfile.mkdtemp()).resolve()
        self.addCleanup(shutil.rmtree, self.t, True)
        t = self.t
        self.origin = t / "origin.git"
        self.seed = t / "seed"
        self.slots = t / "slots"
        self.leases = t / "leases"
        self.state = t / "state"
        self.cwds = t / "cwds"
        self.cwds.write_text("")
        self.db = t / "gh.json"
        self.db.write_text(json.dumps({"prs": []}))
        self.log = t / "agents.log"
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
                    "GIT_COMMITTER_EMAIL": "t@t", "GIT_CONFIG_GLOBAL": "/dev/null"}
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

    def coord(self, *args, ok=None):
        p = subprocess.run([sys.executable, str(COORD), *args], env=self.env, capture_output=True, text=True)
        self.out = p.stdout + p.stderr
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
        self.set_pr(self.st()["a"]["pr"], state="MERGED")
        self.run_until(lambda: self.status("b") not in ("pending",))
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
        marker = self.t / "reviewed-once"
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
        for name in ("claude", "codex"):
            for review in (False, True):
                argv = self.c.ADAPTERS[name]("p", "s", ["Read"], review)
                self.assertFalse([a for a in argv if "dangerously" in a or a == "bypassPermissions"], argv)


if __name__ == "__main__":
    unittest.main(verbosity=2)

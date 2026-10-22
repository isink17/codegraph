#!/usr/bin/env python3
"""Tests for campaign.py against the coordinator test fixture: throwaway
remote, slots and leases, fake gh, fake codex, fake Plane, fake ci-local,
fake merge-exception and fake plane-sync. Never touches the live pool,
Plane or GitHub.

    python3 scripts/agent-coordinator/test_campaign.py
"""

import fcntl
import json
import os
import signal
import subprocess
import sys
import textwrap
import time
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.dont_write_bytecode = True
from test_coordinator import A, B, R, Fixture, install_fake_codex, POOL  # noqa: E402

CAMPAIGN = HERE / "campaign.py"

FAKE_PLANE = textwrap.dedent('''\
    #!/usr/bin/env python3
    import json, os, sys
    cfg = json.load(open(os.environ["FAKE_PLANE"]))
    if cfg.get("down"):
        sys.exit("connection refused")
    path = sys.argv[1]
    if path.endswith("/states/"):
        print(json.dumps({"results": [{"id": g, "group": g} for g in
                                      ("backlog", "unstarted", "started", "completed", "cancelled")]}))
    else:
        tk = path.split("/")[1]
        print(json.dumps({"id": tk, "state": cfg["tickets"][tk], "name": f"ticket {tk}",
                          "description_stripped": "do the thing"}))
''')

# Stand-in for the owner's local CI reproduction: checks the slot is at the
# exact head and writes the evidence the real script writes.
FAKE_CI = textwrap.dedent('''\
    #!/bin/bash
    slot=$1 head=$2 base=$3 out=$4
    echo "ci $head $base $*" >>"$CG_TEST_LOG.ci"
    [ "$(git -C "$slot" rev-parse HEAD)" = "$head" ] || { echo "slot not at head"; exit 2; }
    mkdir -p "$out"
    rc=0; [ -e "$CTL/ci-fail" ] && rc=1
    [ -e "$CTL/ci-symlink" ] && ln -s "$CTL/secret" "$out/leak"
    if [ -e "$CTL/ci-dirlink" ]; then mv "$out" "$out.real"; ln -s "$CTL/secretdir" "$out"; fi
    echo "{\\"step\\":\\"native-test\\",\\"rc\\":$rc}" >"$out/steps.jsonl"
    if [ $rc = 0 ]; then failed='[]'; ok=true; else failed='["native-test"]'; ok=false; fi
    echo "{\\"head\\":\\"$head\\",\\"base\\":\\"$base\\",\\"all_passed\\":$ok,\\"failed\\":$failed}" >"$out/summary.json"
    echo "native-test rc=$rc"
    exit $rc
''')

# Stand-in for merge-exception.sh: same arguments and review-header check;
# --merge fast-forwards v2.0 to the head and marks the PR merged.
FAKE_MERGE = textwrap.dedent('''\
    #!/bin/bash
    pr=$1 sha=$2 base=$3 review=$4 cidir=$5 mode=${6:-verify}
    echo "$mode $pr $sha $base" >>"$CTL/merge-calls"
    fail() { echo "merge-exception: BLOCKED: $*"; exit 1; }
    want=$(printf 'PR: #%s\\nReviewed-Head: %s\\nVerdict: APPROVE\\nBlocking: 0' "$pr" "$sha")
    [ "$(head -n 4 "$review")" = "$want" ] || fail "review header"
    grep -q '"all_passed":true' "$cidir/summary.json" || fail "local ci"
    [ "$(git --git-dir "$FAKE_ORIGIN" rev-parse refs/heads/v2.0)" = "$base" ] || fail "base moved"
    [ -e "$CTL/merge-refuse" ] && fail "refused by test"
    echo "merge-exception: PASS (verify): #$pr $sha on $base"
    [ "$mode" = --merge ] || exit 0
    if [ -e "$CTL/merge-slow" ]; then touch "$CTL/merging"; sleep 4; fi
    if [ -e "$CTL/restore-fail" ]; then
        echo "merge-exception: RULESET_RESTORE_FAILED: ruleset 24420032 may be weakened; stop all merges"; exit 3
    fi
    git --git-dir "$FAKE_ORIGIN" update-ref refs/heads/v2.0 "$sha"
    python3 - "$pr" <<'EOF'
    import json, os, sys
    db = json.load(open(os.environ["FAKE_GH_DB"]))
    next(p for p in db["prs"] if p["number"] == int(sys.argv[1]))["state"] = "MERGED"
    json.dump(db, open(os.environ["FAKE_GH_DB"], "w"))
    EOF
    echo "merge-exception: MERGED #$pr as $sha; waiver record x"
''')

FAKE_SYNC = textwrap.dedent('''\
    #!/bin/bash
    echo "$1 $2 $3" >>"$CTL/sync-calls"
    [ -e "$CTL/sync-fail" ] && { echo "plane-sync: FAILED: unreachable"; exit 1; }
    grep -q "Reviewed head" "$3" || exit 1
    echo "plane-sync: SYNCED $1 #$2"
''')


class CampaignFixture(Fixture):
    def setUp(self):
        super().setUp()
        t = self.t
        install_fake_codex(t, self.env)
        self.camp = t / "camp"
        self.camp.mkdir()
        self.ctl = t / "ctl"
        self.ctl.mkdir()
        (t / "exc").mkdir()
        for path, body in ((t / "bin/plane", FAKE_PLANE), (t / "exc/ci-local.sh", FAKE_CI),
                           (t / "exc/merge-exception.sh", FAKE_MERGE), (t / "bin/plane-sync", FAKE_SYNC)):
            path.write_text(body)
            path.chmod(0o755)
        self.plane = t / "plane.json"
        self.set_plane(tickets={"CG-1": "started"})
        self.env.update(CG_CAMPAIGN_PLANE_CMD=str(t / "bin/plane"), FAKE_PLANE=str(self.plane),
                        CG_CAMPAIGN_PLANE_SYNC=str(t / "bin/plane-sync"), CTL=str(self.ctl))
        self.state = self.camp / "coordinator"

    def set_plane(self, **cfg):
        self.plane.write_text(json.dumps(cfg))

    def authorize(self, tickets, mode="exception", **extra):
        self.auth = {"campaign": "test", "merge": {"mode": mode, "exception_dir": str(self.t / "exc")},
                     "tickets": tickets, **extra}
        (self.camp / "authorization.json").write_text(json.dumps(self.auth))

    def ctask(self, tid, **kw):
        return {"id": tid, "description": f"task {tid}", "agent": "codex", "scope": [f"work/{tid}*"],
                "acceptance": ["works"], "validate": [f"test -s work/{tid}.txt"], "review": {"agent": "codex"},
                "max_attempts": 3, **kw}

    def codex_plan(self, plan):
        (self.t / "log/codex-plan.json").write_text(json.dumps(plan))

    def campaign(self, *args, minutes="3", ok=None, timeout=240):
        argv = [sys.executable, str(CAMPAIGN), args[0], "--dir", str(self.camp), *args[1:]]
        if args[0] == "run":
            argv += ["--minutes", minutes, "--poll", "0.2"]
        p = subprocess.run(argv, env=self.env, capture_output=True, text=True, timeout=timeout)
        self.out = p.stdout + p.stderr
        self.rc = p.returncode
        if ok is not None:
            self.assertEqual(p.returncode == 0, ok, self.out)
        return p

    def cstate(self):
        return json.loads((self.camp / "campaign.json").read_text())

    def cevents(self):
        f = self.camp / "events.jsonl"
        return [json.loads(l) for l in f.read_text().splitlines()] if f.exists() else []

    def events(self):
        return [json.loads(l) for l in (self.state / "events.jsonl").read_text().splitlines()]

    def calls(self, name):
        f = self.ctl / name
        return f.read_text().splitlines() if f.exists() else []

    def codex_calls(self):
        f = self.t / "log/codex-calls.jsonl"
        return [json.loads(l) for l in f.read_text().splitlines()] if f.exists() else []

    def origin_tip(self):
        return self.git("--git-dir", str(self.origin), "rev-parse", "refs/heads/v2.0")


class Campaign(CampaignFixture):
    # ------------------------------------------------------------ continuation across executions

    def test_three_sequential_tasks_complete_in_one_unattended_run(self):
        self.authorize({"CG-1": {"tasks": [self.ctask("a"), self.ctask("b", depends_on=["a"]),
                                           self.ctask("c", depends_on=["b"])]}})
        self.campaign("run", ok=True)
        self.assertIn("IDLE", self.out)
        self.assertEqual({k: v["status"] for k, v in self.st().items()},
                         {"a": "completed", "b": "completed", "c": "completed"})
        calls = self.codex_calls()
        self.assertEqual([c["key"] for c in calls], ["a", "review:a", "b", "review:b", "c", "review:c"])
        merges = [l for l in self.calls("merge-calls") if l.startswith("--merge")]
        self.assertEqual(len(merges), 3)
        self.assertEqual(len(self.calls("sync-calls")), 3)
        self.assertTrue(all(l.startswith("CG-1 ") for l in self.calls("sync-calls")))
        self.assertEqual(self.origin_tip(), self.st()["c"]["approved_head"])
        # b and c were built on the merged base, not on the original one.
        self.assertIn("a.txt", self.git("--git-dir", str(self.origin), "ls-tree", "-r", "--name-only",
                                         self.st()["c"]["approved_head"]))
        self.assertEqual(len(self.calls("../log/agents.log.ci") or (self.t / "log/agents.log.ci").read_text()
                             .splitlines()), 3)
        for s in (A, B, R):
            self.assertEqual(self.pool_state(s), "free")
        self.assert_no_token_leak()
        self.assertIn("[a:implement@", self.out)  # live agent output reached the terminal
        self.assertIn("$ git commit -qm a", self.out)

    def test_early_exit_and_unproven_completion_claim_are_continued(self):
        self.codex_plan({"a": ["claim-done", "commit"], "b": ["commit", "commit"]})
        self.authorize({"CG-1": {"tasks": [self.ctask("a"),
                                           self.ctask("b", validate=["grep -q 'b 1' work/b.txt"])]}})
        self.campaign("run", ok=True)
        self.assertEqual([t["status"] for t in self.st().values()], ["completed", "completed"])
        self.assertIn("branch_update", [e["event"] for e in self.cevents()])  # parallel PR went stale
        self.assertEqual(len([e for e in self.events() if e["event"] == "reviewed"]), 3)  # updated head re-reviewed
        cont = [e for e in self.events() if e["event"] == "continue"]
        self.assertEqual(sorted((e["task"], e["reason"].split(";")[0]) for e in cont),
                         [("a", "agent produced no new commit"), ("b", "validation failed: grep -q 'b 1' work/b.txt")])
        impl = [c for c in self.codex_calls() if c["role"] == "implement"]
        self.assertEqual(sorted(c["key"] for c in impl), ["a", "a", "b", "b"])

    def test_blocked_task_does_not_stop_independent_work(self):
        self.authorize({"CG-1": {"tasks": [self.ctask("x", validate=["false"], max_attempts=2),
                                           self.ctask("y"), self.ctask("z", depends_on=["x"])]}})
        self.campaign("run", ok=True)
        st = self.st()
        self.assertEqual((st["x"]["status"], st["y"]["status"], st["z"]["status"]), ("failed", "completed", "pending"))
        self.assertIsNotNone(self.lease(st["x"]["slot"]))  # failed work keeps its slot
        self.assertIn("IDLE", self.out)

    # ------------------------------------------------------------ pool state

    def test_worker_a_leased_and_worker_b_behind_base(self):
        p = subprocess.run([str(POOL), "acquire", "--slot", A, "--task", "other", "--agent", "owner"],
                           env=self.env, capture_output=True, text=True, check=True)
        before = self.lease(A)
        clone = self.t / "adv"
        self.git("clone", "-q", "-b", "v2.0", str(self.origin), str(clone))
        self.git("-C", str(clone), "commit", "-q", "--allow-empty", "-m", "advance")
        self.git("-C", str(clone), "push", "-q", "origin", "HEAD:v2.0")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=True)
        self.assertEqual(self.status("a"), "completed")
        self.assertEqual(self.lease(A), before)  # never touched
        self.assertNotIn("token=", self.out)
        self.assertIn(p.stdout.split("token=")[1].strip(), self.lease(A))

    def test_inconsistent_slot_and_leased_slot_are_left_alone(self):
        subprocess.run([str(POOL), "acquire", "--slot", A, "--task", "other", "--agent", "owner"],
                       env=self.env, capture_output=True, text=True, check=True)
        (self.slots / B / "owner-wip.txt").write_text("keep me")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", minutes="0.1", ok=True)
        self.assertIn("DEADLINE", self.out)
        self.assertIn("BLOCKED_POOL_BUSY", self.out)
        self.assertEqual(self.status("a"), "pending")
        self.assertEqual((self.slots / B / "owner-wip.txt").read_text(), "keep me")
        self.assertIsNone(self.lease(B))

    # ------------------------------------------------------------ review

    def test_reviewer_rejection_sends_work_back(self):
        self.codex_plan({"review:a": ["reject", "approve"]})
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=True)
        self.assertEqual(self.status("a"), "completed")
        calls = self.codex_calls()
        self.assertEqual([c["key"] for c in calls], ["a", "review:a", "a", "review:a"])
        self.assertIn("review requested changes", calls[2]["prompt"])
        reviews = [c for c in calls if c["role"] == "review"]
        self.assertFalse(any(c["slot_write_ok"] or c["ref_write_ok"] or c["state_readable"] for c in reviews))
        merged = [l for l in self.calls("merge-calls") if l.startswith("--merge")]
        self.assertEqual(merged, [f"--merge 1 {self.st()['a']['approved_head']} " + merged[0].split()[-1]])

    # ------------------------------------------------------------ Plane

    def test_plane_unavailable_plans_and_merges_nothing_then_recovers(self):
        self.set_plane(down=True)
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", minutes="0.05", ok=True)
        self.assertIn("unavailable", self.out)
        self.assertIn("DEADLINE", self.out)
        self.assertEqual(self.st() if (self.state / "state.json").exists() else {}, {})
        self.assertIn("plane_unavailable", [e["event"] for e in self.cevents()])
        self.set_plane(tickets={"CG-1": "started"})
        self.campaign("run", ok=True)
        self.assertEqual(self.status("a"), "completed")
        self.assertIn("plane_available", [e["event"] for e in self.cevents()])

    def test_no_unauthorized_work_or_merge(self):
        self.set_plane(tickets={"CG-1": "started", "CG-2": "completed"})
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}, "CG-2": {"tasks": [self.ctask("b")]}}, mode="none")
        self.campaign("run", ok=True)
        self.assertEqual(list(self.st()), ["a"])  # CG-2 is done in Plane: never planned
        self.assertEqual(self.status("a"), "awaiting_ci")
        self.assertEqual(self.calls("merge-calls"), [])
        self.assertEqual(self.calls("sync-calls"), [])
        self.assertIn("IDLE", self.out)

    def test_ticket_closed_in_plane_cancels_its_pending_task(self):
        self.set_plane(tickets={"CG-1": "started"})
        self.authorize({"CG-1": {"tasks": [self.ctask("a"), self.ctask("b", depends_on=["a"])]}}, mode="none")
        self.campaign("run", ok=True)
        self.assertEqual(self.status("b"), "pending")
        self.set_plane(tickets={"CG-1": "cancelled"})
        self.campaign("run", ok=True)
        self.assertEqual(self.status("b"), "interrupted")
        self.assertIn("cancelled_closed_ticket", [e["event"] for e in self.cevents()])

    def test_post_merge_sync_failure_blocks_merging_until_it_succeeds(self):
        (self.ctl / "sync-fail").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a"), self.ctask("b")]}})
        self.campaign("run", minutes="1", ok=True)
        merges = [l for l in self.calls("merge-calls") if l.startswith("--merge")]
        self.assertEqual(len(merges), 1)
        self.assertEqual(len(self.cstate()["unsynced"]), 1)
        (self.ctl / "sync-fail").unlink()
        self.campaign("run", ok=True)
        self.assertEqual(self.cstate()["unsynced"], [])
        self.assertEqual(len([l for l in self.calls("merge-calls") if l.startswith("--merge")]), 2)
        self.assertEqual(len(self.calls("sync-calls")), 3)  # failed, its retry, the second merge
        synced = [e for e in self.cevents() if e["event"] == "plane_sync"]
        self.assertEqual([e["exit"] for e in synced][-2:], [0, 0])

    # ------------------------------------------------------------ merge safety

    def test_ruleset_restore_failure_halts_every_later_merge(self):
        (self.ctl / "restore-fail").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a"), self.ctask("b")]}})
        self.campaign("run", ok=False)
        self.assertIn("HALT", self.out)
        self.assertEqual(len([l for l in self.calls("merge-calls") if l.startswith("--merge")]), 1)
        self.assertIn("ruleset restoration failed", self.cstate()["halt"])
        self.assertEqual(self.calls("sync-calls"), [])
        (self.ctl / "restore-fail").unlink()
        self.campaign("run", ok=False)  # the halt persists until the owner clears it
        self.assertEqual(len([l for l in self.calls("merge-calls") if l.startswith("--merge")]), 1)

    def test_failed_local_ci_never_reaches_the_merge_script(self):
        (self.ctl / "ci-fail").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=True)
        self.assertEqual(self.status("a"), "awaiting_ci")
        self.assertEqual(self.calls("merge-calls"), [])
        self.assertEqual(self.cstate()["merges"]["a"]["attempts"], 2)
        self.assertIsNone(self.cstate()["ci_lease"])
        for s in (A, B):
            self.assertEqual(self.pool_state(s), "free")

    def start_campaign(self):
        return subprocess.Popen([sys.executable, str(CAMPAIGN), "run", "--dir", str(self.camp), "--minutes", "3",
                                 "--poll", "0.2"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True)

    def wait_for(self, path, limit=60):
        for _ in range(limit * 10):
            if path.exists():
                return
            time.sleep(0.1)
        self.fail(f"{path} never appeared")

    def test_supervisor_killed_during_merge_halts_merging_on_restart(self):
        (self.ctl / "merge-slow").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a"), self.ctask("b")]}})
        p = self.start_campaign()
        self.wait_for(self.ctl / "merging")
        p.kill()
        p.communicate(timeout=30)
        self.campaign("run", ok=False)
        self.assertIn("stopped while merging", self.cstate()["halt"])
        self.assertEqual(len([l for l in self.calls("merge-calls") if l.startswith("--merge")]), 1)

    def test_interrupt_during_merge_waits_for_the_merge_to_finish(self):
        (self.ctl / "merge-slow").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        p = self.start_campaign()
        self.wait_for(self.ctl / "merging")
        p.send_signal(signal.SIGINT)
        out, _ = p.communicate(timeout=120)
        self.assertIn("MERGED PR #1", out)
        self.assertIsNone(self.cstate()["halt"])
        self.assertEqual(self.cstate()["merges"]["a"]["status"], "merged")
        self.assertNotIn("merging", self.cstate()["merges"]["a"])
        self.assertEqual(len(self.calls("sync-calls")), 1)

    def test_symlinks_in_local_ci_evidence_are_rejected(self):
        (self.ctl / "secret").write_text("lease token")
        (self.ctl / "ci-symlink").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=True)
        self.assertEqual(self.calls("merge-calls"), [])
        self.assertIn("symbolic links", self.cstate()["merges"]["a"]["reason"])
        self.assertFalse([f for f in self.camp.rglob("leak")])

    def test_symlinked_local_ci_output_directory_is_rejected(self):
        (self.ctl / "secretdir").mkdir()
        (self.ctl / "secretdir/summary.json").write_text("{}")
        (self.ctl / "secretdir/lease.token").write_text("token")
        (self.ctl / "ci-dirlink").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=True)
        self.assertEqual(self.calls("merge-calls"), [])
        self.assertIn("symbolic links", self.cstate()["merges"]["a"]["reason"])
        self.assertFalse([f for f in self.camp.rglob("lease.token")])

    def test_campaign_files_writable_by_agents_are_refused(self):
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=True, minutes="0")
        self.env["CG_COORD_SANDBOX_WRITABLE"] += f":{self.t / 'exc'}"
        self.campaign("run", ok=False, minutes="0")
        self.assertIn("writable to agents", self.out)

    # ------------------------------------------------------------ control

    def test_restart_after_interruption_does_not_duplicate_work(self):
        self.codex_plan({"a": ["sleep-commit"]})
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        p = subprocess.Popen([sys.executable, str(CAMPAIGN), "run", "--dir", str(self.camp), "--minutes", "3",
                              "--poll", "0.2"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                             text=True)
        for _ in range(100):
            if (self.state / "state.json").exists() and self.status("a") == "running":
                break
            time.sleep(0.1)
        self.assertEqual(self.status("a"), "running")
        p.send_signal(signal.SIGINT)
        out, _ = p.communicate(timeout=30)
        self.assertEqual(p.returncode, 130, out)
        self.assertEqual(self.status("a"), "running")  # the agent outlives the supervisor
        self.campaign("run", ok=True)
        self.assertEqual(self.status("a"), "completed")
        self.assertEqual([c["key"] for c in self.codex_calls()], ["a", "review:a"])

    def test_duplicate_tasks_and_second_supervisor_refused(self):
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}, "CG-2": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=False)
        self.assertIn("listed under CG-1 and CG-2", self.out)
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}}, mode="none")
        fd = os.open(self.camp / "campaign.lock", os.O_RDWR | os.O_CREAT)
        fcntl.flock(fd, fcntl.LOCK_EX)
        try:
            self.campaign("run", ok=False)
            self.assertIn("another campaign supervisor", self.out)
        finally:
            os.close(fd)
        self.campaign("run", ok=True)
        self.campaign("run", ok=True)  # replanning the same authorization adds nothing
        self.assertEqual(len([e for e in self.cevents() if e["event"] == "planned"]), 1)
        self.assertEqual(len([c for c in self.codex_calls() if c["role"] == "implement"]), 1)

    def test_deadline_and_stop(self):
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", minutes="0", ok=True)
        self.assertIn("DEADLINE", self.out)
        self.assertFalse((self.state / "state.json").exists())  # nothing planned or launched
        self.campaign("stop", ok=True)
        self.campaign("run", ok=True)
        self.assertIn("stopped: STOP", self.out)
        self.assertEqual(self.codex_calls(), [])
        (self.camp / "STOP").unlink()
        self.campaign("run", ok=True)
        self.assertEqual(self.status("a"), "completed")

    def test_interrupted_state_writes(self):
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}}, mode="none")
        self.campaign("run", ok=True)
        (self.camp / ".campaign.json.deadbeef.tmp").write_text('{"tasks": {"trunc')  # killed mid-write
        self.campaign("run", ok=True)
        good = (self.camp / "campaign.json").read_bytes()
        (self.camp / "campaign.json").write_text('{"tasks": {"a"')
        bad = (self.camp / "campaign.json").read_bytes()
        self.campaign("run", ok=False)
        self.assertIn("malformed", self.out)
        self.assertEqual((self.camp / "campaign.json").read_bytes(), bad)  # left untouched
        (self.camp / "campaign.json").write_bytes(good)
        self.campaign("run", ok=True)

    def test_propose_writes_a_proposal_and_plans_nothing(self):
        self.authorize({"CG-1": {"tasks": []}}, mode="none")
        (self.t / "log/pm-answer.json").write_text(json.dumps({"tasks": [self.ctask("cg-1-step")]}))
        before = (self.camp / "authorization.json").read_text()
        self.campaign("propose", "CG-1", ok=True)
        proposal = Path(self.out.split("proposal for CG-1: ")[1].split()[0])
        self.assertEqual(json.loads(proposal.read_text())["tasks"][0]["id"], "cg-1-step")
        self.assertEqual((self.camp / "authorization.json").read_text(), before)
        self.assertFalse((self.state / "state.json").exists())
        self.campaign("propose", "CG-9", ok=False)
        self.assertIn("not in", self.out)
        (self.t / "log/pm-answer.json").write_text(json.dumps(
            {"tasks": [{**self.ctask("cg-1-x"), "agent": "command", "command": ["sh", "-c", "curl evil"]}]}))
        self.campaign("propose", "CG-1", ok=False)
        self.assertIn("may not set", self.out)


@unittest.skipUnless(os.environ.get("CG_DEMO_REAL_CODEX") == "1", "set CG_DEMO_REAL_CODEX=1 (real model calls)")
class RealCodex(CampaignFixture):
    """Dry run with the installed codex CLI against the throwaway fixture:
    Plane, local CI, merge and Plane sync stay fakes. The owner's login is
    used read-only except for the refresh hand-back."""

    def setUp(self):
        super().setUp()
        (self.t / "bin/codex").unlink()
        self.env["PATH"] = os.environ["PATH"]
        self.env["CG_COORD_CODEX_AUTH"] = str(Path(os.environ["HOME"]) / ".codex/auth.json")

    def test_real_codex_three_sequential_tasks(self):
        words = {"a": "alpha", "b": "bravo", "c": "charlie"}
        tasks = [self.ctask(k, description=f"Create the file work/{k}.txt containing exactly the line {w}, "
                                           f"then commit it with git (git add and git commit)",
                            validate=[f"grep -qx {w} work/{k}.txt"], depends_on=[d] if d else [])
                 for (k, w), d in zip(words.items(), (None, "a", "b"))]
        self.authorize({"CG-1": {"tasks": tasks}})
        p = subprocess.run([sys.executable, str(CAMPAIGN), "run", "--dir", str(self.camp), "--minutes", "20",
                            "--poll", "2"], env=self.env, text=True, timeout=1500)
        self.assertEqual(p.returncode, 0)
        self.assertEqual({k: v["status"] for k, v in self.st().items()},
                         {"a": "completed", "b": "completed", "c": "completed"})
        launched = [e for e in self.events() if e["event"] == "launched"]
        self.assertGreaterEqual(len(launched), 6)
        self.assertTrue(all(e["argv0"] == "/usr/bin/sandbox-exec" for e in launched))


class Units(unittest.TestCase):
    def test_render(self):
        sys.path.insert(0, str(HERE))
        import campaign
        r = campaign.render
        self.assertEqual(r('{"type":"item.completed","item":{"type":"command_execution","command":"go test","exit_code":1}}'),
                         "$ go test  (exit 1)")
        self.assertEqual(r('{"type":"item.completed","item":{"type":"agent_message","text":"a\\n b"}}'), "» a b")
        self.assertIsNone(r('{"type":"thread.started"}'))
        self.assertEqual(r("plain"), "plain")


if __name__ == "__main__":
    unittest.main(verbosity=2)

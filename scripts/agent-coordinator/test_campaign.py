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
    if [ -e "$CTL/ci-write" ]; then (echo x >"$slot/planted.go") 2>/dev/null && echo SLOT-WRITTEN >>"$CG_TEST_LOG.ci" ||
        echo SLOT-WRITE-DENIED >>"$CG_TEST_LOG.ci"; fi
    [ "$(git -C "$slot" rev-parse HEAD)" = "$head" ] || { echo "slot not at head"; exit 2; }
    mkdir -p "$out"
    rc=0; [ -e "$CTL/ci-fail" ] && rc=1
    lie=0; [ -e "$CTL/ci-lie" ] && lie=1
    [ -e "$CTL/ci-symlink" ] && ln -s "$CTL/secret" "$out/leak"
    if [ -e "$CTL/ci-dirlink" ]; then mv "$out" "$out.real"; ln -s "$CTL/secretdir" "$out"; fi
    echo "{\\"step\\":\\"native-test\\",\\"rc\\":$rc}" >"$out/steps.jsonl"
    if [ $rc = 0 ] || [ $lie = 1 ]; then failed='[]'; ok=true; else failed='["native-test"]'; ok=false; fi
    [ $lie = 1 ] && rc=1
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
    if [ -e "$CTL/revoke-on-verify" ]; then echo '{"tickets": {"CG-9": {"tasks": []}}, "merge": {"mode": "none"}}' >"$(cat "$CTL/revoke-on-verify")"; fi
    echo "merge-exception: PASS (verify): #$pr $sha on $base"
    [ "$mode" = --merge ] || exit 0
    if [ -e "$CTL/merge-slow" ]; then touch "$CTL/merging"; sleep 4; fi
    if [ -e "$CTL/restore-fail" ]; then
        echo "merge-exception: RULESET_RESTORE_FAILED: ruleset 24420032 may be weakened; stop all merges"; exit 3
    fi
    [ -e "$CTL/merge-crash-before" ] && { cp "$CTL/relaxed.json" "$FAKE_RULESET"; kill -9 $PPID; sleep 30; }
    git --git-dir "$FAKE_ORIGIN" update-ref refs/heads/v2.0 "$sha"
    python3 - "$pr" "$sha" <<'EOF'
    import json, os, sys
    db = json.load(open(os.environ["FAKE_GH_DB"]))
    p = next(p for p in db["prs"] if p["number"] == int(sys.argv[1]))
    p.update(state="MERGED", squash=sys.argv[2], merged_head=sys.argv[2])
    json.dump(db, open(os.environ["FAKE_GH_DB"], "w"))
    EOF
    [ -e "$CTL/merge-crash-after" ] && { kill -9 $PPID; sleep 30; }
    [ -e "$CTL/merge-no-ack" ] && exit 0
    echo "merge-exception: MERGED #$pr as $sha; waiver record x"
''')

FAKE_SYNC = textwrap.dedent('''\
    #!/bin/bash
    echo "$1 $2 $3" >>"$CTL/sync-calls"
    [ -e "$CTL/sync-fail" ] && { echo "plane-sync: FAILED: unreachable"; exit 1; }
    grep -q "Reviewed head" "$3" || exit 1
    grep -qx "$1#$2" "$CTL/comments" 2>/dev/null || echo "$1#$2" >>"$CTL/comments"
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
        ruleset = {"name": "v2.0 merge gate", "target": "branch", "enforcement": "active", "conditions": {},
                   "rules": [{"type": "required_status_checks"}], "bypass_actors": []}
        (t / "ruleset-24420032-start.json").write_text(json.dumps(ruleset))
        (t / "ruleset-live.json").write_text(json.dumps(ruleset))
        (self.ctl / "relaxed.json").write_text(json.dumps({**ruleset, "rules": []}))
        self.env.update(FAKE_RULESET=str(t / "ruleset-live.json"), FAKE_GH_DOWN=str(self.ctl / "gh-down"))
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

    def wait_for_call(self, key, limit=60):
        for _ in range(limit * 10):
            if any(c["key"] == key for c in self.codex_calls()):
                return
            time.sleep(0.1)
        self.fail(f"codex was never called for {key}")

    def wait_for(self, path, limit=60):
        for _ in range(limit * 10):
            if path.exists():
                return
            time.sleep(0.1)
        self.fail(f"{path} never appeared")

    def merges(self):
        return [l for l in self.calls("merge-calls") if l.startswith("--merge")]

    def crash_in_merge(self, ctl, tasks=("a", "b")):
        """Runs until the merge script kills the supervisor (SIGKILL, as a
        crash or power loss would) at the boundary named by ctl."""
        (self.ctl / ctl).write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask(t) for t in tasks]}})
        p = self.start_campaign()
        out, _ = p.communicate(timeout=180)
        self.assertEqual(p.returncode, -9, out)
        (self.ctl / ctl).unlink()
        self.assertIn("merging", json.dumps(self.cstate()["merges"]))

    def test_crash_after_github_merge_is_reconciled_not_repeated(self):
        self.crash_in_merge("merge-crash-after")
        self.campaign("run", ok=True)
        self.assertIsNone(self.cstate()["halt"])
        self.assertEqual({k: v["status"] for k, v in self.st().items()}, {"a": "completed", "b": "completed"})
        self.assertEqual(len(self.merges()), 2)  # a merged once, never again; then b
        self.assertIn("merge_reconciled", [e["event"] for e in self.cevents()])
        self.assertEqual(len(self.calls("comments")), 2)  # the crash lost no Plane sync

    def test_crash_with_ruleset_relaxed_halts_until_owner_verifies(self):
        self.crash_in_merge("merge-crash-before")
        self.campaign("run", ok=False)
        self.assertIn("uncertain outcome", self.cstate()["halt"])
        self.assertIn("ruleset not verified", self.cstate()["halt"])
        self.campaign("run", ok=False)
        self.assertEqual(len(self.merges()), 1)
        self.assertEqual(self.calls("sync-calls"), [])

    def test_unreadable_github_after_crash_halts(self):
        self.crash_in_merge("merge-crash-after", tasks=("a",))
        (self.ctl / "gh-down").write_text("")
        self.campaign("run", ok=False)
        self.assertIn("state unreadable", self.cstate()["halt"])
        self.assertEqual(len(self.merges()), 1)

    def test_orphaned_merge_script_is_waited_for_not_repeated(self):
        (self.ctl / "merge-slow").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        p = self.start_campaign()
        self.wait_for(self.ctl / "merging")
        p.kill()  # the supervisor dies; the merge script, in its own session, carries on
        p.communicate(timeout=30)
        self.campaign("run", ok=True)
        self.assertIn("still running; waiting", self.out)
        self.assertEqual(len(self.merges()), 1)
        self.assertEqual(self.status("a"), "completed")
        self.assertIsNone(self.cstate()["halt"])

    def test_marker_is_reconciled_against_the_mode_it_was_written_in(self):
        self.crash_in_merge("merge-crash-before", tasks=("a",))
        auth = json.loads((self.camp / "authorization.json").read_text())
        auth["merge"] = {"mode": "none"}  # the owner switches modes; the ruleset is still relaxed
        (self.camp / "authorization.json").write_text(json.dumps(auth))
        self.campaign("run", ok=False)
        self.assertIn("ruleset not verified", self.cstate()["halt"])

    def test_restore_failure_keeps_the_marker_for_reconciliation(self):
        (self.ctl / "restore-fail").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=False)
        self.assertIn("merging", self.cstate()["merges"]["a"])
        (self.ctl / "restore-fail").unlink()
        st = self.cstate()
        st["halt"] = None  # the owner verified the ruleset and cleared the halt
        (self.camp / "campaign.json").write_text(json.dumps(st))
        self.campaign("run", ok=True)
        self.assertIn("not merged", json.dumps([e for e in self.cevents() if e["event"] == "merge_reconciled"]))
        self.assertEqual(self.status("a"), "completed")

    def test_failed_ci_run_with_a_passing_summary_is_never_accepted(self):
        (self.ctl / "ci-lie").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=True)
        self.assertEqual(self.calls("merge-calls"), [])
        self.assertFalse((self.camp / "merges/pr-1" / self.st()["a"]["approved_head"] / "ci-local").exists())
        (self.ctl / "ci-lie").unlink()
        st = self.cstate()
        st["merges"]["a"]["attempts"] = 0
        (self.camp / "campaign.json").write_text(json.dumps(st))
        self.campaign("run", ok=True)  # a restart re-runs CI rather than trusting the old files
        self.assertEqual(len(self.merges()), 1)
        self.assertEqual(len((self.t / "log/agents.log.ci").read_text().splitlines()), 3)

    def test_merge_without_acknowledgement_is_settled_from_github(self):
        (self.ctl / "merge-no-ack").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=True)
        self.assertEqual(self.status("a"), "completed")
        self.assertEqual(len(self.merges()), 1)
        self.assertEqual(self.cstate()["merges"]["a"]["status"], "merged")

    def test_ruleset_not_restored_after_a_reported_success_halts(self):
        (self.ctl / "merge-no-ack").write_text("")
        (self.ctl / "relaxed.json").replace(self.t / "ruleset-live.json")  # the script "restored" nothing
        self.authorize({"CG-1": {"tasks": [self.ctask("a"), self.ctask("b")]}})
        self.campaign("run", ok=False)
        self.assertIn("ruleset not verified", self.cstate()["halt"])
        self.assertEqual(len(self.merges()), 1)

    # ------------------------------------------------------------ authorization revocation

    def write_auth(self, tickets, mode="exception"):
        self.authorize(tickets, mode=mode)

    def test_revocation_mid_implementation_stops_review_but_not_other_work(self):
        self.codex_plan({"a": ["sleep-commit"]})
        self.set_plane(tickets={"CG-1": "started", "CG-2": "started"})
        self.write_auth({"CG-1": {"tasks": [self.ctask("a")]}, "CG-2": {"tasks": [self.ctask("z")]}})
        p = self.start_campaign()
        self.wait_for_call("a")
        self.write_auth({"CG-2": {"tasks": [self.ctask("z")]}})  # CG-1 revoked while a runs
        out, _ = p.communicate(timeout=180)
        self.assertEqual(self.status("z"), "completed", out)
        self.assertEqual(self.status("a"), "awaiting_review")  # its work is published and kept, not reviewed
        self.assertNotIn("review:a", [c["key"] for c in self.codex_calls()])
        self.assertIn("deauthorized", [e["event"] for e in self.cevents()])
        self.assertTrue(self.git("--git-dir", str(self.origin), "rev-parse", "task/a"))
        self.write_auth({"CG-1": {"tasks": [self.ctask("a")]}, "CG-2": {"tasks": [self.ctask("z")]}})
        self.campaign("run", ok=True)
        self.assertEqual(self.status("a"), "completed")

    def test_revocation_blocks_branch_update(self):
        self.set_plane(tickets={"CG-1": "started", "CG-2": "started"})
        self.write_auth({"CG-1": {"tasks": [self.ctask("a")]}, "CG-2": {"tasks": [self.ctask("b")]}}, mode="none")
        self.campaign("run", ok=True)
        a, b = self.st()["a"], self.st()["b"]
        self.git("--git-dir", str(self.origin), "update-ref", "refs/heads/v2.0", a["approved_head"])
        self.set_pr(a["pr"], state="MERGED")
        self.write_auth({"CG-1": {"tasks": [self.ctask("a")]}})  # CG-2 revoked
        self.campaign("run", ok=True)
        self.assertEqual(self.status("b"), "blocked")
        self.assertNotIn("branch_update", [e["event"] for e in self.cevents()])
        self.write_auth({"CG-1": {"tasks": [self.ctask("a")]}, "CG-2": {"tasks": [self.ctask("b")]}})
        self.campaign("run", ok=True)
        self.assertIn("branch_update", [e["event"] for e in self.cevents()])
        self.assertEqual(self.status("b"), "completed")

    def test_revocation_right_before_merge_prevents_it(self):
        (self.ctl / "revoke-on-verify").write_text(str(self.camp / "authorization.json"))
        self.write_auth({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=True)
        self.assertEqual(self.merges(), [])
        self.assertIn("deauthorized", self.cstate()["merges"]["a"]["reason"])

    def test_invalid_authorization_fails_closed(self):
        self.write_auth({"CG-1": {"tasks": [self.ctask("a")]}}, mode="none")
        self.campaign("run", ok=True)
        (self.camp / "authorization.json").write_text('{"tickets": ')
        self.set_plane(tickets={"CG-1": "started"})
        self.campaign("run", ok=False)  # refuses to start on an unreadable file
        self.assertIn("authorization.json", self.out)
        self.assertEqual(self.status("a"), "awaiting_ci")

    # ------------------------------------------------------------ live output

    def test_restart_resumes_the_live_stream_without_loss_or_duplicates(self):
        self.codex_plan({"a": ["sleep-commit"]})
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        p = self.start_campaign()
        self.wait_for_call("a")
        p.kill()
        first, _ = p.communicate(timeout=30)
        self.campaign("run", ok=True)
        both = first + self.out
        self.assertEqual(both.count("$ git commit -qm a"), 1, both)
        self.assertEqual(both.count("» reviewed"), 1)
        (self.camp / "stream-offsets.json").write_text("[[[")  # damaged: no replay
        self.campaign("status", ok=True)
        self.campaign("run", ok=True)
        self.assertNotIn("$ git commit -qm a", self.out)

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
        rej = [e for e in self.cevents() if e["event"] == "ci_rejected"]
        self.assertEqual(len(rej), 1)
        self.assertRegex(" ".join(rej[0]["problems"]), "symbolic link|not a directory")
        self.assertIn("was rejected", self.cstate()["merges"]["a"]["reason"])
        self.assertFalse([f for f in self.camp.rglob("leak")])

    def test_symlinked_local_ci_output_directory_is_rejected(self):
        (self.ctl / "secretdir").mkdir()
        (self.ctl / "secretdir/summary.json").write_text("{}")
        (self.ctl / "secretdir/lease.token").write_text("token")
        (self.ctl / "ci-dirlink").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        self.campaign("run", ok=True)
        self.assertEqual(self.calls("merge-calls"), [])
        rej = [e for e in self.cevents() if e["event"] == "ci_rejected"]
        self.assertEqual(len(rej), 1)
        self.assertRegex(" ".join(rej[0]["problems"]), "symbolic link|not a directory")
        self.assertIn("was rejected", self.cstate()["merges"]["a"]["reason"])
        self.assertFalse([f for f in self.camp.rglob("lease.token")])
        self.campaign("run", ok=True)  # a restart does not accept the rejected head's evidence
        self.assertEqual(self.calls("merge-calls"), [])

    def test_local_ci_refuses_a_slot_with_leftover_files_and_cannot_write_it(self):
        (self.ctl / "ci-write").write_text("")
        self.authorize({"CG-1": {"tasks": [self.ctask("a")]}})
        (self.seed / ".git/info").mkdir(exist_ok=True)
        (self.seed / ".git/info/exclude").write_text("*.bin\n")
        for s in (A, B):  # an earlier run left an ignored file behind; pool.sh still sees the slot idle
            (self.slots / s / "leftover.bin").write_text("x")
        self.campaign("run", ok=True, minutes="0.5")
        self.assertEqual(self.calls("merge-calls"), [])
        self.assertIn("untracked or ignored files", json.dumps(self.cstate()["merges"]))
        for s in (A, B):
            (self.slots / s / "leftover.bin").unlink()
        st = self.cstate()
        st["merges"]["a"]["attempts"] = 0
        (self.camp / "campaign.json").write_text(json.dumps(st))
        self.campaign("run", ok=True)
        self.assertIn("SLOT-WRITE-DENIED", (self.t / "log/agents.log.ci").read_text())
        self.assertEqual(len(self.merges()), 1)

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
        (self.seed / "TODO.md").write_text("private notes")  # an untracked file of the primary checkout
        (self.t / "log/pm-canary").write_text(str(self.seed / "TODO.md"))
        before = (self.camp / "authorization.json").read_text()
        self.campaign("propose", "CG-1", ok=True)
        proposal = Path(self.out.split("proposal for CG-1: ")[1].split()[0])
        self.assertEqual(json.loads(proposal.read_text())["tasks"][0]["id"], "cg-1-step")
        pm = [c for c in self.codex_calls() if c["key"] == "pm"]
        self.assertEqual(len(pm), 1)
        self.assertNotEqual(os.path.realpath(pm[0]["cwd"]), os.path.realpath(self.seed))  # a clean export, not the checkout
        self.assertFalse(pm[0]["canary_readable"])  # the primary checkout's ignored files stay unreadable
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
    def setUp(self):
        sys.path.insert(0, str(HERE))
        import campaign
        import coordinator
        campaign.co = coordinator
        self.cp, self.co = campaign, coordinator
        import tempfile
        self.d = Path(tempfile.mkdtemp())
        self.addCleanup(__import__("shutil").rmtree, self.d, True)

    def summary(self, text):
        (self.d / "ci").mkdir(exist_ok=True)
        (self.d / "ci/summary.json").write_text(text)
        return self.cp.ci_passed(self.d / "ci", "h" * 40, "b" * 40)

    def test_ci_summary_is_untrusted(self):
        good = {"head": "h" * 40, "base": "b" * 40, "all_passed": True, "failed": []}
        self.assertTrue(self.summary(json.dumps(good)))
        bad = ["", "{", json.dumps(good)[:-5], "[" * 100000 + "]" * 100000, "null", "[]", '"x"',
               json.dumps({**good, "all_passed": "true"}), json.dumps({**good, "failed": None}),
               json.dumps({**good, "head": ["h" * 40]}), json.dumps({**good, "pad": "x" * (2 << 20)}),
               json.dumps({**good, "base": "c" * 40})]
        for text in bad:
            self.assertFalse(self.summary(text), text[:60])
        self.assertFalse(self.cp.ci_passed(self.d / "ci", "h" * 40, "b" * 40, [f"{'h' * 40}:{'b' * 40}"]))
        self.assertFalse(self.cp.ci_passed(self.d / "missing", "h" * 40, "b" * 40))

    def test_agent_output_parsing_is_bounded(self):
        deep = "[" * 200000 + "]" * 200000
        self.assertIsNone(self.co.parse_json(deep))
        self.assertIsNone(self.co.parse_json("{" + "x" * (2 << 20)))
        self.assertEqual(self.co.agent_text("codex", deep + "\n" + '{"type":"item.completed","item":[1]}\n'), "")
        self.assertEqual(self.co.agent_text("codex", '{"type":"item.completed","item":{"type":"agent_message","text":7}}'), "")
        self.assertEqual(self.cp.render('{"type":"item.completed","item":{"type":"agent_message","text":{"a":1}}}'), "» ")
        self.assertIsNotNone(self.cp.render(deep[:500]))

    def test_evidence_copy_is_bounded_and_link_free(self):
        src, dst = self.d / "out", self.d / "dst"
        deep = src
        for i in range(40):
            deep = deep / f"d{i}"
        deep.mkdir(parents=True)
        (deep / "x").write_text("x")
        rejected, errors = self.cp.copy_evidence(src, dst)
        self.assertTrue(rejected and "nesting" in rejected[0])
        many = self.d / "many"
        many.mkdir()
        old = dict(self.cp.EVIDENCE_LIMITS)
        self.cp.EVIDENCE_LIMITS.update(files=10)
        try:
            for i in range(20):
                (many / f"f{i}").write_text("x")
            self.assertTrue(self.cp.copy_evidence(many, self.d / "dst2")[0])
        finally:
            self.cp.EVIDENCE_LIMITS.update(old)
        os.symlink(many, self.d / "linkdir")
        self.assertTrue(self.cp.copy_evidence(self.d / "linkdir", self.d / "dst3")[0])
        self.assertEqual(self.cp.copy_evidence(self.d / "nothing", self.d / "dst4")[0], [])  # missing: an error
        ok = self.d / "ok"
        (ok / "logs").mkdir(parents=True)
        (ok / "summary.json").write_text("{}")
        (ok / "logs/a.log").write_text("a")
        self.assertEqual(self.cp.copy_evidence(ok, self.d / "dst5"), ([], []))
        self.assertEqual((self.d / "dst5/logs/a.log").read_text(), "a")

    def test_render(self):
        r = self.cp.render
        self.assertEqual(r('{"type":"item.completed","item":{"type":"command_execution","command":"go test","exit_code":1}}'),
                         "$ go test  (exit 1)")
        self.assertEqual(r('{"type":"item.completed","item":{"type":"agent_message","text":"a\\n b"}}'), "» a b")
        self.assertIsNone(r('{"type":"thread.started"}'))
        self.assertEqual(r("plain"), "plain")


if __name__ == "__main__":
    unittest.main(verbosity=2)

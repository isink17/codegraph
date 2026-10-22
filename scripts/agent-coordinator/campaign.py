#!/usr/bin/env python3
"""Campaign supervisor: drives coordinator.py over owner-authorized, Plane-open
work for a bounded wall-clock budget, then merges through the existing merge
authority and synchronises Plane.

Policy and safety contract: docs/agent-campaign.md. Task lifecycle, leases,
agents and review stay with coordinator.py and pool.sh; this program never
writes a lease file, never edits coordinator state and never merges except by
running the owner-configured merge script.

    campaign.py run --dir DIR [--minutes N] [--poll SECONDS]
    campaign.py status --dir DIR
    campaign.py stop --dir DIR
    campaign.py propose --dir DIR CG-N

Exit codes: 0 stopped (idle, deadline, STOP), 1 refused or halted, 2 usage,
130 interrupted.
"""

import argparse
import fcntl
import html
import json
import os
import re
import shutil
import signal
import stat
import subprocess
import sys
import time
import urllib.request
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path

HERE = Path(__file__).resolve().parent
COORD = HERE / "coordinator.py"
co = None  # coordinator module, imported once the campaign's state paths are in the environment

PLANE_URL = "http://192.168.1.219/api/v1/workspaces/ivan-dev"
PLANE_PROJECT = "6f0dae86-fc12-4548-ae80-b5aa7de5f540"
PLANE_KEYCHAIN = "plane-mcp-ivan-dev"
PLANE_CMD = os.environ.get("CG_CAMPAIGN_PLANE_CMD")
PLANE_SYNC = os.environ.get("CG_CAMPAIGN_PLANE_SYNC",
                            str(Path.home() / "Projekti/codegraph-evidence/plane-sync/plane-sync.sh"))
REPO_SLUG = "isink17/codegraph"
OPEN_GROUPS = {"backlog", "unstarted", "started"}
MERGE_MODES = ("none", "gate", "exception")
MAX_MERGE_ATTEMPTS = 2
MAX_BRANCH_UPDATES = 3
SYNC_RETRY_SECONDS = 60
CAMPAIGN_FIELDS = {"ticket"}


class Refused(Exception):
    pass


class PlaneDown(Exception):
    pass


def now():
    return datetime.now(timezone.utc)


def stamp(t=None):
    return (t or now()).strftime("%Y-%m-%dT%H:%M:%SZ")


def say(tag, msg):
    print(f"[{datetime.now().strftime('%H:%M:%S')}] [{tag}] {msg}", flush=True)


# ---------------------------------------------------------------- Plane (read-only)

def plane_get(path):
    if PLANE_CMD:
        p = subprocess.run([PLANE_CMD, path], capture_output=True, text=True)
        if p.returncode != 0:
            raise PlaneDown(p.stderr.strip() or f"exit {p.returncode}")
        return json.loads(p.stdout)
    key = subprocess.run(["/usr/bin/security", "find-generic-password", "-a", os.environ.get("USER", ""),
                          "-s", PLANE_KEYCHAIN, "-w"], capture_output=True, text=True)
    if key.returncode != 0:
        raise PlaneDown("no Plane API key in the keychain")
    req = urllib.request.Request(f"{PLANE_URL}/{path}", headers={"X-API-Key": key.stdout.strip()})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return json.loads(r.read())
    except (OSError, ValueError) as e:
        raise PlaneDown(str(e))


def plane_tickets(tickets):
    """{ticket: {"group", "name", "description"}} for every ticket; raises
    PlaneDown when Plane cannot answer for all of them."""
    states = {s["id"]: s["group"] for s in plane_get(f"projects/{PLANE_PROJECT}/states/").get("results", [])}
    out = {}
    for tk in tickets:
        w = plane_get(f"work-items/{tk}/")
        if states.get(w.get("state")) is None:
            raise PlaneDown(f"{tk}: unknown state {w.get('state')}")
        out[tk] = {"group": states[w["state"]], "name": w.get("name", ""),
                   "description": w.get("description_stripped") or ""}
    return out


# ---------------------------------------------------------------- campaign files

class Campaign:
    def __init__(self, d):
        self.dir = Path(d).resolve()
        self.auth_file = self.dir / "authorization.json"
        self.state_file = self.dir / "campaign.json"
        self.events_file = self.dir / "events.jsonl"
        self.stop_file = self.dir / "STOP"
        self.coord_dir = self.dir / "coordinator"
        self.offsets = None  # first pass: start at the end of existing logs
        self.sync_tried = 0.0  # per process: a restarted supervisor retries at once

    def lock(self):
        fd = os.open(self.dir / "campaign.lock", os.O_RDWR | os.O_CREAT, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            os.close(fd)
            raise Refused(f"another campaign supervisor owns {self.dir}")
        self.lock_fd = fd

    def load(self):
        self.auth = load_authorization(self.auth_file)
        if self.state_file.exists():
            try:
                self.state = json.loads(self.state_file.read_text())
                assert isinstance(self.state.get("tasks"), dict)
            except (ValueError, AssertionError, AttributeError) as e:
                raise Refused(f"{self.state_file} is malformed ({e}); left untouched")
        else:
            self.state = {"version": 1, "tasks": {}, "merges": {}, "unsynced": [], "halt": None, "deadline": None}

    def save(self):
        co.write_atomic(self.state_file, json.dumps(self.state, indent=2, sort_keys=True) + "\n")

    def event(self, event, **data):
        with open(self.events_file, "a") as f:
            f.write(json.dumps({**data, "utc": stamp(), "event": event}, sort_keys=True) + "\n")
            f.flush()
            os.fsync(f.fileno())

    def coord_env(self):
        return {**os.environ, "CG_COORD_STATE": str(self.coord_dir), "CG_COORD_STOP": str(self.stop_file)}

    def coordinator(self, *args, stream=True):
        """Runs coordinator.py on the campaign's state; output is streamed."""
        p = subprocess.Popen([sys.executable, str(COORD), *args], env=self.coord_env(), text=True,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        out = []
        for line in p.stdout:
            out.append(line)
            if stream:
                say("coordinator", line.rstrip())
        return p.wait(), "".join(out)

    def tasks(self):
        f = self.coord_dir / "state.json"
        return json.loads(f.read_text())["tasks"] if f.exists() else {}


def load_authorization(path):
    """The owner's authorization: the only source of work and merge rights."""
    try:
        a = json.loads(Path(path).read_text())
    except (OSError, ValueError) as e:
        raise Refused(f"cannot read {path}: {e}")
    if not isinstance(a, dict) or not isinstance(a.get("tickets"), dict) or not a["tickets"]:
        raise Refused(f"{path}: needs a non-empty \"tickets\" object")
    merge = a.setdefault("merge", {"mode": "none"})
    if merge.get("mode") not in MERGE_MODES:
        raise Refused(f"{path}: merge.mode must be one of {MERGE_MODES}")
    if merge["mode"] == "exception":
        d = Path(merge.get("exception_dir", ""))
        for f in ("merge-exception.sh", "ci-local.sh"):
            if not os.access(d / f, os.X_OK):
                raise Refused(f"{path}: merge.exception_dir has no executable {f}")
    if a.get("expires") and stamp()[:10] > a["expires"]:
        raise Refused(f"{path}: authorization expired on {a['expires']}")
    seen = {}
    for tk, spec in a["tickets"].items():
        if not re.fullmatch(r"CG-\d+", tk) or not isinstance(spec, dict) or not isinstance(spec.get("tasks"), list):
            raise Refused(f"{path}: ticket {tk!r} needs a \"tasks\" list")
        for t in spec["tasks"]:
            tid = t.get("id") if isinstance(t, dict) else None
            if tid in seen:
                raise Refused(f"{path}: task {tid} is listed under {seen[tid]} and {tk}")
            seen[tid] = tk
    return a


def task_specs(auth):
    """[(ticket, coordinator task spec)] in authorization order."""
    return [(tk, {k: v for k, v in t.items() if k not in CAMPAIGN_FIELDS})
            for tk, spec in auth["tickets"].items() for t in spec["tasks"]]


# ---------------------------------------------------------------- planning

def plan(c, plane):
    """Plans authorized tasks of open tickets that the coordinator does not
    know yet, and cancels pending tasks whose ticket is no longer open.
    Plane is never written here."""
    known = c.tasks()
    new = []
    for tk, spec in task_specs(c.auth):
        tid = spec["id"]
        group = plane[tk]["group"]
        if tid in known:
            c.state["tasks"].setdefault(tid, tk)
            if group not in OPEN_GROUPS and known[tid]["status"] == co.PENDING:
                say("pm", f"{tid}: {tk} is {group} in Plane; cancelling the pending task")
                c.coordinator("cancel", tid)
                c.event("cancelled_closed_ticket", task=tid, ticket=tk, group=group)
            continue
        if group not in OPEN_GROUPS:
            continue
        new.append(spec)
        c.state["tasks"][tid] = tk
    if not new:
        return
    f = c.dir / "plans" / f"plan-{now().strftime('%Y%m%dT%H%M%S')}-{uuid.uuid4().hex[:6]}.json"
    f.parent.mkdir(exist_ok=True)
    co.write_atomic(f, json.dumps({"tasks": new}, indent=2) + "\n", 0o644)
    rc, out = c.coordinator("plan", str(f))
    if rc != 0:
        for spec in new:
            c.state["tasks"].pop(spec["id"], None)
        c.event("plan_refused", file=str(f), output=out.strip()[-500:])
        raise Refused(f"coordinator refused the plan {f}: {out.strip()}")
    c.save()
    c.event("planned", tasks=[s["id"] for s in new], file=str(f))


# ---------------------------------------------------------------- live output

def render(line):
    try:
        rec = json.loads(line)
    except ValueError:
        return line.rstrip()[:300] or None
    if not isinstance(rec, dict):
        return None
    item = rec.get("item") or {}
    kind = rec.get("type")
    if kind == "result":  # Claude Code json output
        return "result: " + (rec.get("result") or "")[:300]
    if item.get("type") == "command_execution":
        if kind == "item.started":
            return f"$ {item.get('command', '')[:200]}"
        if kind == "item.completed":
            return f"$ {item.get('command', '')[:200]}  (exit {item.get('exit_code')})"
    if item.get("type") == "agent_message" and kind == "item.completed":
        return "» " + " ".join((item.get("text") or "").split())[:300]
    if kind in ("turn.completed", "turn.failed", "error"):
        return kind + (f": {rec.get('error') or rec.get('message')}" if kind != "turn.completed" else "")
    return None


def stream_agents(c):
    """Prints what every agent run has written since the last pass, also
    runs that started and ended between two passes."""
    slots = {t["id"]: t.get("slot") for t in c.tasks().values()}
    if c.offsets is None:
        c.offsets = {str(f): f.stat().st_size for f in (c.coord_dir / "runs").glob("*/output.log")}
    for log in sorted((c.coord_dir / "runs").glob("*/output.log")):
        m = re.match(r"(.+)-(implement|review)-\d+-[0-9a-f]+$", log.parent.name)
        if not m:
            continue
        try:
            with open(log, "rb") as f:
                f.seek(c.offsets.get(str(log), 0))
                data = f.read()
        except OSError:
            continue
        cut = data.rfind(b"\n") + 1
        c.offsets[str(log)] = c.offsets.get(str(log), 0) + cut
        for line in data[:cut].decode(errors="replace").splitlines():
            text = render(line)
            if text:
                say(f"{m.group(1)}:{m.group(2)}@{slots.get(m.group(1)) or '-'}", text)


# ---------------------------------------------------------------- merging

def run_logged(c, tag, argv, log, cwd=None, env=None):
    """Runs argv with its output streamed to the terminal and kept in log."""
    log.parent.mkdir(parents=True, exist_ok=True)
    with open(log, "a") as f:
        f.write(f"$ {' '.join(map(str, argv))}\n")
        p = subprocess.Popen([str(a) for a in argv], cwd=cwd, env=env, text=True,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        out = []
        for line in p.stdout:
            f.write(line)
            out.append(line)
            say(tag, line.rstrip())
        rc = p.wait()
        f.write(f"exit {rc}\n")
    return rc, "".join(out)


def run_merge(c, tag, argv, log):
    """Runs the merge authority's --merge step so that nothing interrupts it
    halfway (it relaxes and restores a ruleset): its own session, output to
    its log rather than a pipe, and SIGINT/SIGTERM deferred until it exits."""
    old = {s: signal.signal(s, signal.SIG_IGN) for s in (signal.SIGINT, signal.SIGTERM)}
    try:
        with open(log, "a") as f:
            f.write(f"$ {' '.join(map(str, argv))}\n")
            start = f.tell()
            f.flush()
            p = subprocess.Popen([str(a) for a in argv], cwd=co.REPO, stdout=f, stderr=subprocess.STDOUT,
                                 stdin=subprocess.DEVNULL, start_new_session=True)
        seen = start
        while True:
            rc = p.poll()
            with open(log, "rb") as f:
                f.seek(seen)
                new = f.read()
            done = len(new) if rc is not None else new.rfind(b"\n") + 1
            for line in new[:done].decode(errors="replace").splitlines():
                say(tag, line)
            seen += done
            if rc is not None:
                break
            time.sleep(0.2)
        with open(log, "a") as f:
            f.write(f"\nexit {rc}\n")
        with open(log, errors="replace") as f:
            f.seek(start)
            return rc, f.read()
    finally:
        for s, h in old.items():
            signal.signal(s, h)


def ci_passed(cidir, head, base):
    try:
        s = json.loads((cidir / "summary.json").read_text())
    except (OSError, ValueError):
        return False
    return s.get("head") == head and s.get("base") == base and s.get("all_passed") is True and s.get("failed") == []


def go_packages(head, base):
    changed = co.run([co.GIT, *co.GIT_SAFE, "-C", str(co.REPO), "diff", "--name-only", base, head, "--", "*.go"],
                     check=True).stdout.split()
    return sorted({"./" + str(Path(f).parent) for f in changed})


def local_ci(c, tid, t, head, base, cidir):
    """Runs the owner's ci-local.sh on the exact head in a worker leased
    through pool.sh, inside the agent sandbox (it runs PR code). Returns
    None when it passed, else why not."""
    tag = f"{tid}:ci-local"
    tokens = c.coord_dir / "tokens"
    why = settle_ci_lease(c)
    if why:
        return why
    txn = uuid.uuid4().hex
    c.state["ci_lease"] = {"task": tid, "txn": txn}
    c.save()
    p = co.run([str(co.POOL), "acquire", "--slot", "auto", "--task", f"ci/{tid}", "--agent", "campaign/ci-local",
                "--owner", f"campaign:{txn}"])
    got = re.search(r"^leased (\S+) token=([0-9a-f]{32})$", p.stdout, re.M)
    if not got:
        c.state["ci_lease"] = None
        c.save()
        return "BLOCKED_POOL_BUSY" if p.returncode == 3 else f"ci-local acquire refused: {p.stderr.strip()}"
    slot, token = got.groups()
    tokens.mkdir(mode=0o700, exist_ok=True)
    co.write_atomic(tokens / f"ci-{txn}.token", token + "\n")
    c.state["ci_lease"] = {"task": tid, "txn": txn, "slot": slot}
    c.save()
    c.event("ci_leased", task=tid, slot=slot)
    sp = co.slot_path(slot)
    why = None
    try:
        co.git(sp, "fetch", "-q", co.REMOTE)
        co.git(sp, "switch", "-q", "--detach", head)
        pkgs = go_packages(head, base)
        # Passed as text: the sandbox denies reads of the evidence root.
        ci = (Path(c.auth["merge"]["exception_dir"]) / "ci-local.sh").read_text()
        argv = ["/bin/bash", "-c", ci, "ci-local.sh", str(sp), head, base, "@OUT@", *([".", *pkgs] if pkgs else [])]
        wrapped = co.sandboxed(argv, slot=sp)
        if not wrapped:
            raise co.Refused("ci-local needs the macOS sandbox")
        argv, scratch = wrapped
        out = Path(scratch) / "ci"
        argv = [str(out) if a == "@OUT@" else a for a in argv]
        try:
            rc, _ = run_logged(c, tag, argv, cidir.parent / "ci-local.log", cwd=sp)
            links = copy_evidence(out, cidir)
            if links:
                why = f"ci-local output contained symbolic links {links[:3]}; evidence rejected"
                (cidir / "summary.json").unlink(missing_ok=True)
        finally:
            shutil.rmtree(scratch, ignore_errors=True)
        if not why and (rc != 0 or not ci_passed(cidir, head, base)):
            why = f"local CI failed for {head[:12]} (exit {rc}); evidence {cidir}"
    except co.Refused as e:
        why = f"ci-local setup failed: {e}"
    finally:
        why = "; ".join(filter(None, [why, settle_ci_lease(c)])) or None
    return why


def copy_evidence(src, dst):
    """Copies the regular files of src (written by PR code) into dst without
    following any symbolic link, src itself included: every directory and
    file is opened relative to its parent's descriptor with O_NOFOLLOW, so a
    link swapped in meanwhile fails instead of being read. Returns the links
    found; nothing else is copied (tmp/ is skipped)."""
    links = []

    def walk(dfd, rel):
        for name in os.listdir(dfd):
            st = os.stat(name, dir_fd=dfd, follow_symlinks=False)
            path = f"{rel}/{name}" if rel else name
            if stat.S_ISLNK(st.st_mode):
                links.append(path)
            elif stat.S_ISDIR(st.st_mode) and path != "tmp":
                fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=dfd)
                try:
                    (dst / path).mkdir(parents=True, exist_ok=True)
                    walk(fd, path)
                finally:
                    os.close(fd)
            elif stat.S_ISREG(st.st_mode):
                fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=dfd)
                with os.fdopen(fd, "rb") as f, open(dst / path, "wb") as g:
                    shutil.copyfileobj(f, g)

    try:
        top = os.open(src, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    except FileNotFoundError:
        return links
    except OSError:
        return [str(src)]
    try:
        dst.mkdir(parents=True, exist_ok=True)
        walk(top, "")
    except OSError as e:
        links.append(f"unreadable: {e}")
    finally:
        os.close(top)
    return links


def settle_ci_lease(c):
    """Releases the ci-local lease recorded in the campaign state, also after
    a crash: no lease with its owner id means none was taken; a lease with
    its token on file is released through pool.sh (which refuses to lose
    work); anything else is left for the owner. Returns why it is unsettled."""
    m = c.state.get("ci_lease")
    if not m:
        return None
    owner = f"campaign:{m['txn']}"
    lease = next(((s, v) for s, v in co.Coordinator(c.coord_dir).pool_status().items()
                  if v.get("owner") == owner), None)
    tok = c.coord_dir / "tokens" / f"ci-{m['txn']}.token"
    if lease:
        if not tok.exists():
            return f"{lease[0]} is leased by {owner} but its token was not recorded; owner must release it"
        r = co.run([str(co.POOL), "release", "--slot", lease[0], "--token", tok.read_text().strip()])
        if r.returncode != 0:
            c.event("ci_release_refused", slot=lease[0], detail=r.stderr.strip())
            return f"release of ci-local slot {lease[0]} refused ({r.stderr.strip()}); lease and token kept"
        c.event("ci_released", slot=lease[0], task=m["task"])
    tok.unlink(missing_ok=True)
    c.state["ci_lease"] = None
    c.save()
    return None


def merge_one(c, tid, t, mode):
    """One merge attempt through the configured authority. Returns the squash
    SHA on success, None otherwise (the reason is recorded)."""
    m = c.state["merges"].setdefault(tid, {"attempts": 0})
    pr, head, review = t["pr"], t["approved_head"], t["review_file"]
    tag = f"{tid}:merge"
    co.run([co.GIT, *co.GIT_SAFE, "-C", str(co.REPO), "fetch", "-q", co.REMOTE], check=True)
    base = co.git(co.REPO, "rev-parse", f"{co.REMOTE}/{t['base']}")
    if co.run([co.GIT, *co.GIT_SAFE, "-C", str(co.REPO), "merge-base", "--is-ancestor", base, head]).returncode:
        m["reason"] = f"head {head[:12]} does not contain {t['base']} {base[:12]}"
        return None
    out = c.dir / "merges" / f"pr-{pr}" / head
    out.mkdir(parents=True, exist_ok=True)
    if mode == "exception":
        cidir = out / "ci-local"
        if not ci_passed(cidir, head, base):
            why = local_ci(c, tid, t, head, base, cidir)
            if why == "BLOCKED_POOL_BUSY":
                return None  # not an attempt: waits for a worker
            if why:
                m["attempts"] += 1
                m["reason"] = why
                return None
        script = Path(c.auth["merge"]["exception_dir"]) / "merge-exception.sh"
        verify = [script, pr, head, base, review, cidir]
    else:
        script = co.Coordinator(c.coord_dir).merge_gate(t["base"])
        verify = [script, "--base", t["base"], pr, head, review]
    m["attempts"] += 1
    c.save()
    rc, text = run_logged(c, tag, verify, out / "merge.log", cwd=co.REPO)
    if rc != 0:
        m["reason"] = f"merge authority refused verification (exit {rc})"
        return None
    m["merging"] = {"head": head, "utc": stamp()}
    c.save()
    rc, text = run_merge(c, tag, [*verify, "--merge"], out / "merge.log")
    m.pop("merging")
    squash = re.search(r"MERGED #?\d+ (?:as )?([0-9a-f]{40})", text)
    if rc == 0 and squash:
        m.update(status="merged", squash=squash.group(1), reason=None, base=base, evidence=str(out))
        return squash.group(1)
    if rc == 3:
        c.state["halt"] = f"ruleset restoration failed while merging PR #{pr}; see {out / 'merge.log'}"
    else:
        # Refused before anything changed only if the PR is still open at
        # the approved head; any other outcome needs the owner.
        try:
            pv = co.Coordinator(c.coord_dir).pr_view(pr)
            still_open = pv["state"] == "OPEN" and pv["headRefOid"] == head
        except co.Refused:
            still_open = False
        if not still_open:
            c.state["halt"] = (f"merge of PR #{pr} ended with exit {rc} and an unknown outcome; verify the PR, "
                               f"ruleset 24420032 and Plane; see {out / 'merge.log'}")
    m["reason"] = c.state["halt"] or f"merge refused (exit {rc}); see {out / 'merge.log'}"
    return None


def sync_body(c, tid, t, ticket):
    m = c.state["merges"][tid]
    others = [f"{o['id']}: {o['status']}" for o in c.tasks().values()
              if c.state["tasks"].get(o["id"]) == ticket and o["id"] != tid]
    e = html.escape
    li = lambda xs: "<ul>" + "".join(f"<li>{e(x)}</li>" for x in xs) + "</ul>"
    ci = ("local macOS ci-local.sh, all steps passed (run by the campaign supervisor); GitHub Actions CI "
          "unavailable and waived under the owner's exception, not passed"
          if c.auth["merge"]["mode"] == "exception" else "GitHub checks green (merge gate)")
    return "".join([
        f"<p>PR: <a href=\"https://github.com/{REPO_SLUG}/pull/{t['pr']}\">#{t['pr']}</a> "
        f"(campaign task <code>{e(tid)}</code>)</p>",
        f"<p>Reviewed head: <code>{t['approved_head']}</code>; verdict APPROVE, blocking 0 "
        f"(independent reviewer: {e(t['review']['agent'])} in the review slot)</p>",
        f"<p>Validation by the coordinator: {li(t['validate'])}</p>",
        f"<p>CI: {e(ci)}</p>",
        f"<p>Merged: squash <code>{m['squash']}</code> onto <code>{m['base']}</code></p>",
        f"<p>Acceptance criteria for this step (not evaluated as ticket completion): {li(t['acceptance'])}</p>",
        f"<p>Remaining campaign work on this ticket: {li(others) if others else 'none planned'}</p>",
        "<p>Platform verification: macOS only; Linux and Windows native unverified.</p>",
        f"<p>Evidence: <code>{e(m['evidence'])}</code></p>",
        "<p>Ticket state unchanged: a merge does not complete a ticket.</p>",
    ])


def sync(c, tid):
    t = c.tasks()[tid]
    ticket = c.state["tasks"][tid]
    body = Path(c.state["merges"][tid]["evidence"]) / "plane-sync.html"
    co.write_atomic(body, sync_body(c, tid, t, ticket), 0o644)
    rc, _ = run_logged(c, f"{tid}:plane", [PLANE_SYNC, ticket, t["pr"], body], body.with_suffix(".log"))
    c.event("plane_sync", task=tid, ticket=ticket, pr=t["pr"], exit=rc)
    return rc == 0


def stale(t):
    return t["status"] == co.BLOCKED and t.get("pr") and (t.get("reason") or "").startswith("stale base")


def update_stale(c):
    """A merge leaves every other open PR behind the base, which the
    coordinator blocks on. Its branch is updated with a merge commit
    (gh pr update-branch, never a force push); the new head voids the review,
    so it is reviewed and validated again. Bounded per task."""
    for tid, t in c.tasks().items():
        m = c.state["merges"].setdefault(tid, {"attempts": 0})
        if tid not in c.state["tasks"] or not stale(t) or m.get("updates", 0) >= MAX_BRANCH_UPDATES:
            continue
        m["updates"] = m.get("updates", 0) + 1
        c.save()
        p = co.run([co.GH, "pr", "update-branch", str(t["pr"])], cwd=co.REPO)
        c.event("branch_update", task=tid, pr=t["pr"], exit=p.returncode, detail=(p.stdout + p.stderr).strip()[-300:])
        say("pm", f"{tid}: PR #{t['pr']} behind {t['base']}: update-branch "
                  + ("done; new head needs a new review" if p.returncode == 0 else f"failed: {p.stderr.strip()}"))


def merge_ready(c, plane):
    """Merges approved tasks one at a time, and only while Plane is in step:
    a failed post-merge sync stops all merging until it is retried
    successfully."""
    mode = c.auth["merge"]["mode"]
    if c.state["unsynced"] and time.monotonic() - c.sync_tried >= SYNC_RETRY_SECONDS:
        c.sync_tried = time.monotonic()
        for tid in list(c.state["unsynced"]):
            if sync(c, tid):
                c.state["unsynced"].remove(tid)
                c.save()
    if mode == "none" or c.state["halt"] or c.state["unsynced"]:
        return
    update_stale(c)
    want = co.AWAITING_CI if mode == "exception" else co.READY
    for tid, t in c.tasks().items():
        m = c.state["merges"].get(tid, {})
        if (t["status"] != want or not t.get("approved_head") or not t.get("review_file") or tid not in c.state["tasks"]
                or m.get("status") == "merged" or m.get("attempts", 0) >= MAX_MERGE_ATTEMPTS
                or plane.get(c.state["tasks"][tid], {}).get("group") not in OPEN_GROUPS):
            continue
        say("pm", f"{tid}: merging PR #{t['pr']} at {t['approved_head'][:12]} ({mode})")
        try:
            squash = merge_one(c, tid, t, mode)
        except co.Refused as e:
            squash = None
            c.state["merges"].setdefault(tid, {"attempts": 0})["reason"] = f"merge step failed: {e}"
        c.save()
        c.event("merge", task=tid, pr=t["pr"], head=t["approved_head"], squash=squash,
                reason=c.state["merges"][tid].get("reason"))
        if not squash:
            say("pm", f"{tid}: not merged: {c.state['merges'][tid].get('reason')}")
            return
        say("pm", f"{tid}: MERGED PR #{t['pr']} as {squash}")
        if not sync(c, tid):
            c.sync_tried = time.monotonic()
            c.state["unsynced"].append(tid)
            c.save()
            say("pm", f"{tid}: Plane sync failed; no further merge until it succeeds")
        return  # one merge per pass: the coordinator reconciles the new base first


# ---------------------------------------------------------------- loop

def movable(c, tasks):
    """Task ids the campaign can still move without the owner."""
    mode = c.auth["merge"]["mode"]
    blocked_merge = c.state["halt"] or c.state["unsynced"]
    out = set()
    for tid, t in tasks.items():
        s, m = t["status"], c.state["merges"].get(tid, {})
        if s in (co.RUNNING, co.REVIEWING, co.AWAITING_REVIEW, co.CHANGES_REQUIRED):
            out.add(tid)
        elif stale(t) and mode != "none" and not blocked_merge and m.get("updates", 0) < MAX_BRANCH_UPDATES:
            out.add(tid)
        elif s in (co.AWAITING_CI, co.READY) and not blocked_merge and m.get("attempts", 0) < MAX_MERGE_ATTEMPTS \
                and ((mode == "exception" and s == co.AWAITING_CI) or (mode == "gate" and s == co.READY)):
            out.add(tid)
    changed = True
    while changed:  # pending tasks whose dependencies can still complete
        changed = False
        for tid, t in tasks.items():
            if t["status"] == co.PENDING and tid not in out and all(
                    d in out or tasks.get(d, {}).get("status") == co.COMPLETED for d in t["depends_on"]):
                out.add(tid)
                changed = True
    return out


def summary(c, tasks, deadline):
    left = max(0, int((deadline - now()).total_seconds() // 60))
    counts = {}
    for t in tasks.values():
        counts[t["status"]] = counts.get(t["status"], 0) + 1
    say("campaign", f"{left} min left; " + (", ".join(f"{k}={v}" for k, v in sorted(counts.items())) or "no tasks"))


def cmd_run(c, args):
    # Everything the supervisor later reads or runs as the owner must be out
    # of the agents' reach.
    writable = ["/private/tmp", "/tmp", "/dev",
                *filter(None, os.environ.get("CG_COORD_SANDBOX_WRITABLE", "").split(":"))]
    owned = [c.dir, Path(PLANE_SYNC)]
    if c.auth["merge"]["mode"] == "exception":
        owned.append(Path(c.auth["merge"]["exception_dir"]))
    for path in owned:
        a = os.path.realpath(path).rstrip("/") + "/"
        for d in writable:
            b = os.path.realpath(d).rstrip("/") + "/"
            if a.startswith(b) or b.startswith(a):
                raise Refused(f"{path} is writable to agents (under {d}); move it")
    for tid, m in c.state["merges"].items():
        if m.get("merging") and not c.state["halt"]:
            c.state["halt"] = (f"the supervisor stopped while merging {tid} at {m['merging']['head'][:12]}; verify "
                               "the PR, ruleset 24420032 and Plane, then remove halt and merging")
            c.event("halt", reason=c.state["halt"])
    try:
        why = settle_ci_lease(c)
    except co.Refused as e:
        why = str(e)
    if why:
        say("campaign", f"ci-local lease: {why}")
    if args.minutes is not None or not c.state.get("deadline") or c.state["deadline"] <= stamp():
        c.state["deadline"] = stamp(now() + timedelta(minutes=args.minutes if args.minutes is not None else 120))
    deadline = datetime.strptime(c.state["deadline"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    c.save()
    c.event("run_start", deadline=c.state["deadline"], pid=os.getpid())
    say("campaign", f"{c.dir.name}: deadline {c.state['deadline']}, merge mode {c.auth['merge']['mode']}")
    code, why = 0, None
    while True:
        if c.stop_file.exists():
            why = "STOP"
        elif now() >= deadline:
            why = "DEADLINE"
        elif c.state["halt"]:
            why, code = f"HALT: {c.state['halt']}", 1
        if why:
            break
        try:
            plane = plane_tickets(list(c.auth["tickets"]))
            if c.state.get("plane_down"):
                c.state["plane_down"] = None
                c.event("plane_available")
        except PlaneDown as e:
            plane = None
            if c.state.get("plane_down") != str(e):
                c.state["plane_down"] = str(e)
                c.event("plane_unavailable", detail=str(e))
            say("plane", f"unavailable ({e}): no planning and no merging until it answers")
        c.save()
        if plane is not None:
            plan(c, plane)
        c.coordinator("run", "--once")
        stream_agents(c)
        if plane is not None:
            merge_ready(c, plane)
            c.save()
        tasks = c.tasks()
        summary(c, tasks, deadline)
        unplanned = plane is None or any(
            spec["id"] not in tasks and plane[tk]["group"] in OPEN_GROUPS for tk, spec in task_specs(c.auth))
        if not movable(c, tasks) and not unplanned and not c.state["unsynced"]:
            why = "IDLE: nothing left that the campaign can move without the owner"
            break
        end = time.monotonic() + args.poll
        while time.monotonic() < end and not c.stop_file.exists():
            time.sleep(min(1.0, args.poll))
            stream_agents(c)
    c.event("run_end", reason=why)
    say("campaign", f"stopped: {why}")
    c.coordinator("status", stream=True)
    return code


def cmd_status(c, args):
    say("campaign", f"deadline={c.state.get('deadline')} halt={c.state.get('halt')} "
                    f"unsynced={c.state.get('unsynced')} plane_down={c.state.get('plane_down')}")
    for tid, m in c.state["merges"].items():
        say("campaign", f"{tid}: merge {m}")
    c.coordinator("status")
    return 0


def cmd_stop(c, args):
    c.stop_file.write_text(stamp() + "\n")
    print(f"STOP written: {c.stop_file}; the supervisor exits after its current step, running agents finish, "
          "no agent is launched or continued. Remove the file and rerun to resume.")
    return 0


PM_FIELDS = {"id", "description", "depends_on", "agent", "scope", "acceptance", "validate", "review", "max_attempts"}

PM_PROMPT = """You are the project manager for CodeGraph ticket {ticket}: {name}

{description}

Break the ticket into at most 5 bounded implementation steps for independent
coding agents. Each step must be small (one PR), have a tight file scope,
concrete acceptance criteria, and validation commands that prove them. Do not
add work the ticket does not ask for. Read the repository (read-only) to choose
scopes. Use only these fields per step: id, description, depends_on, agent,
scope, acceptance, validate, review, max_attempts. Use agent "codex",
review {{"agent": "codex", "focus": "..."}}, max_attempts 3, ids prefixed with
"{prefix}-". Reply with only the JSON object {{"tasks": [...]}}."""


def cmd_propose(c, args):
    """PM decomposition: a read-only Codex run proposes steps for an
    authorized ticket. The proposal is written for the owner to review; it
    is never planned automatically."""
    tk = args.ticket
    if tk not in c.auth["tickets"]:
        raise Refused(f"{tk} is not in {c.auth_file}; the PM decomposes only authorized tickets")
    try:
        info = plane_tickets([tk])[tk]
    except PlaneDown as e:
        raise Refused(f"Plane unavailable: {e}")
    prompt = PM_PROMPT.format(ticket=tk, name=info["name"], description=info["description"][:6000],
                              prefix=tk.lower())
    argv, scratch, auth = co.isolate("codex", co.ADAPTERS["codex"](prompt, None, [], True), readonly=True)
    out_dir = c.dir / "proposals"
    out_dir.mkdir(exist_ok=True)
    log = out_dir / f"{tk}-{now().strftime('%Y%m%dT%H%M%S')}.log"
    try:
        rc, text = run_logged(c, f"pm:{tk}", argv, log, cwd=co.REPO)
    finally:
        co.drop_scratch({"scratch": scratch, "codex_auth": auth})
    answer = co.agent_text("codex", text)
    proposal = log.with_suffix(".json")
    try:
        data = json.loads(answer[answer.index("{"):answer.rindex("}") + 1])
        co.write_atomic(proposal, json.dumps(data, indent=2) + "\n", 0o644)
        if not isinstance(data, dict) or not isinstance(data.get("tasks"), list) or \
                not all(isinstance(t, dict) and isinstance(t.get("review", {}), dict) for t in data["tasks"]):
            raise ValueError("expected {\"tasks\": [step objects with an object review]}")
        for t in data["tasks"]:
            extra = set(t) - PM_FIELDS
            if extra or t.get("agent") not in ("codex", "claude") or \
                    t.get("review", {}).get("agent") not in ("codex", "claude") or set(t.get("review", {})) - {"agent", "focus"}:
                raise ValueError(f"step {t.get('id')} uses fields or agents a proposal may not set: {sorted(extra)}")
        co.load_tasks(proposal, c.tasks())
    except (ValueError, co.Refused) as e:
        raise Refused(f"PM proposal is not a valid task list ({e}); raw output in {log}")
    print(f"proposal for {tk}: {proposal}\nReview it and copy the steps into {c.auth_file} to authorize them.")
    return 0 if rc == 0 else 1


def main(argv=None):
    global co
    ap = argparse.ArgumentParser(prog="campaign.py", description=__doc__.split("\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("run")
    p.add_argument("--dir", required=True)
    p.add_argument("--minutes", type=float)
    p.add_argument("--poll", type=float, default=30.0)
    for name in ("status", "stop"):
        sub.add_parser(name).add_argument("--dir", required=True)
    p = sub.add_parser("propose")
    p.add_argument("--dir", required=True)
    p.add_argument("ticket")
    args = ap.parse_args(argv)
    c = Campaign(args.dir)
    if not c.dir.is_dir():
        print(f"refused: {c.dir} is not a directory", file=sys.stderr)
        return 1
    os.environ["CG_COORD_STATE"] = str(c.coord_dir)
    os.environ["CG_COORD_STOP"] = str(c.stop_file)
    os.environ["CG_COORD_DENY_READ"] = ":".join(filter(None, [os.environ.get("CG_COORD_DENY_READ"), str(c.dir)]))
    sys.path.insert(0, str(HERE))
    sys.dont_write_bytecode = True
    import coordinator
    co = coordinator
    try:
        if args.cmd == "stop":
            return cmd_stop(c, args)
        if args.cmd != "status":  # status only reads
            c.lock()
        c.load()
        return {"run": cmd_run, "status": cmd_status, "propose": cmd_propose}[args.cmd](c, args)
    except (Refused, co.Refused) as e:
        print(f"refused: {e}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        c.event("run_end", reason="interrupted")
        print("\ninterrupted: running agents continue; rerun the same command to resume", file=sys.stderr)
        return 130


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    sys.exit(main())

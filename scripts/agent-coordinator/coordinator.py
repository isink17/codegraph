#!/usr/bin/env python3
"""Coordinator for bounded agent tasks on the CodeGraph agent pool.

Policy and safety contract: docs/agent-coordinator.md. Slot ownership is
delegated entirely to scripts/agent-pool/pool.sh; this program never writes
lease files and never creates worktrees.

    coordinator.py plan <tasks.json>
    coordinator.py run [--once] [--poll SECONDS]    (alias: resume)
    coordinator.py status
    coordinator.py cancel <task-id>

Exit codes: 0 ok, 1 refused or a task failed/interrupted, 2 usage.
"""

import argparse
import fcntl
import fnmatch
import json
import os
import re
import shlex
import signal
import subprocess
import sys
import time
import uuid
from pathlib import Path

HERE = Path(__file__).resolve().parent
HOME = Path.home()
POOL = Path(os.environ.get("CG_COORD_POOL", HERE.parent / "agent-pool" / "pool.sh"))
STATE_DIR = Path(os.environ.get("CG_COORD_STATE", HOME / "Projekti/codegraph-evidence/coordinator"))
POOL_ROOT = Path(os.environ.get("CG_POOL_ROOT", HOME / "Projekti"))
REPO = Path(os.environ.get("CG_POOL_REPO", HOME / "Projekti/codegraph"))
REMOTE = os.environ.get("CG_POOL_REMOTE", "origin")
GH = os.environ.get("CG_COORD_GH", "gh")
GATE = os.environ.get("CG_COORD_GATE", str(REPO / ".github/scripts/merge-gate.sh"))
REVIEW_SLOT = "codegraph-review"
MAX_IMPL = 2

PENDING, RUNNING, AWAITING_REVIEW, REVIEWING, CHANGES_REQUIRED = (
    "pending", "running", "awaiting_review", "reviewing", "review_changes_required")
AWAITING_CI, READY, COMPLETED, BLOCKED, FAILED = (
    "awaiting_ci", "ready_for_owner", "completed", "blocked", "failed")
INTERRUPTED = "interrupted"
TERMINAL = {COMPLETED}
HOLDS_SLOT = {RUNNING, REVIEWING}

TASK_FIELDS = {"id", "description", "base", "branch", "depends_on", "agent", "scope",
               "acceptance", "validate", "review", "prompt", "publish", "allowed_tools", "command", "pr"}


class Refused(Exception):
    """A precondition failed; nothing unsafe was done."""


# ---------------------------------------------------------------- utilities

def now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def run(argv, cwd=None, check=False, env=None):
    p = subprocess.run(argv, cwd=cwd, env=env, capture_output=True, text=True)
    if check and p.returncode != 0:
        raise Refused(f"{shlex.join(argv)} failed: {p.stderr.strip() or p.stdout.strip()}")
    return p


def git(slot_path, *args, check=True):
    return run(["git", "-C", str(slot_path), *args], check=check).stdout.strip()


def write_atomic(path, text, mode=0o600):
    tmp = path.with_name(f".{path.name}.{uuid.uuid4().hex}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    os.fchmod(fd, mode)
    with os.fdopen(fd, "w") as f:
        f.write(text)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def proc_start(pid):
    """Start time of pid as reported by ps, or None when it is not running.
    A pid plus its start time identifies a process; a pid alone does not."""
    p = run(["ps", "-o", "lstart=", "-p", str(pid)])
    return (p.stdout.strip() or None) if p.returncode == 0 else None


def scope_prefix(pattern):
    return re.split(r"[*?\[]", pattern, maxsplit=1)[0]


def scopes_overlap(a, b):
    """Conservative: two scopes overlap unless every pair of patterns has
    literal prefixes that cannot meet. An empty scope overlaps everything."""
    if not a or not b:
        return True
    for x in a:
        for y in b:
            px, py = scope_prefix(x), scope_prefix(y)
            if px.startswith(py) or py.startswith(px):
                return True
    return False


def in_scope(path, scope):
    return any(fnmatch.fnmatchcase(path, pat) for pat in scope)


# ---------------------------------------------------------------- task model

def load_tasks(path):
    try:
        data = json.loads(Path(path).read_text())
    except (OSError, ValueError) as e:
        raise Refused(f"cannot read task file {path}: {e}")
    tasks = data.get("tasks") if isinstance(data, dict) else None
    if not isinstance(tasks, list) or not tasks:
        raise Refused("task file needs a non-empty \"tasks\" list")
    out = {}
    for t in tasks:
        if not isinstance(t, dict):
            raise Refused("each task must be an object")
        unknown = set(t) - TASK_FIELDS
        if unknown:
            raise Refused(f"task {t.get('id')}: unknown fields {sorted(unknown)}")
        tid = t.get("id")
        if not isinstance(tid, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", tid):
            raise Refused(f"invalid task id {tid!r}")
        if tid in out:
            raise Refused(f"duplicate task id {tid}")
        for req in ("description", "scope", "acceptance", "validate"):
            if not t.get(req):
                raise Refused(f"task {tid}: {req} is required")
        if t.get("agent", "claude") not in ADAPTERS:
            raise Refused(f"task {tid}: agent must be one of {sorted(ADAPTERS)}")
        if t.get("agent") == "command" and not isinstance(t.get("command"), list):
            raise Refused(f"task {tid}: agent \"command\" needs a \"command\" argv list")
        review = t.get("review", {})
        if review.get("agent", "claude") not in ADAPTERS:
            raise Refused(f"task {tid}: review agent must be one of {sorted(ADAPTERS)}")
        out[tid] = {
            "id": tid,
            "description": t["description"],
            "base": t.get("base", "v2.0"),
            "branch": t.get("branch", f"task/{tid}"),
            "depends_on": list(t.get("depends_on", [])),
            "agent": t.get("agent", "claude"),
            "command": t.get("command"),
            "scope": list(t["scope"]),
            "acceptance": list(t["acceptance"]),
            "validate": list(t["validate"]),
            "review": {"agent": review.get("agent", "claude"), "command": review.get("command"),
                       "focus": review.get("focus", "")},
            "prompt": t.get("prompt", ""),
            "publish": t.get("publish", True),
            "allowed_tools": list(t.get("allowed_tools", [])),
            "pr": t.get("pr"),
        }
    for tid, t in out.items():
        for d in t["depends_on"]:
            if d not in out:
                raise Refused(f"task {tid} depends on unknown task {d}")
    seen, done = set(), set()

    def visit(n):
        if n in done:
            return
        if n in seen:
            raise Refused(f"dependency cycle through {n}")
        seen.add(n)
        for d in out[n]["depends_on"]:
            visit(d)
        done.add(n)
    for tid in out:
        visit(tid)
    branches = [t["branch"] for t in out.values()]
    if len(set(branches)) != len(branches):
        raise Refused("two tasks share a branch")
    return out


# ---------------------------------------------------------------- adapters
#
# Each adapter returns the argv to run in the slot. Only flags verified
# against the installed CLIs' --help are used; none bypasses permissions.

def implement_prompt(task, slot_path, findings=""):
    lines = [
        f"Task {task['id']}: {task['description']}",
        f"Working directory: {slot_path} (branch {task['branch']}, based on {REMOTE}/{task['base']}).",
        "Allowed scope (do not change files outside it): " + ", ".join(task["scope"]),
        "Acceptance criteria:", *[f"- {a}" for a in task["acceptance"]],
        "Validation commands the coordinator will run afterwards:", *[f"- {v}" for v in task["validate"]],
        "Commit your work on the current branch before exiting. Do not push, open PRs,",
        "change branches, create worktrees, or touch other directories.",
    ]
    if task["prompt"]:
        lines += ["", task["prompt"]]
    if findings:
        lines += ["", "An independent review requested changes. Address them:", findings]
    return "\n".join(lines)


def review_prompt(task, pr, head, base, diff_file):
    return "\n".join([
        f"Independent review of PR #{pr} for task {task['id']}: {task['description']}",
        f"The working tree is detached at the exact head {head}. The complete diff",
        f"`git diff {REMOTE}/{base}...{head}` is in {diff_file}. Do not modify, create or delete any file.",
        "Acceptance criteria:", *[f"- {a}" for a in task["acceptance"]],
        task["review"]["focus"],
        "End your answer with exactly these lines:",
        "Verdict: APPROVE or Verdict: REQUEST_CHANGES",
        "Blocking: <number of blocking findings>",
    ])


# No Bash for reviewers: git diff/log accept --output, which writes files.
# The coordinator hands the reviewer the diff as a file instead.
READ_ONLY_TOOLS = ["Read", "Grep", "Glob"]
IMPL_TOOLS = ["Read", "Edit", "Write", "Grep", "Glob", "Bash(git add:*)", "Bash(git commit:*)",
              "Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)"]


def claude_argv(prompt, session, tools, review, add_dirs=()):
    argv = ["claude", "-p", prompt, "--output-format", "json", "--session-id", session,
            "--permission-mode", "dontAsk"]
    for d in add_dirs:
        argv += ["--add-dir", str(d)]
    argv += ["--allowedTools", *tools]
    if review:
        argv += ["--disallowedTools", "Edit", "Write", "NotebookEdit"]
    return argv


def codex_argv(prompt, session, tools, review, add_dirs=()):
    return ["codex", "exec", "--json", "-s", "read-only" if review else "workspace-write", prompt]


def command_argv(prompt, session, tools, review, add_dirs=()):
    raise AssertionError("command adapter argv comes from the task")


ADAPTERS = {"claude": claude_argv, "codex": codex_argv, "command": command_argv}


# ---------------------------------------------------------------- state

class Coordinator:
    def __init__(self, state_dir=STATE_DIR):
        self.dir = Path(state_dir)
        self.state_file = self.dir / "state.json"
        self.events = self.dir / "events.jsonl"
        self.tokens = self.dir / "tokens"
        self.runs = self.dir / "runs"
        self.lock_fd = None
        self.state = None
        self.prepare_refused = []

    # Exclusive ownership: flock is released by the kernel if we crash, so a
    # dead coordinator never blocks a restart, and two live ones never overlap.
    def lock(self):
        self.dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        for d in (self.tokens, self.runs):
            d.mkdir(exist_ok=True, mode=0o700)
        os.chmod(self.tokens, 0o700)
        fd = os.open(self.dir / "coordinator.lock", os.O_RDWR | os.O_CREAT, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            os.close(fd)
            raise Refused(f"another coordinator owns {self.dir}")
        self.lock_fd = fd

    def load(self):
        if not self.state_file.exists():
            if any(self.tokens.iterdir()) or self.events.exists():
                raise Refused(f"{self.state_file} is missing but tokens or events exist; "
                              "refusing to start from nothing (inspect and restore it)")
            self.state = {"version": 1, "tasks": {}}
            return
        try:
            self.state = json.loads(self.state_file.read_text())
            assert isinstance(self.state.get("tasks"), dict)
        except (ValueError, AssertionError, AttributeError) as e:
            raise Refused(f"{self.state_file} is malformed ({e}); left untouched")

    def save(self):
        write_atomic(self.state_file, json.dumps(self.state, indent=2, sort_keys=True) + "\n")

    def event(self, _task, _event, **data):
        rec = {**data, "utc": now(), "task": _task, "event": _event}
        with open(self.events, "a") as f:
            f.write(json.dumps(rec, sort_keys=True) + "\n")
            f.flush()
            os.fsync(f.fileno())

    def set(self, t, status, **fields):
        old = t["status"]
        t.update(fields)
        t["status"] = status
        self.save()
        self.event(t["id"], "status", old=old, new=status,
                   **{k: v for k, v in fields.items() if k != "findings"})

    def block(self, t, reason, status=BLOCKED):
        if t["status"] == status and t.get("reason") == reason:
            return
        self.set(t, status, reason=reason)
        print(f"{t['id']}: {status}: {reason}")

    # ------------------------------------------------------------ tokens
    # Lease tokens live only in mode-0600 files in a mode-0700 directory,
    # never in state.json, events, logs or prompts.

    def token_path(self, t):
        return self.tokens / f"{t['id']}.{t['slot']}.token"

    def store_token(self, t, token):
        write_atomic(self.token_path(t), token + "\n")

    def read_token(self, t):
        try:
            return self.token_path(t).read_text().strip()
        except OSError:
            return None

    # ------------------------------------------------------------ pool.sh

    def pool(self, *args):
        return run([str(POOL), *args])

    def pool_status(self):
        out = self.pool("status").stdout
        return {m.group(1): m.group(2) for m in re.finditer(r"^(\S+): (\S+)$", out, re.M)}

    def acquire(self, t, slot, review=False):
        args = ["acquire", "--slot", slot, "--task", t["id"], "--agent", f"coordinator/{t['agent']}"]
        if t.get("pr"):
            args += ["--pr", str(t["pr"])]
        if review:
            args.append("--review")
        # Recorded before the lease exists: a crash before the slot is saved
        # leaves this marker, and the task is then interrupted rather than
        # given a second slot.
        t["starting"] = slot
        self.save()
        p = self.pool(*args)
        m = re.search(r"^leased (\S+) token=([0-9a-f]+)$", p.stdout, re.M)
        if p.returncode != 0 or not m:
            t.pop("starting")
            self.save()
            return None, p.returncode, p.stderr.strip()
        t["slot"] = m.group(1)
        self.store_token(t, m.group(2))
        t.pop("starting")
        self.save()
        return m.group(1), 0, ""

    def release(self, t, status, **fields):
        """Releases the task's slot and records the next status in the same
        save, so no crash can separate them."""
        token = self.read_token(t)
        if not token:
            return False, "lease token missing"
        p = self.pool("release", "--slot", t["slot"], "--token", token)
        if p.returncode != 0:
            return False, p.stderr.strip()
        token_file, slot = self.token_path(t), t["slot"]
        self.event(t["id"], "released", slot=slot)
        self.set(t, status, slot=None, process=None, **fields)
        token_file.unlink()
        return True, ""

    # ------------------------------------------------------------ GitHub

    def gh(self, *args):
        p = run([GH, *args], cwd=REPO)
        if p.returncode != 0:
            raise Refused(f"gh {' '.join(args[:2])} failed: {p.stderr.strip()}")
        return p.stdout

    def pr_view(self, pr):
        return json.loads(self.gh("pr", "view", str(pr), "--json",
                                  "number,state,headRefOid,headRefName,baseRefName,statusCheckRollup"))

    def find_pr(self, branch):
        prs = json.loads(self.gh("pr", "list", "--head", branch, "--state", "all", "--json", "number,state"))
        open_prs = [p for p in prs if p["state"] == "OPEN"]
        return open_prs[0]["number"] if open_prs else None


# ---------------------------------------------------------------- processes

def launch(c, t, argv, cwd, kind):
    """Starts argv detached in its own session. The wrapper records the exit
    status in a file, so it survives a coordinator crash."""
    run_dir = c.runs / f"{t['id']}-{kind}-{int(time.time())}-{uuid.uuid4().hex[:6]}"
    run_dir.mkdir(mode=0o700)
    exit_file = run_dir / "exit"
    wrapper = ['/bin/sh', '-c', '"$@"; rc=$?; echo $rc >"$0.tmp" && mv "$0.tmp" "$0"', str(exit_file), *argv]
    with open(run_dir / "output.log", "w") as log:
        proc = subprocess.Popen(wrapper, cwd=cwd, stdout=log, stderr=subprocess.STDOUT,
                                stdin=subprocess.DEVNULL, start_new_session=True)
    info = {"pid": proc.pid, "start": proc_start(proc.pid), "dir": str(run_dir), "cwd": str(cwd),
            "kind": kind, "argv0": argv[0]}
    c.event(t["id"], "launched", **info)
    return info


def agent_text(agent, out):
    """The agent's final answer: Claude's json "result", Codex's last
    agent_message item, or the raw output of a command agent."""
    if agent == "command":
        return out
    text = ""
    for line in out.splitlines():
        try:
            rec = json.loads(line)
        except ValueError:
            continue
        if not isinstance(rec, dict):
            continue
        if agent == "claude" and rec.get("type") == "result":
            text = rec.get("result") or ""
        item = rec.get("item") or {}
        if agent == "codex" and rec.get("type") == "item.completed" and item.get("type") == "agent_message":
            text = item.get("text") or ""
    return text


def process_state(info):
    """running | exited | lost. 'lost' means the process is gone and left no
    exit status: interrupted, never success."""
    exit_file = Path(info["dir"]) / "exit"
    if exit_file.exists():
        return "exited", int(exit_file.read_text().strip() or 1)
    start = proc_start(info["pid"])
    if start and start == info["start"]:
        return "running", None
    return "lost", None


def slot_path(slot):
    return POOL_ROOT / slot


# ---------------------------------------------------------------- scheduling

def deps_met(c, t):
    return all(c.state["tasks"][d]["status"] == COMPLETED for d in t["depends_on"])


def conflicts(c, t):
    for o in c.state["tasks"].values():
        # Serialised until the other task's PR is merged, so two open PRs
        # never touch the same files.
        if o["id"] != t["id"] and o["status"] not in (PENDING, COMPLETED) \
                and scopes_overlap(o["scope"], t["scope"]):
            return o["id"]
    return None


def start_impl(c, t, findings=""):
    slot, rc, err = c.acquire(t, "auto")
    if not slot:
        if rc == 3:
            return "BLOCKED_POOL_BUSY"
        c.event(t["id"], "acquire_refused", detail=err)
        return f"acquire refused: {err}"
    p = slot_path(slot)
    try:
        if run(["git", "-C", str(p), "rev-parse", "-q", "--verify", f"refs/heads/{t['branch']}"]).returncode == 0:
            git(p, "switch", "-q", t["branch"])
        else:
            git(p, "switch", "-q", "-c", t["branch"], "--no-track", f"{REMOTE}/{t['base']}")
    except Refused as e:
        c.block(t, f"cannot check out {t['branch']} in {slot}: {e}; lease kept")
        return None
    t["base_sha"] = git(p, "rev-parse", f"{REMOTE}/{t['base']}")
    t["session"] = str(uuid.uuid4())
    prompt = implement_prompt(t, p, findings)
    tools = IMPL_TOOLS + t["allowed_tools"]
    argv = t["command"] if t["agent"] == "command" else ADAPTERS[t["agent"]](prompt, t["session"], tools, False)
    t["process"] = launch(c, t, argv, p, "implement")
    c.set(t, RUNNING, attempt=t.get("attempt", 0) + 1)
    print(f"{t['id']}: running in {slot}")
    return None


def finish_impl(c, t, rc):
    """Never trusts the exit code alone: the tree must be clean, the branch
    must hold new commits, every changed file must be in scope, and every
    validation command must pass. Any failure keeps the lease and the work."""
    p = slot_path(t["slot"])
    evidence = Path(t["process"]["dir"])
    if rc != 0:
        return c.block(t, f"agent exited {rc}; work and lease kept in {t['slot']}", FAILED)
    if git(p, "status", "--porcelain", "--untracked-files=all"):
        return c.block(t, f"agent left {t['slot']} dirty; lease kept", FAILED)
    if git(p, "symbolic-ref", "--short", "HEAD", check=False) != t["branch"]:
        return c.block(t, f"agent left {t['slot']} off branch {t['branch']}; lease kept", FAILED)
    head = git(p, "rev-parse", "HEAD")
    if head == t.get("head") or git(p, "rev-list", "--count", f"{t['base_sha']}..HEAD") == "0":
        return c.block(t, "agent produced no new commit; lease kept", FAILED)
    changed = git(p, "diff", "--no-renames", "--name-only", f"{t['base_sha']}...HEAD").splitlines()
    outside = [f for f in changed if not in_scope(f, t["scope"])]
    if outside:
        return c.block(t, f"changes outside scope: {outside[:5]}; lease kept", FAILED)
    for i, cmd in enumerate(t["validate"]):
        v = subprocess.run(["/bin/sh", "-c", cmd], cwd=p, capture_output=True, text=True)
        (evidence / f"validate-{i}.log").write_text(f"$ {cmd}\nexit {v.returncode}\n{v.stdout}{v.stderr}")
        c.event(t["id"], "validated", command=cmd, exit=v.returncode, log=str(evidence / f"validate-{i}.log"))
        if v.returncode != 0:
            return c.block(t, f"validation failed: {cmd}; lease kept", FAILED)
    if git(p, "status", "--porcelain", "--untracked-files=all"):
        return c.block(t, "validation left the tree dirty; lease kept", FAILED)
    t["head"] = head
    if not t["publish"]:
        return c.block(t, f"validated at {head[:12]}; publish disabled, lease kept for the owner")
    push = run(["git", "-C", str(p), "push", "-q", REMOTE, f"{t['branch']}:refs/heads/{t['branch']}"])
    if push.returncode != 0:
        return c.block(t, f"push failed (no force is ever used): {push.stderr.strip()}; lease kept")
    try:
        pr = t.get("pr") or c.find_pr(t["branch"])
        if not pr:
            url = c.gh("pr", "create", "--base", t["base"], "--head", t["branch"],
                       "--title", t["description"][:72], "--body", pr_body(t)).strip()
            pr = int(url.rstrip("/").rsplit("/", 1)[-1])
    except (Refused, ValueError) as e:
        return c.block(t, f"PR not opened: {e}; lease kept")
    t["pr"] = pr
    c.event(t["id"], "published", pr=pr, head=head)
    ok, err = c.release(t, AWAITING_REVIEW)
    if not ok:
        return c.block(t, f"release of {t['slot']} refused: {err}")
    print(f"{t['id']}: PR #{pr} at {head[:12]}, awaiting review")


def pr_body(t):
    return "\n".join([t["description"], "", "Acceptance criteria:", *[f"- {a}" for a in t["acceptance"]],
                      "", "Validation:", *[f"- `{v}`" for v in t["validate"]],
                      "", f"Coordinator task `{t['id']}`. Requires an independent review of the exact head SHA."])


def start_review(c, t):
    try:
        pr = c.pr_view(t["pr"])
    except Refused as e:
        return f"{e}"
    if pr["state"] != "OPEN":
        return reconcile_pr(c, t, pr)
    head = pr["headRefOid"]
    run(["git", "-C", str(REPO), "fetch", "-q", REMOTE])
    slot, rc, err = c.acquire(t, REVIEW_SLOT, review=True)
    if not slot:
        if c.pool_status().get(REVIEW_SLOT) == "busy":
            return "BLOCKED_REVIEW_BUSY"
        c.event(t["id"], "acquire_refused", detail=err)
        return f"review acquire refused: {err}"
    p = slot_path(slot)
    try:
        git(p, "fetch", "-q", REMOTE, f"+refs/pull/{t['pr']}/head:refs/coordinator/review", check=False)
        git(p, "checkout", "-q", "--detach", head)
    except Refused as e:
        return c.block(t, f"review checkout of {head[:12]} failed: {e}; review lease kept")
    t["review_session"] = str(uuid.uuid4())
    diff_dir = c.runs / f"{t['id']}-diff-{head[:12]}"
    diff_dir.mkdir(mode=0o700, exist_ok=True)
    diff_file = diff_dir / "pr.diff"
    write_atomic(diff_file, git(p, "diff", f"{REMOTE}/{t['base']}...{head}") + "\n")
    prompt = review_prompt(t, t["pr"], head, t["base"], diff_file)
    r = t["review"]
    argv = r["command"] if r["agent"] == "command" else ADAPTERS[r["agent"]](
        prompt, t["review_session"], READ_ONLY_TOOLS, True, [diff_dir])
    t["process"] = launch(c, t, argv, p, "review")
    c.set(t, REVIEWING, review_head=head)
    print(f"{t['id']}: reviewing PR #{t['pr']} at {head[:12]}")


def finish_review(c, t, rc):
    """The review artifact names the head the coordinator checked out, and is
    only valid if the review slot is still clean and at that head."""
    p = slot_path(t["slot"])
    head = t["review_head"]
    out = (Path(t["process"]["dir"]) / "output.log").read_text(errors="replace")
    if rc != 0:
        return c.block(t, f"reviewer exited {rc}; review lease kept", FAILED)
    if git(p, "status", "--porcelain", "--untracked-files=all") or git(p, "rev-parse", "HEAD") != head:
        return c.block(t, "review slot was modified during review; review invalid, lease kept", FAILED)
    text = agent_text(t["review"]["agent"], out)
    verdict = re.findall(r"^Verdict: (APPROVE|REQUEST_CHANGES)\s*$", text, re.M)
    blocking = re.findall(r"^Blocking: (\d+)\s*$", text, re.M)
    if not verdict or not blocking:
        return c.block(t, "review output has no Verdict/Blocking lines; review lease kept", FAILED)
    approve = verdict[-1] == "APPROVE" and blocking[-1] == "0"
    artifact = Path(t["process"]["dir"]) / f"review-{t['pr']}-{head[:7]}.txt"
    body = re.sub(r"^(PR|Reviewed-Head|Verdict|Blocking):.*\n?", "", text, flags=re.M)
    write_atomic(artifact, "\n".join([
        f"PR: #{t['pr']}", f"Reviewed-Head: {head}",
        f"Verdict: {'APPROVE' if approve else 'REQUEST_CHANGES'}", f"Blocking: {blocking[-1] if not approve else 0}",
        f"Reviewer: coordinator/{t['review']['agent']} session {t['review_session']} (review slot)",
        "", body.strip(), ""]), mode=0o644)
    c.event(t["id"], "reviewed", head=head, verdict=verdict[-1], blocking=int(blocking[-1]), artifact=str(artifact))
    if approve:
        ok, err = c.release(t, AWAITING_CI, review_file=str(artifact), approved_head=head)
    else:
        ok, err = c.release(t, CHANGES_REQUIRED, findings=body.strip()[-8000:], approved_head=None)
    if not ok:
        return c.block(t, f"review slot release refused: {err}")
    print(f"{t['id']}: review of {head[:12]}: {'APPROVE' if approve else 'REQUEST_CHANGES'}")


def reconcile_pr(c, t, pr=None):
    """GitHub is the source of truth for the PR head, its state and CI."""
    try:
        pr = pr or c.pr_view(t["pr"])
    except Refused as e:
        return f"{e}"
    if pr["state"] == "MERGED":
        return c.set(t, COMPLETED, reason=None)
    if pr["state"] == "CLOSED":
        return c.block(t, f"PR #{t['pr']} was closed without merge")
    head = pr["headRefOid"]
    if t.get("approved_head") and head != t["approved_head"]:
        c.event(t["id"], "head_changed", approved=t["approved_head"], current=head)
        return c.set(t, AWAITING_REVIEW, approved_head=None, review_file=None, reason="PR head changed after review")
    run(["git", "-C", str(REPO), "fetch", "-q", REMOTE])
    base = run(["git", "-C", str(REPO), "rev-parse", f"{REMOTE}/{t['base']}"]).stdout.strip()
    if run(["git", "-C", str(REPO), "merge-base", "--is-ancestor", base, head]).returncode != 0:
        if t["status"] != BLOCKED or t.get("reason", "").split(":")[0] != "stale base":
            c.block(t, f"stale base: PR #{t['pr']} does not contain {REMOTE}/{t['base']} {base[:12]}; "
                       "owner must update the branch (then CI and a new review are required)")
        return None
    if t["status"] == BLOCKED and t.get("reason", "").startswith("stale base"):
        c.set(t, AWAITING_REVIEW if not t.get("approved_head") else AWAITING_CI, reason=None)
    if not t.get("approved_head"):
        if t["status"] != AWAITING_REVIEW:
            c.set(t, AWAITING_REVIEW)
        return None
    checks = pr.get("statusCheckRollup") or []
    # A list, not a dict: two checks with one name must both pass.
    states = [((x.get("name") or x.get("context")), (x.get("conclusion") or x.get("state") or "PENDING"))
              for x in checks]
    pending = ("PENDING", "EXPECTED")
    bad = [(n, s) for n, s in states if s not in ("SUCCESS", "NEUTRAL", "SKIPPED", *pending)]
    if bad:
        return c.block(t, f"CI not green at {head[:12]}: {bad}")
    if "ci" not in [n for n, _ in states] or any(s in pending for _, s in states):
        if t["status"] != AWAITING_CI:
            c.set(t, AWAITING_CI)
        return None
    gate = run([GATE, "--base", t["base"], str(t["pr"]), head, t["review_file"]], cwd=REPO)
    c.event(t["id"], "merge_gate_verify", exit=gate.returncode, output=(gate.stdout + gate.stderr).strip()[-500:])
    if gate.returncode != 0:
        return c.block(t, f"merge gate refused: {(gate.stdout + gate.stderr).strip()[-300:]}")
    if t["status"] != READY:
        c.set(t, READY, reason=None)
        print(f"{t['id']}: MERGE_READY PR #{t['pr']} at {head[:12]} (owner approval required)")
    return None


# ---------------------------------------------------------------- reconcile

def reconcile(c):
    """Runs before any new action: matches every task holding a slot against
    its lease and its process. A lease is never released or reclaimed here."""
    status = c.pool_status()
    for t in c.state["tasks"].values():
        if t["status"] not in HOLDS_SLOT:
            continue
        if not t.get("slot") or not c.read_token(t):
            c.block(t, "running task has no recorded slot or token", INTERRUPTED)
            continue
        if status.get(t["slot"]) not in ("busy", "stale-suspected"):
            c.block(t, f"{t['slot']} is {status.get(t['slot'])}, not leased; owner must inspect", INTERRUPTED)
            continue
        st, rc = process_state(t["process"])
        if st == "running":
            continue
        if st == "lost":
            c.block(t, f"agent process vanished without an exit status; lease and work kept in {t['slot']}",
                    INTERRUPTED)
            continue
        c.event(t["id"], "exited", exit=rc, kind=t["process"]["kind"])
        try:
            (finish_impl if t["status"] == RUNNING else finish_review)(c, t, rc)
        except Refused as e:
            c.block(t, f"post-run check failed: {e}; lease kept", FAILED)


def schedule(c):
    """One pass. Returns True when something changed."""
    reconcile(c)
    # Moves idle, unleased slots to the current base; pool.sh refuses any
    # slot that is leased, dirty, in use or not a fast-forward.
    prep = c.pool("prepare", "--all")
    refused = [l for l in prep.stderr.splitlines() if "is leased" not in l]
    if refused and refused != c.prepare_refused:
        c.event(None, "prepare_refused", detail=refused)
    c.prepare_refused = refused
    tasks = c.state["tasks"]
    changed = False
    busy_note = None
    for t in tasks.values():
        if t["status"] in (AWAITING_CI, READY) or (t["status"] == BLOCKED and t.get("pr")
                                                    and t.get("reason", "").startswith(("stale base", "CI not green", "merge gate refused"))):
            before = t["status"]
            try:
                reconcile_pr(c, t)
            except Refused as e:
                print(f"{t['id']}: {e}")
            changed |= t["status"] != before
    for t in tasks.values():
        if t["status"] in (PENDING, CHANGES_REQUIRED, AWAITING_REVIEW) and (t.get("starting") or t.get("slot")):
            # Crashed while acquiring a slot: a lease with task=<id> may exist
            # (pool.sh status shows it). It stays until the owner inspects it.
            c.block(t, f"a previous slot acquisition ({t.get('slot') or t.get('starting')}) did not complete; "
                       f"check pool.sh status for task={t['id']}", INTERRUPTED)
    for t in tasks.values():
        if t["status"] == AWAITING_REVIEW and not any(o["status"] == REVIEWING for o in tasks.values()):
            r = start_review(c, t)
            if r:
                busy_note = f"{t['id']}: {r}"
            changed |= t["status"] != AWAITING_REVIEW
    for t in tasks.values():
        if t["status"] not in (PENDING, CHANGES_REQUIRED):
            continue
        if sum(o["status"] == RUNNING for o in tasks.values()) >= MAX_IMPL:
            busy_note = "BLOCKED_POOL_BUSY"
            break
        if not deps_met(c, t):
            continue
        other = conflicts(c, t)
        if other:
            continue
        findings = t.get("findings", "") if t["status"] == CHANGES_REQUIRED else ""
        r = start_impl(c, t, findings)
        if r == "BLOCKED_POOL_BUSY":
            busy_note = r
            break
        if r:
            busy_note = f"{t['id']}: {r}"
        changed = True
    if busy_note:
        print(busy_note)
    return changed


# ---------------------------------------------------------------- commands

def cmd_plan(c, args):
    tasks = load_tasks(args.tasks)
    existing = c.state["tasks"]
    for tid, t in tasks.items():
        if tid in existing:
            if existing[tid]["status"] != PENDING:
                raise Refused(f"task {tid} already {existing[tid]['status']}; plan cannot redefine it")
        # A task naming an existing PR starts at review: v1 observes it.
        existing[tid] = {**t, "status": AWAITING_REVIEW if t["pr"] else PENDING, "slot": None, "process": None}
    c.save()
    for tid in tasks:
        c.event(tid, "planned")
    for t in tasks.values():
        clash = [o for o in tasks if o != t["id"] and scopes_overlap(t["scope"], tasks[o]["scope"])]
        print(f"{t['id']}: deps={t['depends_on'] or '-'} agent={t['agent']} branch={t['branch']}"
              + (f" serialised-with={clash}" if clash else ""))
    return 0


def cmd_run(c, args):
    while True:
        changed = schedule(c)
        live = [t for t in c.state["tasks"].values() if t["status"] in HOLDS_SLOT]
        if args.once or (not live and not changed):
            break
        time.sleep(args.poll)
    cmd_status(c, args, pool=False)
    if any(t["status"] in (FAILED, INTERRUPTED) for t in c.state["tasks"].values()):
        return 1
    return 0


def cmd_status(c, args, pool=True):
    for t in c.state["tasks"].values():
        bits = [t["status"]]
        for k in ("slot", "pr"):
            if t.get(k):
                bits.append(f"{k}={t[k]}")
        if t.get("head"):
            bits.append(f"head={t['head'][:12]}")
        if t.get("reason") and t["status"] in (BLOCKED, FAILED, INTERRUPTED):
            bits.append(f"reason={t['reason']}")
        print(f"{t['id']}: " + " ".join(bits))
    if pool:
        print(c.pool("status").stdout, end="")
    return 0


def cmd_cancel(c, args):
    """Stops the task's agent by process group only when pid and start time
    still match the recorded process. Never touches the work tree or lease."""
    t = c.state["tasks"].get(args.task)
    if not t:
        raise Refused(f"unknown task {args.task}")
    info = t.get("process")
    if t["status"] in HOLDS_SLOT and info:
        st, _ = process_state(info)
        if st == "running":
            os.killpg(info["pid"], signal.SIGTERM)
            c.event(t["id"], "cancel_signal", pid=info["pid"])
            for _ in range(50):
                if process_state(info)[0] != "running":
                    break
                time.sleep(0.1)
    if t["status"] == COMPLETED:
        raise Refused(f"task {t['id']} is already completed")
    held = f"; lease on {t['slot']} kept, work preserved" if t.get("slot") else ""
    c.block(t, f"cancelled by owner{held}", INTERRUPTED)
    return 0


def main(argv=None):
    ap = argparse.ArgumentParser(prog="coordinator.py", description=__doc__.split("\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("plan")
    p.add_argument("tasks")
    p = sub.add_parser("run", aliases=["resume"])
    p.add_argument("--once", action="store_true", help="one reconcile and scheduling pass, then exit")
    p.add_argument("--poll", type=float, default=30.0)
    sub.add_parser("status")
    p = sub.add_parser("cancel")
    p.add_argument("task")
    args = ap.parse_args(argv)
    c = Coordinator()
    try:
        c.lock()
        c.load()
        return {"plan": cmd_plan, "run": cmd_run, "resume": cmd_run, "status": cmd_status, "cancel": cmd_cancel}[args.cmd](c, args)
    except Refused as e:
        print(f"refused: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())

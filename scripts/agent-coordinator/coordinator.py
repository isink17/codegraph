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
# Unset: the gate committed on <remote>/<base> is used, never the main
# checkout's copy, which may be stale or modified.
GATE_OVERRIDE = os.environ.get("CG_COORD_GATE")
GATE_PATH = ".github/scripts/merge-gate.sh"
LEASES = Path(os.environ.get("CG_POOL_LEASES", HOME / "Projekti/codegraph-evidence/pool"))
CANCEL_WAIT = float(os.environ.get("CG_COORD_CANCEL_WAIT", "5"))
SANDBOX_EXEC = "/usr/bin/sandbox-exec"
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


def fault(point):
    """Test hook: CG_COORD_FAULT=<point> kills the coordinator at that point,
    as a crash would, without cleanup."""
    if os.environ.get("CG_COORD_FAULT") == point:
        os._exit(70)


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


def group_alive(pgid):
    """True while any process of the group remains. Agents run as session
    leaders, so the group outlives the wrapper while a child survives."""
    try:
        os.killpg(pgid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


# ---------------------------------------------------------------- isolation
#
# Agents run as the owner's user, so file modes cannot hide the lease tokens
# from them. On macOS every agent and validation command runs under
# sandbox-exec with reads and writes of the coordinator state and the lease
# directory denied; only the review diff is readable. Where no sandbox
# exists, claude and codex agents are refused; command agents (owner-written
# argv, used by tests) run unsandboxed.

def sandbox_available():
    return sys.platform == "darwin" and os.access(SANDBOX_EXEC, os.X_OK)


def sandboxed(argv, allow_read=()):
    if not sandbox_available():
        return None
    def q(p):
        p = os.path.realpath(p)
        if '"' in p or "\\" in p:
            raise Refused(f"cannot sandbox path {p!r}")
        return f'(subpath "{p}")'
    profile = (f"(version 1)(allow default)(deny file-read-data file-write* {q(STATE_DIR)} {q(LEASES)})"
               + "".join(f"(allow file-read-data {q(d)})" for d in allow_read))
    return [SANDBOX_EXEC, "-p", profile, *argv]


def isolate(agent, argv, allow_read=()):
    wrapped = sandboxed(argv, allow_read)
    if wrapped:
        return wrapped
    if agent != "command":
        raise Refused(f"no sandbox on this platform; refusing to run a {agent} agent that could read lease tokens")
    return argv


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
        # codex exec applies its own Seatbelt sandbox, which cannot nest in
        # ours, and its read-only mode still reads every file: no boundary
        # keeps it from the lease tokens.
        if "codex" in (t.get("agent"), review.get("agent")):
            raise Refused(f"task {tid}: the codex adapter is disabled until it can be isolated from lease tokens")
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
        """{slot: {"state": ..., lease fields...}} from pool.sh status, which
        never prints tokens. Refuses when the status cannot be read: every
        decision based on it must fail closed."""
        p = self.pool("status")
        if p.returncode != 0:
            raise Refused(f"pool.sh status failed: {p.stderr.strip()}")
        slots, cur = {}, None
        for line in p.stdout.splitlines():
            m = re.match(r"^(codegraph-[\w.-]+): (free|busy|stale-suspected|inconsistent)$", line)
            if m:
                cur = slots[m.group(1)] = {"state": m.group(2)}
            elif cur is not None and line.startswith("  ") and "=" in line:
                k, v = line.strip().split("=", 1)
                cur.setdefault(k, v)
        return slots

    def lease_held(self, t):
        """True when the task's own lease is still on its slot. Leases are
        matched by the transaction id written as their owner, so a lease
        someone else took on the same slot is never mistaken for ours."""
        lease = self.pool_status().get(t["slot"], {})
        if t.get("lease_txn"):
            return lease.get("owner") == f"coordinator:{t['lease_txn']}"
        return lease.get("task") == t["id"]  # lease taken by a v1 coordinator

    # An acquire is a transaction: the marker (with a fresh id passed to
    # pool.sh as the lease owner) is saved first, pool.sh writes its output,
    # which holds the token, and its exit status to private files in the token
    # directory. Whatever point a crash or malformed output interrupts, the
    # files and the pool's leases decide the outcome; the marker is cleared
    # only when no lease carries the id, and while any marker is open no
    # other slot is acquired.

    def acquire(self, t, slot, review=False):
        txn = uuid.uuid4().hex
        args = ["acquire", "--slot", slot, "--task", t["id"], "--agent", f"coordinator/{t['agent']}",
                "--owner", f"coordinator:{txn}"]
        if t.get("pr"):
            args += ["--pr", str(t["pr"])]
        if review:
            args.append("--review")
        t["starting"] = {"slot": slot, "txn": txn, "from": t["status"]}
        self.save()
        self.event(t["id"], "acquire_begin", slot=slot, txn=txn)
        base = self.tokens / f"acquire-{txn}"
        subprocess.run(["/bin/sh", "-c", '"$@" >"$0.out" 2>"$0.err"; echo $? >"$0.rc.tmp" && mv "$0.rc.tmp" "$0.rc"',
                        str(base), str(POOL), *args], start_new_session=True)
        fault("acquire_before_parse")
        return self.settle_acquire(t)

    def settle_acquire(self, t):
        """Returns (slot, rc, err). slot is set when the lease is ours and its
        token is stored. slot None with the marker still on the task means
        the outcome is unknown; with the marker gone, nothing was leased."""
        m = t["starting"]
        owner = f"coordinator:{m['txn']}"
        base = self.tokens / f"acquire-{m['txn']}"
        files = {k: Path(f"{base}.{k}") for k in ("out", "err", "rc")}
        # Order matters: first make sure no process of this transaction can
        # still write a lease, then look for one.
        if find_wrapper(owner):
            return None, None, "pool.sh acquire is still running"
        locked = not files["rc"].exists() and list(LEASES.glob(".*.lock"))
        held = next((s for s, v in self.pool_status().items() if v.get("owner") == owner), None)
        if held:
            out = files["out"].read_text() if files["out"].exists() else ""
            got = re.search(r"^leased (\S+) token=([0-9a-f]{32})$", out, re.M)
            if not got or got.group(1) != held:
                return None, None, f"{held} is leased with owner={owner} but pool.sh output did not yield its token"
            fault("acquire_before_token")
            t["slot"], t["lease_txn"] = held, m["txn"]
            self.store_token(t, got.group(2))
            t.pop("starting")
            self.save()
            self.event(t["id"], "acquired", slot=held, txn=m["txn"])
            rc, err = 0, ""
        elif locked:
            # pool.sh holds a slot lock while it writes a lease; a lock left
            # by a killed pool.sh may hide a lease in the making.
            return None, None, f"pool.sh was interrupted and a slot lock is held in {LEASES}"
        else:
            # pool.sh finished, never ran, or died outside a slot lock, and
            # no lease carries the id: nothing was leased.
            rc = (int(files["rc"].read_text().strip() or 1) if files["rc"].exists() else 1) or 1
            err = files["err"].read_text().strip() if files["err"].exists() else "pool.sh did not run"
            t.pop("starting")
            self.save()
            self.event(t["id"], "acquire_none", txn=m["txn"], exit=rc)
        for f in files.values():
            f.unlink(missing_ok=True)
        return (held, rc, err) if held else (None, rc, err)

    def release(self, t, status, **fields):
        """Releases the task's slot and records the next status. The intent is
        saved first, so a crash after pool.sh released the slot leaves a
        record that reconcile completes from the lease state."""
        token = self.read_token(t)
        if not token:
            return False, "lease token missing"
        t["releasing"] = {"next": status, "fields": fields}
        self.save()
        p = self.pool("release", "--slot", t["slot"], "--token", token)
        fault("release_before_save")
        try:
            held = self.lease_held(t)
        except Refused as e:
            return False, f"release outcome unknown ({e}); intent kept for reconcile"
        if held:
            t.pop("releasing")
            self.save()
            return False, p.stderr.strip() or "lease still held after release"
        self.finish_release(t)
        return True, ""

    def finish_release(self, t):
        r = t.pop("releasing")
        token_file = self.token_path(t)
        self.event(t["id"], "released", slot=t["slot"])
        self.set(t, r["next"], slot=None, process=None, lease_txn=None, **r["fields"])
        token_file.unlink(missing_ok=True)

    def merge_gate(self, base):
        """The gate to verify with: CG_COORD_GATE when set, otherwise the
        gate committed on <remote>/<base>. Either must accept --base (its
        usage, printed on no arguments with exit 2, must name it)."""
        if GATE_OVERRIDE:
            gate = Path(GATE_OVERRIDE)
        else:
            blob = run(["git", "-C", str(REPO), "rev-parse", "-q", "--verify",
                        f"{REMOTE}/{base}:{GATE_PATH}"]).stdout.strip()
            if not blob:
                raise Refused(f"no {GATE_PATH} on {REMOTE}/{base}")
            (self.dir / "gates").mkdir(mode=0o700, exist_ok=True)
            gate = self.dir / "gates" / f"{blob}.sh"
            write_atomic(gate, run(["git", "-C", str(REPO), "cat-file", "blob", blob], check=True).stdout, 0o700)
        if not os.access(gate, os.X_OK):
            raise Refused(f"{gate} is not an executable file")
        probe = run([str(gate)], cwd=REPO)
        if probe.returncode != 2 or "--base" not in probe.stdout + probe.stderr:
            raise Refused(f"{gate} does not support --base")
        return gate

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

def launch(c, t, argv, cwd, kind, status, **fields):
    """Starts argv detached in its own session and moves the task to status.
    The task is saved as holding the process before it exists; the wrapper
    writes its pid before running the agent and the exit status after, and
    its command line names the run directory. So whatever point a crash
    interrupts, reconcile finds the process or proves it never started."""
    run_dir = c.runs / f"{t['id']}-{kind}-{int(time.time())}-{uuid.uuid4().hex[:6]}"
    run_dir.mkdir(mode=0o700)
    exit_file = run_dir / "exit"
    wrapper = ['/bin/sh', '-c', 'echo $$ >"$0.pid.tmp" && mv "$0.pid.tmp" "$0.pid" || exit 125; '
               '"$@"; rc=$?; echo $rc >"$0.tmp" && mv "$0.tmp" "$0"', str(exit_file), *argv]
    info = {"pid": None, "start": None, "dir": str(run_dir), "cwd": str(cwd), "kind": kind, "argv0": argv[0]}
    c.set(t, status, process=info, **fields)
    fault("launch_before_popen")
    with open(run_dir / "output.log", "w") as log:
        proc = subprocess.Popen(wrapper, cwd=cwd, stdout=log, stderr=subprocess.STDOUT,
                                stdin=subprocess.DEVNULL, start_new_session=True)
    fault("launch_after_popen")
    info.update(pid=proc.pid, start=proc_start(proc.pid))
    fault("launch_before_save")
    c.save()
    fault("launch_after_save")
    c.event(t["id"], "launched", **info)
    return info


def find_wrapper(needle):
    """(pid, start) of a live process with needle as one of its arguments
    (wrappers name their private files); None when there is none. Refuses
    when ps cannot be read."""
    p = run(["ps", "-axww", "-o", "pid=,command="])
    if p.returncode != 0:
        raise Refused(f"ps failed: {p.stderr.strip()}")
    for line in p.stdout.splitlines():
        pid, _, cmd = line.strip().partition(" ")
        if needle in cmd:
            start = proc_start(pid)
            if start:
                return int(pid), start
    return None


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
    """running | exited | lost | unstarted. 'lost' means the process is gone
    and left no exit status: interrupted, never success. 'unstarted' is
    proven: the wrapper writes its pid file before the agent runs, and no
    wrapper for this run is alive. A pid found for an unrecorded launch is
    adopted into info (the caller saves it)."""
    run_dir = Path(info["dir"])
    exit_file = run_dir / "exit"
    if exit_file.exists():
        return "exited", int(exit_file.read_text().strip() or 1)
    if info.get("pid") is None:
        found = find_wrapper(str(run_dir / "exit"))
        if found:
            info["pid"], info["start"] = found
            return "running", None
        if exit_file.exists():  # exited between the two checks
            return "exited", int(exit_file.read_text().strip() or 1)
        return ("lost" if (run_dir / "exit.pid").exists() else "unstarted"), None
    start = proc_start(info["pid"])
    if start and start == info["start"]:
        return "running", None
    # Another process with this pid: the pid was reused, which the kernel
    # never does while it names a live group, so ours is gone.
    if start:
        return "lost", None
    # The wrapper is gone, but an agent that survived a signal keeps the
    # group alive: never treat that as stopped.
    if group_alive(info["pid"]):
        return "running", None
    return "lost", None


def slot_path(slot):
    return POOL_ROOT / slot


# ---------------------------------------------------------------- scheduling

def deps_met(c, t):
    return all(c.state["tasks"][d]["status"] == COMPLETED for d in t["depends_on"])


def conflicts(c, t):
    for o in c.state["tasks"].values():
        if o["id"] == t["id"] or o["status"] in (PENDING, COMPLETED) or not scopes_overlap(o["scope"], t["scope"]):
            continue
        # Serialised until the other task's PR is merged, so two open PRs
        # never touch the same files. Two tasks that both already have a PR
        # (adopted PRs, or rework) gain nothing by waiting for each other's
        # merge, which would deadlock their rework; they only never change
        # files at the same time.
        if t.get("pr") and o.get("pr") and o["status"] != RUNNING:
            continue
        return o["id"]
    return None


def start_impl(c, t, findings=""):
    slot, rc, err = c.acquire(t, "auto")
    if not slot:
        if rc == 3:
            return "BLOCKED_POOL_BUSY"
        if t.get("starting"):
            c.block(t, f"acquire outcome unknown: {err}; owner must inspect", INTERRUPTED)
            return None
        c.event(t["id"], "acquire_refused", detail=err)
        return f"acquire refused: {err}"
    return begin_impl(c, t, findings)


def begin_impl(c, t, findings=""):
    slot = t["slot"]
    p = slot_path(slot)
    try:
        if run(["git", "-C", str(p), "rev-parse", "-q", "--verify", f"refs/heads/{t['branch']}"]).returncode == 0:
            git(p, "switch", "-q", t["branch"])
        else:
            # An adopted PR's branch exists only on the remote: continue it.
            remote = f"{REMOTE}/{t['branch']}"
            start = remote if run(["git", "-C", str(p), "rev-parse", "-q", "--verify",
                                   f"refs/remotes/{remote}"]).returncode == 0 else f"{REMOTE}/{t['base']}"
            git(p, "switch", "-q", "-c", t["branch"], "--no-track", start)
    except Refused as e:
        c.block(t, f"cannot check out {t['branch']} in {slot}: {e}; lease kept")
        return None
    t["base_sha"] = git(p, "rev-parse", f"{REMOTE}/{t['base']}")
    t["head"] = t.get("head") or git(p, "rev-parse", "HEAD")  # the agent must add a commit to this
    t["session"] = str(uuid.uuid4())
    prompt = implement_prompt(t, p, findings)
    tools = IMPL_TOOLS + t["allowed_tools"]
    argv = t["command"] if t["agent"] == "command" else ADAPTERS[t["agent"]](prompt, t["session"], tools, False)
    try:
        argv = isolate(t["agent"], argv)
    except Refused as e:
        c.block(t, f"{e}; lease kept")
        return None
    launch(c, t, argv, p, "implement", RUNNING, attempt=t.get("attempt", 0) + 1)
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
        # Validation runs code the agent wrote: same sandbox as the agent.
        sh_cmd = ["/bin/sh", "-c", cmd]
        v = subprocess.run(sandboxed(sh_cmd) or sh_cmd, cwd=p, capture_output=True, text=True)
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
    run(["git", "-C", str(REPO), "fetch", "-q", REMOTE])
    slot, rc, err = c.acquire(t, REVIEW_SLOT, review=True)
    if not slot:
        if t.get("starting"):
            return c.block(t, f"review acquire outcome unknown: {err}; owner must inspect", INTERRUPTED)
        if c.pool_status().get(REVIEW_SLOT, {}).get("state") == "busy":
            return "BLOCKED_REVIEW_BUSY"
        c.event(t["id"], "acquire_refused", detail=err)
        return f"review acquire refused: {err}"
    return begin_review(c, t, pr["headRefOid"])


def begin_review(c, t, head):
    p = slot_path(t["slot"])
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
    try:
        argv = isolate(r["agent"], argv, allow_read=[diff_dir])
    except Refused as e:
        return c.block(t, f"{e}; review lease kept")
    launch(c, t, argv, p, "review", REVIEWING, review_head=head)
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
    try:
        gate_path = c.merge_gate(t["base"])
    except Refused as e:
        return c.block(t, f"merge gate unavailable: {e}")
    gate = run([str(gate_path), "--base", t["base"], str(t["pr"]), head, t["review_file"]], cwd=REPO)
    c.event(t["id"], "merge_gate_verify", exit=gate.returncode, output=(gate.stdout + gate.stderr).strip()[-500:])
    if gate.returncode != 0:
        return c.block(t, f"merge gate refused: {(gate.stdout + gate.stderr).strip()[-300:]}")
    if t["status"] != READY:
        c.set(t, READY, reason=None)
        print(f"{t['id']}: MERGE_READY PR #{t['pr']} at {head[:12]} (owner approval required)")
    return None


# ---------------------------------------------------------------- reconcile

def recover_release(c, t):
    """A crash interrupted release(), so pool.sh may or may not have released
    the slot. The lease decides. Still ours: the intent is dropped and the
    normal checks continue. Released: the recorded next status applies, but
    a released slot proves only the release, so a published head must still
    be the PR's head on GitHub; otherwise the owner decides."""
    if c.lease_held(t):
        t.pop("releasing")
        c.save()
        c.event(t["id"], "release_not_done", slot=t["slot"])
        return
    if t["releasing"]["next"] == AWAITING_REVIEW:
        try:
            pr = c.pr_view(t["pr"])
        except Refused as e:
            return c.block(t, f"{t['slot']} was released but PR #{t['pr']} cannot be read ({e}); owner must inspect",
                           INTERRUPTED)
        if pr["state"] != "OPEN" or pr["headRefOid"] != t["head"]:
            return c.block(t, f"{t['slot']} was released but PR #{t['pr']} is {pr['state']} at "
                              f"{pr['headRefOid'][:12]}, not the published {t['head'][:12]}; owner must inspect",
                           INTERRUPTED)
    c.event(t["id"], "release_recovered", slot=t["slot"])
    c.finish_release(t)


def recover_acquire(c, t):
    """Finishes an acquire transaction a crash or malformed output left open.
    A lease carrying its id with a recorded token is adopted and the task
    continues; no lease carrying it means nothing was leased; anything else
    stays open for the owner."""
    m = t["starting"]
    if not isinstance(m, dict):  # a v1 marker has no transaction id to match
        return c.block(t, f"a previous slot acquisition ({m}) did not complete; "
                          f"check pool.sh status for task={t['id']}", INTERRUPTED)
    slot, rc, err = c.settle_acquire(t)
    if slot:
        c.event(t["id"], "acquire_recovered", slot=slot, txn=m["txn"])
        if slot == REVIEW_SLOT:
            try:
                head = c.pr_view(t["pr"])["headRefOid"]
            except Refused as e:
                return c.block(t, f"{e}; review lease kept", INTERRUPTED)
            return begin_review(c, t, head)
        return begin_impl(c, t, t.get("findings", "") if m["from"] == CHANGES_REQUIRED else "")
    if t.get("starting"):
        return c.block(t, f"acquire transaction {m['txn']} ({m['slot']}) unresolved: {err}; inspect pool.sh "
                          f"status for owner=coordinator:{m['txn']}; no slot is acquired while it is open",
                       INTERRUPTED)
    c.event(t["id"], "acquire_recovered_none", txn=m["txn"], exit=rc)
    if t["status"] != m["from"]:
        c.set(t, m["from"], reason=None)


def reconcile(c):
    """Runs before any new action: completes interrupted release and acquire
    transactions, then matches every task holding a slot against its own
    lease and its process. A lease is never released or reclaimed here."""
    for t in c.state["tasks"].values():
        if t.get("releasing"):
            recover_release(c, t)
    for t in c.state["tasks"].values():
        if t.get("starting"):
            recover_acquire(c, t)
    status = c.pool_status()
    for t in c.state["tasks"].values():
        if t["status"] not in HOLDS_SLOT or t.get("releasing"):
            continue
        if not t.get("slot") or not c.read_token(t):
            c.block(t, "running task has no recorded slot or token", INTERRUPTED)
            continue
        lease = status.get(t["slot"], {})
        ours = (lease.get("owner") == f"coordinator:{t['lease_txn']}" if t.get("lease_txn")
                else lease.get("task") == t["id"])
        if lease.get("state") not in ("busy", "stale-suspected") or not ours:
            c.block(t, f"{t['slot']} is {lease.get('state')} and not leased by this task; owner must inspect",
                    INTERRUPTED)
            continue
        info = t["process"]
        recorded = info.get("pid")
        try:
            st, rc = process_state(info)
        except Refused as e:
            c.block(t, f"cannot determine the agent process ({e}); lease and work kept", INTERRUPTED)
            continue
        if info.get("pid") != recorded:
            c.save()
            c.event(t["id"], "process_adopted", pid=info["pid"], start=info["start"])
        if st == "running":
            continue
        if st == "unstarted":
            c.block(t, f"the agent was never started (coordinator stopped while launching); lease kept in "
                       f"{t['slot']}, nothing relaunched", INTERRUPTED)
            continue
        if t.get("cancel_requested"):
            c.block(t, f"cancelled by owner; agent {st}; lease on {t['slot']} kept, work preserved", INTERRUPTED)
            continue
        if st == "lost":
            c.block(t, f"agent process vanished without an exit status; lease and work kept in {t['slot']}",
                    INTERRUPTED)
            continue
        c.event(t["id"], "exited", exit=rc, kind=info["kind"])
        fault("finish_before_checks")
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
                                                    and t.get("reason", "").startswith(("stale base", "CI not green", "merge gate"))):
            before = t["status"]
            try:
                reconcile_pr(c, t)
            except Refused as e:
                print(f"{t['id']}: {e}")
            changed |= t["status"] != before
    if any(t.get("starting") for t in tasks.values()):
        print("acquire transaction open: no slot is acquired until it is resolved")
        return changed
    for t in tasks.values():
        if any(o.get("starting") for o in tasks.values()):
            break
        if t["status"] == AWAITING_REVIEW and not any(o["status"] == REVIEWING for o in tasks.values()):
            r = start_review(c, t)
            if r:
                busy_note = f"{t['id']}: {r}"
            changed |= t["status"] != AWAITING_REVIEW
    for t in tasks.values():
        if t["status"] not in (PENDING, CHANGES_REQUIRED):
            continue
        if any(o.get("starting") for o in tasks.values()):
            break
        if sum(o["status"] == RUNNING for o in tasks.values()) >= MAX_IMPL:
            busy_note = "BLOCKED_POOL_BUSY"
            break
        if not deps_met(c, t):
            continue
        other = conflicts(c, t)
        if other != t.get("waiting_on"):
            t["waiting_on"] = other
            c.save()
            c.event(t["id"], "waiting_on", other=other)
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
        for k in ("slot", "pr", "waiting_on"):
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
    """Stops the task's agent by SIGTERM to its process group, only when the
    recorded pid still has the recorded start time and leads its own group.
    Reports the truth: if the group survives the wait, cancellation is
    incomplete, the task keeps its status, lease and work, and the slot is
    not reused; reconcile marks it interrupted once the process has exited.
    Never touches the work tree or lease."""
    t = c.state["tasks"].get(args.task)
    if not t:
        raise Refused(f"unknown task {args.task}")
    if t["status"] == COMPLETED:
        raise Refused(f"task {t['id']} is already completed")
    info = t.get("process")
    if t["status"] in HOLDS_SLOT and info:
        st, _ = process_state(info)
        c.save()  # a pid found for an unrecorded launch is kept
        if st == "running":
            pid = info["pid"]
            start = proc_start(pid)
            if start == info["start"]:
                try:
                    verified = os.getpgid(pid) == pid
                except ProcessLookupError:
                    verified = False
            else:
                # Leader gone: the group id cannot have been reused while
                # the group still exists.
                verified = start is None and group_alive(pid)
            if not verified:
                t["cancel_requested"] = True
                c.save()
                raise Refused(f"cannot verify the agent of {t['id']} (pid {pid}); nothing signalled, "
                              "lease and work kept")
            t["cancel_requested"] = True
            c.save()
            os.killpg(pid, signal.SIGTERM)
            c.event(t["id"], "cancel_signal", pid=pid)
            deadline = time.monotonic() + CANCEL_WAIT
            while group_alive(pid) and time.monotonic() < deadline:
                time.sleep(0.1)
            if group_alive(pid):
                c.event(t["id"], "cancel_incomplete", pid=pid)
                print(f"{t['id']}: cancellation incomplete: agent process group {pid} still alive after "
                      f"SIGTERM; lease on {t['slot']} and work kept, slot not reused", file=sys.stderr)
                return 1
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

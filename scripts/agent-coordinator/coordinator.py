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
import functools
import hashlib
import json
import os
import re
import shlex
import signal
import stat
import subprocess
import shutil
import sys
import tempfile
import time
import uuid
from pathlib import Path
from urllib.parse import urlparse

HERE = Path(__file__).resolve().parent
HOME = Path.home()
POOL = Path(os.environ.get("CG_COORD_POOL", HERE.parent / "agent-pool" / "pool.sh"))
STATE_DIR = Path(os.environ.get("CG_COORD_STATE", HOME / "Projekti/codegraph-evidence/coordinator"))
POOL_ROOT = Path(os.environ.get("CG_POOL_ROOT", HOME / "Projekti"))
REPO = Path(os.environ.get("CG_POOL_REPO", HOME / "Projekti/codegraph"))
REMOTE = os.environ.get("CG_POOL_REMOTE", "origin")
# Resolved once, so later lookups never pick up a binary placed on PATH.
GH = shutil.which(os.environ.get("CG_COORD_GH", "gh")) or os.environ.get("CG_COORD_GH", "gh")
GIT = shutil.which("git") or "git"
# Unset: the gate committed on <remote>/<base> is used, never the main
# checkout's copy, which may be stale or modified.
GATE_OVERRIDE = os.environ.get("CG_COORD_GATE")
GATE_PATH = ".github/scripts/merge-gate.sh"
LEASES = Path(os.environ.get("CG_POOL_LEASES", HOME / "Projekti/codegraph-evidence/pool"))
CANCEL_WAIT = float(os.environ.get("CG_COORD_CANCEL_WAIT", "5"))
SANDBOX_EXEC = "/usr/bin/sandbox-exec"
# Files in a slot that tools load as configuration when the owner later
# works there unsandboxed. All are git-ignored or untracked-safe, so the
# scope and dirty checks would not see them.
SLOT_CONFIG = [".claude", ".mcp.json", "CLAUDE.local.md", ".envrc"]
REVIEW_SLOT = "codegraph-review"
WORKERS = ["codegraph-worker-a", "codegraph-worker-b"]
MAX_IMPL = 2
# Present: no agent is launched or continued; running agents are left alone.
STOP_FILE = Path(os.environ.get("CG_COORD_STOP", STATE_DIR / "STOP"))
# Codex agents get a private CODEX_HOME seeded with these; the owner's
# ~/.codex (config, hooks, rules, MCP servers, sessions) is never loaded.
CODEX_AUTH = Path(os.environ.get("CG_COORD_CODEX_AUTH", HOME / ".codex/auth.json"))
CODEX_CONFIG = os.environ.get("CG_COORD_CODEX_CONFIG")
# Unreadable to every agent and validation command: other coordinators' and
# campaigns' state and tokens live under the evidence root.
EVIDENCE = Path(os.environ.get("CG_COORD_EVIDENCE", HOME / "Projekti/codegraph-evidence"))
DENY_READ = [p for p in os.environ.get("CG_COORD_DENY_READ", "").split(":") if p]
# Owner credentials no agent needs. Only denied explicitly in the legacy
# profile Claude Code agents run in; every other sandboxed process reads
# nothing in the home directory that is not on its allow-list.
SECRETS = [".ssh", ".config/gh", ".netrc", ".git-credentials", ".codex", "Library/Keychains"]
# Readable to strict-sandboxed processes: the system, not the home directory.
SYSTEM_READ = ["/System", "/usr", "/bin", "/sbin", "/Library", "/opt", "/Applications", "/private/etc",
               "/private/var/db", "/private/var/select", "/dev"]
# The only environment a sandboxed process inherits; everything else (tokens,
# agent sockets, the owner's settings) stays out. CG_COORD_SANDBOX_ENV adds
# owner-chosen names.
ENV_KEEP = ["PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "SHELL",
            *filter(None, os.environ.get("CG_COORD_SANDBOX_ENV", "").split(":"))]
CLT = Path("/Library/Developer/CommandLineTools")
ALLOW_CLAUDE = os.environ.get("CG_COORD_ALLOW_CLAUDE") == "1"
# Set by the campaign supervisor: a JSON file {"tasks": [ids]} of the tasks
# currently authorized. Unset: every planned task is (owner-driven runs).
AUTH_GATE = os.environ.get("CG_COORD_AUTHORIZED")
MAX_JSON_LINE = 1 << 20
TOOLS = ["codex", "node", "go", "gofmt", "git", "python3", "bash", "sh", "jq", "sed", "awk"]
MAX_ATTEMPTS = 5

PENDING, RUNNING, AWAITING_REVIEW, REVIEWING, CHANGES_REQUIRED = (
    "pending", "running", "awaiting_review", "reviewing", "review_changes_required")
AWAITING_CI, READY, COMPLETED, BLOCKED, FAILED = (
    "awaiting_ci", "ready_for_owner", "completed", "blocked", "failed")
INTERRUPTED = "interrupted"
TERMINAL = {COMPLETED}
HOLDS_SLOT = {RUNNING, REVIEWING}

TASK_FIELDS = {"id", "description", "base", "branch", "depends_on", "agent", "scope",
               "acceptance", "validate", "review", "prompt", "publish", "allowed_tools", "command", "pr",
               "max_attempts"}


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


# Belt and braces with the sandbox: the coordinator never runs hooks or an
# fsmonitor from a slot an agent has worked in.
GIT_SAFE = ["-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"]


def git(slot_path, *args, check=True):
    return run([GIT, *GIT_SAFE, "-C", str(slot_path), *args], check=check).stdout.strip()


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


def owner_writable():
    """CG_COORD_SANDBOX_WRITABLE entries, refused if one would contain a
    protected path or a PATH directory."""
    protected = [STATE_DIR, LEASES, REPO, HOME / ".claude", HOME / ".claude.json", HERE, POOL.parent,
                 HOME / ".gitconfig", HOME / ".config/git", HOME / ".config/gh",
                 *[d for d in os.environ.get("PATH", "").split(":") if d]]
    out = []
    for d in filter(None, os.environ.get("CG_COORD_SANDBOX_WRITABLE", "").split(":")):
        r = os.path.realpath(d)
        bad = [p for p in protected if (os.path.realpath(p) + "/").startswith(r.rstrip("/") + "/")]
        if bad:
            raise Refused(f"CG_COORD_SANDBOX_WRITABLE entry {d} contains protected {bad[0]}")
        out.append(r)
    return out


@functools.lru_cache(maxsize=None)
def tool_dirs():
    """Directories the strict profile lets agents read for their tools: each
    tool's bin directory and, when safe, its install prefix (for codex, the
    Node installation holding its package). A prefix that is the home
    directory, contains it, or holds anything protected is not used;
    CG_COORD_SANDBOX_READABLE adds owner-listed directories under the same
    rule."""
    protected = [HOME, STATE_DIR, LEASES, EVIDENCE, REPO, HERE, *DENY_READ, *[HOME / d for d in SECRETS],
                 HOME / ".local", HOME / ".config", HOME / "Library"]
    home = os.path.realpath(HOME)
    def safe(d):
        r = os.path.realpath(d).rstrip("/") + "/"
        # A directory right in the home directory (~/.local, ~/go, ~/bin)
        # holds more than one tool; never allow it whole.
        if os.path.dirname(r.rstrip("/")) == home:
            return False
        return not any((os.path.realpath(p).rstrip("/") + "/").startswith(r) for p in protected)
    out = []
    for name in TOOLS:
        w = shutil.which(name)
        if not w:
            continue
        for path in {w, os.path.realpath(w)}:
            for d in (Path(path).parent, Path(path).parent.parent):
                if safe(d):
                    out.append(str(d))
    for d in filter(None, os.environ.get("CG_COORD_SANDBOX_READABLE", "").split(":")):
        if not safe(d):
            raise Refused(f"CG_COORD_SANDBOX_READABLE entry {d} contains a protected path")
        out.append(d)
    gomod = os.environ.get("GOMODCACHE") or str(HOME / "go/pkg/mod")
    goroot = run(["go", "env", "GOROOT"]).stdout.strip() if shutil.which("go") else ""
    return tuple(sorted({*out, gomod, *([goroot] if goroot else [])}))


def listening_ports():
    """Ports something on this host already listens on (TCP and UDP),
    read outside the sandbox; None when that cannot be determined. They
    are denied inside: a local service (adb, an API proxy, a file server)
    would hand out what the file and network rules keep back. Ports opened
    later, such as a test's own listener, stay reachable. netstat sees
    every user's sockets, lsof adds what it can name;
    CG_COORD_SANDBOX_LOCAL_PORTS lists ports the owner allows anyway."""
    ports = set()
    for proto, state in (("tcp", "LISTEN"), ("udp", None)):
        p = run(["/usr/sbin/netstat", "-an", "-p", proto])
        if p.returncode != 0:
            return None
        for line in p.stdout.splitlines():
            f = line.split()
            if len(f) >= 4 and f[0].startswith(proto) and (state is None or f[-1] == state):
                m = re.search(r"\.(\d+)$", f[3])
                if m:
                    ports.add(int(m.group(1)))
    p = run(["/usr/sbin/lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-iUDP", "-Fn"])
    if p.returncode not in (0, 1) or (p.returncode == 1 and p.stderr.strip()):
        return None
    ports |= {int(m.group(1)) for line in p.stdout.splitlines() if line.startswith("n")
              for m in [re.search(r":(\d+)$", line.split("->")[0])] if m}
    allowed = {int(x) for x in os.environ.get("CG_COORD_SANDBOX_LOCAL_PORTS", "").split(",") if x.strip().isdigit()}
    return sorted(ports - allowed - {0})


@functools.lru_cache(maxsize=None)
def sdk_root():
    """Resolved outside the sandbox: inside it, cc must not go through xcrun,
    whose lookup cache in the per-user temp directory the owner's own
    builds trust."""
    p = run(["xcrun", "--show-sdk-path"]) if shutil.which("xcrun") else None
    return p.stdout.strip() if p and p.returncode == 0 else ""


def sandboxed(argv, allow_read=(), slot=None, readonly=False, codex=False, keychain=False, cwd=None):
    """(argv, scratch): argv run under sandbox-exec, and the private scratch
    directory the caller removes afterwards; None where no sandbox exists.
    readonly: only the scratch directory and /dev are writable (/tmp too under
    the keychain profile; no slot, no repository). codex: CODEX_HOME is a
    private directory in scratch (see codex_home). keychain: the legacy
    profile for Claude Code agents, which keep their login in the keychain:
    reads are allowed except the protected paths. Everything else runs in the strict profile: reads only of the
    system, its tools, the Go module cache, the slot, the repository's git
    directory, its scratch directory and allow_read; network only to
    localhost and, for codex (its model API), TCP 443."""
    if not sandbox_available():
        return None
    extra = owner_writable()  # may refuse: before anything is created
    if (os.path.realpath(tempfile.gettempdir()) + "/").startswith("/private/tmp/"):
        raise Refused(f"TMPDIR {tempfile.gettempdir()} is under /tmp; scratch directories there would be shared "
                      "with Claude Code agents")
    scratch = new_scratch()
    try:
        return sandbox_argv(argv, scratch, extra, allow_read, slot, readonly, codex, keychain, cwd)
    except BaseException:
        release_scratch(scratch)
        raise


def sandbox_argv(argv, scratch, extra, allow_read, slot, readonly, codex, keychain, cwd):
    def q(p, kind="subpath"):
        p = os.path.realpath(p)
        if '"' in p or "\\" in p:
            raise Refused(f"cannot sandbox path {p!r}")
        return f'({kind} "{p}")'
    def rx(p, tail):
        return f'(regex #"^{re.escape(os.path.realpath(p))}{tail}")'
    # Writes are denied by default: the coordinator, pool.sh and the owner's
    # own tools later run git, gh, Go, Claude Code and their scripts outside
    # the sandbox, so an agent must not change any file they execute or read
    # configuration from. An agent may write its slot, the git objects and
    # refs a commit needs, its own worktree admin files except their config
    # and pointers, a private scratch directory (TMPDIR, Go and XDG caches;
    # the shared Go module cache stays readable) and /dev; /tmp only under the
    # keychain profile, which Claude Code needs. Claude Code's session data is
    # not writable: Claude Code runs without it (verified with 2.1.296), and
    # its shell snapshots, session environments, memory and IDE locks are
    # loaded by the owner's own sessions. Owner-listed directories are added.
    gitdir = REPO / ".git"
    writable = [q("/dev"), q(scratch), *[q(d) for d in extra]]
    if keychain:
        writable.append(q("/private/tmp"))  # Claude Code keeps per-session files there
    if not readonly:
        # Branches only: remote-tracking refs decide which merge gate runs
        # and what pool.sh treats as the base.
        writable += [q(gitdir / "objects"), q(gitdir / "refs/heads"), q(gitdir / "logs/refs/heads")]
    no_write = [q(STATE_DIR), q(LEASES), q(gitdir / "config", "literal"), q(gitdir / "hooks"),
                q(gitdir / "objects/info"),
                rx(gitdir / "worktrees", r"/[^/]+/(config|commondir|gitdir)"), q(HERE), q(POOL.parent)]
    if slot and not readonly:
        writable += [q(slot), q(gitdir / "worktrees" / Path(slot).name)]
        no_write += [q(Path(slot) / ".git", "literal"),
                     rx(slot, "/(.*/)?(" + "|".join(re.escape(f) for f in SLOT_CONFIG) + ")(/|$)")]
    protected = [q(STATE_DIR), q(LEASES), q(EVIDENCE), *[q(d) for d in DENY_READ]]
    # Later rules win: allow-list first, then the denials it must not undo.
    profile = f"(version 1)(allow default)(deny file-write*)(allow file-write* {' '.join(writable)})"
    if keychain:
        profile += (f"(deny file-read-data {' '.join(protected)})"
                    f"(deny file-read* {' '.join(q(HOME / d) for d in SECRETS if d != 'Library/Keychains')})")
    else:
        readable = [q(scratch), *[q(d) for d in [*SYSTEM_READ, *tool_dirs(), *extra]], q(gitdir)]
        if slot:
            readable.append(q(slot))
        profile += (f"(deny file-read*)(allow file-read-metadata)(allow file-read* (literal \"/\") {' '.join(readable)})"
                    # Denying the keychain files keeps TLS trust evaluation
                    # working; denying the security daemon does not.
                    f"(deny file-read* {' '.join(protected)} {q('/Library/Keychains')} "
                    f"{' '.join(q(HOME / d) for d in SECRETS)})"
                    "(deny network-outbound)(allow network-outbound (remote ip \"localhost:*\") "
                    "(remote unix-socket (path-literal \"/private/var/run/mDNSResponder\")))"
                    + ("(allow network-outbound (remote tcp \"*:443\"))" if codex else ""))
        ports = listening_ports()
        if ports is None:  # unknown: no local service either
            profile += '(deny network-outbound (remote ip "localhost:*"))'
        elif ports:
            profile += "(deny network-outbound " + " ".join(f'(remote ip "localhost:{n}")' for n in ports) + ")"
    profile += (f"(deny file-write* {' '.join(no_write)})"
                + "".join(f"(allow file-read-data {q(d)})" for d in allow_read))
    env = [f"{k}={os.environ[k]}" for k in ENV_KEEP if k in os.environ]
    env += [f"{k}={scratch}/{v}" for k, v in (("TMPDIR", ""), ("GOCACHE", "go-build"), ("GOPATH", "gopath"),
                                               ("XDG_CACHE_HOME", "cache"), ("XDG_CONFIG_HOME", "config"),
                                               ("GOENV", "config/go/env"))]
    if cwd or slot:
        env.append(f"PWD={os.path.realpath(cwd or slot)}")  # without it, getcwd needs the parents
    if not keychain:
        # The owner's ~/.gitconfig is unreadable (it may name credential
        # helpers); the repository's own config, with its identity, is not.
        env += ["GIT_CONFIG_GLOBAL=/dev/null"]
        if (CLT / "usr/bin/clang").exists() and sdk_root():
            env += [f"CC={CLT}/usr/bin/clang", f"SDKROOT={sdk_root()}",
                    f"PATH={CLT}/usr/bin:{os.environ.get('PATH', '/usr/bin:/bin')}"]
    if codex:
        env.append(f"CODEX_HOME={scratch}/codex-home")
    gomod = os.environ.get("GOMODCACHE") or str(HOME / "go/pkg/mod")
    return [SANDBOX_EXEC, "-p", profile, "/usr/bin/env", "-i", *env, f"GOMODCACHE={gomod}", *argv], scratch


def isolate(agent, argv, allow_read=(), slot=None, readonly=False, cwd=None):
    """(argv, scratch or None, codex auth digest or None)."""
    if agent == "claude" and not ALLOW_CLAUDE:
        raise Refused("Claude Code agents need the keychain, which also holds the owner's GitHub token; "
                      "set CG_COORD_ALLOW_CLAUDE=1 to run them anyway")
    wrapped = sandboxed(argv, allow_read, slot, readonly, agent == "codex", agent == "claude", cwd)
    if not wrapped:
        if agent != "command":
            raise Refused(f"no sandbox on this platform; refusing to run a {agent} agent that could read lease tokens")
        return argv, None, None
    if agent != "codex":
        return *wrapped, None
    try:
        return *wrapped, codex_home(Path(wrapped[1]) / "codex-home")
    except OSError as e:
        release_scratch(wrapped[1])
        raise Refused(f"cannot prepare CODEX_HOME: {e}")


# Scratch directories hold copied Codex logins. Each lives in a private root
# keyed by the state directory, next to a lock file its creator (and, after
# launch, the agent's process tree) holds, so a restarted coordinator can
# tell its own abandoned scratch from anything else and from live agents'.
_scratch_locks = {}


def scratch_root():
    key = hashlib.sha256(os.path.realpath(STATE_DIR).encode()).hexdigest()[:12]
    root = Path(tempfile.gettempdir()) / f"cg-coord-{key}"
    try:
        root.mkdir(mode=0o700)
    except FileExistsError:
        pass
    st = os.lstat(root)
    if not stat.S_ISDIR(st.st_mode) or st.st_uid != os.getuid() or st.st_mode & 0o077:
        raise Refused(f"{root} is not a private directory of this user; no scratch is created or removed there")
    return root


def new_scratch():
    """The lock file comes first and stays locked, so a sweep never sees the
    directory unowned."""
    root = scratch_root()
    while True:
        fd, lock = tempfile.mkstemp(prefix="cg-agent-", suffix=".lock", dir=root)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            # A sweep may have unlinked the lock before it was locked.
            try:
                ours = os.path.samestat(os.fstat(fd), os.lstat(lock))
            except FileNotFoundError:
                ours = False
            if not ours:
                os.close(fd)
                continue
            scratch = lock[:-len(".lock")]
            os.mkdir(scratch, 0o700)
        except BaseException:
            try:
                os.unlink(lock)
            except FileNotFoundError:
                pass
            os.close(fd)
            raise
        _scratch_locks[scratch] = fd
        return scratch


def scratch_fds(scratch):
    """pass_fds for a child that should hold scratch's lock while it lives."""
    fd = _scratch_locks.get(scratch)
    return (fd,) if fd is not None else ()


def release_scratch(scratch):
    shutil.rmtree(scratch, ignore_errors=True)
    if not os.path.exists(scratch):
        try:
            os.unlink(f"{scratch}.lock")
        except FileNotFoundError:
            pass
    fd = _scratch_locks.pop(scratch, None)
    if fd is not None:
        os.close(fd)


def sweep_scratch(referenced):
    """Removes this coordinator's scratch directories that nothing owns any
    more (a crash or kill came between their creation and their removal).
    Kept: any a task records (reconcile decides those). Kept and reported:
    any whose lock is held or missing, any a live process names in its arguments
    or has open (lsof; macOS hides other processes' environment), any lsof
    cannot vouch for, and anything not made by new_scratch. Refuses without
    a process list. Returns (removed, kept) names."""
    root = scratch_root()
    p = run(["ps", "-axww", "-o", "command="])
    if p.returncode != 0:
        raise Refused(f"ps failed: {p.stderr.strip()}")
    removed, kept = [], []
    for e in sorted(root.iterdir()):
        if e.name.endswith(".lock") and re.fullmatch(r"cg-agent-\w+", e.name[:-5]):
            if not e.with_suffix("").exists() and os.path.lexists(e):
                fd = os.open(e, os.O_RDONLY | os.O_NOFOLLOW)
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    e.unlink()
                except BlockingIOError:
                    pass
                finally:
                    os.close(fd)
            continue
        st = os.lstat(e)
        lock = e.with_name(e.name + ".lock")
        if os.path.realpath(e) in referenced:
            continue
        if (not re.fullmatch(r"cg-agent-\w+", e.name) or not stat.S_ISDIR(st.st_mode)
                or st.st_uid != os.getuid() or os.path.realpath(e) in referenced or not lock.exists()
                or any(n in p.stdout for n in {str(e), os.path.realpath(e)})):
            kept.append(e.name)
            continue
        fd = os.open(lock, os.O_RDONLY | os.O_NOFOLLOW)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            os.close(fd)
            kept.append(e.name)
            continue
        try:
            busy = run(["lsof", "-nP", "-t", "+D", str(e)])  # 1 and silent: nothing open (a warning keeps it)
            if busy.returncode != 1 or busy.stdout.strip() or busy.stderr.strip():
                kept.append(e.name)
                continue
            release_scratch(str(e))
        finally:
            os.close(fd)
        (kept if e.exists() else removed).append(e.name)
    return removed, kept


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def codex_home(home):
    """Seeds a private CODEX_HOME with the owner's login (and an optional
    owner-written config.toml). Returns the login's digest, which
    codex_login_note compares afterwards."""
    home.mkdir(mode=0o700)
    shutil.copyfile(CODEX_AUTH, home / "auth.json")
    os.chmod(home / "auth.json", 0o600)
    if CODEX_CONFIG:
        shutil.copyfile(CODEX_CONFIG, home / "config.toml")
    return sha256(CODEX_AUTH)


def codex_login_note(info):
    """The copy is never written back: an agent controls it. If Codex
    refreshed it during the run, the owner is told, because a refresh may
    rotate the token the owner's own copy holds."""
    digest = info.get("codex_auth")
    new = Path(info["scratch"]) / "codex-home/auth.json"
    try:
        if digest and new.exists() and sha256(new) != digest:
            return ("Codex changed its login copy during the run (token refresh?); not copied back. If the "
                    "owner's codex later reports an invalid login, run `codex login`.")
    except OSError:
        pass
    return None


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

def load_tasks(path, known=()):
    """Validated tasks from path. Dependencies may also name tasks in known
    (already planned)."""
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
        if t.get("agent", "codex") not in ADAPTERS:
            raise Refused(f"task {tid}: agent must be one of {sorted(ADAPTERS)}")
        if t.get("agent") == "command" and not isinstance(t.get("command"), list):
            raise Refused(f"task {tid}: agent \"command\" needs a \"command\" argv list")
        review = t.get("review", {})
        if review.get("agent", "codex") not in ADAPTERS:
            raise Refused(f"task {tid}: review agent must be one of {sorted(ADAPTERS)}")
        attempts = t.get("max_attempts", 1)
        if type(attempts) is not int or not 1 <= attempts <= MAX_ATTEMPTS:
            raise Refused(f"task {tid}: max_attempts must be an integer from 1 to {MAX_ATTEMPTS}")
        out[tid] = {
            "id": tid,
            "description": t["description"],
            "base": t.get("base", "v2.0"),
            "branch": t.get("branch", f"task/{tid}"),
            "depends_on": list(t.get("depends_on", [])),
            "agent": t.get("agent", "codex"),
            "command": t.get("command"),
            "scope": list(t["scope"]),
            "acceptance": list(t["acceptance"]),
            "validate": list(t["validate"]),
            "review": {"agent": review.get("agent", "codex"), "command": review.get("command"),
                       "focus": review.get("focus", "")},
            "prompt": t.get("prompt", ""),
            "publish": t.get("publish", True),
            "allowed_tools": list(t.get("allowed_tools", [])),
            "pr": t.get("pr"),
            "max_attempts": attempts,
        }
    for tid, t in out.items():
        for d in t["depends_on"]:
            if d not in out and d not in known:
                raise Refused(f"task {tid} depends on unknown task {d}")
    seen, done = set(), set()

    def visit(n):
        if n in done:
            return
        if n in seen:
            raise Refused(f"dependency cycle through {n}")
        seen.add(n)
        for d in out[n]["depends_on"]:
            if d in out:
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

def implement_prompt(task, slot_path, findings="", previous=""):
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
    if previous:
        lines += ["", "A previous attempt ended without passing the coordinator's checks:", previous,
                  "Its work is still in this directory. Continue from the current state; the task is",
                  "complete only when the checks pass, not when you say so."]
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
    # codex exec's own Seatbelt sandbox cannot nest inside ours, so it is
    # switched off; this argv is only ever run under sandboxed() (isolate
    # refuses codex where no sandbox exists), which is the boundary:
    # writable slot for implementers, nothing but scratch for reviewers.
    return ["codex", "exec", "--json", "--ephemeral", "--dangerously-bypass-approvals-and-sandbox", prompt]


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
        self.scratch_kept = []

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
        except (ValueError, AssertionError, AttributeError, RecursionError) as e:
            raise Refused(f"{self.state_file} is malformed ({type(e).__name__}); left untouched")

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
        slots = WORKERS if m["slot"] == "auto" else [m["slot"]]
        locked = not files["rc"].exists() and any((LEASES / f".{s}.lock").exists() for s in slots)
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
            # Read from a private bare repository in the state directory,
            # which agents can neither read nor write: the shared object store
            # is writable to them, and git trusts objects it already has.
            url, store = self.private_store()
            run([GIT, *GIT_SAFE, "-C", str(store), "fetch", "-q", "--no-tags", url,
                 f"+refs/heads/{base}:refs/heads/{base}"], check=True)
            remote_sha = run([GIT, *GIT_SAFE, "ls-remote", url, f"refs/heads/{base}"],
                             check=True).stdout.split("\t")[0].strip()
            local_sha = run([GIT, *GIT_SAFE, "-C", str(store), "rev-parse", "-q", "--verify",
                             f"refs/heads/{base}"]).stdout.strip()
            if not remote_sha or remote_sha != local_sha:
                raise Refused(f"{base} moved during the gate fetch ({local_sha[:12]} vs {remote_sha[:12]})")
            blob = run([GIT, *GIT_SAFE, "-C", str(store), "rev-parse", "-q", "--verify",
                        f"{remote_sha}:{GATE_PATH}"]).stdout.strip()
            if not blob:
                raise Refused(f"no {GATE_PATH} on {REMOTE}/{base}")
            (self.dir / "gates").mkdir(mode=0o700, exist_ok=True)
            gate = self.dir / "gates" / f"{blob}.sh"
            write_atomic(gate, run([GIT, *GIT_SAFE, "-C", str(store), "cat-file", "blob", blob], check=True).stdout, 0o700)
        if not os.access(gate, os.X_OK):
            raise Refused(f"{gate} is not an executable file")
        probe = run([str(gate)], cwd=REPO)
        if probe.returncode != 2 or "--base" not in probe.stdout + probe.stderr:
            raise Refused(f"{gate} does not support --base")
        return gate

    # ------------------------------------------------------------ verified content
    # Agents can write the shared object store, and git does not re-hash
    # objects it already has, so a checkout from it may differ from what the
    # remote holds at the same SHA. What is reviewed and tested is therefore
    # checked against a private repository fetched from the remote.

    def private_store(self):
        url = run([GIT, *GIT_SAFE, "-C", str(REPO), "remote", "get-url", REMOTE], check=True).stdout.strip()
        store = self.dir / "gate-repo"
        if not (store / "HEAD").exists():
            run([GIT, "init", "-q", "--bare", str(store)], check=True)
        return url, store

    def fetch_verified(self, branch, sha):
        """Fetches the remote branch into the private repository; refuses
        unless it is exactly sha."""
        url, store = self.private_store()
        run([GIT, *GIT_SAFE, "-C", str(store), "fetch", "-q", "--no-tags", url,
             f"+refs/heads/{branch}:refs/verify/{branch}"], check=True)
        got = run([GIT, *GIT_SAFE, "-C", str(store), "rev-parse", "-q", "--verify", f"refs/verify/{branch}"]).stdout.strip()
        if got != sha:
            raise Refused(f"remote {branch} is {got[:12] or 'missing'}, not {sha[:12]}")
        return store

    def worktree_matches(self, path, sha):
        """True when the files in path are exactly the tree of sha in the
        private repository: tracked content and modes, and no other file,
        ignored ones included (Go compiles ignored directories too)."""
        _, store = self.private_store()
        idx = self.dir / f".verify-{uuid.uuid4().hex}.index"
        env = {**os.environ, "GIT_INDEX_FILE": str(idx)}
        g = [GIT, *GIT_SAFE, "--git-dir", str(store), "--work-tree", str(path)]
        try:
            run([*g, "read-tree", sha], cwd=path, env=env, check=True)
            run([*g, "add", "-A", "--force", "."], cwd=path, env=env, check=True)
            tree = run([*g, "write-tree"], cwd=path, env=env, check=True).stdout.strip()
        finally:
            idx.unlink(missing_ok=True)
        return tree == run([*g, "rev-parse", f"{sha}^{{tree}}"], check=True).stdout.strip()

    # ------------------------------------------------------------ GitHub

    def gh(self, *args):
        p = run([GH, *args], cwd=REPO)
        if p.returncode != 0:
            raise Refused(f"gh {' '.join(args[:2])} failed: {p.stderr.strip()}")
        return p.stdout

    def pr_view(self, pr):
        return json.loads(self.gh("pr", "view", str(pr), "--json",
                                  "number,state,headRefOid,headRefName,baseRefName,statusCheckRollup,headRepository"))

    def remote_repository_name(self):
        configured = os.environ.get("CG_COORD_REPOSITORY_NAME")
        if configured:
            return configured.casefold()
        url = run([GIT, *GIT_SAFE, "-C", str(REPO), "remote", "get-url", REMOTE], check=True).stdout.strip()
        name = urlparse(url).path.lstrip("/") if "://" in url else url.split(":", 1)[-1]
        return name.removesuffix(".git").casefold()

    def verify_pr_head_repository(self, pr):
        repository = pr.get("headRepository") or {}
        name = repository.get("nameWithOwner", "") if isinstance(repository, dict) else ""
        if not name or name.casefold() != self.remote_repository_name():
            raise Refused("fork pull requests are unsupported by review orchestration")

    def find_pr(self, branch):
        prs = json.loads(self.gh("pr", "list", "--head", branch, "--state", "all", "--json", "number,state"))
        open_prs = [p for p in prs if p["state"] == "OPEN"]
        return open_prs[0]["number"] if open_prs else None


# ---------------------------------------------------------------- processes

def launch(c, t, argv, cwd, kind, status, scratch=None, codex_auth=None, **fields):
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
    info = {"pid": None, "start": None, "dir": str(run_dir), "cwd": str(cwd), "kind": kind, "argv0": argv[0],
            "scratch": scratch, "codex_auth": codex_auth}
    fault("launch_before_record")
    c.set(t, status, process=info, **fields)
    fault("launch_before_popen")
    with open(run_dir / "output.log", "w") as log:
        # The agent's process tree inherits the scratch lock: held while any of it lives.
        proc = subprocess.Popen(wrapper, cwd=cwd, stdout=log, stderr=subprocess.STDOUT,
                                stdin=subprocess.DEVNULL, start_new_session=True, pass_fds=scratch_fds(scratch))
    if scratch in _scratch_locks:
        os.close(_scratch_locks.pop(scratch))
    fault("launch_after_popen")
    start = None
    for _ in range(3):  # ps can fail transiently under load
        start = proc_start(proc.pid)
        if start or (exit_file).exists():
            break
        time.sleep(0.1)
    info.update(pid=proc.pid, start=start)
    fault("launch_before_save")
    c.save()
    fault("launch_after_save")
    c.event(t["id"], "launched", **info)
    return info


def drop_scratch(info):
    """Removes a run's scratch directory once its agent is known gone."""
    if info.get("scratch"):
        note = codex_login_note(info)
        if note:
            print(note)
        release_scratch(info["scratch"])


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
        rec = parse_json(line)
        if not isinstance(rec, dict):
            continue
        item = rec.get("item")
        item = item if isinstance(item, dict) else {}
        found = None
        if agent == "claude" and rec.get("type") == "result":
            found = rec.get("result")
        if agent == "codex" and rec.get("type") == "item.completed" and item.get("type") == "agent_message":
            found = item.get("text")
        if found is not None:
            text = found if isinstance(found, str) else ""
    return text


def parse_json(text, limit=MAX_JSON_LINE):
    """JSON from untrusted output, or None: oversized, malformed and too
    deeply nested input are all just not JSON."""
    if len(text) > limit:
        return None
    try:
        return json.loads(text)
    except (ValueError, RecursionError):
        return None


def exited_or_lost(exit_file):
    """The wrapper writes the exit file just before it exits, so a process
    found gone after the first check may have left one meanwhile."""
    if exit_file.exists():
        return "exited", int(exit_file.read_text().strip() or 1)
    return "lost", None


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
        pid_file = run_dir / "exit.pid"
        if not pid_file.exists():
            return "unstarted", None
        pid = int(pid_file.read_text().strip())
        if group_alive(pid):  # wrapper gone, agent alive: adopt the group
            info["pid"], info["start"] = pid, None
            return "running", None
        return exited_or_lost(exit_file)
    start = proc_start(info["pid"])
    if start and start == info["start"]:
        return "running", None
    # Another process with this pid: the pid was reused, which the kernel
    # never does while it names a live group, so ours is gone. Without a
    # recorded start (ps failed at launch, or an adopted group) the group
    # decides, conservatively.
    if start and info["start"]:
        return exited_or_lost(exit_file)
    # The wrapper is gone, but an agent that survived a signal keeps the
    # group alive: never treat that as stopped.
    if group_alive(info["pid"]):
        return "running", None
    return exited_or_lost(exit_file)


def slot_path(slot):
    return POOL_ROOT / slot


# ---------------------------------------------------------------- scheduling

def authorized(c, t, action):
    """True when the campaign's gate (if any) authorizes t now. Fails closed:
    an unreadable or malformed gate authorizes nothing. A refusal is
    recorded once per task and action."""
    if not AUTH_GATE:
        return True
    try:
        data = parse_json(Path(AUTH_GATE).read_text())
        ok = isinstance(data, dict) and isinstance(data.get("tasks"), list) and t["id"] in data["tasks"]
    except OSError:
        ok = False
    if not ok and t.get("deauthorized") != action:
        t["deauthorized"] = action
        c.save()
        c.event(t["id"], "deauthorized", action=action)
    elif ok and t.get("deauthorized"):
        t.pop("deauthorized")
        c.save()
    return ok


DEAUTHORIZED = "deauthorized"


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
    t["tries"] = 0
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


def begin_impl(c, t, findings="", previous=""):
    slot = t["slot"]
    p = slot_path(slot)
    try:
        if run([GIT, *GIT_SAFE, "-C", str(p), "rev-parse", "-q", "--verify", f"refs/heads/{t['branch']}"]).returncode == 0:
            git(p, "switch", "-q", t["branch"])
            # The PR branch may have moved on the remote (base merged in on
            # GitHub); continue from there, never from behind it.
            remote = f"refs/remotes/{REMOTE}/{t['branch']}"
            if run([GIT, *GIT_SAFE, "-C", str(p), "rev-parse", "-q", "--verify", remote]).returncode == 0:
                before = git(p, "rev-parse", "HEAD")
                git(p, "merge", "-q", "--ff-only", remote)
                if git(p, "rev-parse", "HEAD") != before:
                    t["head"] = git(p, "rev-parse", "HEAD")  # the agent must add a commit to this
        else:
            # An adopted PR's branch exists only on the remote: continue it.
            remote = f"{REMOTE}/{t['branch']}"
            start = remote if run([GIT, *GIT_SAFE, "-C", str(p), "rev-parse", "-q", "--verify",
                                   f"refs/remotes/{remote}"]).returncode == 0 else f"{REMOTE}/{t['base']}"
            git(p, "switch", "-q", "-c", t["branch"], "--no-track", start)
    except Refused as e:
        c.block(t, f"cannot check out {t['branch']} in {slot}: {e}; lease kept")
        return None
    t["base_sha"] = git(p, "rev-parse", f"{REMOTE}/{t['base']}")
    t["head"] = t.get("head") or git(p, "rev-parse", "HEAD")  # the agent must add a commit to this
    t["session"] = str(uuid.uuid4())
    t["impl_findings"] = findings
    prompt = implement_prompt(t, p, findings, previous)
    tools = IMPL_TOOLS + t["allowed_tools"]
    argv = t["command"] if t["agent"] == "command" else ADAPTERS[t["agent"]](prompt, t["session"], tools, False)
    try:
        argv, scratch, auth = isolate(t["agent"], argv, slot=p)
    except Refused as e:
        c.block(t, f"{e}; lease kept")
        return None
    launch(c, t, argv, p, "implement", RUNNING, scratch, auth, attempt=t.get("attempt", 0) + 1,
           tries=t.get("tries", 0) + 1)
    print(f"{t['id']}: running in {slot}")
    return None


def planted_config(p):
    """Ignored files in the slot that tools load as configuration."""
    return [l[3:] for l in git(p, "status", "--porcelain", "--ignored", "--untracked-files=all").splitlines()
            if l.startswith("!! ") and set(l[3:].rstrip("/").split("/")) & set(SLOT_CONFIG)]


def unfinished(c, t, reason):
    """An agent that stopped short of the checks is continued in the same
    slot with the reason, up to max_attempts executions; then, or under
    STOP, the task fails with its lease and work kept."""
    if t.get("tries", 1) >= t.get("max_attempts", 1):
        return c.block(t, reason, FAILED)
    if STOP_FILE.exists():
        return c.block(t, f"{reason}; not continued: {STOP_FILE} present", INTERRUPTED)
    if not authorized(c, t, "continue"):
        return c.block(t, f"{DEAUTHORIZED}: not continued ({reason}); lease and work kept")
    c.event(t["id"], "continue", reason=reason, tries=t.get("tries", 1))
    print(f"{t['id']}: continuing ({t.get('tries', 1) + 1}/{t['max_attempts']}): {reason}")
    return begin_impl(c, t, t.get("impl_findings", ""), reason)


def finish_impl(c, t, rc):
    """Never trusts the exit code alone: the tree must be clean, the branch
    must hold new commits, every changed file must be in scope, and every
    validation command must pass. Any failure keeps the lease and the work."""
    p = slot_path(t["slot"])
    evidence = Path(t["process"]["dir"])
    # Never continued, whatever else went wrong.
    planted = planted_config(p)
    if planted:
        return c.block(t, f"ignored tool configuration in {t['slot']}: {planted[:5]}; lease kept", FAILED)
    if git(p, "symbolic-ref", "--short", "HEAD", check=False) != t["branch"]:
        return c.block(t, f"agent left {t['slot']} off branch {t['branch']}; lease kept", FAILED)
    if rc != 0:
        return unfinished(c, t, f"agent exited {rc}; work and lease kept in {t['slot']}")
    if git(p, "status", "--porcelain", "--untracked-files=all"):
        return unfinished(c, t, f"agent left {t['slot']} dirty; lease kept")
    head = git(p, "rev-parse", "HEAD")
    if head == t.get("head") or git(p, "rev-list", "--count", f"{t['base_sha']}..HEAD") == "0":
        return unfinished(c, t, "agent produced no new commit; lease kept")
    changed = git(p, "diff", "--no-renames", "--name-only", f"{t['base_sha']}...HEAD").splitlines()
    outside = [f for f in changed if not in_scope(f, t["scope"])]
    if outside:
        return unfinished(c, t, f"changes outside scope: {outside[:5]}; lease kept")
    for i, cmd in enumerate(t["validate"]):
        # Validation runs code the agent wrote: same sandbox as the agent.
        argv, scratch = sandboxed(["/bin/sh", "-c", cmd], slot=p) or (["/bin/sh", "-c", cmd], None)
        try:
            v = subprocess.run(argv, cwd=p, capture_output=True, text=True, pass_fds=scratch_fds(scratch))
        finally:
            if scratch:
                release_scratch(scratch)
        (evidence / f"validate-{i}.log").write_text(f"$ {cmd}\nexit {v.returncode}\n{v.stdout}{v.stderr}")
        c.event(t["id"], "validated", command=cmd, exit=v.returncode, log=str(evidence / f"validate-{i}.log"))
        if v.returncode != 0:
            return unfinished(c, t, f"validation failed: {cmd}; lease kept")
    if git(p, "status", "--porcelain", "--untracked-files=all"):
        return unfinished(c, t, "validation left the tree dirty; lease kept")
    t["head"] = head
    if not t["publish"]:
        return c.block(t, f"validated at {head[:12]}; publish disabled, lease kept for the owner")
    push = run([GIT, *GIT_SAFE, "-C", str(p), "push", "-q", REMOTE, f"{t['branch']}:refs/heads/{t['branch']}"])
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
    try:
        c.verify_pr_head_repository(pr)
    except Refused as e:
        return f"PR head not verifiable yet: {e}"
    run([GIT, *GIT_SAFE, "-C", str(REPO), "fetch", "-q", REMOTE])
    try:
        c.fetch_verified(pr["headRefName"], pr["headRefOid"])
    except Refused as e:
        return f"PR head not verifiable yet: {e}"
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
        token = c.read_token(t)
        if not token:
            raise Refused("review lease token missing")
        checkout = c.pool("review-checkout", "--slot", t["slot"], "--token", token,
                          "--branch", c.pr_view(t["pr"])["headRefName"], "--head", head)
        if checkout.returncode != 0:
            raise Refused(checkout.stderr.strip() or checkout.stdout.strip() or
                          "pool.sh review-checkout refused")
        branch = c.pr_view(t["pr"])["headRefName"]
        store = c.fetch_verified(branch, head)
        if not c.worktree_matches(p, head):
            c.event(t["id"], "checkout_mismatch", slot=t["slot"], head=head)
            leftovers = git(p, "status", "--porcelain", "--ignored", "--untracked-files=all")
            why = (f"untracked or ignored files are in the review slot: {leftovers.splitlines()[:3]}" if leftovers
                   else "the shared object store may have been altered")
            return c.block(t, f"the review checkout of {head[:12]} differs from the remote's content ({why}); "
                              "review lease kept", FAILED)
        url = run([GIT, *GIT_SAFE, "-C", str(REPO), "remote", "get-url", REMOTE], check=True).stdout.strip()
        run([GIT, *GIT_SAFE, "-C", str(store), "fetch", "-q", "--no-tags", url,
             f"+refs/heads/{t['base']}:refs/verify-base/{t['base']}"], check=True)
        diff = run([GIT, *GIT_SAFE, "-C", str(store), "diff", f"refs/verify-base/{t['base']}...{head}"],
                   check=True).stdout
    except Refused as e:
        return c.block(t, f"review checkout of {head[:12]} failed: {e}; review lease kept")
    t["review_session"] = str(uuid.uuid4())
    diff_dir = c.runs / f"{t['id']}-diff-{head[:12]}"
    diff_dir.mkdir(mode=0o700, exist_ok=True)
    diff_file = diff_dir / "pr.diff"
    write_atomic(diff_file, diff + "\n")
    prompt = review_prompt(t, t["pr"], head, t["base"], diff_file)
    r = t["review"]
    argv = r["command"] if r["agent"] == "command" else ADAPTERS[r["agent"]](
        prompt, t["review_session"], READ_ONLY_TOOLS, True, [diff_dir])
    try:
        argv, scratch, auth = isolate(r["agent"], argv, allow_read=[diff_dir], slot=p, readonly=True)
    except Refused as e:
        return c.block(t, f"{e}; review lease kept")
    launch(c, t, argv, p, "review", REVIEWING, scratch, auth, review_head=head)
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
    planted = planted_config(p)
    if planted:
        return c.block(t, f"ignored tool configuration in the review slot: {planted[:5]}; review "
                          "invalid, lease kept", FAILED)
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
    run([GIT, *GIT_SAFE, "-C", str(REPO), "fetch", "-q", REMOTE])
    base = run([GIT, *GIT_SAFE, "-C", str(REPO), "rev-parse", f"{REMOTE}/{t['base']}"]).stdout.strip()
    if run([GIT, *GIT_SAFE, "-C", str(REPO), "merge-base", "--is-ancestor", base, head]).returncode != 0:
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
    if slot and STOP_FILE.exists():
        return c.block(t, f"acquired {slot} before a stop; not launched: {STOP_FILE} present, lease kept",
                       INTERRUPTED)
    if slot and not authorized(c, t, "launch"):
        return c.block(t, f"{DEAUTHORIZED}: acquired {slot} but not launched; lease kept")
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
            drop_scratch(info)
            c.block(t, f"the agent was never started (coordinator stopped while launching); lease kept in "
                       f"{t['slot']}, nothing relaunched", INTERRUPTED)
            continue
        if t.get("cancel_requested"):
            drop_scratch(info)
            c.block(t, f"cancelled by owner; agent {st}; lease on {t['slot']} kept, work preserved", INTERRUPTED)
            continue
        if st == "lost":
            run_dir = Path(info["dir"])
            c.event(t["id"], "process_lost", pid=info.get("pid"), start=info.get("start"),
                    start_now=proc_start(info["pid"]) if info.get("pid") else None,
                    pid_file=(run_dir / "exit.pid").exists(), exit_file=(run_dir / "exit").exists())
            drop_scratch(info)
            c.block(t, f"agent process vanished without an exit status; lease and work kept in {t['slot']}",
                    INTERRUPTED)
            continue
        c.event(t["id"], "exited", exit=rc, kind=info["kind"])
        drop_scratch(info)
        fault("finish_before_checks")
        try:
            (finish_impl if t["status"] == RUNNING else finish_review)(c, t, rc)
        except Refused as e:
            c.block(t, f"post-run check failed: {e}; lease kept", FAILED)
    referenced = {os.path.realpath(t["process"]["scratch"]) for t in c.state["tasks"].values()
                  if isinstance(t.get("process"), dict) and t["process"].get("scratch")}
    try:
        removed, kept = sweep_scratch(referenced)
    except (Refused, OSError) as e:
        c.event(None, "scratch_sweep_refused", detail=str(e))
    else:
        if removed or kept != c.scratch_kept:
            c.event(None, "scratch_swept", removed=removed, kept=kept)
        c.scratch_kept = kept


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
    if STOP_FILE.exists():
        print(f"STOP: {STOP_FILE} present; no agent is launched")
        return changed
    for t in tasks.values():
        if any(o.get("starting") for o in tasks.values()):
            break
        if t["status"] == AWAITING_REVIEW and not any(o["status"] == REVIEWING for o in tasks.values()):
            if not authorized(c, t, "review"):
                continue
            r = start_review(c, t)
            if r:
                busy_note = f"{t['id']}: {r}"
            changed |= t["status"] != AWAITING_REVIEW
    for t in tasks.values():
        if t["status"] == BLOCKED and (t.get("reason") or "").startswith(DEAUTHORIZED) and t.get("slot") \
                and authorized(c, t, "resume"):
            if not c.read_token(t) or not c.lease_held(t):
                c.block(t, f"reauthorized, but its lease on {t['slot']} is gone; owner must inspect", INTERRUPTED)
                continue
            c.event(t["id"], "reauthorized", slot=t["slot"])
            if t["slot"] == REVIEW_SLOT:
                try:
                    begin_review(c, t, c.pr_view(t["pr"])["headRefOid"])
                except Refused as e:
                    print(f"{t['id']}: {e}")
            else:
                begin_impl(c, t, t.get("impl_findings", ""), t["reason"])
            changed = True
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
        if not authorized(c, t, "implement"):
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
    existing = c.state["tasks"]
    tasks = load_tasks(args.tasks, existing)
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
    c.save()  # the events a pass writes are never left without a state file
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
    if info and process_state(info)[0] != "running":
        drop_scratch(info)
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

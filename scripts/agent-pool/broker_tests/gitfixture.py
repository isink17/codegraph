"""Disposable Git repositories for the observer tests, and two oracles.

Everything lives under one private temporary directory that must not resolve
into the CodeGraph checkouts or the evidence tree (where the live pool lives).
Git runs here only as a test oracle, inside these fixtures, with no system or
global config, no hooks, no credential helper, no network protocol and no
optional index refresh."""

import hashlib
import os
import shutil
import stat
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from broker import indexverify  # noqa: E402

GIT = shutil.which("git")
_HOME = Path.home().resolve()
PROTECTED = tuple(_HOME / "Projekti" / n for n in (
    "codegraph", "codegraph-worker-a", "codegraph-worker-b", "codegraph-review", "codegraph-evidence"))
HAS_BIRTHTIME = hasattr(os.stat("/"), "st_birthtime")
EMPTY_TREE = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
SAFE = ("-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-c", "protocol.allow=never",
        "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "core.fsmonitor=false",
        "-c", "init.defaultBranch=main", "-c", "advice.detachedHead=false")


def guard(path):
    p = Path(path).resolve()
    for root in PROTECTED:
        if p == root or root in p.parents:
            raise RuntimeError(f"refusing protected path {p}")
    return p


class Lab:
    def __init__(self):
        self.root = guard(tempfile.mkdtemp(prefix="o5-observer-"))
        os.chmod(self.root, 0o700)
        home = self.root / "home"
        home.mkdir()
        self.env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(home),
                    "XDG_CONFIG_HOME": str(home), "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                    "GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0", "LC_ALL": "C",
                    "GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@invalid",
                    "GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@invalid",
                    "GIT_AUTHOR_DATE": "1762819200 +0000", "GIT_COMMITTER_DATE": "1762819200 +0000"}

    def cleanup(self):
        os.chmod(self.root, 0o700)
        for d, subdirs, _ in os.walk(self.root):
            for n in subdirs:
                if not os.path.islink(os.path.join(d, n)):
                    os.chmod(os.path.join(d, n), 0o700)
        shutil.rmtree(self.root)

    def path(self, name):
        return str(guard(self.root / name))

    def git(self, cwd, *args, check=True):
        guard(cwd)
        p = subprocess.run([GIT, *SAFE, *args], cwd=cwd, env=self.env, capture_output=True, timeout=120)
        if check and p.returncode:
            raise AssertionError(f"git {args}: {p.stderr.decode(errors='replace')}")
        return p

    def write(self, repo, rel, data, mode=0o644):
        p = Path(repo) / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        if p.is_symlink() or p.exists():
            p.unlink()
        p.write_bytes(data)
        os.chmod(p, mode)

    def link(self, repo, rel, target):
        p = Path(repo) / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        if p.is_symlink() or p.exists():
            p.unlink()
        os.symlink(target, p)

    def repo(self, name="repo", files=None, commit=True):
        """files: {rel: bytes | ("exec", bytes) | ("link", target)}."""
        r = self.path(name)
        os.mkdir(r)
        self.git(r, "init", "-q")
        for rel, spec in (files or {"README": b"hello\n"}).items():
            if isinstance(spec, tuple) and spec[0] == "link":
                self.link(r, rel, spec[1])
            elif isinstance(spec, tuple):
                self.write(r, rel, spec[1], 0o755)
            else:
                self.write(r, rel, spec)
        if commit:
            self.git(r, "add", "-A")
            self.git(r, "commit", "-q", "-m", "fixture")
        return r

    def ctx(self, repo, **kw):
        kw.setdefault("require_birthtime", HAS_BIRTHTIME)
        return indexverify.Context(repo, repo + "/.git", repo + "/.git", **kw)

    def linked(self, repo, name, branch):
        wt = self.path(name)
        self.git(repo, "worktree", "add", "-q", wt, "-b", branch)
        admin = Path(repo, ".git", "worktrees", os.path.basename(wt))
        return wt, indexverify.Context(wt, str(admin), repo + "/.git", require_birthtime=HAS_BIRTHTIME)

    def observe(self, ctx):
        for p in (ctx.worktree, ctx.admin, ctx.common):
            if self.root not in guard(p).parents:
                raise RuntimeError(f"observation outside the fixture root: {p}")
        return indexverify.observe(ctx)

    # ------------------------------------------------------------ oracles

    def status_clean(self, wt):
        """Git's own view, isolated from host config and without refreshing."""
        out = self.git(wt, "status", "--porcelain=v1", "-z", "--ignored", "--untracked-files=all").stdout
        return out == b""

    def truth_clean(self, wt):
        """Independent path-based ground truth: content hashes, exact names and
        types, staged state, and no entry that the index does not account for."""
        head = self.git(wt, "rev-parse", "-q", "--verify", "HEAD^{tree}", check=False).stdout.strip()
        tree = head.decode() or EMPTY_TREE
        if self.git(wt, "diff-index", "--cached", "--quiet", tree, check=False).returncode:
            return False
        tracked, dirs = {}, set()
        for rec in self.git(wt, "ls-files", "-s", "-z").stdout.split(b"\0"):
            if not rec:
                continue
            meta, path = rec.split(b"\t", 1)
            mode, oid, stage = meta.split(b" ")
            if stage != b"0":
                return False
            tracked[path] = (mode, oid)
            parts = path.split(b"/")
            dirs.update(b"/".join(parts[:i]) for i in range(1, len(parts)))
        seen = set()
        root = os.fsencode(wt)

        def scan(rel):
            for name in os.listdir(root + (b"/" + rel if rel else b"")):
                if not rel and name == b".git":
                    continue
                p = rel + b"/" + name if rel else name
                st = os.lstat(root + b"/" + p)
                seen.add(p)
                if stat.S_ISDIR(st.st_mode):
                    if p not in dirs:
                        return False
                    if scan(p) is False:
                        return False
                elif p not in tracked:
                    return False
                else:
                    mode, oid = tracked[p]
                    if mode == b"120000":
                        if not stat.S_ISLNK(st.st_mode):
                            return False
                        data = os.readlink(root + b"/" + p)
                    else:
                        if not stat.S_ISREG(st.st_mode) or bool(st.st_mode & 0o100) != (mode == b"100755"):
                            return False
                        with open(root + b"/" + p, "rb") as f:
                            data = f.read()
                    if hashlib.sha1(b"blob %d\0" % len(data) + data).hexdigest().encode() != oid:
                        return False
            return True

        return scan(b"") is not False and set(tracked) <= seen

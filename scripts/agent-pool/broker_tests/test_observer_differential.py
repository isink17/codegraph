"""Seeded differential test of the Git observer against two oracles that run
only inside disposable fixtures: Git's own status, and an independent
path-based ground truth (content hashes, exact names and types, staged state).

For supported states the observer must agree with the ground truth exactly,
and CLEAN must also be clean for Git. States with an unsupported feature must
never be CLEAN. This is a necessary property, not a proof of security.

OBSERVER_DIFF_STATES sets the number of supported states (default 2000);
OBSERVER_DIFF_REPORT names a file that receives the counts as JSON."""

import collections
import json
import os
import random
import shutil
import unittest
from pathlib import Path

from gitfixture import Lab

from broker.indexverify import Status

STATES = int(os.environ.get("OBSERVER_DIFF_STATES", "2000"))
SEED = 20261112
FILES = {"README": b"hello\n", "LICENSE": b"MIT\n", "src/a.py": b"print(1)\n", "src/b.py": b"print(2)\n",
         "src/pkg/c.py": b"x = 3\n", "src/pkg/deep/d.txt": b"deep\n", "docs/guide.md": b"# guide\n",
         "bin/run": ("exec", b"#!/bin/sh\necho run\n"), "bin/tool": ("exec", b"#!/bin/sh\n"),
         "docs/link": ("link", "../README"), "src/pkg/up": ("link", "../a.py"), ".gitignore": b"*.log\nbuild/\n"}


def entries(wt, want):
    out = []
    for d, dirs, files in os.walk(wt):
        if d == wt and ".git" in dirs:
            dirs.remove(".git")
        rel = os.path.relpath(d, wt)
        names = files + [n for n in dirs if os.path.islink(os.path.join(d, n))] if want != "dir" else dirs
        for n in names:
            p = os.path.join(d, n)
            kind = "dir" if want == "dir" else "link" if os.path.islink(p) else "file"
            if kind == want and n != ".git":
                out.append(os.path.normpath(os.path.join(rel, n)))
    return sorted(out)


def pick(rng, wt, kind):
    found = entries(wt, kind)
    return rng.choice(found) if found else None


def m_edit(lab, wt, rng):
    p = pick(rng, wt, "file")
    if p:
        data = Path(wt, p).read_bytes() if rng.random() < 0.1 else rng.randbytes(rng.randint(0, 40))
        Path(wt, p).write_bytes(data)


def m_same_size_restore_mtime(lab, wt, rng):
    p = pick(rng, wt, "file")
    if p and os.path.getsize(Path(wt, p)):
        st = os.stat(Path(wt, p))
        data = bytearray(Path(wt, p).read_bytes())
        data[rng.randrange(len(data))] ^= 0x20
        Path(wt, p).write_bytes(bytes(data))
        os.utime(Path(wt, p), ns=(st.st_atime_ns, st.st_mtime_ns))


def m_chmod(lab, wt, rng):
    p = pick(rng, wt, "file")
    if p:
        os.chmod(Path(wt, p), os.stat(Path(wt, p)).st_mode ^ 0o111)


def m_delete(lab, wt, rng):
    p = pick(rng, wt, rng.choice(("file", "link")))
    if p:
        os.unlink(Path(wt, p))


def m_untracked(lab, wt, rng):
    d = pick(rng, wt, "dir") or "."
    lab.write(wt, os.path.join(d, rng.choice(("", "new/")) + f"u{rng.randrange(99)}"), rng.randbytes(5))


def m_ignored(lab, wt, rng):
    lab.write(wt, rng.choice(("x.log", "src/y.log", "build/out.o")), b"ignored")


def m_empty_dir(lab, wt, rng):
    os.makedirs(Path(wt, pick(rng, wt, "dir") or ".", f"e{rng.randrange(99)}"), exist_ok=True)


def m_file_to_link_same_blob(lab, wt, rng):
    p = pick(rng, wt, "file")
    if p:
        data = Path(wt, p).read_bytes()
        if data and b"\0" not in data and len(data) < 200:
            os.unlink(Path(wt, p))
            os.symlink(data, Path(wt, p))


def m_link_to_file_same_blob(lab, wt, rng):
    p = pick(rng, wt, "link")
    if p:
        target = os.readlink(Path(wt, p))
        os.unlink(Path(wt, p))
        Path(wt, p).write_bytes(os.fsencode(target))


def m_retarget(lab, wt, rng):
    p = pick(rng, wt, "link")
    if p:
        os.unlink(Path(wt, p))
        os.symlink(rng.choice(("README", "../x", "nowhere")), Path(wt, p))


def m_stage(lab, wt, rng):
    p = pick(rng, wt, rng.choice(("file", "link")))
    if p:
        lab.git(wt, "add", "-A", "--", p, check=False)


def m_stage_new(lab, wt, rng):
    lab.write(wt, f"staged{rng.randrange(99)}", rng.randbytes(4))
    lab.git(wt, "add", "-A", "--", ".", check=False)


def m_unstage_delete(lab, wt, rng):
    p = pick(rng, wt, "file")
    if p:
        lab.git(wt, "rm", "-q", "--cached", "--", p, check=False)


def m_rewrite_same(lab, wt, rng):
    p = pick(rng, wt, "file")
    if p:
        tmp = Path(wt, p + ".tmp~")
        tmp.write_bytes(Path(wt, p).read_bytes())
        os.chmod(tmp, os.stat(Path(wt, p)).st_mode)
        os.rename(tmp, Path(wt, p))


def m_touch(lab, wt, rng):
    p = pick(rng, wt, "file")
    if p:
        os.utime(Path(wt, p), ns=(rng.randrange(1 << 40) * 1000, rng.randrange(1 << 40) * 1000))


def m_commit_all(lab, wt, rng):
    lab.git(wt, "add", "-A", check=False)
    lab.git(wt, "commit", "-q", "--allow-empty", "-m", "state", check=False)


def m_branch(lab, wt, rng):
    lab.git(wt, "checkout", "-q", "-b", f"b{rng.randrange(1 << 30)}", check=False)


def m_detach(lab, wt, rng):
    lab.git(wt, "checkout", "-q", "--detach", check=False)


def m_pack_refs(lab, wt, rng):
    lab.git(wt, "pack-refs", "--all", check=False)


def m_rename(lab, wt, rng):
    p = pick(rng, wt, "file")
    if p:
        os.rename(Path(wt, p), Path(wt, p + "-renamed"))


def m_case_rename(lab, wt, rng):
    p = pick(rng, wt, "file")
    if p:
        q = os.path.join(os.path.dirname(p), os.path.basename(p).swapcase())
        if q != p:
            os.rename(Path(wt, p), Path(wt, q))


def m_dir_to_symlink(lab, wt, rng):
    d = pick(rng, wt, "dir")
    if d:
        out = lab.path(f"out-{rng.randrange(1 << 40)}")
        shutil.move(Path(wt, d), out)
        os.symlink(out, Path(wt, d))


def m_restore(lab, wt, rng):
    lab.git(wt, "reset", "-q", "--hard", check=False)
    lab.git(wt, "clean", "-q", "-fdx", check=False)


def m_noop(lab, wt, rng):
    pass


SUPPORTED = [m_edit, m_same_size_restore_mtime, m_chmod, m_delete, m_untracked, m_ignored, m_empty_dir,
             m_file_to_link_same_blob, m_link_to_file_same_blob, m_retarget, m_stage, m_stage_new,
             m_unstage_delete, m_rewrite_same, m_touch, m_commit_all, m_branch, m_detach, m_pack_refs, m_rename,
             m_case_rename, m_dir_to_symlink, m_restore, m_restore, m_noop, m_noop]


def tracked_file(lab, wt, rng):
    """A tracked regular file present in the worktree, committing one if needed."""
    found = [r.split(b"\t", 1)[1].decode() for r in lab.git(wt, "ls-files", "-s", "-z").stdout.split(b"\0")
             if r[:6] in (b"100644", b"100755")]
    found = [p for p in found if os.path.isfile(Path(wt, p)) and not os.path.islink(Path(wt, p))]
    if found:
        return rng.choice(sorted(found))
    lab.write(wt, "anchor-file", b"a")
    lab.git(wt, "add", "anchor-file")
    lab.git(wt, "commit", "-q", "-m", "anchor")
    return "anchor-file"


def h_assume_unchanged(lab, wt, rng):
    p = tracked_file(lab, wt, rng)
    lab.git(wt, "update-index", "--assume-unchanged", "--", p)
    lab.write(wt, p, b"hidden edit\n")


def h_skip_worktree(lab, wt, rng):
    lab.git(wt, "update-index", "--skip-worktree", "--", tracked_file(lab, wt, rng))


def h_intent_to_add(lab, wt, rng):
    lab.write(wt, "intent", b"i")
    lab.git(wt, "add", "-N", "intent")


def h_gc(lab, wt, rng):
    lab.git(wt, "gc", "-q")


def h_attributes(lab, wt, rng):
    lab.write(wt, os.path.join(pick(rng, wt, "dir") or ".", ".gitattributes"), b"* text=auto\n")


def h_fifo(lab, wt, rng):
    os.mkfifo(Path(wt, pick(rng, wt, "dir") or ".", "fifo"))


def h_hardlink(lab, wt, rng):
    os.link(Path(wt, tracked_file(lab, wt, rng)), lab.path(f"hl-{rng.randrange(1 << 40)}"))


def h_index_v4(lab, wt, rng):
    lab.git(wt, "update-index", "--index-version", "4")


def h_config(lab, wt, rng):
    common = lab.git(wt, "rev-parse", "--git-common-dir").stdout.decode().strip()
    with open(Path(wt, common, "config"), "ab") as f:
        f.write(rng.choice((b"[core]\n\tautocrlf = true\n", b"[core]\n\tfsmonitor = true\n",
                            b"[core]\n\tuntrackedCache = true\n", b"[feature]\n\tmanyFiles = true\n")))


def h_shallow(lab, wt, rng):
    common = lab.git(wt, "rev-parse", "--git-common-dir").stdout.decode().strip()
    Path(wt, common, "shallow").write_bytes(lab.git(wt, "rev-parse", "HEAD").stdout)


HOSTILE = [h_assume_unchanged, h_skip_worktree, h_intent_to_add, h_gc, h_attributes, h_fifo, h_hardlink, h_index_v4,
           h_config, h_shallow]


class Differential(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.lab = Lab()
        cls.template = cls.lab.repo("template", FILES)
        cls.lab.write(cls.template, "src/b.py", b"print(22)\n")
        cls.lab.git(cls.template, "commit", "-qam", "second")
        cls.counts = collections.Counter()

    @classmethod
    def tearDownClass(cls):
        report = os.environ.get("OBSERVER_DIFF_REPORT")
        if report:
            Path(report).write_text(json.dumps(dict(sorted(cls.counts.items())), indent=1) + "\n")
        cls.lab.cleanup()

    def state(self, i, linked):
        d = self.lab.path(f"s{i}")
        shutil.copytree(self.template, d, symlinks=True)
        if linked:
            wt, ctx = self.lab.linked(d, f"s{i}-wt", f"wt{i}")
            return d, wt, ctx
        return d, d, self.lab.ctx(d)

    def drop(self, *paths):
        for p in paths:
            if os.path.lexists(p):
                for d, subdirs, _ in os.walk(p):
                    for n in subdirs:
                        if not os.path.islink(os.path.join(d, n)):
                            os.chmod(os.path.join(d, n), 0o700)
                shutil.rmtree(p)

    def test_supported_states_agree_with_both_oracles(self):
        for i in range(STATES):
            rng = random.Random(SEED * 1_000_003 + i)
            linked = i % 10 == 9
            d, wt, ctx = self.state(i, linked)
            applied = [m.__name__ for m in rng.choices(SUPPORTED, k=rng.randint(0, 3))]
            for name in applied:
                globals()[name](self.lab, wt, rng)
            obs = self.lab.observe(ctx)
            truth, git_clean = self.lab.truth_clean(wt), self.lab.status_clean(wt)
            msg = (i, linked, applied, obs)
            self.assertIn(obs.status, (Status.CLEAN, Status.NOT_CLEAN), msg)
            self.assertEqual(obs.clean, truth, msg)
            if obs.clean:
                self.assertTrue(git_clean, msg)
            self.counts[f"supported:{obs.status.value}"] += 1
            self.counts[f"supported:git_clean_observer_not_clean"] += git_clean and not obs.clean
            self.drop(d, wt)
        self.assertGreater(self.counts["supported:CLEAN"], STATES // 10)
        self.assertGreater(self.counts["supported:NOT_CLEAN"], STATES // 3)

    def test_unsupported_states_are_never_clean(self):
        for i in range(max(STATES // 4, len(HOSTILE))):
            rng = random.Random(SEED * 7_000_003 + i)
            linked = i % 10 == 9
            d, wt, ctx = self.state(STATES + i, linked)
            for m in rng.choices(SUPPORTED, k=rng.randint(0, 2)):
                m(self.lab, wt, rng)
            m_restore(self.lab, wt, rng) if rng.random() < 0.5 else None
            hostile = HOSTILE[i % len(HOSTILE)]
            hostile(self.lab, wt, rng)
            obs = self.lab.observe(ctx)
            self.assertFalse(obs.clean, (i, hostile.__name__, obs))
            self.counts[f"unsupported:{obs.status.value}"] += 1
            self.drop(d, wt)

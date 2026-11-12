"""Read-only Git observer: CLEAN fixtures, differences, hostile filesystems and
repositories, malformed Git data, identity binding and result semantics. Every
repository is a disposable fixture under a private temporary directory."""

import hashlib
import os
import random
import shutil
import socket
import unittest
import zlib
from pathlib import Path
from unittest import mock

from gitfixture import HAS_BIRTHTIME, Lab

from broker import fdwalk, gitraw, indexverify as iv
from broker.fdwalk import Limits, Unknown
from broker.indexverify import Status

FILES = {"README": b"hello\n", "src/a.py": b"print(1)\n", "src/pkg/b.py": b"x = 2\n",
         "bin/run": ("exec", b"#!/bin/sh\necho run\n"), "docs/link": ("link", "../README"),
         ".gitignore": b"*.log\n"}
OLD_NS = 978307200 * 10 ** 9


def index_bytes(entries, version=2, exts=b""):
    """entries: [(path, mode, flags_extra, xflags)] with a fixed oid each."""
    out = b"DIRC" + version.to_bytes(4, "big") + len(entries).to_bytes(4, "big")
    for path, mode, extra, xflags in entries:
        flags = min(len(path), 0xFFF) | extra | (0x4000 if xflags is not None else 0)
        e = bytes(24) + mode.to_bytes(4, "big") + bytes(12) + hashlib.sha1(path).digest() + flags.to_bytes(2, "big")
        if xflags is not None:
            e += xflags.to_bytes(2, "big")
        e += path
        e += bytes(((len(e) + 8) & ~7) - len(e))
        out += e
    out += exts
    return out + hashlib.sha1(out).digest()


def loose(typ, body):
    data = typ + b" %d\0" % len(body) + body
    return hashlib.sha1(data).hexdigest().encode(), zlib.compress(data)


class Base(unittest.TestCase):
    def setUp(self):
        self.lab = Lab()
        self.addCleanup(self.lab.cleanup)

    def make(self, files=FILES, name="repo"):
        r = self.lab.repo(name, files)
        return r, self.lab.ctx(r)

    def see(self, ctx, status, code=None):
        obs = self.lab.observe(ctx)
        self.assertIs(obs.status, status, obs)
        if code is not None:
            self.assertIn(code, [r[0] for r in obs.reasons], obs)
        return obs


class CleanFixtures(Base):
    def test_minimal_committed_repository(self):
        r, ctx = self.make({"README": b"hello\n"})
        obs = self.see(ctx, Status.CLEAN)
        self.assertTrue(self.lab.status_clean(r) and self.lab.truth_clean(r))
        d = dict(obs.digests)
        self.assertEqual(d["head_ref"], "refs/heads/main")
        self.assertEqual(d["commit"], self.lab.git(r, "rev-parse", "HEAD").stdout.decode().strip())
        self.assertEqual(d["tree"], self.lab.git(r, "rev-parse", "HEAD^{tree}").stdout.decode().strip())

    def test_directories_executable_and_symlink(self):
        r, ctx = self.make()
        self.see(ctx, Status.CLEAN)
        self.assertTrue(self.lab.status_clean(r))

    def test_linked_worktree_with_authorized_roots(self):
        r, ctx = self.make()
        wt, wctx = self.lab.linked(r, "wt", "feature")
        self.assertNotEqual(wctx.admin, wctx.common)
        self.see(wctx, Status.CLEAN)
        self.see(ctx, Status.CLEAN)
        self.lab.write(wt, "src/a.py", b"changed\n")
        self.see(wctx, Status.NOT_CLEAN, "modified")
        self.see(ctx, Status.CLEAN)

    def test_index_metadata_cache_tree_and_version_3(self):
        r, ctx = self.make()
        index = Path(r, ".git", "index").read_bytes()
        self.assertIn(b"TREE", index[12:])
        self.see(ctx, Status.CLEAN)
        self.lab.git(r, "update-index", "--index-version", "3")
        self.see(ctx, Status.CLEAN)

    def test_repeated_observation_is_stable(self):
        r, ctx = self.make()
        a, b = self.lab.observe(ctx), self.lab.observe(ctx)
        self.assertEqual((a.digests, a.binding), (b.digests, b.binding))
        self.assertIs(iv.combine([a, b]).status, Status.CLEAN)
        self.see(self.lab.ctx(r, expected=a.binding), Status.CLEAN)

    def test_unborn_repository(self):
        r = self.lab.path("empty")
        os.mkdir(r)
        self.lab.git(r, "init", "-q")
        ctx = self.lab.ctx(r)
        obs = self.see(ctx, Status.CLEAN)
        self.assertEqual(dict(obs.digests)["commit"], "")
        self.lab.write(r, "new", b"x")
        self.see(ctx, Status.NOT_CLEAN, "untracked")
        self.lab.git(r, "add", "new")
        self.see(ctx, Status.NOT_CLEAN, "staged")

    def test_packed_refs_and_detached_head(self):
        r, ctx = self.make()
        self.lab.git(r, "pack-refs", "--all")
        self.assertFalse(Path(r, ".git", "refs", "heads", "main").exists())
        self.see(ctx, Status.CLEAN)
        self.lab.git(r, "checkout", "-q", "--detach")
        obs = self.see(ctx, Status.CLEAN)
        self.assertEqual(dict(obs.digests)["head_ref"], "")

    def test_second_commit_with_parent(self):
        r, ctx = self.make()
        self.lab.write(r, "src/a.py", b"print(2)\n")
        self.lab.git(r, "commit", "-qam", "two")
        self.see(ctx, Status.CLEAN)


class Differences(Base):
    def test_unstaged_modification_deletion_and_mode(self):
        r, ctx = self.make()
        self.lab.write(r, "src/a.py", b"print(9)\n")
        self.see(ctx, Status.NOT_CLEAN, "modified")
        self.lab.git(r, "checkout", "--", "src/a.py")
        os.unlink(Path(r, "src/pkg/b.py"))
        self.see(ctx, Status.NOT_CLEAN, "deleted")
        self.lab.git(r, "checkout", "--", "src/pkg/b.py")
        os.chmod(Path(r, "README"), 0o755)
        self.see(ctx, Status.NOT_CLEAN, "mode-changed")
        os.chmod(Path(r, "README"), 0o644)
        self.see(ctx, Status.CLEAN)

    def test_staged_only_change(self):
        r, ctx = self.make()
        self.lab.write(r, "README", b"staged\n")
        self.lab.git(r, "add", "README")
        self.assertFalse(self.lab.status_clean(r))
        obs = self.see(ctx, Status.NOT_CLEAN, "staged")
        self.assertNotIn("modified", [x[0] for x in obs.reasons])

    def test_untracked_ignored_and_empty_directory(self):
        r, ctx = self.make()
        self.lab.write(r, "notes.txt", b"n")
        self.see(ctx, Status.NOT_CLEAN, "untracked")
        os.unlink(Path(r, "notes.txt"))
        self.lab.write(r, "src/debug.log", b"ignored by .gitignore")
        self.see(ctx, Status.NOT_CLEAN, "untracked")
        os.unlink(Path(r, "src/debug.log"))
        os.mkdir(Path(r, "src/empty"))
        self.see(ctx, Status.NOT_CLEAN, "untracked")

    def test_type_changes_with_the_same_blob(self):
        r, ctx = self.make()
        self.lab.write(r, "docs/link", b"../README")
        self.see(ctx, Status.NOT_CLEAN, "type-changed")
        self.lab.link(r, "docs/link", "../README")
        self.see(ctx, Status.CLEAN)
        self.lab.link(r, "README", "hello\n")
        self.see(ctx, Status.NOT_CLEAN, "type-changed")

    def test_directory_replaced_by_file(self):
        r, ctx = self.make()
        shutil.rmtree(Path(r, "src/pkg"))
        self.lab.write(r, "src/pkg", b"x = 2\n")
        self.see(ctx, Status.NOT_CLEAN, "deleted")

    def test_racy_same_size_edit_with_restored_mtime(self):
        r, ctx = self.make()
        p = Path(r, "README")
        os.utime(p, ns=(OLD_NS, OLD_NS))
        self.lab.git(r, "add", "README")
        self.lab.git(r, "commit", "-q", "--allow-empty", "-m", "old mtime")
        self.see(ctx, Status.CLEAN)
        p.write_bytes(b"HELLO\n")
        os.utime(p, ns=(OLD_NS, OLD_NS))
        overlooked = self.lab.git(r, "-c", "core.trustctime=false", "-c", "core.checkStat=minimal", "status",
                                  "--porcelain").stdout == b""
        self.assertTrue(overlooked, "the stat-trusting Git status should miss this edit")
        self.see(ctx, Status.NOT_CLEAN, "modified")

    def test_assume_unchanged_hides_an_edit_from_git_but_not_from_the_observer(self):
        r, ctx = self.make()
        self.lab.git(r, "update-index", "--assume-unchanged", "README")
        self.lab.write(r, "README", b"edited\n")
        self.assertTrue(self.lab.status_clean(r))
        self.see(ctx, Status.UNKNOWN, "unsupported-index-feature")

    def test_reasons_are_bounded_and_carry_no_content(self):
        r, ctx = self.make()
        secret = b"SECRET-CANARY-0123456789"
        self.lab.write(r, "README", secret)
        for i in range(40):
            self.lab.write(r, f"u{i:02}", secret)
        obs = self.see(ctx, Status.NOT_CLEAN)
        self.assertEqual(len(obs.reasons), iv.MAX_REASONS)
        self.assertNotIn(secret.decode(), repr((obs.reasons, obs.digests, obs.binding)))

    def test_control_bytes_in_names_are_escaped(self):
        r, ctx = self.make()
        self.lab.write(r, "evil\x1b]52;c;x\x07name", b"x")
        obs = self.see(ctx, Status.NOT_CLEAN, "untracked")
        self.assertTrue(all(0x20 <= ord(c) <= 0x7E for _, p in obs.reasons for c in p))


class Hostile(Base):
    def test_every_open_is_descriptor_relative_nofollow_and_read_only(self):
        r, ctx = self.make()
        calls = []
        real = os.open

        def spy(path, flags, *a, **kw):
            calls.append((path, flags, kw.get("dir_fd")))
            return real(path, flags, *a, **kw)

        with mock.patch.object(os, "open", spy):
            self.see(ctx, Status.CLEAN)
        self.assertTrue(calls)
        write = os.O_WRONLY | os.O_RDWR | os.O_CREAT | os.O_TRUNC | os.O_APPEND
        for path, flags, dir_fd in calls:
            self.assertTrue(flags & os.O_NOFOLLOW and not flags & write, (path, flags))
            self.assertTrue(path == "/" or (dir_fd is not None and fdwalk.valid_name(path)), (path, dir_fd))

    def test_symlinked_directory_is_never_followed(self):
        r, ctx = self.make()
        outside = Path(self.lab.path("outside"))
        shutil.copytree(Path(r, "src"), outside)
        shutil.rmtree(Path(r, "src"))
        os.symlink(outside, Path(r, "src"))
        os.chmod(outside, 0)
        self.see(ctx, Status.NOT_CLEAN, "deleted")
        self.assertFalse(self.lab.status_clean(r))

    def test_symlink_in_an_authorized_root_path(self):
        r, ctx = self.make()
        alias = self.lab.path("alias")
        os.symlink(r, alias)
        self.see(iv.Context(alias, alias + "/.git", alias + "/.git", require_birthtime=HAS_BIRTHTIME),
                 Status.UNKNOWN, "root-not-a-plain-directory")

    def test_directory_swapped_during_the_scan(self):
        r, ctx = self.make()
        real, done = fdwalk.read_at, []

        def swap(dirfd, name, *a):
            if name == b"b.py" and not done:
                done.append(1)
                aside = self.lab.path("aside")
                os.rename(Path(r, "src"), aside)
                shutil.copytree(aside, Path(r, "src"), symlinks=True)
            return real(dirfd, name, *a)

        with mock.patch.object(fdwalk, "read_at", swap):
            self.see(ctx, Status.UNKNOWN, "changed-during-observation")

    def test_file_replaced_between_stat_and_open(self):
        r, ctx = self.make()
        real = fdwalk.read_at

        def replace(dirfd, name, m, *a):
            if name == b"a.py":
                tmp = Path(r, "src", ".tmp")
                tmp.write_bytes(b"print(1)\n")
                os.rename(tmp, Path(r, "src", "a.py"))
            return real(dirfd, name, m, *a)

        with mock.patch.object(fdwalk, "read_at", replace):
            self.see(ctx, Status.UNKNOWN, "changed-during-observation")

    def test_file_rewritten_after_it_was_hashed(self):
        r, ctx = self.make()
        real = fdwalk.read_at

        def rewrite(dirfd, name, m, *a):
            data = real(dirfd, name, m, *a)
            if name == b"README":
                with open(Path(r, "README"), "r+b") as f:
                    f.write(b"HELLO\n")
                os.utime(Path(r, "README"), ns=(m.mtime_ns, m.mtime_ns))
            return data

        with mock.patch.object(fdwalk, "read_at", rewrite):
            self.see(ctx, Status.UNKNOWN, "changed-during-observation")

    def test_repointed_git_file(self):
        r, ctx = self.make()
        wt, wctx = self.lab.linked(r, "wt", "feature")
        other = self.lab.path("other-admin")
        shutil.copytree(wctx.admin, other)
        os.chmod(other, 0)
        Path(wt, ".git").write_bytes(b"gitdir: " + other.encode() + b"\n")
        self.see(wctx, Status.IDENTITY_CHANGED, "git-link-mismatch")
        Path(wt, ".git").write_bytes(b"gitdir: ../repo/.git/worktrees/wt\n")
        self.see(wctx, Status.IDENTITY_CHANGED, "git-link-mismatch")

    def test_unauthorized_external_git_directory(self):
        r, ctx = self.make()
        wt, wctx = self.lab.linked(r, "wt", "feature")
        r2, _ = self.make(name="repo2")
        wt2, w2ctx = self.lab.linked(r2, "wt2", "feature")
        forged = iv.Context(wt2, wctx.admin, wctx.common, require_birthtime=HAS_BIRTHTIME)
        self.see(forged, Status.IDENTITY_CHANGED, "git-link-mismatch")

    def test_common_dir_substitution(self):
        r, ctx = self.make()
        wt, wctx = self.lab.linked(r, "wt", "feature")
        copy = self.lab.path("common-copy")
        shutil.copytree(wctx.common, copy, symlinks=True)
        Path(wctx.admin, "commondir").write_bytes(copy.encode() + b"\n")
        self.see(wctx, Status.IDENTITY_CHANGED, "git-link-mismatch")
        Path(wctx.admin, "commondir").write_bytes(b"../..\n")
        Path(wctx.admin, "gitdir").write_bytes(b"/elsewhere/.git\n")
        self.see(wctx, Status.IDENTITY_CHANGED, "git-link-mismatch")

    def test_ordinary_repository_presented_as_linked(self):
        r, ctx = self.make()
        self.see(iv.Context(r, self.lab.path("admin"), r + "/.git", require_birthtime=HAS_BIRTHTIME),
                 Status.UNKNOWN, "root-missing")
        os.mkdir(self.lab.path("admin"))
        self.see(iv.Context(r, self.lab.path("admin"), r + "/.git", require_birthtime=HAS_BIRTHTIME),
                 Status.IDENTITY_CHANGED, "git-link-mismatch")

    def test_admin_root_that_is_not_the_worktree_git_directory(self):
        r, ctx = self.make()
        copy = self.lab.path("git-copy")
        shutil.copytree(Path(r, ".git"), copy, symlinks=True)
        real = fdwalk.open_root
        with mock.patch.object(fdwalk, "open_root", lambda p: real(copy if p == ctx.admin else p)):
            self.see(ctx, Status.IDENTITY_CHANGED, "git-link-mismatch")

    def test_replaced_roots_change_the_binding(self):
        r, ctx = self.make()
        first = self.see(ctx, Status.CLEAN)
        bound = self.lab.ctx(r, expected=first.binding)
        os.rename(Path(r, ".git"), Path(r, ".git-old"))
        shutil.copytree(Path(r, ".git-old"), Path(r, ".git"), symlinks=True)
        shutil.rmtree(Path(r, ".git-old"))
        self.see(bound, Status.IDENTITY_CHANGED, "binding-mismatch")
        self.see(ctx, Status.CLEAN)
        moved = self.lab.path("moved")
        os.rename(r, moved)
        shutil.copytree(moved, r, symlinks=True)
        self.see(self.lab.ctx(r, expected=self.lab.observe(ctx).binding), Status.CLEAN)
        self.see(self.lab.ctx(r, expected=first.binding), Status.IDENTITY_CHANGED, "binding-mismatch")

    def test_root_replaced_during_the_scan(self):
        r, ctx = self.make()
        real, done = fdwalk.walk, []

        def walk(*a, **kw):
            out = real(*a, **kw)
            if not done:
                done.append(1)
                os.rename(r, self.lab.path("gone"))
                shutil.copytree(self.lab.path("gone"), r, symlinks=True)
            return out

        with mock.patch.object(fdwalk, "walk", walk):
            obs = self.lab.observe(ctx)
        self.assertIn(obs.status, (Status.UNKNOWN, Status.IDENTITY_CHANGED), obs)

    def test_repository_changed_during_the_scan(self):
        r, ctx = self.make()
        real, done = fdwalk.walk, []

        def walk(*a, **kw):
            out = real(*a, **kw)
            if not done:
                done.append(1)
                self.lab.git(r, "commit", "-q", "--allow-empty", "-m", "racing")
            return out

        with mock.patch.object(fdwalk, "walk", walk):
            self.see(ctx, Status.UNKNOWN, "changed-during-observation")

    def test_git_pointer_changed_during_the_scan(self):
        r, ctx = self.make()
        wt, wctx = self.lab.linked(r, "wt", "feature")
        real, done = fdwalk.walk, []

        def walk(*a, **kw):
            out = real(*a, **kw)
            if not done:
                done.append(1)
                Path(wt, ".git").write_bytes(b"gitdir: /elsewhere\n")
            return out

        with mock.patch.object(fdwalk, "walk", walk):
            obs = self.lab.observe(wctx)
        self.assertIn(obs.status, (Status.UNKNOWN, Status.IDENTITY_CHANGED), obs)

    def test_hardlinked_tracked_file_is_not_read(self):
        r, ctx = self.make()
        os.link(Path(r, "README"), self.lab.path("outside-link"))
        opened = []
        real = os.open
        with mock.patch.object(os, "open", lambda p, f, *a, **kw: opened.append(p) or real(p, f, *a, **kw)):
            self.see(ctx, Status.UNKNOWN, "hardlink")
        self.assertNotIn(b"README", opened)

    def test_hardlinked_untracked_file(self):
        r, ctx = self.make()
        os.link(self.lab.path("repo/README"), Path(r, "copy"))
        self.see(ctx, Status.UNKNOWN, "hardlink")
        os.unlink(Path(r, "copy"))
        Path(self.lab.path("solo")).write_bytes(b"x")
        os.link(self.lab.path("solo"), Path(r, "solo"))
        self.see(ctx, Status.NOT_CLEAN, "untracked")

    def test_fifo_is_never_opened(self):
        r, ctx = self.make()
        os.mkfifo(Path(r, "pipe"))
        self.see(ctx, Status.UNKNOWN, "special-file")
        os.unlink(Path(r, "pipe"))
        os.unlink(Path(r, "README"))
        os.mkfifo(Path(r, "README"))
        self.see(ctx, Status.UNKNOWN, "special-file")

    def test_socket_is_never_opened(self):
        r, ctx = self.make()
        cwd = os.getcwd()
        s = socket.socket(socket.AF_UNIX)
        self.addCleanup(s.close)
        try:
            os.chdir(Path(r, "src"))
            s.bind("sock")
        except PermissionError:
            self.skipTest("this environment forbids binding a Unix socket")
        finally:
            os.chdir(cwd)
        self.see(ctx, Status.UNKNOWN, "special-file")

    def test_other_device_is_refused(self):
        r, ctx = self.make()
        real = fdwalk.lstat_at

        def other_dev(dirfd, name, path):
            m = real(dirfd, name, path)
            return m._replace(dev=m.dev + 1) if m is not None and name == b"pkg" else m

        with mock.patch.object(fdwalk, "lstat_at", other_dev):
            self.see(ctx, Status.UNKNOWN, "cross-device")

    def test_limits(self):
        r, ctx = self.make()
        for limits, code in ((Limits(max_index=64), "limit-exceeded"), (Limits(max_file=4), "limit-exceeded"),
                             (Limits(max_entries=3), "limit-exceeded"), (Limits(max_depth=1), "limit-exceeded"),
                             (Limits(max_total=10), "limit-exceeded"), (Limits(max_object=16), "limit-exceeded"),
                             (Limits(max_trees=1), "limit-exceeded")):
            self.see(self.lab.ctx(r, limits=limits), Status.UNKNOWN, code)

    def test_zlib_bomb(self):
        r, ctx = self.make()
        commit = self.lab.git(r, "rev-parse", "HEAD").stdout.strip().decode()
        p = Path(r, ".git", "objects", commit[:2], commit[2:])
        c = zlib.compressobj(9)
        bomb = c.compress(b"commit 209715200\0") + b"".join(c.compress(bytes(1 << 20)) for _ in range(200)) + c.flush()
        self.assertLess(len(bomb), 1 << 20)
        os.chmod(p, 0o644)
        p.write_bytes(bomb)
        self.see(self.lab.ctx(r, limits=Limits(max_object=1 << 20)), Status.UNKNOWN, "limit-exceeded")

    def test_corrupted_loose_object(self):
        r, ctx = self.make()
        tree = self.lab.git(r, "rev-parse", "HEAD^{tree}").stdout.strip().decode()
        p = Path(r, ".git", "objects", tree[:2], tree[2:])
        os.chmod(p, 0o644)
        typ, body = gitraw.parse_object(tree.encode(), p.read_bytes(), 1 << 20)
        p.write_bytes(zlib.compress(b"tree %d\0" % len(body) + body.replace(b"README", b"READMF")))
        self.see(ctx, Status.UNKNOWN, "malformed-object")

    def test_packed_objects_are_unsupported(self):
        r, ctx = self.make()
        self.lab.git(r, "gc", "-q")
        self.assertTrue(any(Path(r, ".git", "objects", "pack").iterdir()))
        self.see(ctx, Status.UNKNOWN, "object-in-unsupported-pack")
        self.assertTrue(self.lab.status_clean(r))

    def test_missing_object(self):
        r, ctx = self.make()
        tree = self.lab.git(r, "rev-parse", "HEAD^{tree}").stdout.strip().decode()
        os.unlink(Path(r, ".git", "objects", tree[:2], tree[2:]))
        self.see(ctx, Status.UNKNOWN, "object-missing")

    def test_object_format_mismatches(self):
        r, ctx = self.make()
        cfg = Path(r, ".git", "config")
        base = cfg.read_bytes()
        cfg.write_bytes(base.replace(b"repositoryformatversion = 0", b"repositoryformatversion = 1")
                        + b"[extensions]\n\tobjectformat = sha256\n")
        self.see(ctx, Status.UNKNOWN, "unsupported-object-format")
        cfg.write_bytes(base)
        Path(r, ".git", "refs", "heads", "main").write_bytes(b"a" * 64 + b"\n")
        self.see(ctx, Status.UNKNOWN, "unsupported-object-format")
        Path(r, ".git", "HEAD").write_bytes(b"b" * 64 + b"\n")
        self.see(ctx, Status.UNKNOWN, "unsupported-object-format")

    def test_unsupported_repository_features(self):
        r, ctx = self.make()
        g = Path(r, ".git")
        head = self.lab.git(r, "rev-parse", "HEAD").stdout.strip()
        cases = [("objects/info/alternates", b"/elsewhere/objects\n"), ("info/grafts", head + b"\n"),
                 ("shallow", head + b"\n"), ("refs/replace/" + head.decode(), head + b"\n"),
                 ("info/attributes", b"* text\n"), ("info/sparse-checkout", b"/src\n"), ("index.lock", b"")]
        for rel, data in cases:
            p = g / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(data)
            self.see(ctx, Status.UNKNOWN, "unsupported-repository-feature")
            p.unlink()
            self.see(ctx, Status.CLEAN)
        packed = g / "packed-refs"
        packed.write_bytes(head + b" refs/replace/" + head + b"\n")
        self.see(ctx, Status.UNKNOWN, "unsupported-repository-feature")
        packed.unlink()

    def test_worktree_config_and_sparse_checkout(self):
        r, ctx = self.make()
        wt, wctx = self.lab.linked(r, "wt", "feature")
        Path(wctx.admin, "config.worktree").write_bytes(b"[core]\n\tsparseCheckout = true\n")
        self.see(wctx, Status.UNKNOWN, "unsupported-repository-feature")
        os.unlink(Path(wctx.admin, "config.worktree"))
        self.lab.git(r, "sparse-checkout", "set", "--no-cone", "/src/")
        obs = self.lab.observe(ctx)
        self.assertIs(obs.status, Status.UNKNOWN, obs)

    def test_attributes_anywhere(self):
        r, ctx = self.make()
        self.lab.write(r, "src/.gitattributes", b"*.py text eol=crlf\n")
        self.see(ctx, Status.UNKNOWN, "attributes-present")
        self.lab.git(r, "add", "-A")
        self.lab.git(r, "commit", "-qm", "attrs")
        self.see(ctx, Status.UNKNOWN, "attributes-present")
        os.unlink(Path(r, "src/.gitattributes"))
        self.see(ctx, Status.UNKNOWN, "attributes-present")

    def test_config_outside_the_subset(self):
        r, ctx = self.make()
        cfg = Path(r, ".git", "config")
        base = cfg.read_bytes()
        for extra in (b"[core]\n\tautocrlf = true\n", b"[core]\n\tworktree = /elsewhere\n",
                      b"[core]\n\tfsmonitor = true\n", b"[include]\n\tpath = /elsewhere\n",
                      b'[includeIf "gitdir:/"]\n\tpath = x\n', b"[filter \"x\"]\n\tclean = cat\n",
                      b'[remote "o"]\n\tpromisor = true\n', b"[core]\n\tbare = true\n",
                      b"[core]\n\tfilemode = \"true\"\n", b"[core]\n\tfilemode = true # c\n",
                      b"[core]\n\tsymlinks = yes\n", b"[gc]\n\tauto = 0\n", b"[index]\n\tskipHash = true\n",
                      b"[core]\n\trepositoryformatversion = 2\n", b"stray = 1\n",
                      b'[remote "o"]\n\turl = a\\\n\tb\n', b'[remote "o"]\n\turl = "a"\n'):
            cfg.write_bytes(base + extra)
            self.see(ctx, Status.UNKNOWN)
        cfg.write_bytes(base + b'[remote "origin"]\n\turl = https://example.invalid/r.git\n'
                        b'[branch "main"]\n\tremote = origin\n[user]\n\tname = x\n')
        self.see(ctx, Status.CLEAN)
        cfg.unlink()
        self.see(ctx, Status.UNKNOWN, "unsupported-repository-feature")

    def test_head_and_ref_shapes(self):
        r, ctx = self.make()
        head, ref = Path(r, ".git", "HEAD"), Path(r, ".git", "refs", "heads", "main")
        oid = ref.read_bytes()
        for data, code in ((b"ref: refs/tags/v1\n", "unsupported-head"), (b"ref: refs/heads/../x\n", "unsupported-head"),
                           (b"ref: refs/heads/main", "unsupported-head"), (b"garbage\n", "malformed-ref"),
                           (oid.upper(), "malformed-ref")):
            head.write_bytes(data)
            self.see(ctx, Status.UNKNOWN, code)
        head.write_bytes(b"ref: refs/heads/main\n")
        ref.write_bytes(b"ref: refs/heads/other\n")
        self.see(ctx, Status.UNKNOWN, "malformed-ref")
        ref.unlink()
        os.symlink(self.lab.path("elsewhere"), ref)
        self.see(ctx, Status.UNKNOWN, "unexpected-file-type")
        ref.unlink()
        ref.write_bytes(oid)
        head.unlink()
        self.see(ctx, Status.UNKNOWN, "unsupported-head")

    def test_index_features_produced_by_git(self):
        r, ctx = self.make()
        cases = [(("update-index", "--skip-worktree", "README"), "unsupported-index-feature"),
                 (("update-index", "--split-index"), "unsupported-index-feature"),
                 (("update-index", "--force-untracked-cache"), "unsupported-index-feature"),
                 (("update-index", "--index-version", "4"), "unsupported-index-feature")]
        for args, code in cases:
            self.lab.git(r, *args)
            self.see(ctx, Status.UNKNOWN, code)
            os.unlink(Path(r, ".git", "index"))
            for p in Path(r, ".git").glob("sharedindex.*"):
                p.unlink()
            self.lab.git(r, "reset", "-q")
            self.see(ctx, Status.CLEAN)
        self.lab.write(r, "added", b"a")
        self.lab.git(r, "add", "-N", "added")
        self.see(ctx, Status.UNKNOWN, "unsupported-index-feature")
        self.lab.git(r, "reset", "-q")
        os.unlink(Path(r, "added"))
        self.lab.git(r, "update-index", "--add", "--cacheinfo", "160000," + "1" * 40 + ",sub")
        self.see(ctx, Status.UNKNOWN, "submodule")

    def test_fsmonitor_extension(self):
        r, ctx = self.make()
        p = Path(r, ".git", "index")
        data = p.read_bytes()[:-20] + b"FSMN" + (8).to_bytes(4, "big") + bytes(8)
        p.write_bytes(data + hashlib.sha1(data).digest())
        self.see(ctx, Status.UNKNOWN, "unsupported-index-feature")

    def test_unresolved_conflict(self):
        r, ctx = self.make()
        self.lab.git(r, "checkout", "-q", "-b", "other")
        self.lab.write(r, "README", b"other\n")
        self.lab.git(r, "commit", "-qam", "other")
        self.lab.git(r, "checkout", "-q", "main")
        self.lab.write(r, "README", b"main\n")
        self.lab.git(r, "commit", "-qam", "main")
        self.lab.git(r, "merge", "-q", "other", check=False)
        self.see(ctx, Status.UNKNOWN, "unresolved-conflict")


class FdwalkUnit(Base):
    def setUp(self):
        super().setUp()
        self.d = self.lab.path("fd")
        os.mkdir(self.d)
        self.fd, st = fdwalk.open_root(self.d)
        self.addCleanup(os.close, self.fd)
        self.dev = st.st_dev

    def code(self, fn, *args):
        with self.assertRaises(Unknown) as cm:
            fn(*args)
        return cm.exception.code

    def test_open_root_rejects_unsafe_paths(self):
        alias = str(self.lab.root / "alias")
        os.symlink(self.d, alias)
        for p, code in ((self.d + "/../fd", "invalid-root"), ("fd", "invalid-root"), ("/", "invalid-root"),
                        (self.d + "/", "invalid-root"), (alias, "root-not-a-plain-directory"),
                        (self.d + "/missing", "root-missing")):
            self.assertEqual(self.code(fdwalk.open_root, p), code, p)

    def test_open_dir_at_detects_a_replaced_directory(self):
        os.mkdir(Path(self.d, "sub"))
        m = fdwalk.lstat_at(self.fd, b"sub", b"sub")
        os.rename(Path(self.d, "sub"), Path(self.d, "old"))
        os.mkdir(Path(self.d, "sub"))
        self.assertEqual(self.code(fdwalk.open_dir_at, self.fd, b"sub", m, b"sub"), "changed-during-observation")

    def test_read_at_detects_a_rewrite_during_the_read(self):
        Path(self.d, "f").write_bytes(b"abcdef")
        m = fdwalk.lstat_at(self.fd, b"f", b"f")
        real = os.read

        def read(fd, n):
            with open(Path(self.d, "f"), "r+b") as f:
                f.write(b"ABCDEF")
            return real(fd, n)

        with mock.patch.object(os, "read", read):
            self.assertEqual(self.code(fdwalk.read_at, self.fd, b"f", m, 100, b"f", self.dev),
                             "changed-during-observation")
        m = fdwalk.lstat_at(self.fd, b"f", b"f")
        self.assertEqual(fdwalk.read_at(self.fd, b"f", m, 100, b"f", self.dev), b"ABCDEF")

    def test_read_at_refusals(self):
        Path(self.d, "f").write_bytes(b"abcdef")
        os.mkfifo(Path(self.d, "p"))
        m = fdwalk.lstat_at(self.fd, b"f", b"f")
        self.assertEqual(self.code(fdwalk.read_at, self.fd, b"f", m, 5, b"f", self.dev), "limit-exceeded")
        self.assertEqual(self.code(fdwalk.read_at, self.fd, b"f", m, 100, b"f", self.dev + 1), "cross-device")
        self.assertEqual(self.code(fdwalk.read_at, self.fd, b"f", m._replace(nlink=2), 100, b"f", self.dev),
                         "hardlink")
        p = fdwalk.lstat_at(self.fd, b"p", b"p")
        self.assertEqual(p.kind, "special")
        self.assertEqual(self.code(fdwalk.read_at, self.fd, b"p", p, 100, b"p", self.dev), "unexpected-file-type")
        self.assertEqual(self.code(fdwalk.read_at, self.fd, b"p", m, 100, b"p", self.dev),
                         "changed-during-observation")

    def test_readlink_at_detects_a_swapped_link(self):
        os.symlink("one", Path(self.d, "l"))
        m = fdwalk.lstat_at(self.fd, b"l", b"l")
        real = os.readlink

        def readlink(name, dir_fd=None):
            out = real(name, dir_fd=dir_fd)
            os.unlink(Path(self.d, "l"))
            os.symlink("two", Path(self.d, "l"))
            return out

        with mock.patch.object(os, "readlink", readlink):
            self.assertEqual(self.code(fdwalk.readlink_at, self.fd, b"l", m, 100, b"l"),
                             "changed-during-observation")
        self.assertEqual(self.code(fdwalk.readlink_at, self.fd, b"l", m._replace(kind="file"), 100, b"l"),
                         "unexpected-file-type")

    def test_walk_bounds_and_special_files(self):
        os.makedirs(Path(self.d, "a/b/c/d"))
        for i in range(5):
            Path(self.d, f"f{i}").write_bytes(b"x")
        self.assertEqual(len(fdwalk.walk(self.fd, self.dev, Limits())), 9)
        self.assertEqual(self.code(fdwalk.walk, self.fd, self.dev, Limits(max_depth=2)), "limit-exceeded")
        self.assertEqual(self.code(fdwalk.walk, self.fd, self.dev, Limits(max_entries=4)), "limit-exceeded")
        self.assertEqual(self.code(fdwalk.walk, self.fd, self.dev, Limits(max_entries=7)), "limit-exceeded")
        self.assertEqual(self.code(fdwalk.walk, self.fd, self.dev + 1, Limits()), "cross-device")
        os.mkfifo(Path(self.d, "a/b/pipe"))
        self.assertEqual(self.code(fdwalk.walk, self.fd, self.dev, Limits()), "special-file")

    def test_paths_under_a_root(self):
        os.makedirs(Path(self.d, "x/y"))
        Path(self.d, "x/y/z").write_bytes(b"zz")
        os.symlink("x", Path(self.d, "lx"))
        self.assertEqual(fdwalk.read_path(self.fd, b"x/y/z", 10, self.dev), b"zz")
        self.assertIsNone(fdwalk.read_path(self.fd, b"x/q/z", 10, self.dev))
        self.assertEqual(self.code(fdwalk.read_path, self.fd, b"lx/y/z", 10, self.dev), "unexpected-file-type")
        self.assertEqual(self.code(fdwalk.read_path, self.fd, b"x/y/z/w", 10, self.dev), "unexpected-file-type")
        self.assertEqual(self.code(fdwalk.read_path, self.fd, b"x/../x/y/z", 10, self.dev), "invalid-name")
        self.assertFalse(fdwalk.dir_is_empty(self.fd, b"x", self.dev, Limits()))
        self.assertTrue(fdwalk.dir_is_empty(self.fd, b"none", self.dev, Limits()))


class IndexParser(unittest.TestCase):
    F = 0o100644

    def parse(self, data, limits=Limits()):
        return iv.parse_index(data, limits)

    def refused(self, data, code):
        with self.assertRaises(Unknown) as cm:
            self.parse(data)
        self.assertEqual(cm.exception.code, code)

    def test_valid_v2_and_v3(self):
        self.assertEqual(set(self.parse(index_bytes([(b"a", self.F, 0, None), (b"b/c", self.F, 0, None)]))),
                         {b"a", b"b/c"})
        self.assertEqual(set(self.parse(index_bytes([(b"a", self.F, 0, 0)], version=3))), {b"a"})
        self.assertEqual(self.parse(None), {})

    def test_malformed(self):
        good = index_bytes([(b"a", self.F, 0, None)])
        self.refused(good[:-1] + bytes([good[-1] ^ 1]), "malformed-index")
        self.refused(b"DIRX" + good[4:-20] + hashlib.sha1(b"DIRX" + good[4:-20]).digest(), "malformed-index")
        self.refused(good[:20], "malformed-index")
        for entries in ([(b"b", self.F, 0, None), (b"a", self.F, 0, None)], [(b"a", self.F, 0, None)] * 2,
                        [(b"../x", self.F, 0, None)], [(b".git/config", self.F, 0, None)],
                        [(b".GIT/config", self.F, 0, None)], [(b"a//b", self.F, 0, None)],
                        [(b"/a", self.F, 0, None)], [(b"a/", self.F, 0, None)], [(b"a", 0o100664, 0, None)],
                        [(b"a", self.F, 0, None), (b"a/b", self.F, 0, None)], [(b"a", 0o40000, 0, None)]):
            self.refused(index_bytes(entries), "malformed-index")
        self.refused(index_bytes([(b"a", self.F, 0, 0)], version=2), "malformed-index")
        body = good[:-20]
        bad_len = bytearray(body)
        bad_len[12 + 61] = 2
        self.refused(bytes(bad_len) + hashlib.sha1(bytes(bad_len)).digest(), "malformed-index")
        pad = bytearray(body)
        pad[-1] = 1
        self.refused(bytes(pad) + hashlib.sha1(bytes(pad)).digest(), "malformed-index")

    def test_unsupported(self):
        self.refused(index_bytes([(b"a", self.F, 0, None)], version=4), "unsupported-index-feature")
        self.refused(index_bytes([(b"a", self.F, 0x8000, None)]), "unsupported-index-feature")
        self.refused(index_bytes([(b"a", self.F, 0, 0x4000)], version=3), "unsupported-index-feature")
        self.refused(index_bytes([(b"a", self.F, 0, 0x2000)], version=3), "unsupported-index-feature")
        self.refused(index_bytes([(b"a", self.F, 0x1000, None)]), "unresolved-conflict")
        self.refused(index_bytes([(b"a", 0o160000, 0, None)]), "submodule")
        for sig in (b"link", b"UNTR", b"FSMN", b"sdir", b"ZZZZ"):
            self.refused(index_bytes([(b"a", self.F, 0, None)], exts=sig + (0).to_bytes(4, "big")),
                         "unsupported-index-feature")
        self.refused(index_bytes([(b"a", self.F, 0, None)], exts=b"TREE" + (99).to_bytes(4, "big")),
                     "malformed-index")
        self.refused(index_bytes([(b"a", self.F, 0, None)] * 0)[:8] + (10 ** 6).to_bytes(4, "big")
                     + hashlib.sha1(b"").digest(), "malformed-index")


class GitParsers(unittest.TestCase):
    def refused(self, fn, *args, code=None):
        with self.assertRaises(Unknown) as cm:
            fn(*args)
        if code:
            self.assertEqual(cm.exception.code, code)

    def test_objects(self):
        oid, raw = loose(b"blob", b"x")
        self.assertEqual(gitraw.parse_object(oid, raw, 100), (b"blob", b"x"))
        self.refused(gitraw.parse_object, b"0" * 40, raw, 100, code="malformed-object")
        self.refused(gitraw.parse_object, oid, raw + b"junk", 100, code="malformed-object")
        self.refused(gitraw.parse_object, oid, raw[:-3], 100, code="malformed-object")
        self.refused(gitraw.parse_object, oid, b"not zlib", 100, code="malformed-object")
        for data in (b"blob 2\0x", b"blob 01\0x", b"blob\0x", b"blob 1x", b"thing 1\0x", b"blob -1\0x"):
            self.refused(gitraw.parse_object, hashlib.sha1(data).hexdigest().encode(), zlib.compress(data), 100,
                         code="malformed-object")
        big, raw_big = loose(b"blob", b"x" * 100)
        self.refused(gitraw.parse_object, big, raw_big, 0, code="limit-exceeded")

    def tree(self, *entries):
        return b"".join(m + b" " + n + b"\0" + bytes(20) for m, n in entries)

    def test_trees(self):
        ok = self.tree((b"100644", b"a"), (b"40000", b"a.d"), (b"100755", b"b.x"), (b"40000", b"b"))
        self.assertEqual([n for n, _, _ in gitraw.parse_tree(b"t", b"tree", ok)], [b"a", b"a.d", b"b.x", b"b"])
        for bad in (self.tree((b"100644", b"b"), (b"100644", b"a")), self.tree((b"100644", b"a"), (b"100644", b"a")),
                    self.tree((b"100664", b"a")), self.tree((b"100644", b"..")), self.tree((b"100644", b".git")),
                    self.tree((b"100644", b"")), self.tree((b"100644", b"a"), (b"40000", b"a")),
                    self.tree((b"40000", b"a"), (b"100644", b"a.b"))[:-1], b"100644 a"):
            self.refused(gitraw.parse_tree, b"t", b"tree", bad, code="malformed-object")
        self.refused(gitraw.parse_tree, b"t", b"blob", ok, code="malformed-object")
        self.refused(gitraw.parse_tree, b"t", b"tree", self.tree((b"160000", b"s")), code="submodule")

    def test_commit(self):
        self.assertEqual(gitraw.parse_commit_tree(b"c", b"commit", b"tree " + b"a" * 40 + b"\nparent x\n"), b"a" * 40)
        for body in (b"parent " + b"a" * 40 + b"\n", b"tree " + b"A" * 40 + b"\n", b"tree " + b"a" * 40):
            self.refused(gitraw.parse_commit_tree, b"c", b"commit", body, code="malformed-object")
        self.refused(gitraw.parse_commit_tree, b"c", b"tree", b"tree " + b"a" * 40 + b"\n", code="malformed-object")

    def test_packed_refs(self):
        a = b"a" * 40
        self.assertEqual(gitraw.parse_packed_refs(b"# pack-refs with: peeled\n" + a + b" refs/heads/x\n^" + a + b"\n"),
                         {b"refs/heads/x": a})
        for bad in (a + b" refs/heads/x", b"^" + a + b"\n", a + b" refs/heads/x\n" + a + b" refs/heads/x\n",
                    a + b"refs/heads/x\n", a + b" heads/x\n", a[:-1] + b" refs/heads/x\n",
                    a + b" refs/heads/x y\n"):
            self.refused(gitraw.parse_packed_refs, bad)

    def test_head_ref_names(self):
        for name in (b"refs/heads/main", b"refs/heads/feat/o5-x_1"):
            self.assertTrue(gitraw.valid_head_ref(name))
        for name in (b"refs/heads/.x", b"refs/heads/x.lock", b"refs/heads/a..b", b"refs/heads/x@{1}",
                     b"refs/heads/x.", b"refs/heads//x", b"refs/tags/x", b"refs/heads/x y", b"refs/heads/"):
            self.assertFalse(gitraw.valid_head_ref(name), name)


class Fuzz(unittest.TestCase):
    """Seeded mutation fuzzing: the parsers may only return a result or raise
    Unknown, never another exception."""
    ROUNDS = 1500

    def mutate(self, rng, data):
        b = bytearray(data)
        for _ in range(rng.randint(1, 4)):
            op = rng.randrange(4)
            if op == 0 and b:
                b[rng.randrange(len(b))] = rng.randrange(256)
            elif op == 1:
                b.insert(rng.randrange(len(b) + 1), rng.randrange(256))
            elif op == 2 and b:
                del b[rng.randrange(len(b)):rng.randrange(len(b) + 1)]
            else:
                b += bytes(rng.randrange(256) for _ in range(rng.randint(1, 8)))
        return bytes(b)

    def run_fuzz(self, seed, seeds, fn, fix_checksum=False):
        rng = random.Random(seed)
        for i in range(self.ROUNDS):
            data = self.mutate(rng, rng.choice(seeds))
            if fix_checksum and rng.random() < 0.7 and len(data) > 20:
                data = data[:-20] + hashlib.sha1(data[:-20]).digest()
            try:
                fn(data)
            except Unknown:
                pass

    def test_index(self):
        F = 0o100644
        seeds = [index_bytes([(b"a", F, 0, None), (b"d/b", 0o100755, 0, None), (b"l", 0o120000, 0, None)]),
                 index_bytes([(b"a", F, 0, 0)], version=3, exts=b"TREE" + (4).to_bytes(4, "big") + b"abcd")]

        def check(data):
            out = iv.parse_index(data, Limits(max_entries=1000))
            for p, (mode, oid) in out.items():
                self.assertTrue(gitraw.valid_path(p) and mode in (F, 0o100755, 0o120000) and len(oid) == 40)

        self.run_fuzz(1, seeds, check, fix_checksum=True)

    def test_config_head_and_refs(self):
        a = b"a" * 40
        self.run_fuzz(2, [b"[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n[remote \"o\"]\n\turl = x\n"],
                      gitraw.parse_config)
        self.run_fuzz(3, [b"ref: refs/heads/main\n", a + b"\n"], gitraw.parse_head)
        self.run_fuzz(4, [b"# pack-refs with: peeled\n" + a + b" refs/heads/x\n^" + a + b"\n"],
                      gitraw.parse_packed_refs)

    def test_objects_and_trees(self):
        tree = b"100644 a\0" + bytes(20) + b"40000 d\0" + bytes(20)
        _, raw = loose(b"tree", tree)
        rng = random.Random(5)

        def obj(data):
            oid = hashlib.sha1(zlib.decompress(data)).hexdigest().encode() if rng.random() < 0.5 else b"0" * 40
            gitraw.parse_object(oid, data, 1 << 16)

        def safe_obj(data):
            try:
                obj(data)
            except zlib.error:
                gitraw.parse_object(b"0" * 40, data, 1 << 16)

        self.run_fuzz(6, [raw], safe_obj)
        self.run_fuzz(7, [tree], lambda d: gitraw.parse_tree(b"t", b"tree", d))
        self.run_fuzz(8, [b"tree " + b"a" * 40 + b"\nauthor x\n"], lambda d: gitraw.parse_commit_tree(b"c", b"commit", d))


class Results(Base):
    def test_status_has_no_truth_value_and_is_immutable(self):
        r, ctx = self.make()
        obs = self.lab.observe(ctx)
        with self.assertRaises(TypeError):
            bool(obs)
        with self.assertRaises(AttributeError):
            obs.status = Status.CLEAN
        self.assertTrue(obs.clean)

    def test_combine_never_upgrades(self):
        r, ctx = self.make()
        clean = self.lab.observe(ctx)
        unknown = iv.Observation(Status.UNKNOWN, [("x", "")])
        changed = iv.Observation(Status.IDENTITY_CHANGED, [("y", "")])
        self.assertIs(iv.combine([unknown, clean]).status, Status.UNKNOWN)
        self.assertIs(iv.combine([clean, unknown, changed]).status, Status.IDENTITY_CHANGED)
        self.lab.git(r, "commit", "-q", "--allow-empty", "-m", "moved")
        other = self.lab.observe(ctx)
        self.assertIs(other.status, Status.CLEAN)
        merged = iv.combine([clean, other])
        self.assertIs(merged.status, Status.UNKNOWN)
        self.assertEqual(merged.reasons[0][0], "observations-disagree")
        with self.assertRaises(TypeError):
            iv.combine([])
        with self.assertRaises(TypeError):
            iv.combine([Status.CLEAN])

    def test_birthtime_binding_per_platform(self):
        r, _ = self.make()
        if HAS_BIRTHTIME:
            obs = self.see(self.lab.ctx(r, require_birthtime=True), Status.CLEAN)
            self.assertTrue(all(x.birth_ns is not None for x in obs.binding[:3]))
        else:
            self.see(self.lab.ctx(r, require_birthtime=True), Status.UNKNOWN, "birthtime-unavailable")
        real = iv._root_id
        with mock.patch.object(iv, "_root_id", lambda st: real(st)._replace(birth_ns=None)):
            self.see(self.lab.ctx(r, require_birthtime=True), Status.UNKNOWN, "birthtime-unavailable")
            obs = self.see(self.lab.ctx(r, require_birthtime=False), Status.CLEAN)
            self.assertTrue(all(x.birth_ns is None for x in obs.binding[:3]))

    def test_platform_without_descriptor_relative_calls(self):
        r, ctx = self.make()
        with mock.patch.object(fdwalk, "SUPPORTED", False):
            self.see(ctx, Status.UNKNOWN, "platform-unsupported")

    def test_invalid_contexts(self):
        r, ctx = self.make()
        for c in (iv.Context("repo", "repo/.git", "repo/.git"), iv.Context(r + "/", r + "//.git", r + "//.git"),
                  iv.Context(r, r + "/.git", self.lab.path("x")), iv.Context(r, r + "/admin", r + "/admin"),
                  iv.Context(r, r + "/inner", self.lab.path("c")), iv.Context(r, r + "/.git", r + "/.git", expected=(1,)),
                  iv.Context("/", "/.git", "/.git")):
            obs = iv.observe(c)
            self.assertEqual((obs.status, obs.reasons[0][0]), (Status.UNKNOWN, "invalid-context"), c)


if __name__ == "__main__":
    unittest.main()

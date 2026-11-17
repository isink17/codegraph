"""The Git byte primitives the observer (`gitraw`) and PR-3b classification
(`gitclass`) share: identical bytes give the same verdict through both, each
in its own error policy (the observer raises Unknown, classification calls
the object corrupt)."""

import hashlib
import unittest
import zlib

import harness  # noqa: F401 - sys.path for broker
from broker import gitbytes, gitclass, gitraw
from broker.fdwalk import Unknown


def loose(raw, oid=None):
    return oid or hashlib.sha1(raw).hexdigest(), zlib.compress(raw)  # noqa: S324 - Git object ids


def both(oid, data, limit=1 << 20):
    try:
        observed = gitraw.parse_object(oid.encode(), data, limit)
    except Unknown as e:
        observed = e.args[0]
    path = b"objects/" + oid[:2].encode() + b"/" + oid[2:].encode()
    real = gitclass.MAX_OBJECT_BYTES
    gitclass.MAX_OBJECT_BYTES = limit
    try:
        classified = gitclass._read_object({path: data}, oid)
    finally:
        gitclass.MAX_OBJECT_BYTES = real
    return observed, classified


class LooseObjects(unittest.TestCase):
    def test_intact_objects_agree(self):
        for raw in (b"blob 6\0hello\n", b"blob 0\0", b"tree 0\0", b"commit 5\0tree ", b"tag 1\0x"):
            oid, data = loose(raw)
            want = (raw.split(b" ")[0], raw.partition(b"\0")[2])
            self.assertEqual(both(oid, data), (want, want), raw)

    def test_malformed_objects_are_refused_by_both(self):
        good_oid, good = loose(b"blob 6\0hello\n")
        cases = {
            "not zlib": (good_oid, b"not zlib"),
            "truncated stream": (good_oid, good[:-4]),
            "trailing data": (good_oid, good + b"x"),
            "another object's bytes": (good_oid, loose(b"blob 6\0howdy\n")[1]),
            "unknown type": loose(b"note 6\0hello\n"),
            "size larger than the body": loose(b"blob 9\0hello\n"),
            "size with a leading zero": loose(b"blob 06\0hello\n"),
            "no space in the header": loose(b"blob6\0hello\n"),
            "header longer than 32 bytes": loose(b"blob " + b"0" * 30 + b"6\0hello\n"),
            "no header terminator": loose(b"blob 6 hello\n"),
        }
        for name, (oid, data) in cases.items():
            with self.subTest(name):
                self.assertEqual(both(oid, data), ("malformed-object", "corrupt"))

    def test_each_reader_keeps_its_own_size_bound(self):
        oid, data = loose(b"blob 100\0" + b"x" * 100)
        self.assertEqual(both(oid, data, limit=200)[1][0], b"blob")
        self.assertEqual(both(oid, data, limit=40), ("limit-exceeded", "corrupt"))
        # 109 inflated bytes: classification accepts at most MAX_OBJECT_BYTES,
        # the observer fewer than its limit + 64.
        self.assertEqual(both(oid, data, limit=109)[1][0], b"blob")
        self.assertEqual(both(oid, data, limit=108)[1], "corrupt")
        self.assertEqual(both(oid, data, limit=46)[0][0], b"blob")
        self.assertEqual(both(oid, data, limit=45)[0], "limit-exceeded")


class GitdirNames(unittest.TestCase):
    def test_both_readers_use_one_predicate(self):
        for name in (b".git", b".GIT", b".Git", ".g\u200cit".encode(), "\ufeff.git".encode()):
            self.assertTrue(gitbytes.is_gitdir_name(name), name)
            self.assertFalse(gitraw.valid_component(name), name)
            self.assertTrue(gitclass.is_git_path(b"sub/" + name + b"/HEAD"), name)
        for name in (b".gitignore", b".github", b"git", b"x.git"):
            self.assertFalse(gitbytes.is_gitdir_name(name), name)
            self.assertTrue(gitraw.valid_component(name), name)

    def test_only_the_exact_name_is_a_repository(self):
        for name in (b".GIT", b".Git", ".g\u200cit".encode(), "\ufeff.git".encode()):
            repos = gitclass.repositories({b"sub/" + name + b"/HEAD": b"ref: refs/heads/main\n"})
            self.assertIn("unknown", repos[b"sub"].features, name)
        repos = gitclass.repositories({b"sub/.git/HEAD": b"ref: refs/heads/main\n"})
        self.assertNotIn("unknown", repos[b"sub"].features)


if __name__ == "__main__":
    unittest.main()

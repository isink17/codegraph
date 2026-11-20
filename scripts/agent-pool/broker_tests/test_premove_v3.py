"""Pre-move eligibility over the typed manifest: the `journal/v3` grammar and
final head, the exact `freeze-start` NONE shapes, `originals/v1` evidence
read back from its bytes, the typed Git classifier (`gitdir_shape`, the
narrowed `bare_shape`, NB-13 and B-1 unrelaxed), the refusal codes of
malformed, absent and ineligible evidence, and v2/v3 schema isolation.
Every input is built in memory; nothing reads a live path or writes a store."""

import ast
import hashlib
import inspect
import textwrap
import types
import unicodedata
import unittest
from pathlib import Path
from unittest import mock

import harness  # noqa: F401 - sys.path for broker
import preserve_world as pw
import test_static
from broker import canonical, fdwalk, freeze, gitclass, gitraw, indexverify, journal, o5contract as oc, preserve
from broker import protocol
from broker.journal import Mismatch
from broker.refusal import Code
from test_manifest_v3 import HFS_IGNORABLE, PRE3, ROOT, Lab, d, f, link, originals_blobs, totals, typed, u
from test_observer import index_bytes
from test_preserve_units import CID, SCTX, header

V3 = journal.J_SCHEMA_V3
H = canonical.sha256
MOVE = {"src_parent": H(b"slot-parent"), "src_name": b"slot".hex(), "dst_parent": H(b"quarantine/op"),
        "dst_name": b"tree".hex(), "root": ROOT}
BACK = {"src_parent": MOVE["dst_parent"], "src_name": MOVE["dst_name"], "dst_parent": MOVE["src_parent"],
        "dst_name": MOVE["src_name"], "root": ROOT}
REPO = pw.git_repo()[0]
WORKTREE = {b"README": b"hello\n", b"main.go": b"package main\n"}


class J3:
    """A `journal/v3` written record by record into a test-owned resolver."""

    def __init__(self, mem, cid=CID, root=ROOT):
        self.mem, self.cid, self.root = mem, cid, root
        self.seq, self.prev, self.early, self.intent, self.back = -1, oc.ZERO64, None, None, None

    def add(self, kind, schema=V3, raw=None, early=None, **fields):
        self.seq += 1
        rec = dict({"schema": schema, "header": header(self.cid, root_id=self.root), "seq": self.seq,
                    "prev": self.prev, "kind": kind}, **fields)
        if self.seq == 1:
            self.early = fields.get("binding", oc.ZERO64)
        if self.seq:
            rec["early"] = early or self.early
        data = raw if raw is not None else canonical.canon(rec)
        self.mem.j[(self.cid, self.seq)] = data
        self.prev = H(data)
        return self.prev

    def start(self):
        return self.add("start")

    def ep(self, binding):
        return self.add("ep.done", binding=binding)

    def move(self, **kw):
        self.intent = self.add("move.intent", **dict(MOVE, **kw))
        return self.intent

    def moved(self):
        return self.add("move.done", intent=self.intent, linked=[])

    def abort(self, reason):
        return self.add("move.abort", intent=self.intent, reason=reason)

    def end(self, outcome="none", reason=""):
        return self.add("end", outcome=outcome, reason=reason)

    def revoke(self, n, originals, attrs=journal.REVOKE_ORDER, done=True):
        for a in attrs:
            for lo in range(0, n + 1, journal.MAX_RANGE):
                hi = min(lo + journal.MAX_RANGE - 1, n)
                self.add("revoke.intent", attr=a, lo=lo, hi=hi, originals=originals[a])
                if done:
                    self.add("revoke.done", attr=a, lo=lo, hi=hi)
        return self.prev

    def thaw(self, ranges):
        for a, lo, hi in ranges:
            self.add("thaw.intent", attr=a, lo=lo, hi=hi)
            self.add("thaw.done", attr=a, lo=lo, hi=hi)
        self.back = self.add("thaw.move.intent", **BACK)
        self.add("thaw.move.done", intent=self.back)
        return self.end("thawed")


def ctx(pre=PRE3, cycle_id=None, **kw):
    return types.SimpleNamespace(pre=types.SimpleNamespace(**dict(vars(pre), cycle_id=cycle_id, stop=False, **kw)),
                                 digest=SCTX)


class Base(unittest.TestCase):
    def setUp(self):
        self.lab = Lab()
        self.mem = self.lab.mem
        self.j = J3(self.mem)
        self.prov = preserve.Preservation(self.mem)
        self.cy = self.lab.cy
        self.prov._cycle = lambda pre: self.cy

    def ep(self, files=None, entries=None, **kw):
        """An EP binding over `files` (a {path: bytes} tree) with valid originals."""
        files = REPO if files is None else files
        entries = typed(files) if entries is None else entries
        nbytes = self.lab.blobs(files.values())
        return self.lab.binding(entries, "early", totals=totals(entries, nbytes), **kw), entries

    def check(self, verb, value, reason="", **bindings):
        """freeze-start derives its cycle; every later verb reads the bound one."""
        c = ctx(cycle_id=None if verb == "freeze-start" else CID)
        self.prov._check_fresult(c, verb, oc.FResult(value, reason, bindings), CID)

    def ok(self, verb, value, reason="", **bindings):
        self.check(verb, value, reason, **bindings)

    def bad(self, fn, detail, code=None):
        with self.assertRaises(Mismatch) as e:
            fn()
        self.assertEqual(e.exception.detail, detail)
        if code is not None:
            self.assertIs(e.exception.code, code)

    def rejected(self, verb, value, reason="", detail="journal_mismatch", **bindings):
        self.bad(lambda: self.check(verb, value, reason, **bindings), detail)

    def walk(self, head):
        return self.cy.journal(head)


# ------------------------------------------------------------------ journal/v3 grammar

class Grammar(Base):
    def full(self, files=None):
        """A complete cycle: EP, move, revoke of all four attributes, `revoked`."""
        ep, es = self.ep(files)
        self.j.start()
        self.j.ep(ep)
        self.j.move()
        moved = self.j.moved()
        revoked = self.j.revoke(len(es), self.lab.originals(es))
        revoked = self.j.add("revoked", escaped=[])
        return ep, es, moved, revoked

    def test_complete_cycle_walks_with_the_merged_fields(self):
        ep, es, moved, head = self.full()
        j = self.walk(head)
        self.assertTrue(j.v3 and j.moved and j.revoked and not j.end)
        self.assertEqual((j.early, j.ep, j.linked, j.escaped), (ep, ep, [], []))
        self.assertEqual(j.revoked_attrs, self.lab.originals(es))
        self.assertEqual(j.thawed, set())
        journal.require_revoked(j)
        self.assertEqual(journal.escaped_state(j, {e[0] for e in es}), "empty")
        self.assertFalse(journal.thaw_complete(j))
        self.assertTrue(self.walk(moved).moved)
        self.assertFalse(self.walk(moved).revoked)

    def test_every_merged_reader_field_is_present(self):
        _, _, _, head = self.full()
        v2 = journal.View([{"kind": "start"}], "0" * 64)
        v3 = self.walk(head)
        for name in vars(v2):
            self.assertTrue(hasattr(v3, name), name)

    def negative(self, build, detail="journal_mismatch", pure=True):
        """Refused through the cycle; with `pure`, also by the walker alone,
        before any rule that needs the EP."""
        head = build()
        self.bad(lambda: self.walk(head), detail)
        if pure:
            self.bad(lambda: journal.walk(self.mem, ctx().pre, head, CID, SCTX), detail)

    def test_first_records(self):
        ep, _ = self.ep()
        for build in (lambda: self.j.add("ep.done", binding=ep),
                      lambda: (self.j.start(), self.j.start())[1],
                      lambda: (self.j.start(), self.j.move())[1],
                      lambda: (self.j.start(), self.j.ep(ep), self.j.ep(ep))[2],
                      lambda: (self.j.start(), self.j.add("move.done", intent=H(b"x"), linked=[]))[1],
                      lambda: (self.j.start(), self.j.end("thawed"))[1]):
            with self.subTest(build):
                self.setUp()
                ep, _ = self.ep()
                self.negative(build)

    def test_early_is_fixed_by_seq_1(self):
        ep, _ = self.ep()
        self.j.start()
        self.negative(lambda: self.j.add("ep.done", binding=ep, early=H(b"other")))
        self.setUp()
        self.j.start()
        self.negative(lambda: self.j.add("end", outcome="none", reason="identity", early=H(b"x")))
        self.setUp()
        ep, _ = self.ep()
        self.j.start()
        self.j.ep(ep)
        self.negative(lambda: self.j.add("move.intent", early=H(b"x"), **MOVE))

    def test_move_intent_rules(self):
        ep, _ = self.ep()
        self.j.start()
        self.j.ep(ep)
        self.negative(lambda: self.j.move(root=H(b"another root")))
        self.setUp()
        ep, _ = self.ep()
        self.j.start()
        self.j.ep(ep)
        self.j.move()
        self.negative(self.j.move)  # a second intent
        for bad in ({"src_name": "2f".join(["61", "62"])}, {"dst_name": ""}, {"src_parent": "X" * 64},
                    {"dst_name": b"..".hex()}, {"root": 7}):
            with self.subTest(bad):
                self.setUp()
                ep, _ = self.ep()
                self.j.start()
                self.j.ep(ep)
                self.negative(lambda: self.j.move(**bad))

    def test_move_outcome_names_its_intent(self):
        for kind, extra in (("move.done", {"linked": []}), ("move.abort", {"reason": "rename_failed"})):
            with self.subTest(kind):
                self.setUp()
                ep, _ = self.ep()
                self.j.start()
                self.j.ep(ep)
                self.j.move()
                self.negative(lambda: self.j.add(kind, intent=H(b"another"), **extra))
        self.setUp()
        ep, _ = self.ep()
        self.j.start()
        self.j.ep(ep)
        self.j.move()
        self.negative(lambda: self.j.add("move.done", intent=self.j.intent, linked=[b"x".hex()]))
        for reason in ("stale_barrier", "identity", 3):
            with self.subTest(reason):
                self.setUp()
                ep, _ = self.ep()
                self.j.start()
                self.j.ep(ep)
                self.j.move()
                self.negative(lambda: self.j.abort(reason))

    def test_after_an_abort_only_its_end(self):
        for nxt in (lambda: self.j.end(reason="identity"), lambda: self.j.end("thawed"),
                    lambda: self.j.add("move.done", intent=self.j.intent, linked=[]), self.j.move):
            with self.subTest(nxt):
                self.setUp()
                ep, _ = self.ep()
                self.j.start()
                self.j.ep(ep)
                self.j.move()
                self.j.abort("rename_failed")
                self.negative(nxt)

    def test_nothing_after_end(self):
        self.j.start()
        self.j.end(reason="identity")
        self.negative(lambda: self.j.end(reason="identity"))

    def test_end_reason_and_outcome_are_closed(self):
        for outcome, reason in (("none", ""), ("none", "Identity"), ("thawed", "x"), ("done", ""), ("none", 7),
                                (["none"], "identity")):
            with self.subTest((outcome, reason)):
                self.setUp()
                self.j.start()
                self.negative(lambda: self.j.add("end", outcome=outcome, reason=reason))

    def test_end_none_only_before_any_move(self):
        ep, _ = self.ep()
        self.j.start()
        self.j.ep(ep)
        self.j.move()
        self.j.moved()
        self.negative(lambda: self.j.end(reason="identity"))

    def revoke_prefix(self):
        ep, es = self.ep()
        self.j.start()
        self.j.ep(ep)
        self.j.move()
        self.j.moved()
        return len(es), self.lab.originals(es)

    def test_revoke_needs_the_move(self):
        ep, es = self.ep()
        self.j.start()
        self.j.ep(ep)
        o = self.lab.originals(es)
        self.negative(lambda: self.j.add("revoke.intent", attr="flags", lo=0, hi=len(es), originals=o["flags"]))

    def test_revoke_attribute_order_and_contiguity(self):
        n, o = self.revoke_prefix()
        cases = [
            lambda: self.j.add("revoke.intent", attr="acl", lo=0, hi=n, originals=o["acl"]),
            lambda: self.j.add("revoke.intent", attr="flags", lo=1, hi=n, originals=o["flags"]),
            lambda: (self.j.add("revoke.intent", attr="flags", lo=0, hi=0, originals=o["flags"]),
                     self.j.add("revoke.done", attr="flags", lo=0, hi=0),
                     self.j.add("revoke.intent", attr="flags", lo=2, hi=n, originals=o["flags"]))[2],
            lambda: (self.j.revoke(n, o, ("flags",)),
                     self.j.add("revoke.intent", attr="mode", lo=0, hi=n, originals=o["mode"]))[1],
            lambda: (self.j.add("revoke.intent", attr="flags", lo=0, hi=0, originals=o["flags"]),
                     self.j.add("revoke.intent", attr="flags", lo=1, hi=n, originals=o["flags"]))[1],
            lambda: (self.j.add("revoke.intent", attr="flags", lo=0, hi=0, originals=o["flags"]),
                     self.j.add("revoke.done", attr="flags", lo=0, hi=1))[1],
            lambda: self.j.add("revoke.done", attr="flags", lo=0, hi=n),
            lambda: self.j.add("revoke.intent", attr="flags", lo=0, hi=journal.MAX_RANGE, originals=o["flags"]),
            lambda: self.j.add("revoke.intent", attr="flags", lo=3, hi=2, originals=o["flags"]),
            lambda: self.j.add("revoke.intent", attr="flags", lo=0, hi=True, originals=o["flags"]),
            lambda: (self.j.add("revoke.intent", attr="flags", lo=0, hi=0, originals=o["flags"]),
                     self.j.add("revoke.done", attr="flags", lo=0, hi=0),
                     self.j.add("revoke.intent", attr="flags", lo=1, hi=n, originals=o["acl"]))[2],
        ]
        for i, build in enumerate(cases):
            with self.subTest(i):
                self.setUp()
                n, o = self.revoke_prefix()
                self.negative(build)

    def test_ranges_must_cover_n_before_the_next_attribute(self):
        """`check_v3`: an attribute stops short of N and the next one starts."""
        n, o = self.revoke_prefix()
        self.j.add("revoke.intent", attr="flags", lo=0, hi=n - 1, originals=o["flags"])
        self.j.add("revoke.done", attr="flags", lo=0, hi=n - 1)
        self.negative(lambda: self.j.revoke(n, o, ("acl",)), pure=False)

    def test_ordinals_never_exceed_n(self):
        n, o = self.revoke_prefix()
        self.negative(lambda: self.j.add("revoke.intent", attr="flags", lo=0, hi=n + 1, originals=o["flags"]),
                      pure=False)

    def test_revoke_intent_names_the_ep_originals(self):
        n, o = self.revoke_prefix()
        self.negative(lambda: self.j.add("revoke.intent", attr="flags", lo=0, hi=n, originals=H(b"other")),
                      pure=False)

    def test_revoked_needs_all_four_complete(self):
        for attrs in (journal.REVOKE_ORDER[:3], ()):
            with self.subTest(attrs):
                self.setUp()
                n, o = self.revoke_prefix()
                self.j.revoke(n, o, attrs)
                self.negative(lambda: self.j.add("revoked", escaped=[]))
        self.setUp()
        n, o = self.revoke_prefix()
        self.j.revoke(n, o, journal.REVOKE_ORDER[:3])
        self.j.add("revoke.intent", attr="owner", lo=0, hi=n - 1, originals=o["owner"])
        self.j.add("revoke.done", attr="owner", lo=0, hi=n - 1)
        self.negative(lambda: self.j.add("revoked", escaped=[]), pure=False)
        self.setUp()
        n, o = self.revoke_prefix()
        self.j.revoke(n, o)
        self.j.add("revoked", escaped=[])
        self.negative(lambda: self.j.add("revoked", escaped=[]))

    def test_thaw_rules(self):
        n, o = self.revoke_prefix()
        self.j.revoke(n, o)
        self.j.add("revoked", escaped=[])
        good = [(a, 0, n) for a in journal.THAW_ORDER]
        head = self.j.thaw(good)
        j = self.walk(head)
        self.assertTrue(journal.thaw_complete(j))
        self.assertEqual(j.thawed, set(journal.ATTRS) | {"move"})
        for ranges in (good[::-1], good[:3], [(a, 0, n - 1) for a in journal.THAW_ORDER]):
            with self.subTest(ranges):
                self.setUp()
                n, o = self.revoke_prefix()
                self.j.revoke(n, o)
                self.j.add("revoked", escaped=[])
                self.negative(lambda: self.j.thaw(ranges))

    def test_thaw_needs_the_move_and_its_exact_inverse(self):
        ep, _ = self.ep()
        self.j.start()
        self.j.ep(ep)
        self.negative(lambda: self.j.add("thaw.move.intent", **BACK))
        for bad in (MOVE, dict(BACK, root=H(b"x")), dict(BACK, dst_name=b"other".hex())):
            with self.subTest(bad):
                self.setUp()
                self.revoke_prefix()
                self.negative(lambda: self.j.add("thaw.move.intent", **bad))
        self.setUp()
        self.revoke_prefix()
        back = self.j.add("thaw.move.intent", **BACK)
        self.negative(lambda: self.j.add("thaw.move.done", intent=H(back.encode())))
        self.setUp()
        self.revoke_prefix()
        self.j.add("thaw.move.intent", **BACK)
        self.negative(lambda: self.j.end("thawed"))

    def test_no_revoke_once_thaw_started(self):
        n, o = self.revoke_prefix()
        self.j.revoke(n, o, ("flags",))
        self.j.add("thaw.intent", attr="flags", lo=0, hi=n)
        self.j.add("thaw.done", attr="flags", lo=0, hi=n)
        self.negative(lambda: self.j.revoke(n, o, ("acl",)))

    def test_thaw_of_an_open_revoke_range(self):
        """Thaw after FREEZE_INCOMPLETE: the open range is thawed too."""
        n, o = self.revoke_prefix()
        self.j.revoke(n, o, ("flags",))
        self.j.add("revoke.intent", attr="acl", lo=0, hi=n, originals=o["acl"])
        head = self.j.thaw([("acl", 0, n), ("flags", 0, n)])
        j = self.walk(head)
        self.assertTrue(journal.thaw_complete(j))
        self.assertEqual(j.ranges["acl"], [(0, n, False)])
        self.setUp()
        n, o = self.revoke_prefix()
        self.j.revoke(n, o, ("flags",))
        self.j.add("revoke.intent", attr="acl", lo=0, hi=n, originals=o["acl"])
        self.negative(lambda: self.j.thaw([("flags", 0, n)]))
        self.setUp()
        n, o = self.revoke_prefix()
        self.j.add("revoke.intent", attr="flags", lo=0, hi=n, originals=o["flags"])
        self.negative(lambda: self.j.add("thaw.move.intent", **BACK))

    def test_records_are_closed(self):
        ep, _ = self.ep()
        for build in (lambda: self.j.add("start", extra=1),
                      lambda: self.j.add("start", schema=journal.J_SCHEMA_V3 + "x"),
                      lambda: (self.j.start(), self.j.add("ep.done", binding=ep, extra=1))[1],
                      lambda: (self.j.start(), self.j.add("revoke.done", attr="flags", original=H(b"x")))[1],
                      lambda: (self.j.start(), self.j.add("teleport"))[1]):
            with self.subTest(build):
                self.setUp()
                ep, _ = self.ep()
                self.negative(build)

    def test_layout_prev_seq_and_schema(self):
        """The chain, the `seq` value and the schema of every record."""
        for build in (lambda: (self.j.start(), self._raw(1, prev=H(b"not the start")))[1],
                      lambda: self._raw(0, kind="start", prev=H(b"x"), early=None),
                      lambda: (self.j.start(), self._raw(1, seq=2))[1],
                      lambda: (self.j.start(), self._raw(1, schema=journal.J_SCHEMA))[1]):
            with self.subTest(build):
                self.setUp()
                self.negative(build)

    def _raw(self, key, kind="end", prev=None, seq=None, schema=V3, early=oc.ZERO64):
        """Writes one record at `key` with chosen layout fields."""
        rec = {"schema": schema, "header": header(CID, root_id=ROOT), "seq": key if seq is None else seq,
               "prev": self.j.prev if prev is None else prev, "kind": kind}
        if kind == "end":
            rec.update(outcome="none", reason="identity")
        if early is not None and key:
            rec["early"] = early
        data = canonical.canon(rec)
        self.mem.j[(CID, key)] = data
        return H(data)

    def test_attribute_outside_the_closed_set(self):
        n, o = self.revoke_prefix()
        self.negative(lambda: self.j.add("revoke.intent", attr="xattr", lo=0, hi=n, originals=o["flags"]))

    def test_thaw_pairing(self):
        """Each `thaw.intent` is followed by its own `thaw.done` before the next."""
        cases = [
            lambda n: (self.j.add("thaw.intent", attr="owner", lo=0, hi=n),
                       self.j.add("thaw.intent", attr="owner", lo=0, hi=n))[1],
            lambda n: (self.j.add("thaw.intent", attr="owner", lo=0, hi=n),
                       self.j.add("thaw.done", attr="owner", lo=0, hi=n - 1))[1],
            lambda n: (self.j.add("thaw.intent", attr="owner", lo=0, hi=n),
                       self.j.add("thaw.done", attr="mode", lo=0, hi=n))[1],
            lambda n: self.j.add("thaw.done", attr="owner", lo=0, hi=n),
        ]
        for i, build in enumerate(cases):
            with self.subTest(i):
                self.setUp()
                n, o = self.revoke_prefix()
                self.j.revoke(n, o)
                self.j.add("revoked", escaped=[])
                self.negative(lambda: build(n))

    def test_move_back_once(self):
        for extra in ("intent", "done"):
            with self.subTest(extra):
                self.setUp()
                self.revoke_prefix()
                back = self.j.add("thaw.move.intent", **BACK)
                if extra == "intent":
                    self.negative(lambda: self.j.add("thaw.move.intent", **BACK))
                else:
                    self.j.add("thaw.move.done", intent=back)
                    self.negative(lambda: self.j.add("thaw.move.done", intent=back))

    def test_boolean_seq_is_not_an_integer(self):
        self.j.start()
        data = canonical.canon({"schema": V3, "header": header(CID, root_id=ROOT), "seq": True, "prev": self.j.prev,
                                "kind": "end", "outcome": "none", "reason": "identity", "early": oc.ZERO64})
        self.mem.j[(CID, 1)] = data
        self.bad(lambda: self.walk(H(data)), "journal_mismatch")


class SchemaIsolation(Base):
    def test_one_schema_per_journal(self):
        self.j.start()
        head = self.j.add("end", schema=journal.J_SCHEMA)
        self.bad(lambda: self.walk(head), "journal_mismatch")
        self.setUp()
        self.j.add("start", schema=journal.J_SCHEMA)
        head = self.j.add("move.done", schema=V3, linked=[], intent=H(b"x"))
        self.bad(lambda: self.walk(head), "journal_mismatch")

    def test_mode_sets(self):
        self.assertEqual(journal.accepted_journals(oc.PROTECTED), (V3,))
        self.assertEqual(journal.accepted_journals(oc.SYNTHETIC), (journal.J_SCHEMA, V3))

        class Forged(str):
            pass
        for mode in (Forged(oc.SYNTHETIC), Forged(oc.PROTECTED), "", None, oc.PROTECTED.lower() + "_"):
            self.assertEqual(journal.accepted_journals(mode), ())

    def test_v3_journal_with_a_v2_manifest(self):
        md = self.lab.put({"schema": preserve.M_SCHEMA, "header": self.lab.hdr, "phase": "early",
                           "entry_index": [], "result": "COMPLETE", "reasons": [], "originals": {}})
        bs = self.lab.put({"header": self.lab.hdr, "set": []})
        b = self.lab.put({"header": self.lab.hdr, "cycle_id": CID, "manifest": md, "blobs": bs})
        self.j.start()
        head = self.j.ep(b)
        self.bad(lambda: self.walk(head), "artifact_mismatch")
        self.j.end(reason="not_preservable")
        self.rejected("freeze-start", "NONE", "not_preservable", "artifact_mismatch", early=b,
                      journal=self.j.prev)

    def test_protected_mode_walks_v3_only(self):
        rec = {"schema": journal.J_SCHEMA, "header": header(CID, root_id=ROOT, mode=oc.PROTECTED), "seq": 0,
               "prev": oc.ZERO64, "kind": "start"}
        self.mem.j[(CID, 0)] = canonical.canon(rec)
        pre = types.SimpleNamespace(**dict(vars(PRE3), mode=oc.PROTECTED))
        self.bad(lambda: journal.walk(self.mem, pre, H(self.mem.j[(CID, 0)]), CID, SCTX), "journal_mismatch")
        rec["schema"] = V3
        self.mem.j[(CID, 0)] = canonical.canon(rec)
        self.assertTrue(journal.walk(self.mem, pre, H(self.mem.j[(CID, 0)]), CID, SCTX).v3)

    def test_v2_journal_never_carries_a_none(self):
        """A merged v2 cycle has no shape for any freeze-start NONE."""
        mem = self.mem
        rec = {"schema": journal.J_SCHEMA, "header": header(CID, root_id=ROOT), "seq": 0, "prev": oc.ZERO64,
               "kind": "start"}
        mem.j[(CID, 0)] = canonical.canon(rec)
        head = H(mem.j[(CID, 0)])
        for reason in preserve.NONE_SHAPES:
            with self.subTest(reason):
                self.rejected("freeze-start", "NONE", reason, journal=head)
        # A v2 `start`, `end` has the kinds of the v3 pre-EP shape.
        end = dict(rec, seq=1, prev=head, kind="end", early=H(b"early"))
        mem.j[(CID, 1)] = canonical.canon(end)
        for reason in ("identity", "mount"):
            with self.subTest(("v2 end", reason)):
                self.rejected("freeze-start", "NONE", reason, journal=H(mem.j[(CID, 1)]))


# ------------------------------------------------------------------ freeze-start NONE

class NoneShapes(Base):
    def shape(self, kinds, reason, binding=None):
        """Writes the journal of `kinds` ending in `end{none, reason}`; returns bindings."""
        ep = binding
        self.j.start()
        if "ep.done" in kinds:
            ep = ep or self.ep()[0]
            self.j.ep(ep)
        if "move.intent" in kinds:
            self.j.move()
            self.j.abort(reason)
        self.j.end(reason=reason)
        return dict({"journal": self.j.prev}, **({"early": ep} if "ep.done" in kinds else {}))

    def test_every_row_is_accepted_with_exactly_its_shape(self):
        for reason, shapes in preserve.NONE_SHAPES.items():
            for kinds in shapes:
                with self.subTest((reason, kinds)):
                    self.setUp()
                    self.ok("freeze-start", "NONE", reason, **self.shape(kinds, reason))

    def test_every_other_shape_is_rejected(self):
        for reason, shapes in preserve.NONE_SHAPES.items():
            for kinds in {preserve._PRE_EP, preserve._AFTER_EP, preserve._ABORTED} - set(shapes):
                with self.subTest((reason, kinds)):
                    self.setUp()
                    b = self.shape(kinds, reason)
                    self.bad(lambda: self.check("freeze-start", "NONE", reason, **b), "journal_mismatch")

    def test_store_failure_only_before_seq_0(self):
        for reason in ("store_space", "store_error"):
            with self.subTest(reason):
                self.setUp()
                self.ok("freeze-start", "NONE", reason)
                self.j.start()
                self.rejected("freeze-start", "NONE", reason)
                self.rejected("freeze-start", "NONE", reason, journal=self.j.prev)

    def test_none_without_its_end_is_rejected(self):
        """Every shape with its `end` removed, at a final head: nothing moved
        is never inferred from a journal that does not say so."""
        cases = [("identity", lambda ep: self.j.start()),
                 ("mount", lambda ep: self.j.start())]
        for reason in ("not_preservable", "identity", "early_unstable"):
            cases.append((reason, lambda ep: (self.j.start(), self.j.ep(ep))[1]))
        for reason in ("rename_failed", "target_exists", "early_unstable"):
            cases.append((reason, lambda ep, r=reason: (self.j.start(), self.j.ep(ep), self.j.move(),
                                                         self.j.abort(r))[3]))
        for reason, build in cases:
            with self.subTest(reason):
                self.setUp()
                ep, _ = self.ep()
                head = build(ep)
                b = {"journal": head} if self.j.seq < 1 else {"journal": head, "early": ep}
                self.bad(lambda: self.check("freeze-start", "NONE", reason, **b), "journal_mismatch")

    def test_store_failure_never_with_bindings(self):
        for reason in ("store_space", "store_error"):
            for b in ({"journal": H(b"x")}, {"early": H(b"x")}, {"journal": H(b"x"), "early": H(b"y")}):
                with self.subTest((reason, b)):
                    self.setUp()
                    self.rejected("freeze-start", "NONE", reason, **b)

    def test_unsupported_layout_is_never_accepted(self):
        b = self.shape(preserve._AFTER_EP, "not_preservable")
        self.rejected("freeze-start", "NONE", "unsupported_layout", "journal_required", **b)

    def test_end_reason_is_the_result_reason(self):
        b = self.shape(preserve._PRE_EP, "identity")
        self.rejected("freeze-start", "NONE", "mount", **b)
        self.setUp()
        b = self.shape(preserve._ABORTED, "rename_failed")
        self.rejected("freeze-start", "NONE", "target_exists", **b)

    def test_early_binding_is_exact(self):
        b = self.shape(preserve._PRE_EP, "identity")
        self.rejected("freeze-start", "NONE", "identity", **dict(b, early=H(b"x")))
        for reason in ("identity", "not_preservable", "rename_failed"):
            with self.subTest(reason):
                self.setUp()
                kinds = preserve.NONE_SHAPES[reason][-1]
                b = self.shape(kinds, reason)
                self.rejected("freeze-start", "NONE", reason, **{k: v for k, v in b.items() if k != "early"})
                other, _ = self.ep(files={b"x": b"y"})
                self.bad(lambda: self.check("freeze-start", "NONE", reason, **dict(b, early=other)),
                         "journal_mismatch")

    def test_journal_binding_is_required_and_closed(self):
        b = self.shape(preserve._AFTER_EP, "not_preservable")
        self.rejected("freeze-start", "NONE", "not_preservable", "journal_required", early=b["early"])
        self.rejected("freeze-start", "NONE", "not_preservable", "fresult_unlisted", final=H(b"f"), **b)

    def test_head_must_be_final(self):
        """A NONE bound to an earlier head while a later record exists."""
        self.j.start()
        early_head = self.j.end(reason="identity")
        self.mem.j[(CID, 2)] = canonical.canon({"x": 1})
        self.rejected("freeze-start", "NONE", "identity", journal=early_head)
        self.setUp()
        ep, _ = self.ep()
        self.j.start()
        at_ep = self.j.ep(ep)
        self.j.end(reason="not_preservable")
        self.rejected("freeze-start", "NONE", "not_preservable", early=ep, journal=at_ep)

    def test_none_at_move_intent_or_after_the_move_is_rejected(self):
        """Nothing moved needs an abort and an end."""
        ep, es = self.ep()
        self.j.start()
        self.j.ep(ep)
        at_intent = self.j.move()
        for reason in ("rename_failed", "early_unstable", "not_preservable", "identity"):
            self.rejected("freeze-start", "NONE", reason, early=ep, journal=at_intent)
        moved = self.j.moved()
        for reason in ("rename_failed", "not_preservable", "identity"):
            self.rejected("freeze-start", "NONE", reason, early=ep, journal=moved)

    def test_not_preservable_reassesses_the_ep(self):
        """The re-assessment must not be eligible. While NB-13 and B-1 stay
        unrelaxed no v3 EP is, so it is forced here."""
        b = self.shape(preserve._AFTER_EP, "not_preservable")
        self.ok("freeze-start", "NONE", "not_preservable", **b)
        eligible = (["symref"], "VERIFIED", "COMPLETE", "ELIGIBLE", [], "a" * 40)
        with mock.patch.object(gitclass, "questions", lambda *a: eligible):
            self.rejected("freeze-start", "NONE", "not_preservable", "preservable", **b)

    def test_not_preservable_over_an_irreversible_ep_is_accepted(self):
        """A partial originals set is valid evidence of irreversibility."""
        o = self.lab.originals(typed(REPO))
        ep, _ = self.ep(originals={k: v for k, v in o.items() if k != "acl"})
        b = self.shape(preserve._AFTER_EP, "not_preservable", ep)
        with mock.patch.object(gitclass, "questions",
                               lambda *a: (["symref"], "VERIFIED", "COMPLETE", "ELIGIBLE", [], "a" * 40)):
            self.ok("freeze-start", "NONE", "not_preservable", **b)

    def test_resume_after_end_is_idempotent(self):
        """Every reason re-validates to the same
        acceptance and the check writes nothing."""
        for reason, shapes in preserve.NONE_SHAPES.items():
            with self.subTest(reason):
                self.setUp()
                b = self.shape(shapes[-1], reason)
                before = (dict(self.mem.j), dict(self.mem.a), dict(self.mem.b))
                self.ok("freeze-start", "NONE", reason, **b)
                self.ok("freeze-start", "NONE", reason, **b)
                self.assertEqual((self.mem.j, self.mem.a, self.mem.b), before)

    def test_none_is_never_a_failure_row(self):
        """Routed through `_FAILURE_ROWS` (the mutant), a NONE bound to a
        journal that shows `move.done` would be accepted; the real check rejects it."""
        ep, _ = self.ep()
        self.j.start()
        self.j.ep(ep)
        self.j.move()
        head = self.j.moved()
        self.rejected("freeze-start", "NONE", "identity", early=ep, journal=head)
        with mock.patch.object(preserve.Preservation, "_check_none", lambda *a: None):
            self.ok("freeze-start", "NONE", "identity", early=ep, journal=head)
        self.assertNotIn(("freeze-start", "NONE"), preserve._FAILURE_ROWS)
        src = inspect.getsource(preserve.Preservation._check_fresult)
        self.assertLess(src.index('("freeze-start", "NONE")'), src.index("_FAILURE_ROWS"))

    def test_relation_admits_not_preservable_without_bindings(self):
        self.assertIn("not_preservable", freeze.RELATION["freeze-start"][2]["NONE"][0])
        self.assertEqual(freeze.RELATION["freeze-start"][2]["NONE"][1], ())
        self.assertLessEqual(set(preserve.NONE_SHAPES) | {"store_space", "store_error"},
                             set(freeze.RELATION["freeze-start"][2]["NONE"][0]))
        self.assertNotIn("unsupported_layout", preserve.NONE_SHAPES)


# ------------------------------------------------------------------ MOVED, REVOKED and thaw over v3

class AfterTheMove(Base):
    def moved(self, files=None, **kw):
        ep, es = self.ep(files, **kw)
        self.j.start()
        self.j.ep(ep)
        self.j.move()
        return ep, es, self.j.moved()

    def test_moved_is_never_eligible_before_the_relaxation(self):
        ep, _, head = self.moved()
        self.rejected("freeze-start", "MOVED", detail="not_preservable", early=ep, journal=head)

    def test_moved_requires_the_move_done_head(self):
        ep, es, head = self.moved()
        self.j.add("thaw.move.intent", **BACK)
        self.rejected("freeze-start", "MOVED", early=ep, journal=self.j.prev)
        with mock.patch.object(gitclass, "questions",
                               lambda *a: (["symref"], "VERIFIED", "COMPLETE", "ELIGIBLE", [], "a" * 40)):
            self.ok("freeze-start", "MOVED", early=ep, journal=head)
            self.rejected("freeze-start", "MOVED", early=ep, journal=self.j.intent)

    def test_moved_requires_all_four_originals(self):
        o = self.lab.originals(typed(REPO))
        ep, _, head = self.moved(originals={k: v for k, v in o.items() if k != "owner"})
        with mock.patch.object(gitclass, "questions",
                               lambda *a: (["symref"], "VERIFIED", "COMPLETE", "ELIGIBLE", [], "a" * 40)):
            self.rejected("freeze-start", "MOVED", detail="not_preservable", early=ep, journal=head)

    def test_revoked_v3(self):
        ep, es, _ = self.moved()
        self.j.revoke(len(es), self.lab.originals(es))
        head = self.j.add("revoked", escaped=[])
        esc = canonical.digest({"escaped": []})
        self.ok("freeze-continue", "REVOKED", early=ep, journal=head, escaped=esc)
        self.rejected("freeze-continue", "REVOKED", early=H(b"x"), journal=head, escaped=esc)
        self.rejected("freeze-continue", "REVOKED", detail="escaped", early=ep, journal=head, escaped=H(b"x"))

    def test_thaw_none_v3(self):
        ep, es, _ = self.moved()
        n = len(es)
        self.j.revoke(n, self.lab.originals(es))
        self.j.add("revoked", escaped=[])
        head = self.j.thaw([(a, 0, n) for a in journal.THAW_ORDER])
        self.ok("thaw", "NONE", journal=head)
        self.mem.j[(CID, self.j.seq + 1)] = b"{}"
        self.rejected("thaw", "NONE", journal=head)

    def test_thaw_complete_needs_every_range(self):
        """`thaw_complete` itself, over a view whose head is `end{thawed}` but
        whose thaw ranges do not match the revoke ranges."""
        view = types.SimpleNamespace(v3=True, end=True, end_rec={"outcome": "thawed"}, moved=True, thaw_moved=True,
                                     ranges={a: [(0, 5, True)] for a in journal.ATTRS},
                                     thaw_ranges={a: [(0, 5, True)] for a in journal.ATTRS})
        self.assertTrue(journal.thaw_complete(view))
        for bad in ({"owner": []}, {"mode": [(0, 4, True)]}, {"acl": [(0, 5, False)]}):
            with self.subTest(bad):
                v = types.SimpleNamespace(**dict(vars(view), thaw_ranges=dict(view.thaw_ranges, **bad)))
                self.assertFalse(journal.thaw_complete(v))

    def test_split_ranges_revoke_and_thaw(self):
        """N above one range: two ranges per attribute are revoked and thawed
        in ascending order; descending is refused."""
        files = {b"f%04d" % i: b"same" for i in range(4100)}
        ep, es, _ = self.moved(files)
        n = len(es)
        self.assertEqual(n, 4100)
        self.j.revoke(n, self.lab.originals(es))
        self.assertEqual(self.j.seq, 3 + 2 * 2 * 4)
        head = self.j.add("revoked", escaped=[])
        self.ok("freeze-continue", "REVOKED", early=ep, journal=head, escaped=canonical.digest({"escaped": []}))
        split = [(0, 4095), (4096, n)]
        mark = (self.j.seq, self.j.prev)
        done = self.j.thaw([(a, lo, hi) for a in journal.THAW_ORDER for lo, hi in split])
        self.ok("thaw", "NONE", journal=done)
        for k in [k for k in self.mem.j if k[1] > mark[0]]:
            del self.mem.j[k]
        self.j.seq, self.j.prev = mark
        self.j.add("thaw.intent", attr="owner", lo=4096, hi=n)
        self.bad(lambda: self.walk(self.j.prev), "journal_mismatch")

    def hest(self, escaped, linked):
        ep, es, _ = self.moved()
        self.j.revoke(len(es), self.lab.originals(es))
        head = self.j.add("revoked", escaped=escaped)
        pre = types.SimpleNamespace(**dict(vars(ctx(cycle_id=CID).pre), release_target=None,
                                           holder_chain=PRE3.barrier_id, digest=H(b"pre")))
        sel = {"evidence": [[k, H(k.encode())] for k in oc.H_EST_KINDS if k not in ("journal_head", "ep_manifest")]
               + [["journal_head", head], ["ep_manifest", ep]], "provider_inputs": []}
        data = {"claimed_set_observation": {"parent": [0, 0o700, 0], "ancestors_root_owned": True, "entries": [],
                                            "linked": linked},
                "writer_capabilities": {"grants": []}, "source_identity": {"root_id": ROOT},
                "artifact_isolation": {"objects": [[ep, 0, 0o400, 0, 1, True], [head, 0, 0o400, 0, 1, True]]}}
        with mock.patch.object(preserve.Preservation, "_e5", lambda *a: None), \
                mock.patch.object(preserve.Preservation, "_ev",
                                  lambda self, i, k, p, decisive=True: {"data": data[k]}), \
                mock.patch.object(preserve.Preservation, "_library", lambda *a: None):
            v = self.prov.handoff_establish(pre, sel, None)
        return v.passed, v.reasons, es

    def test_handoff_establish_over_v3(self):
        """H-EST reads `linked` and `escaped` of a v3 journal (the barrier,
        library and evidence inputs are stubbed to pass)."""
        self.assertEqual(self.hest([], [0o700, []])[:2], (True, []))
        self.setUp()
        es = typed(REPO)
        self.assertEqual(self.hest([es[-1][0]], [0o700, []])[:2], (False, ["escaped_present"]))
        self.setUp()
        self.assertEqual(self.hest([], [0o700, [b"a".hex()]])[:2], (False, ["linked"]))

    def test_thaw_none_without_revoke(self):
        """MOVED then thaw: no revoke range, only the move is reversed."""
        self.moved()
        head = self.j.thaw([])
        self.ok("thaw", "NONE", journal=head)

    def test_merged_readers_over_v3(self):
        """D-THAW, D-QUIES and the snapshot inputs read a v3 journal
        through the merged fields; D-QUAR refuses as `not_preservable`."""
        ep, es, _ = self.moved()
        self.j.revoke(len(es), self.lab.originals(es))
        head = self.j.add("revoked", escaped=[])
        pre = ctx(cycle_id=CID).pre
        self.prov._cycle = lambda p: self.cy
        inputs = {"journal_head": head, "ep_manifest": ep, "original_blobs": ep}
        self.assertEqual(self.prov._D_THAW(pre, inputs, {}), {})
        self.assertEqual(self.prov._D_QUIES(pre, inputs, {}), {})
        self.prov._snap_common(pre, inputs)
        with self.assertRaises(Mismatch) as e:
            self.prov._D_QUAR(pre, inputs, {})
        self.assertTrue(e.exception.detail.startswith("not_preservable("), e.exception.detail)
        self.assertIn("unknown", e.exception.detail)


# ------------------------------------------------------------------ originals/v1

class Originals(Base):
    def blob(self, o):
        return self.lab.blob(canonical.canon(o))

    def ep_with(self, attr, digest):
        es = typed(REPO)
        o = dict(self.lab.originals(es), **{attr: digest})
        return lambda: self.ep(originals=o)[0]

    def early(self, build):
        b = build()
        return lambda: self.cy.binding_v3(b, "early")

    def test_valid_originals_pass(self):
        b, es = self.ep()
        _, m, _, bs = self.cy.binding_v3(b, "early")
        self.assertTrue(set(m["originals"].values()) <= set(bs))
        self.assertTrue(self.cy.reversible(m, bs))

    def test_absent_blob_is_evidence_unavailable(self):
        self.bad(self.early(self.ep_with("mode", H(b"never written"))), "evidence_unavailable",
                 Code.PROVENANCE_INSUFFICIENT)

    def test_altered_bytes_are_artifact_mismatch(self):
        """The stored digest is never trusted: the bytes are re-hashed."""
        es = typed(REPO)
        o = self.lab.originals(es)
        good = self.mem.b[o["flags"]]
        self.mem.b[o["flags"]] = good.replace(b'"n"', b'"n" ')
        self.bad(lambda: self.cy.binding_v3(self.ep(originals=o)[0], "early"), "artifact_mismatch",
                 Code.STATE_UNKNOWN)
        self.mem.b[o["flags"]] = b"not bytes".decode()
        self.bad(lambda: self.cy.binding_v3(self.ep(originals=o)[0], "early"), "artifact_mismatch")

    def test_valid_bytes_under_another_digest(self):
        """Canonical, `originals_ok` bytes stored under a digest they do not
        hash to: only the re-hash refuses them."""
        es = typed(REPO)
        o = self.lab.originals(es)
        other = canonical.canon({"schema": preserve.O_SCHEMA, "attr": "mode", "n": len(es),
                                 "values": [[ROOT, 0o600]] + [[H(b"object|%d" % i), 0o600]
                                                              for i in range(1, len(es) + 1)]})
        self.assertTrue(preserve.originals_ok(protocol.parse_doc(other), "mode", len(es)))
        self.mem.b[o["mode"]] = other
        self.bad(lambda: self.cy.binding_v3(self.ep(originals=o)[0], "early"), "artifact_mismatch",
                 Code.STATE_UNKNOWN)

    def complete_cycle(self, o):
        """A complete EP with originals `o`, moved, revoked over all four
        attributes and thawed; returns (ep, revoked head, thawed head)."""
        ep, es = self.ep(originals=o)
        self.j.start()
        self.j.ep(ep)
        self.j.move()
        self.j.moved()
        n = len(es)
        self.j.revoke(n, o)
        revoked = self.j.add("revoked", escaped=[])
        thawed = self.j.thaw([(a, 0, n) for a in journal.THAW_ORDER])
        return ep, revoked, thawed

    def test_every_attribute_is_checked(self):
        """An absent blob, or a hash-valid one that fails `originals_ok`, for
        each attribute in turn: the EP read refuses, and so do REVOKED,
        thaw NONE and MOVED over it."""
        es = typed(REPO)
        esc = canonical.digest({"escaped": []})
        eligible = (["symref"], "VERIFIED", "COMPLETE", "ELIGIBLE", [], "a" * 40)
        for attr in journal.ATTRS:
            for how, detail in (("absent", "evidence_unavailable"), ("invalid", "artifact_mismatch")):
                with self.subTest((attr, how)):
                    self.setUp()
                    o = self.lab.originals(es)
                    if how == "absent":
                        o[attr] = H(b"never written|" + attr.encode())
                    else:
                        bad = originals_blobs(self.lab.blob, len(es), root=H(b"another root"), attrs=(attr,))
                        o[attr] = bad[attr]
                    ep, revoked, thawed = self.complete_cycle(o)
                    self.bad(lambda: self.cy.binding_v3(ep, "early"), detail)
                    self.rejected("freeze-continue", "REVOKED", detail=detail, early=ep, journal=revoked,
                                  escaped=esc)
                    self.rejected("thaw", "NONE", detail=detail, journal=thawed)
                    with mock.patch.object(gitclass, "questions", lambda *a: eligible):
                        self.rejected("freeze-start", "MOVED", detail=detail, early=ep, journal=self.j.prev)

    def test_thaw_none_needs_a_completed_thaw(self):
        """thaw NONE bound to a final head that is not `end{thawed}`."""
        es = typed(REPO)
        ep, es2 = self.ep()
        self.j.start()
        self.j.ep(ep)
        self.j.move()
        self.j.moved()
        self.j.revoke(len(es2), self.lab.originals(es2))
        head = self.j.add("revoked", escaped=[])
        self.rejected("thaw", "NONE", journal=head)
        self.assertEqual(len(es), len(es2))

    def test_substituted_blob_of_another_attribute(self):
        es = typed(REPO)
        o = self.lab.originals(es)
        self.bad(self.early(lambda: self.ep(originals=dict(o, mode=o["flags"]))[0]), "artifact_mismatch")

    def values(self, attr, n, value, root=ROOT):
        return [[root, value]] + [[H(b"object|%d" % i), value] for i in range(1, n + 1)]

    def test_originals_ok_negatives(self):
        """Missing root, `n` mismatch, duplicate key, out-of-domain
        value, closed keys and types."""
        n = 3
        good = {"schema": preserve.O_SCHEMA, "attr": "mode", "n": n, "values": self.values("mode", n, 0o644)}
        self.assertTrue(preserve.originals_ok(good, "mode", n))
        dup = self.values("mode", n, 0o644)
        dup[2][0] = dup[1][0]
        for bad in (dict(good, values=good["values"][1:]), dict(good, n=n + 1), dict(good, n=True),
                    dict(good, values=dup), dict(good, attr="flags"), dict(good, schema="codegraph.o5.originals/v2"),
                    dict(good, extra=1), {k: v for k, v in good.items() if k != "attr"},
                    dict(good, values=self.values("mode", n, 0o10000)), dict(good, values=self.values("mode", n, -1)),
                    dict(good, values=self.values("mode", n, True)), dict(good, values=[["X" * 64, 0]] * (n + 1)),
                    dict(good, values=[[ROOT, 0, 0]] + good["values"][1:]), [good], None):
            with self.subTest(bad):
                self.assertFalse(preserve.originals_ok(bad, "mode", n))

    def test_value_domains(self):
        u32 = (1 << 32) - 1
        ace = [1, "0" * 32, 2, 3]
        for attr, ok, bad in (("owner", ([0, 0], [u32, u32], [501, 20]), ([u32 + 1, 0], [-1, 0], [0], [0, 0, 0],
                                                                         [True, 0], 0)),
                              ("mode", (0, 0o7777, 0o4755), (0o10000, -1, "0644", True)),
                              ("flags", (0, u32), (u32 + 1, -1, False, [0])),
                              ("acl", ([], [ace], [ace] * preserve.MAX_ACL),
                               ([ace] * (preserve.MAX_ACL + 1), [[1, "0" * 31, 2, 3]], [[1, "G" * 32, 2, 3]],
                                [[1, "0" * 32, 2]], [[-1, "0" * 32, 2, 3]], [[1, "0" * 32, u32 + 1, 3]], {}, 0))):
            for v in ok:
                with self.subTest((attr, v)):
                    self.assertTrue(preserve._value_ok(attr, v))
            for v in bad:
                with self.subTest((attr, v)):
                    self.assertFalse(preserve._value_ok(attr, v))

    def test_root_entry_is_the_slot_root(self):
        es = typed(REPO)
        o = originals_blobs(self.lab.blob, len(es), root=H(b"another root"))
        self.bad(self.early(lambda: self.ep(originals=o)[0]), "artifact_mismatch")

    def test_every_attribute_lists_the_same_objects(self):
        es = typed(REPO)
        o = self.lab.originals(es)
        vals = canonical.canon({"schema": preserve.O_SCHEMA, "attr": "mode", "n": len(es),
                                "values": [[ROOT, 0o644]] + [[H(b"swapped|%d" % i), 0o644]
                                                             for i in range(1, len(es) + 1)]})
        self.bad(self.early(lambda: self.ep(originals=dict(o, mode=self.lab.blob(vals)))[0]), "artifact_mismatch")

    def test_count_follows_the_ep(self):
        es = typed(REPO)
        o = originals_blobs(self.lab.blob, len(es) - 1)
        self.bad(self.early(lambda: self.ep(originals=o)[0]), "artifact_mismatch")

    def test_large_valid_originals_parse(self):
        """Above the 64 KiB document bound, inside MAX_ORIGINALS."""
        n = 3000
        files = {b"f%04d" % i: b"x" for i in range(n)}
        es = typed(files)
        acl = [[1, "ab" * 16, 7, 0]] * 2
        o = originals_blobs(self.lab.blob, len(es))
        o["acl"] = self.lab.blob(canonical.canon({"schema": preserve.O_SCHEMA, "attr": "acl", "n": len(es),
                                                  "values": [[ROOT, acl]] + [[H(b"object|%d" % i), acl]
                                                                             for i in range(1, len(es) + 1)]}))
        self.assertGreater(len(self.mem.b[o["acl"]]), protocol.MAX_DOC)
        b, _ = self.ep(files, originals=o)
        _, m, _, _ = self.cy.binding_v3(b, "early")
        self.assertEqual(m["originals"], o)

    def test_bound_is_the_capture_file_bound_and_checked_before_parsing(self):
        self.assertEqual(preserve.MAX_ORIGINALS, fdwalk.Limits().max_file)
        self.assertEqual(protocol.MAX_DOC, 65536)
        big = b" " * (preserve.MAX_ORIGINALS + 1)
        b, _ = self.ep(originals={"owner": self.lab.blob(big)})
        real, sizes = protocol.parse_doc, []

        def spy(raw, limit=protocol.MAX_DOC):
            sizes.append(len(raw))
            return real(raw, limit)
        with mock.patch.object(protocol, "parse_doc", spy):
            self.bad(lambda: self.cy.binding_v3(b, "early"), "artifact_mismatch")
        self.assertTrue(sizes)
        self.assertLessEqual(max(sizes), journal.MAX_OBJECT)

    def test_document_bound_unchanged_for_other_callers(self):
        doc = canonical.canon({"k": "x" * protocol.MAX_DOC})
        with self.assertRaises(Exception):
            protocol.parse_doc(doc)
        self.assertEqual(protocol.parse_doc(doc, len(doc)), {"k": "x" * protocol.MAX_DOC})
        self.assertEqual(inspect.signature(protocol.parse_doc).parameters["limit"].default, protocol.MAX_DOC)

    def test_malformed_encodings_refuse(self):
        es = typed(REPO)
        n = len(es)
        good = canonical.canon({"schema": preserve.O_SCHEMA, "attr": "mode", "n": n,
                                "values": self.values("mode", n, 0o644)})
        for raw in (good + b"\n", good.replace(b":", b": ", 1), good.replace(b'"n":%d' % n, b'"n":%d.0' % n),
                    good.replace(b'"n":%d' % n, b'"n":99999999999999999999'), b"[" * 100000, b"",
                    good[:-1], b'{"attr":"mode","attr":"mode"}'):
            with self.subTest(raw[:40]):
                self.setUp()
                o = dict(self.lab.originals(es), mode=self.lab.blob(raw))
                self.bad(self.early(lambda: self.ep(originals=o)[0]), "artifact_mismatch")


# ------------------------------------------------------------------ refusal codes

class RefusalCodes(Base):
    def test_absent_v3_object_is_evidence_unavailable(self):
        self.bad(lambda: self.cy.binding_v3(H(b"absent"), "early"), "evidence_unavailable",
                 Code.PROVENANCE_INSUFFICIENT)
        b, _ = self.ep()
        idx = self.cy.binding_v3(b, "early")[1]["entry_index"][0]
        del self.mem.a[(CID, idx)]
        self.bad(lambda: self.cy.binding_v3(b, "early"), "evidence_unavailable")

    def test_malformed_v3_objects_are_artifact_mismatch(self):
        for raw in (b"{}" + b" " * journal.MAX_OBJECT, b'{"a":1 }', b"[]", b"\xff",
                    canonical.canon({"kind": "entries"})):
            with self.subTest(raw[:20]):
                self.setUp()
                self.mem.a[(CID, H(raw))] = raw
                self.bad(lambda: self.cy.binding_v3(H(raw), "final"), "artifact_mismatch", Code.STATE_UNKNOWN)

    def test_v2_codes_are_unchanged(self):
        raw = b"{}" + b" " * journal.MAX_OBJECT
        self.mem.a[(CID, H(raw))] = raw
        self.bad(lambda: self.cy.binding(H(raw), "final"), "evidence_unavailable")

    def test_precedence_absent_then_malformed_then_ineligible(self):
        """Absent evidence, malformed evidence and valid ineligible evidence
        stay three outcomes through the NONE and MOVED checks."""
        self.j.start()
        self.j.ep(H(b"absent ep"))
        self.j.end(reason="not_preservable")
        self.rejected("freeze-start", "NONE", "not_preservable", "evidence_unavailable", early=H(b"absent ep"),
                      journal=self.j.prev)
        self.setUp()
        raw = b'{"a":1 }'
        self.mem.a[(CID, H(raw))] = raw
        self.j.start()
        self.j.ep(H(raw))
        self.j.end(reason="not_preservable")
        self.rejected("freeze-start", "NONE", "not_preservable", "artifact_mismatch", early=H(raw),
                      journal=self.j.prev)
        self.setUp()
        ep, _ = self.ep()
        self.j.start()
        self.j.ep(ep)
        self.j.end(reason="not_preservable")
        self.ok("freeze-start", "NONE", "not_preservable", early=ep, journal=self.j.prev)


# ------------------------------------------------------------------ eligibility and the typed classifier

def kinds_of(entries):
    return [(bytes.fromhex(e[0]), e[1]) for e in entries]


def repos(entries, files=None):
    files = REPO if files is None else files
    git = {p: v for p, v in files.items() if gitclass.is_git_path(p)}
    return gitclass.repositories_typed(git, kinds_of(entries))


def unknown(entries, root=b"", files=None):
    r = repos(entries, files).get(root)
    return r is not None and "unknown" in r.features


class Eligibility(Base):
    def assess(self, files=None, entries=None):
        b, es = self.ep(files, entries)
        _, m, es, _ = self.cy.binding_v3(b, "early")
        return self.cy.assess(m, es, True)

    def test_empty_complete_manifest_is_never_eligible(self):
        """A valid empty COMPLETE manifest establishes nothing."""
        b, es = self.ep(files={})
        _, m, es, bs = self.cy.binding_v3(b, "early")
        self.assertEqual((m["result"], es), ("COMPLETE", []))
        a = self.cy.assess(m, es, True)
        self.assertEqual((a.q1, a.q4, a.q5, a.head), ("COMPLETE", "INCOMPLETE", "NOT_ELIGIBLE", None))
        self.assertEqual(preserve.refusals_v3(es), [])
        self.j.start()
        self.j.ep(b)
        self.j.end(reason="not_preservable")
        self.ok("freeze-start", "NONE", "not_preservable", early=b, journal=self.j.prev)

    def test_valid_nonempty_complete_manifest_reaches_the_classifier(self):
        """A complete capture of an honest repository: Q1 COMPLETE, every blob
        and original re-hashed, and NOT_ELIGIBLE only by NB-13 (`objects` and
        `refs` are `d` entries at administrative names; that rule is relaxed only with the real restorer)."""
        a = self.assess()
        self.assertEqual(a.q1, "COMPLETE")
        self.assertEqual(a.q5, "NOT_ELIGIBLE")
        self.assertEqual(a.reasons, ["unknown"])
        self.assertEqual(a.head, gitclass.repositories({p: v for p, v in REPO.items() if p.startswith(b".git/")})[
            b""].head_oid)
        with mock.patch.object(gitclass, "_ADMIN_DIRS", frozenset()):
            a = self.assess()
        self.assertEqual((a.q1, a.q3, a.q4, a.q5, a.reasons), ("COMPLETE", "VERIFIED", "COMPLETE", "ELIGIBLE", []))

    def test_incomplete_capture_is_not_eligible(self):
        for result, reasons in (("INCOMPLETE", ["path_bound"]), ("INCOMPLETE", ["unstable"])):
            with self.subTest(reasons):
                self.setUp()
                es = typed(REPO)
                b, _ = self.ep(entries=es, result=result, reasons=reasons)
                _, m, es, _ = self.cy.binding_v3(b, "early")
                with mock.patch.object(gitclass, "_ADMIN_DIRS", frozenset()):
                    a = self.cy.assess(m, es, True)
                self.assertEqual((a.q1, a.q4, a.q5), ("INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE"))
        es = typed(REPO) + [u("zz", "fifo")]
        b, _ = self.ep(entries=sorted(es))
        _, m, es, _ = self.cy.binding_v3(b, "early")
        self.assertEqual(m["result"], "INCOMPLETE")

    def test_missing_blob_is_raw_incomplete_and_bytes_are_recounted(self):
        es = typed(REPO)
        b, _ = self.ep(entries=es)
        _, m, es, _ = self.cy.binding_v3(b, "early")
        del self.mem.b[H(REPO[b"README"])]
        self.assertEqual(self.cy.assess(m, es, True).q1, "INCOMPLETE")
        self.setUp()
        es = typed(REPO)
        self.lab.blobs(REPO.values())
        b = self.lab.binding(es, "early", totals=totals(es, 1))
        _, m, es, _ = self.cy.binding_v3(b, "early")
        self.bad(lambda: self.cy.assess(m, es, True), "artifact_mismatch")

    def test_worktree_symlink_and_alias_refuse(self):
        nfd = unicodedata.normalize("NFD", "café").encode()
        for files, extra, why in ((REPO, [link("lnk", b"README")], "symlink"),
                                  ({**REPO, b"readme": b"x"}, [], "name_alias"),
                                  ({**REPO, "café".encode(): b"x", nfd: b"y"}, [], "name_alias")):
            with self.subTest(why):
                self.setUp()
                es = sorted(typed(files) + extra, key=lambda e: e[0])
                with mock.patch.object(gitclass, "_ADMIN_DIRS", frozenset()):
                    a = self.assess(files, es)
                self.assertEqual(a.q5, "NOT_ELIGIBLE")
                self.assertIn(why, a.reasons)
                self.assertEqual(a.q4, "INCOMPLETE" if why == "name_alias" else "COMPLETE")

    def test_classifier_head_must_match(self):
        es = typed(REPO)
        b, _ = self.ep(entries=es, classifier_head="0" * 40)
        _, m, es, _ = self.cy.binding_v3(b, "early")
        self.bad(lambda: self.cy.assess(m, es, False), "artifact_mismatch")


class TypedClassifier(unittest.TestCase):
    """The typed classifier input: roots, `gitdir_shape`, `bare_shape` and the unrelaxed merged rules."""

    def repo(self, prefix=b"", extra=(), drop=()):
        files = {prefix + p: v for p, v in REPO.items() if p not in drop}
        return files, sorted(typed(files) + list(extra), key=lambda e: e[0])

    def test_gitdir_shape(self):
        files, es = self.repo()
        k = dict(kinds_of(es))
        self.assertTrue(gitclass.gitdir_shape(k, b".git"))
        for change in ({b".git/HEAD": "d"}, {b".git/HEAD": None}, {b".git/objects": "f"}, {b".git/refs": None},
                       {b".git": "l"}, {b".git": "f"}):
            with self.subTest(change):
                k2 = {p: t for p, t in {**k, **change}.items() if t is not None}
                self.assertFalse(gitclass.gitdir_shape(k2, b".git"))

    def test_merged_rules_stay_unrelaxed(self):
        """NB-13 over every entry type and B-1 over files: an honest v3
        repository is `unknown` until the relaxation."""
        _, es = self.repo()
        self.assertTrue(unknown(es))
        with mock.patch.object(gitclass, "_ADMIN_DIRS", frozenset()):
            self.assertFalse(unknown(es))

    def test_root_from_any_entry_type(self):
        """An empty `.git`, a `.git` symlink, file or `u` entry each define a root."""
        for es in ([d(".git")], [link(".git", b"/elsewhere")], [u(".git", "fifo")], [d(".git"), d(".git/refs")],
                   [f(".git", b"gitdir: /x\n")]):
            with self.subTest(es):
                files = {bytes.fromhex(e[0]): b"gitdir: /x\n" for e in es if e[1] == "f"}
                r = gitclass.repositories_typed(files, kinds_of(es))
                self.assertIn("unknown", r[b""].features)

    def test_link_or_unsupported_below_a_gitdir(self):
        for extra in ([link(".git/hooks", b"/x")], [u(".git/logs", "fifo")], [d(".git/logs"),
                                                                              link(".git/logs/HEAD", b"x")]):
            with self.subTest(extra):
                _, es = self.repo(extra=extra)
                with mock.patch.object(gitclass, "_ADMIN_DIRS", frozenset()):
                    self.assertTrue(unknown(es))

    def test_exact_dotgit_only(self):
        for name in (b".GIT", b".Git", ".g‌it".encode()):
            with self.subTest(name):
                files = {name + p[4:]: v for p, v in REPO.items()}
                es = typed(files)
                with mock.patch.object(gitclass, "_ADMIN_DIRS", frozenset()):
                    self.assertTrue(unknown(es, files=files))

    def test_gitfile_is_refused(self):
        """A `.git` file, even pointing inside the tree."""
        files = {b"sub/.git": b"gitdir: ../.git/modules/sub\n"}
        es = typed(files)
        r = gitclass.repositories_typed(files, kinds_of(es))
        self.assertTrue({"gitlink_file", "unknown"} <= r[b"sub"].features)

    def test_bare_shape_positive_cases(self):
        """Narrowed scope: administrative names inside the gitdir are not
        discovery origins."""
        extra = [d(".git/logs"), f(".git/logs/HEAD", b"x"), d(".git/logs/refs"), d(".git/logs/refs/heads"),
                 f(".git/logs/refs/heads/main", b"x"), d(".git/refs/remotes"), d(".git/refs/remotes/origin"),
                 f(".git/refs/remotes/origin/HEAD", b"ref: refs/remotes/origin/main\n")]
        _, es = self.repo(extra=extra)
        self.assertEqual(gitclass.bare_shape(kinds_of(es)), [])
        with mock.patch.object(gitclass, "_ADMIN_DIRS", frozenset()):
            self.assertFalse(unknown(es))

    def test_bare_shape_negative_cases(self):
        """Forged bare shapes at the slot root and in worktree directories."""
        cases = {
            "sub/.git + sub/commondir": ([d("sub"), d("sub/.git"), f("sub/commondir", b"../x\n")], b"sub"),
            "file objects and refs at root": ([f("objects", b"", "x"), f("refs", b"", "x")], b""),
            "symlinked HEAD at root": ([link("HEAD", b".git/HEAD")], b""),
            "forged triple nested": ([d("a"), d("a/b"), f("a/b/HEAD", b"ref: refs/heads/x\n"), d("a/b/objects"),
                                      d("a/b/refs")], b"a/b"),
            "commondir alone": ([d("w"), f("w/commondir", b"x")], b"w"),
            "fold spellings at root": ([f("head", b"x")], b""),
            "fold spellings nested": ([d("n"), d("n/OBJECTS"), d("n/Refs")], b"n"),
        }
        for name, (extra, where) in cases.items():
            with self.subTest(name):
                _, es = self.repo(extra=extra)
                self.assertIn(where, gitclass.bare_shape(kinds_of(es)))
                self.assertTrue(unknown(es, where))

    def test_bare_shape_folds_ignorable_and_unicode_spellings(self):
        for name in ("HE\u200dAD", "\ufeffcommondir"):
            with self.subTest(name):
                _, es = self.repo(extra=[f(name, b"x")])
                self.assertIn(b"", gitclass.bare_shape(kinds_of(es)))

    def test_administrative_alias_of_a_directory(self):
        """An empty directory spelled like an administrative name below the
        gitdir is `unknown` even with NB-13 out of the way."""
        _, es = self.repo(extra=[d(".git/objects/Pack")])
        with mock.patch.object(gitclass, "_ADMIN_DIRS", frozenset()):
            self.assertTrue(unknown(es))
            _, clean = self.repo()
            self.assertFalse(unknown(clean))

    def test_empty_gitdir_and_nested_gitdir_without_refs(self):
        """An empty gitdir and a nested gitdir without `refs`."""
        _, es = self.repo(extra=[d("N"), d("N/.git"), f("N/.git/HEAD", b"ref: refs/heads/main\n"),
                                 d("N/.git/objects")])
        self.assertTrue(unknown(es, b"N", files={**REPO, b"N/.git/HEAD": b"ref: refs/heads/main\n"}))
        self.assertTrue(unknown([d(".git")]))

    def test_every_kind_at_every_administrative_directory(self):
        """Never eligible unless `d`; NB-13 is unrelaxed, so not even then."""
        for name in sorted(gitclass._ADMIN_DIRS):
            for e in (f(".git/" + name.decode(), b"x"), link(".git/" + name.decode(), b"/x"),
                      u(".git/" + name.decode(), "fifo"), d(".git/" + name.decode())):
                with self.subTest((name, e[1])):
                    files = {p: v for p, v in REPO.items() if not p.startswith(b".git/" + name + b"/")
                             and p != b".git/" + name}
                    parents = {b"/".join(bytes.fromhex(e[0]).split(b"/")[:k])
                               for k in range(1, bytes.fromhex(e[0]).count(b"/") + 1)}
                    es = {x[0]: x for x in typed(files)}
                    for p in parents:
                        es.setdefault(p.hex(), d(p.decode()))
                    es[e[0]] = e
                    entries = [es[k] for k in sorted(es)]
                    if e[1] == "f":
                        files[bytes.fromhex(e[0])] = b"x"
                    self.assertTrue(unknown(entries, files=files))

    def test_scope_mutants_fail(self):
        """A scope without the slot root or the worktree directories
        misses the forged shapes; a scope that enters gitdirs refuses the positives."""
        real = gitclass.bare_shape
        root_case = [f("objects", b"", "x"), f("refs", b"", "x")]
        nested_case = [d("a"), d("a/b"), f("a/b/HEAD", b"x"), d("a/b/objects"), d("a/b/refs")]
        positive = [d(".git/logs"), f(".git/logs/HEAD", b"x")]

        def mutant(skip_root=False, skip_nested=False, enter_gitdirs=False):
            def bare(typed_):
                if enter_gitdirs:
                    typed_ = [(p.replace(b".git/", b"g/"), t) for p, t in typed_]
                got = real(typed_)
                return [x for x in got if not (skip_root and x == b"" or skip_nested and x)]
            return bare

        for kw, extra, expect_refused in (({"skip_root": True}, root_case, True),
                                          ({"skip_nested": True}, nested_case, True),
                                          ({"enter_gitdirs": True}, positive, False)):
            with self.subTest(kw):
                _, es = self.repo(extra=extra)
                k = kinds_of(es)
                self.assertEqual(bool(real(k)), expect_refused)
                self.assertNotEqual(bool(mutant(**kw)(k)), expect_refused)

    def test_alias_folding_covers_every_kind(self):
        for extra in ([d("Docs"), d("docs")], [d("x"), link("X", b"y")], [u("a", "fifo"), f("A", b"")]):
            with self.subTest(extra):
                self.assertIn("name_alias", preserve.refusals_v3(sorted(extra, key=lambda e: e[0])))


# ------------------------------------------------------------------ every HFS+-ignorable code point

class HfsIgnorable(unittest.TestCase):
    def test_sixteen_code_points(self):
        self.assertEqual(len(HFS_IGNORABLE), 16)
        self.assertEqual(len(set(HFS_IGNORABLE)), 16)
        for cp in HFS_IGNORABLE:
            self.assertRegex(cp, gitclass.HFS_IGNORED)

    def test_every_spelling_is_refused_end_to_end(self):
        f644 = 0o100644
        for cp in HFS_IGNORABLE:
            for name in ((".g" + cp + "it").encode(), (cp + ".git").encode(), (".git" + cp).encode()):
                with self.subTest((hex(ord(cp)), name)):
                    self.assertTrue(gitclass.gitbytes.is_gitdir_name(name))
                    # Observer: an index path and a tree entry with that component.
                    with self.assertRaises(fdwalk.Unknown) as e:
                        indexverify.parse_index(index_bytes([(b"sub/" + name + b"/config", f644, 0, None)]),
                                                fdwalk.Limits())
                    self.assertEqual(e.exception.code, "malformed-index")
                    tree = b"100644 " + name + b"\0" + bytes(20)
                    with self.assertRaises(fdwalk.Unknown) as e:
                        gitraw.parse_tree(b"t", b"tree", tree, 100)
                    self.assertEqual(e.exception.code, "malformed-object")
                    # Classifier: merged and typed, and a symlink below that spelling.
                    files = {b"sub/" + name + p[4:]: v for p, v in REPO.items()}
                    self.assertIn("unknown", gitclass.repositories(files)[b"sub"].features)
                    es = typed(files)
                    self.assertIn("unknown", gitclass.repositories_typed(files, kinds_of(es))[b"sub"].features)
                    self.assertEqual(preserve.refusals_v3([d(name.decode()), link(name.decode() + "/x", b"y")]),
                                     ["unknown"])
        for name in (b".gitignore", b".github", b"git"):
            self.assertFalse(gitclass.gitbytes.is_gitdir_name(name))
            self.assertEqual(set(indexverify.parse_index(index_bytes([(name, f644, 0, None)]), fdwalk.Limits())),
                             {name})


# ------------------------------------------------------------------ static

class Static(unittest.TestCase):
    SRC = Path(test_static.BROKER) / "preserve.py"

    def test_no_tuple_unpacking_of_manifest_entries(self):
        """A loop or comprehension that unpacks its item iterates a call
        (the typed accessors, `enumerate`, `.items()`), never a bare entry list."""
        def allowed(it):
            if isinstance(it, ast.BinOp):
                return allowed(it.left) and allowed(it.right)
            if not isinstance(it, ast.Call):
                return False
            f = it.func
            return isinstance(f, ast.Name) and f.id in ("_file_blobs", "enumerate") \
                or isinstance(f, ast.Attribute) and f.attr in ("items", "get")
        tree = ast.parse(self.SRC.read_text())
        for node in ast.walk(tree):
            if isinstance(node, (ast.For, ast.comprehension)) and isinstance(node.target, (ast.Tuple, ast.List)):
                self.assertTrue(allowed(node.iter), f"line {node.iter.lineno}: {ast.unparse(node.iter)}")

    def test_inventory_chunker_unchanged_and_separate(self):
        """`chunks_of` is byte-identical to the merged one and neither
        chunker calls the other."""
        src = inspect.getsource(preserve.chunks_of) + inspect.getsource(preserve._chunk_obj)
        self.assertEqual(hashlib.sha256(src.encode()).hexdigest(), CHUNKS_OF_SHA256)
        for fn, other in ((preserve.manifest_chunks, "chunks_of"), (preserve.chunks_of, "manifest_chunks")):
            names = {n.id for n in ast.walk(ast.parse(inspect.getsource(fn))) if isinstance(n, ast.Name)}
            self.assertNotIn(other, names)

    def test_new_readers_hold_no_write_path(self):
        for fn in (preserve._Cycle.originals_v3, preserve._Cycle.final_head, preserve._Cycle.bound,
                   preserve.Preservation._check_none, journal.walk, journal.check_v3, journal._Grammar.ok,
                   gitclass.repositories_typed, gitclass.bare_shape, gitclass.gitdir_shape):
            calls = {n.func.attr for n in ast.walk(ast.parse(textwrap.dedent(inspect.getsource(fn))))
                     if isinstance(n, ast.Call) and isinstance(n.func, ast.Attribute)}
            self.assertFalse(calls & {"put", "write", "rename", "unlink", "ingest", "commit"}, fn.__name__)


# `chunks_of` and `_chunk_obj` as merged at d7d2cabad20538be8b047e3d1fd3150d3728bbd6.
CHUNKS_OF_SHA256 = "7ace2092ebcd2329c4f86f1ef53e412b029d6c6e8ba53baa64d7e7c0c1b49969"


if __name__ == "__main__":
    unittest.main()

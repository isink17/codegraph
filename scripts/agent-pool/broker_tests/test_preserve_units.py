"""PR-3b pure units: journal lineage and freshness (N-R41-3), manifest
reconstruction and chunk bounds (N-R41-5, N-R41-6), Git feature
classification over byte fixtures, escaped-set classification and typed
neutralization (N-R41-9). Inputs are built in memory; nothing reads a live
path."""

import os
import types
import unittest
from unittest import mock

import harness  # noqa: F401 - sys.path for broker
import preserve_world as pw
from broker import canonical, gitclass, journal, o5contract as oc, preserve
from broker.journal import Mismatch
from broker.refusal import Code

CID = canonical.sha256(b"cycle-1")
CID2 = canonical.sha256(b"cycle-2")
SCTX = canonical.sha256(b"start-ctx")
PRE = types.SimpleNamespace(slot="s", op_id="op1", g_pre=1, cutover_head="c" * 64, barrier_id="b" * 64,
                            root_id="r" * 64, mode=oc.SYNTHETIC)


def header(cid=CID, **kw):
    h = {"slot": "s", "op_id": "op1", "g_pre": 1, "cycle_id": cid, "start_ctx": SCTX, "cutover_head": "c" * 64,
         "barrier_id": "b" * 64, "root_id": "r" * 64, "mode": oc.SYNTHETIC}
    h.update(kw)
    return h


class Mem:
    """An exact-key resolver over test-owned dicts."""

    def __init__(self):
        self.j, self.a, self.b = {}, {}, {}

    def journal(self, cid, seq):
        return self.j.get((cid, seq))

    def artifact(self, cid, d):
        return self.a.get((cid, d))

    def blob(self, d):
        return self.b.get(d)


class Journal:
    def __init__(self, mem, cid=CID):
        self.mem, self.cid, self.prev, self.seq, self.early = mem, cid, oc.ZERO64, -1, "e" * 64

    def add(self, kind, raw=None, **extra):
        self.seq += 1
        rec = dict({"schema": journal.J_SCHEMA, "header": header(self.cid), "seq": self.seq, "prev": self.prev,
                    "kind": kind}, **extra)
        if self.seq:
            rec["early"] = self.early
        data = raw if raw is not None else canonical.canon(rec)
        self.mem.j[(self.cid, self.seq)] = data
        self.prev = canonical.sha256(data)
        return self.prev

    def full(self, escaped=()):
        self.add("start")
        self.add("move.done", linked=[])
        heads = {"move": self.prev}
        for a in journal.ATTRS:
            self.add("revoke.done", attr=a, original="0" * 63 + "1")
        heads["revoked"] = self.add("revoked", escaped=list(escaped))
        return heads


def walk(mem, head, cid=CID):
    return journal.walk(mem, PRE, head, cid, SCTX)


class JournalLineage(unittest.TestCase):
    def setUp(self):
        self.mem = Mem()
        self.j = Journal(self.mem)
        self.heads = self.j.full()

    def refuse(self, fn, detail="journal_mismatch"):
        with self.assertRaises(Mismatch) as e:
            fn()
        self.assertEqual(e.exception.detail, detail)

    def test_current_journal_verifies(self):
        v = walk(self.mem, self.heads["revoked"])
        self.assertTrue(v.revoked)
        self.assertEqual([r["seq"] for r in v.recs], list(range(7)))
        journal.require_revoked(v)

    def test_historical_point_of_the_same_cycle_before_revocation_refuses(self):
        """N-R41-3: an older head of this cycle verifies as a prefix, but a
        binding past REVOKED refuses it."""
        v = walk(self.mem, self.heads["move"])
        self.assertFalse(v.revoked)
        self.refuse(lambda: journal.require_revoked(v))

    def test_record_fields_are_checked_per_kind(self):
        """Each record kind's own fields are validated: a second `revoked`, an
        unsorted, duplicated or non-hex `linked` list, an unknown `revoke.done`
        attribute or a non-digest original, an unknown `thaw.done` step."""
        def chain(*recs):
            mem = Mem()
            j, head = Journal(mem), None
            for kind, extra in recs:
                head = j.add(kind, **extra)
            return mem, head
        start, revoked = ("start", {}), ("revoked", {"escaped": []})
        ok = ("move.done", {"linked": []})
        attr = [("revoke.done", {"attr": a, "original": "0" * 63 + "1"}) for a in journal.ATTRS]
        mem, head = chain(start, ok, *attr, revoked, ("thaw.done", {"attr": "move"}))
        self.assertTrue(walk(mem, head).revoked)
        bad = {
            "second revoked": [start, ok, *attr, revoked, revoked],
            "linked unsorted": [start, ("move.done", {"linked": ["62", "61"]})],
            "linked duplicated": [start, ("move.done", {"linked": ["61", "61"]})],
            "linked not hex": [start, ("move.done", {"linked": ["zz"]})],
            "unknown attribute": [start, ok, ("revoke.done", {"attr": "xattr", "original": "0" * 63 + "1"})],
            "original not a digest": [start, ok, ("revoke.done", {"attr": "acl", "original": "1"})],
            "unknown thaw step": [start, ok, ("thaw.done", {"attr": "xattr"})],
        }
        for name, recs in bad.items():
            with self.subTest(name):
                mem, head = chain(*recs)
                self.refuse(lambda: walk(mem, head))

    def test_journal_of_another_cycle_refuses(self):
        other = Journal(self.mem, CID2)
        head = other.full()["revoked"]
        self.refuse(lambda: walk(self.mem, head))
        self.refuse(lambda: walk(self.mem, self.heads["revoked"], CID2))
        moved = dict(journal.parse(self.mem.j[(CID2, 0)])[0])
        self.mem.j[(CID, 0)] = canonical.canon(moved)
        self.refuse(lambda: walk(self.mem, self.heads["revoked"]), "cycle_binding")

    def test_missing_revocation_refuses(self):
        mem = Mem()
        j = Journal(mem)
        j.add("start")
        head = j.add("move.done", linked=[])
        self.refuse(lambda: journal.require_revoked(walk(mem, head)))

    def test_corrupt_chain_refuses(self):
        key = (CID, 3)
        rec = canonical.canon(dict(journal.parse(self.mem.j[key])[0], prev="f" * 64))
        self.mem.j[key] = rec
        self.refuse(lambda: walk(self.mem, self.heads["revoked"]))

    def test_interrupted_write_refuses(self):
        self.mem.j[(CID, 6)] = self.mem.j[(CID, 6)][:-3]
        self.refuse(lambda: walk(self.mem, self.heads["revoked"]))

    def test_gap_refuses(self):
        del self.mem.j[(CID, 2)]
        self.refuse(lambda: walk(self.mem, self.heads["revoked"]))

    def test_first_record_must_be_start_with_the_zero_predecessor(self):
        mem = Mem()
        j = Journal(mem)
        j.prev = "1" * 64
        head = j.add("start")
        self.refuse(lambda: walk(mem, head))

    def test_unknown_kind_and_record_after_end_refuse(self):
        self.j.add("end")
        head = self.j.add("thaw.done", attr="owner")
        self.refuse(lambda: walk(self.mem, head))
        mem = Mem()
        j = Journal(mem)
        j.add("start")
        head = j.add("teleport")
        self.refuse(lambda: walk(mem, head))

    def test_foreign_header_field_refuses(self):
        mem = Mem()
        rec = {"schema": journal.J_SCHEMA, "header": header(g_pre=2), "seq": 0, "prev": oc.ZERO64, "kind": "start"}
        mem.j[(CID, 0)] = canonical.canon(rec)
        self.refuse(lambda: walk(mem, canonical.digest(rec)), "barrier_binding")

    def test_extra_header_key_refuses(self):
        mem = Mem()
        rec = {"schema": journal.J_SCHEMA, "header": dict(header(), auth="a" * 64), "seq": 0, "prev": oc.ZERO64,
               "kind": "start"}
        mem.j[(CID, 0)] = canonical.canon(rec)
        self.refuse(lambda: walk(mem, canonical.digest(rec)), "barrier_binding")

    def test_thaw_requires_every_committed_step_reversed(self):
        for a in journal.ATTRS:
            self.j.add("thaw.done", attr=a)
        self.j.add("end")
        self.assertFalse(journal.thaw_complete(walk(self.mem, self.j.prev)))
        mem = Mem()
        j = Journal(mem)
        j.full()
        for a in journal.ATTRS + ("move",):
            j.add("thaw.done", attr=a)
        self.assertTrue(journal.thaw_complete(walk(mem, j.add("end"))))

    def test_seq0_of_another_kind_refuses(self):
        mem = Mem()
        rec = {"schema": journal.J_SCHEMA, "header": header(), "seq": 0, "prev": oc.ZERO64, "kind": "end"}
        mem.j[(CID, 0)] = canonical.canon(rec)
        self.refuse(lambda: walk(mem, canonical.digest(rec)))

    def test_inconsistent_early_refuses(self):
        mem = Mem()
        j = Journal(mem)
        j.add("start")
        j.add("move.done", linked=[])
        j.early = "d" * 64
        head = j.add("revoked", escaped=[])
        self.refuse(lambda: walk(mem, head))


class Escaped(unittest.TestCase):
    def state(self, escaped, paths=("61", "62")):
        mem = Mem()
        head = Journal(mem).full(escaped)["revoked"]
        return journal.escaped_state(walk(mem, head), set(paths))

    def test_states(self):
        self.assertEqual(self.state([]), "empty")
        self.assertEqual(self.state(["61"]), "present")
        self.assertEqual(self.state(["63"]), "unclassifiable")
        self.assertEqual(self.state(["62", "61"]), "unclassifiable")

    def test_malformed_members_are_unclassifiable_not_an_exception(self):
        """N-2: unhashable or mixed members refuse instead of raising TypeError."""
        for bad in ([["61"]], [1, "61"], [{"p": "61"}], [True, "61"]):
            self.assertEqual(self.state(bad), "unclassifiable", bad)

    def test_absent_section_is_unclassifiable(self):
        mem = Mem()
        j = Journal(mem)
        j.add("start")
        head = j.add("move.done", linked=[])
        self.assertEqual(journal.escaped_state(walk(mem, head), {"61"}), "unclassifiable")

    def test_oversize_section_is_unclassifiable(self):
        """N-R41-6: an escaped list past MAX_ESCAPED refuses (it also exceeds the
        per-object bound, which fails the parse first)."""
        paths = ["%04x" % i for i in range(journal.MAX_ESCAPED + 1)]
        self.assertEqual(self.state(paths[:-1], paths), "present")
        self.assertEqual(self.state(paths, paths), "unclassifiable")


class Chunks(unittest.TestCase):
    H = header()
    M = "m" * 64

    def chunk_sizes(self, members):
        out = preserve.chunks_of(self.H, "dirty", self.M, members, 1000)
        return [len(c) for c in out], [len(canonical.canon(preserve._chunk_obj(self.H, "dirty", self.M, n, c)))
                                       for n, c in enumerate(out)]

    def test_entry_limit_below_at_and_above(self):
        ids = ["i" + canonical.sha256(str(i).encode())[:3] for i in range(4097)]
        short = ["%x" % (0x10 + i) for i in range(4097)]
        for n, want in ((4095, [4095]), (4096, [4096]), (4097, [4096, 1])):
            self.assertEqual(self.chunk_sizes(sorted(set(short))[:n])[0], want, n)
        self.assertTrue(all(s <= preserve.CHUNK_BYTES for s in self.chunk_sizes(ids)[1]))

    def test_byte_limit_below_at_and_above(self):
        base = len(canonical.canon(preserve._chunk_obj(self.H, "dirty", self.M, 0, [])))
        room = preserve.CHUNK_BYTES - base
        one = "ab" * ((room - 2) // 2)
        exact = one + "a" * ((room - 2) - len(one))
        if len(exact) % 2:
            exact = exact[:-1]
        self.assertEqual(self.chunk_sizes([exact])[0], [1])
        self.assertIsNone(preserve.chunks_of(self.H, "dirty", self.M, [exact + "aa" * 2], 1000))

    def test_entry_count_fits_but_bytes_do_not(self):
        """4,096 realistic hex paths exceed 49,152 bytes: the chunk splits by bytes."""
        paths = sorted(("src/d%04d/f%04d.txt" % (i // 50, i)).encode().hex() for i in range(4096))
        counts, sizes = self.chunk_sizes(paths)
        self.assertGreater(len(counts), 1)
        self.assertEqual(sum(counts), 4096)
        self.assertTrue(all(s <= preserve.CHUNK_BYTES for s in sizes))

    def test_item_ids_are_key_compliant_and_header_free(self):
        inv, preserved = preserve.inventory_items(self.M, self.H, {"dirty": ["61", "62"], "untracked": [],
                                                                   "ignored": [], "unpublished_commits": []})
        (i,) = inv["dirty"]
        self.assertRegex(i, canonical.KEY)
        self.assertEqual(i, preserve.item_id("dirty", self.M, 0, ["61", "62"]))
        inv2, _ = preserve.inventory_items(self.M, dict(self.H, slot="t"), {"dirty": ["61", "62"]})
        self.assertEqual(inv2["dirty"], [i])
        self.assertEqual(preserved[i], {"verified": True, "to": "raw:" + self.M})

    def test_inventory_cap(self):
        many = sorted(("%06x" % i) for i in range(4096 * preserve.MAX_CHUNKS + 1))
        self.assertEqual(preserve.inventory_items(self.M, self.H, {"dirty": many}), (None, None))
        self.assertEqual(preserve.inventory_items(self.M, self.H, {"dirty": ["a/b"]}), (None, None))


class Reconstruction(unittest.TestCase):
    """R4.1 §3.4 entry_index reconstruction through `_Cycle.entries`."""

    def setUp(self):
        self.mem = Mem()
        self.cy = preserve._Cycle(self.mem, PRE, CID, SCTX)

    def put(self, o, cid=CID):
        data = canonical.canon(o)
        d = canonical.sha256(data)
        self.mem.a[(cid, d)] = data
        return d

    def manifest(self, chunks_entries, per_index=512):
        chunks = [self.put({"header": header(), "kind": "entries", "n": n, "entries": es})
                  for n, es in enumerate(chunks_entries)]
        index = [self.put({"header": header(), "kind": "entry_index", "n": n, "chunks": chunks[i:i + per_index]})
                 for n, i in enumerate(range(0, len(chunks), per_index))]
        return {"entry_index": index}, chunks, index

    def entries(self, n, start=0):
        return [[(b"f%06d" % i).hex(), canonical.sha256(str(i).encode())] for i in range(start, start + n)]

    def mismatch(self, m):
        with self.assertRaises(Mismatch):
            self.cy.entries(m)

    def test_a_file_and_a_path_below_it_refuse(self):
        """N-6: `a` and `a/b` cannot both be files, adjacent or not."""
        h = canonical.sha256(b"x")
        for paths in (["61", "612f62"], ["61", "612d78", "612f62"], ["61", "62", "622f63", "622f632f64"]):
            self.mismatch(self.manifest([[[p, h] for p in paths]])[0])
        self.assertEqual(len(self.cy.entries(self.manifest([[["612d78", h], ["612f62", h]]])[0])), 2)

    def test_every_path_component_is_one_the_capture_walk_accepts(self):
        """R2 §3.2: an empty, `.` or `..` component, a NUL or a component over
        255 bytes refuses; 255 bytes is accepted."""
        h = canonical.sha256(b"x")
        for bad in (b"/.git/HEAD", b"./a", b"a/./b", b"a/../b", b"a//b", b"a/", b"a/\0b", b"a/" + b"n" * 256):
            with self.subTest(bad):
                self.mismatch(self.manifest([[[bad.hex(), h]]])[0])
        ok = b"a/" + b"n" * 255
        self.assertEqual(self.cy.entries(self.manifest([[[ok.hex(), h]]])[0]), [(ok.hex(), h)])

    def test_path_depth_is_the_capture_walks(self):
        """NB-6: at most `fdwalk.Limits.max_depth` (64) separators, as captured."""
        h = canonical.sha256(b"x")
        ok = b"/".join([b"d"] * 64 + [b"f"])
        self.assertEqual(len(self.cy.entries(self.manifest([[[ok.hex(), h]]])[0])), 1)
        self.mismatch(self.manifest([[[b"/".join([b"d"] * 65 + [b"f"]).hex(), h]]])[0])

    def test_non_bytes_blob_is_unavailable_not_an_exception(self):
        """Q3e: a resolver value that is not bytes is a missing blob."""
        d = canonical.sha256(b"x")
        self.mem.b[d] = b"x"
        self.assertEqual(self.cy.blobs([d]), {d: b"x"})
        for bad in ("x", bytearray(b"x"), memoryview(b"x"), None):
            self.mem.b[d] = bad
            self.assertIsNone(self.cy.blobs([d]), type(bad))

    def test_non_bytes_barrier_record_is_unavailable_not_an_exception(self):
        d = canonical.sha256(b"rec")
        res = types.SimpleNamespace(barrier=lambda bid, seq: {0: "rec", 1: bytearray(b"rec")}.get(seq))
        pre = types.SimpleNamespace(holder_chain="b" * 64)
        with self.assertRaises(Mismatch) as e:
            preserve.Preservation(res)._barrier_record(pre, d)
        self.assertEqual(e.exception.detail, "enforcement_record_unavailable")

    def test_external_index_objects(self):
        """A manifest whose entries need several index objects."""
        m, chunks, index = self.manifest([self.entries(2, 2 * k) for k in range(5)], per_index=2)
        self.assertEqual(len(index), 3)
        self.assertEqual(len(self.cy.entries(m)), 10)

    def test_missing_duplicate_overlapping_and_conflicting_chunks_refuse(self):
        m, chunks, _ = self.manifest([self.entries(2), self.entries(2, 2)])
        del self.mem.a[(CID, chunks[1])]
        self.mismatch(m)
        self.mismatch(self.manifest([self.entries(2), self.entries(2)])[0])
        self.mismatch(self.manifest([self.entries(3), self.entries(2, 2)])[0])
        bad = self.entries(2, 2)
        bad[0][1] = "f" * 64
        self.mismatch(self.manifest([self.entries(2), [self.entries(2)[1]] + bad[1:]])[0])

    def test_misnumbered_index_objects_refuse(self):
        m, chunks, index = self.manifest([self.entries(2, 2 * k) for k in range(4)], per_index=2)
        self.assertEqual(len(self.cy.entries(m)), 8)
        self.mismatch({"entry_index": index[::-1]})
        renumbered = self.put({"header": header(), "kind": "entry_index", "n": 1, "chunks": chunks[:2]})
        self.mismatch({"entry_index": [renumbered, index[1]]})

    def test_reordered_or_misnumbered_chunks_refuse(self):
        m, chunks, index = self.manifest([self.entries(2), self.entries(2, 2)])
        swapped = self.put({"header": header(), "kind": "entry_index", "n": 0, "chunks": chunks[::-1]})
        self.mismatch({"entry_index": [swapped]})

    def test_bounds(self):
        """The member bound below, at and above (scaled down: 4,096 entries of
        64-hex blobs never fit 49,152 bytes, so the byte bound decides first)."""
        with mock.patch.object(preserve, "MAX_ENTRIES", 3):
            self.assertEqual(len(self.cy.entries(self.manifest([self.entries(2)])[0])), 2)
            self.assertEqual(len(self.cy.entries(self.manifest([self.entries(3)])[0])), 3)
            self.mismatch(self.manifest([self.entries(4)])[0])
        with mock.patch.object(preserve, "MAX_INDEX_CHUNKS", 2):
            self.mismatch(self.manifest([self.entries(1, k) for k in range(3)], per_index=3)[0])
        self.mismatch({"entry_index": ["0" * 64] * (preserve.MAX_INDEX + 1)})
        self.mismatch(self.manifest([[]])[0])

    def test_misnumbered_chunk_refuses(self):
        c = self.put({"header": header(), "kind": "entries", "n": 5, "entries": self.entries(1)})
        i = self.put({"header": header(), "kind": "entry_index", "n": 0, "chunks": [c]})
        self.mismatch({"entry_index": [i]})

    def binding(self, blobs=None, cycle_id=CID, verify=None):
        es = self.entries(2)
        m, _, _ = self.manifest([es])
        md = self.put({"schema": preserve.M_SCHEMA, "header": header(), "phase": "final", "result": "COMPLETE",
                       "reasons": [], **m})
        bs = self.put({"header": header(), "set": sorted(blobs if blobs is not None else {b for _, b in es})})
        b = {"header": header(), "cycle_id": cycle_id, "manifest": md, "blobs": bs}
        if verify is not None:
            b.update(reverified=verify, sealed_at=1)
        return self.put(b), es

    def test_binding_rules(self):
        d, es = self.binding()
        self.assertEqual(self.cy.binding(d, "final")[2], [tuple(e) for e in es])
        d, _ = self.binding(verify=True)
        self.cy.binding(d, "final", verify=True)
        for kw, detail in (({"blobs": [es[0][1]]}, "artifact_mismatch"), ({"cycle_id": CID2}, "cycle_binding"),
                           ({"verify": False}, "evidence_unavailable")):
            d, _ = self.binding(**kw)
            with self.assertRaises(Mismatch) as e:
                self.cy.binding(d, "final", verify="verify" in kw)
            self.assertEqual(e.exception.detail, detail, kw)

    def test_missing_blob_makes_raw_capture_incomplete(self):
        files, _ = pw.git_repo()
        es = sorted((p.hex(), canonical.sha256(v)) for p, v in files.items())
        for p, v in files.items():
            self.mem.b[canonical.sha256(v)] = v
        m = {"result": "COMPLETE"}
        self.assertEqual(self.cy.assess(m, es, True).q1, "COMPLETE")
        del self.mem.b[canonical.sha256(files[b"README"])]
        self.assertEqual(self.cy.assess(m, es, True).q1, "INCOMPLETE")
        self.assertEqual(self.cy.assess(m, es, False).q1, "COMPLETE")
        del self.mem.b[canonical.sha256(files[b".git/HEAD"])]
        a = self.cy.assess(m, es, False)
        self.assertEqual((a.q1, a.q4, a.q5), ("INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE"))

    def test_oversize_object_refuses(self):
        """Every artifact object is <= 49,152 bytes (R4.1 §3.4)."""
        big = [[(b"f%04d" % i).hex() + "ab" * 40, canonical.sha256(str(i).encode())] for i in range(400)]
        self.assertGreater(len(canonical.canon({"header": header(), "kind": "entries", "n": 0, "entries": big})),
                           journal.MAX_OBJECT)
        self.mismatch(self.manifest([big])[0])
        with mock.patch.object(journal, "MAX_OBJECT", 1 << 20):
            self.assertEqual(len(self.cy.entries(self.manifest([big])[0])), 400)

    def test_chunk_of_another_cycle_is_unreadable(self):
        m, chunks, _ = self.manifest([self.entries(2)])
        data = self.mem.a.pop((CID, chunks[0]))
        self.mem.a[(CID2, chunks[0])] = data
        self.mismatch(m)


def classify(files):
    return gitclass.questions(gitclass.repositories(files), "COMPLETE")


class GitClassification(unittest.TestCase):
    def q(self, *variant, extra=None):
        files, _ = pw.git_repo(variant=variant)
        files.update(extra or {})
        return classify(files)

    def test_supported_repository(self):
        feats, q3, q4, q5, reasons, head = self.q()
        _, coid = pw.git_repo()
        self.assertEqual((q3, q4, q5, reasons, head), ("VERIFIED", "COMPLETE", "ELIGIBLE", [], coid))

    def test_feature_matrix(self):
        rows = {
            ("packed_refs",): ("VERIFIED", "COMPLETE", "ELIGIBLE"),
            ("packed_only",): ("INCOMPLETE", "COMPLETE", "NOT_ELIGIBLE"),
            ("symref",): ("VERIFIED", "COMPLETE", "ELIGIBLE"),
            ("dangling_symref",): ("VERIFIED", "COMPLETE", "ELIGIBLE"),
            ("detached",): ("VERIFIED", "COMPLETE", "ELIGIBLE"),
            ("lock",): ("VERIFIED", "COMPLETE", "ELIGIBLE"),
            ("shallow",): ("INCOMPLETE", "COMPLETE", "NOT_ELIGIBLE"),
            ("alternates",): ("INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE"),
            ("unborn",): ("INCOMPLETE", "COMPLETE", "NOT_ELIGIBLE"),
            ("head_tag",): ("INCOMPLETE", "COMPLETE", "NOT_ELIGIBLE"),
            ("sha256",): ("INCOMPLETE", "COMPLETE", "NOT_ELIGIBLE"),
            ("malformed_ref",): ("INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE"),
            ("malformed_packed",): ("INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE"),
            ("missing_object",): ("INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE"),
            ("corrupt_object",): ("INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE"),
        }
        for variant, want in rows.items():
            _, q3, q4, q5, _, _ = self.q(*variant)
            self.assertEqual((q3, q4, q5), want, variant)

    def test_findings_are_named(self):
        self.assertIn("shallow", self.q("shallow")[4])
        self.assertIn("alternates", self.q("alternates")[4])
        self.assertIn("unborn_head", self.q("unborn")[4])
        self.assertIn("packed", self.q("packed_only")[4])
        self.assertIn("residue", self.q("lock")[0])
        self.assertIn("dangling_symref", self.q("dangling_symref")[0])

    def test_no_masking(self):
        """Every finding is reported; one defect never hides another."""
        feats = self.q("malformed_ref", "shallow", "alternates")[0]
        self.assertTrue({"malformed_ref", "shallow", "alternates"} <= set(feats))

    def test_nested_repository_defect_propagates(self):
        nested, _ = pw.git_repo(variant=("malformed_ref",), prefix=b"vendor/lib/")
        _, q3, q4, q5, reasons, _ = self.q(extra=nested)
        self.assertEqual((q4, q5), ("INCOMPLETE", "NOT_ELIGIBLE"))
        good, _ = pw.git_repo(prefix=b"vendor/lib/")
        _, q3, q4, q5, _, _ = self.q(extra=good)
        self.assertEqual((q4, q5), ("COMPLETE", "ELIGIBLE"))

    def test_gitlink_file_is_external(self):
        _, _, q4, _, reasons, _ = self.q(extra={b"sub/.git": b"gitdir: /elsewhere\n"})
        self.assertEqual(q4, "INCOMPLETE")
        self.assertIn("gitlink_file", reasons)

    def test_gitdir_names_compare_case_insensitively(self):
        for name in (b".git", b".GIT", b".Git", b".gIT", ".g\u200cit".encode(), "\ufeff.GIT".encode()):
            self.assertTrue(gitclass.is_git_path(b"sub/" + name + b"/HEAD"), name)
        for path in (b".gitignore", b".github/workflows/ci.yml", b"git/HEAD", b"x.git/HEAD", b".git2/HEAD",
                     b"sub/.gi t/HEAD", b"\xff.git/HEAD"):
            self.assertFalse(gitclass.is_git_path(path), path)

    def test_nested_repository_under_any_spelling_is_classified(self):
        """B-2: `sub/.GIT` is `sub/.git` on a case-insensitive volume, so its
        alternates, shallow file, gitlink file and defects are findings."""
        for name in (b".GIT", b".Git", ".g\u200dit".encode()):
            def nested(*variant, extra=None):
                files, _ = pw.git_repo(worktree={b"w": b"x\n"}, variant=variant, prefix=b"sub/")
                files.update(extra or {})
                return {p.replace(b"sub/.git", b"sub/" + name, 1): d for p, d in files.items()}
            feats, _, q4, q5, reasons, _ = self.q(extra=nested())
            self.assertEqual((q4, q5, "nested" in feats), ("COMPLETE", "ELIGIBLE", True), name)
            _, _, q4, _, reasons, _ = self.q(extra=nested("alternates"))
            self.assertEqual(q4, "INCOMPLETE", name)
            self.assertIn("alternates", reasons)
            self.assertEqual(self.q(extra=nested("shallow"))[3], "NOT_ELIGIBLE", name)
            self.assertIn("missing_object", self.q(extra=nested("missing_object"))[4])
            _, _, q4, _, reasons, _ = self.q(extra={b"sub/" + name: b"gitdir: /elsewhere\n"})
            self.assertEqual(q4, "INCOMPLETE", name)
            self.assertIn("gitlink_file", reasons)

    def test_two_spellings_of_one_gitdir_are_ambiguous(self):
        for extra in ({b".GIT/description": b"benign\n"}, {b".Git/HEAD": b"ref: refs/heads/main\n"}):
            files, _ = pw.git_repo()
            files.update(extra)
            _, _, q4, q5, reasons, _ = classify(files)
            self.assertEqual((q4, q5, reasons), ("INCOMPLETE", "NOT_ELIGIBLE", ["unknown"]), extra)

    def test_two_paths_that_name_one_file_are_ambiguous(self):
        """Distinct captured paths that one APFS volume stores as one file:
        the bytes classified need not be the bytes restored there."""
        files, _ = pw.git_repo()
        k = next(p for p in files if p.startswith(b".git/objects/"))
        cafe, cafe_nfd = "caf\u00e9/".encode(), "cafe\u0301/".encode()
        nested, _ = pw.git_repo(prefix=cafe)
        for name, extra in (("same path below the gitdir", {b"/" + k: b"junk"}),
                            ("case", {k.replace(b"objects", b"OBJECTS", 1): b"junk"}),
                            ("case of the last component", {b".git/refs/heads/MAIN": files[b".git/refs/heads/main"]}),
                            ("ignorable code point", {k.replace(b"objects", "obj\u200cects".encode(), 1): b"junk"}),
                            ("normalization of the root",
                             {**nested, **{cafe_nfd + q[len(cafe):]: d for q, d in nested.items()}})):
            with self.subTest(name):
                _, _, q4, q5, reasons, _ = classify({**files, **extra})
                self.assertEqual((q4, q5), ("INCOMPLETE", "NOT_ELIGIBLE"))
                self.assertIn("unknown", reasons)
        both = {**nested, **{cafe_nfd + q[len(cafe):]: d for q, d in nested.items()}}
        repos = gitclass.repositories({p: d for p, d in both.items() if gitclass.is_git_path(p)})
        self.assertEqual({r: "unknown" in repos[r].features for r in repos}, {cafe[:-1]: True, cafe_nfd[:-1]: True})
        _, _, q4, q5, reasons, _ = classify({**files, **nested, b"README.md": b"x", b"readme": b"x"})
        self.assertEqual((q4, q5, reasons), ("COMPLETE", "ELIGIBLE", []))

    def test_a_file_that_is_another_paths_directory_is_ambiguous(self):
        """On APFS a file `.git/OBJECTS` and the directory `.git/objects/`
        cannot both exist, so neither the objects nor the file restore as
        classified. Worktree paths count (`captured`), and a git path that is
        not UTF-8 has no APFS name."""
        files, _ = pw.git_repo()
        k = next(p for p in files if p.startswith(b".git/objects/"))
        sub, _ = pw.git_repo(prefix=b"sub/")
        for name, gitfiles, captured in (
                ("file over the objects directory", {**files, b".git/OBJECTS": b"x"}, None),
                ("file over a fan-out directory", {**files, k[:15].upper(): b"x"}, None),
                ("file over the refs directory", {**files, b".git/Refs": b"x"}, None),
                ("worktree file over a nested root", {**files, **sub}, [*files, *sub, b"Sub"]),
                ("root that is not UTF-8", {**files, **{b"\xff" + q: d for q, d in sub.items()}}, None)):
            with self.subTest(name):
                repos = gitclass.repositories(gitfiles, captured)
                _, _, q4, q5, reasons, _ = gitclass.questions(repos, "COMPLETE")
                self.assertEqual((q4, q5), ("INCOMPLETE", "NOT_ELIGIBLE"))
                self.assertIn("unknown", reasons)
        repos = gitclass.repositories({**files, **sub}, [*files, *sub, b"Sub2", b"sub.txt"])
        self.assertEqual(gitclass.questions(repos, "COMPLETE")[2:5], ("COMPLETE", "ELIGIBLE", []))

    def test_administrative_names_match_as_apfs_matches_them(self):
        """B-1 (6b85f780 review): Git on APFS opens `.git/Shallow` as `shallow`
        and `.git/INFO/grafts` as `info/grafts`, so a case variant of a name
        read here is a file Git sees and this classifier would not."""
        files, coid = pw.git_repo()
        oid = coid.encode()
        cfg = files[b".git/config"]
        for name, change in (("Shallow", {b".git/Shallow": oid + b"\n"}),
                             ("SHALLOW", {b".git/SHALLOW": oid + b"\n"}),
                             ("info/Grafts", {b".git/info/Grafts": oid + b"\n"}),
                             ("INFO/grafts", {b".git/INFO/grafts": oid + b"\n"}),
                             ("Packed-Refs", {b".git/Packed-Refs": b"f" * 40 + b" refs/heads/x\n"}),
                             ("Commondir", {b".git/Commondir": b"../elsewhere\n"}),
                             ("objects/Info/alternates", {b".git/objects/Info/alternates": b"/elsewhere\n"}),
                             ("Config only", {b".git/config": None,
                                              b".git/Config": cfg + b"[extensions]\n\tobjectformat = sha256\n"})):
            with self.subTest(name):
                got = {p: d for p, d in {**files, **change}.items() if d is not None}
                _, _, q4, q5, reasons, _ = classify(got)
                self.assertEqual((q4, q5), ("INCOMPLETE", "NOT_ELIGIBLE"))
                self.assertIn("unknown", reasons)
        legit = {b".git/logs/HEAD": b"x", b".git/ORIG_HEAD": oid + b"\n", b".git/FETCH_HEAD": b"",
                 b".git/COMMIT_EDITMSG": b"m\n", b".git/description": b"d\n", b".git/info/exclude": b"",
                 b".git/hooks/pre-commit.sample": b"", b".git/refs/heads/Feature": oid + b"\n"}
        self.assertEqual(classify({**files, **legit})[2:5], ("COMPLETE", "ELIGIBLE", []))

    def test_one_directory_spelled_two_ways_is_ambiguous(self):
        """APFS keeps the first spelling of a directory, so `refs/heads/Feat/a`
        and `refs/heads/feat/b` land in one directory."""
        files, coid = pw.git_repo()
        both = {**files, b".git/refs/heads/Feat/a": coid.encode() + b"\n",
                b".git/refs/heads/feat/b": coid.encode() + b"\n"}
        _, _, q4, q5, reasons, _ = classify(both)
        self.assertEqual((q4, q5, "unknown" in reasons), ("INCOMPLETE", "NOT_ELIGIBLE", True))
        del both[b".git/refs/heads/Feat/a"]
        self.assertEqual(classify(both)[2:5], ("COMPLETE", "ELIGIBLE", []))

    def test_config_include_is_external(self):
        """A config included from outside the captured tree is never preserved."""
        base = pw.git_repo()[0][b".git/config"]
        for sect in (b'[include]\n\tpath = /elsewhere/agent.config\n', b'[includeIf "gitdir:/x/"]\n\tpath = y\n'):
            _, _, q4, q5, reasons, _ = self.q(extra={b".git/config": base + sect})
            self.assertEqual((q4, q5, reasons), ("INCOMPLETE", "NOT_ELIGIBLE", ["config_include"]), sect)

    def test_external_administration_is_never_complete(self):
        base = pw.git_repo()[0][b".git/config"]
        for extra in ({b".git/commondir": b"../main/.git\n"}, {b".git/config": base + b"\tworktree = /elsewhere\n"},
                      {b".git/config": base.replace(b"bare = false", b"bare = true")}):
            _, _, q4, q5, reasons, _ = self.q(extra=extra)
            self.assertEqual((q4, q5, reasons), ("INCOMPLETE", "NOT_ELIGIBLE", ["commondir"]), extra)

    def test_object_whose_header_size_is_wrong(self):
        """A loose object hashed over a header whose size does not match its body."""
        files, _ = pw.git_repo()
        raw = b"blob 99\0hello\n"
        oid = pw.hashlib.sha1(raw).hexdigest()  # noqa: S324 - Git object id
        files[b".git/objects/" + oid[:2].encode() + b"/" + oid[2:].encode()] = pw.zlib.compress(raw)
        files[b".git/refs/tags/bad"] = oid.encode() + b"\n"
        _, _, q4, _, reasons, _ = classify(files)
        self.assertEqual((q4, reasons), ("INCOMPLETE", ["corrupt_object"]))

    def test_no_repository_is_never_eligible(self):
        _, q3, q4, q5, _, head = classify({b"README": b"x"})
        self.assertEqual((q3, q4, q5, head), ("INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE", None))

    def test_raw_incomplete_is_never_complete(self):
        files, _ = pw.git_repo()
        self.assertEqual(gitclass.questions(gitclass.repositories(files), "INCOMPLETE")[2:4],
                         ("INCOMPLETE", "NOT_ELIGIBLE"))

    def test_valid_commit_under_another_id(self):
        """Only the SHA-1 differs: a decompressible, canonically sized commit
        of valid structure whose links resolve, stored under the HEAD commit's
        id. Only the integrity check can refuse it (mutant G4)."""
        files, coid = pw.git_repo()
        raw = pw.zlib.decompress(files[pw.loose_key(coid)])
        body = raw.split(b"\0", 1)[1].replace(b"\nm\n", b"\nanother message\n")
        other = b"commit %d\0" % len(body) + body
        self.assertNotEqual(pw.hashlib.sha1(other).hexdigest(), coid)  # noqa: S324
        files[pw.loose_key(coid)] = pw.zlib.compress(other)
        _, q3, q4, q5, reasons, _ = classify(files)
        self.assertEqual((q3, q4, q5, reasons), ("INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE", ["corrupt_object"]))

    def test_object_whose_content_is_not_its_id(self):
        """Valid zlib under another object's id: integrity, not decompression, refuses.
        Since link types are checked, a blob in the commit's place is also a
        wrong type, so this test alone no longer proves the SHA-1 check;
        `test_valid_commit_under_another_id` does."""
        files, coid = pw.git_repo()
        _, other = pw.gobj(b"blob", b"something else\n")
        files[b".git/objects/" + coid[:2].encode() + b"/" + coid[2:].encode()] = other
        _, q3, q4, _, reasons, _ = classify(files)
        self.assertEqual((q3, q4), ("INCOMPLETE", "INCOMPLETE"))
        self.assertIn("corrupt_object", reasons)

    def test_oversize_ref_and_64_hex_ref(self):
        self.assertIn("oversize_ref", self.q(extra={b".git/refs/heads/big": b"a" * 5000 + b"\n"})[4])
        self.assertIn("unsupported_object_format", self.q(extra={b".git/refs/heads/x": b"a" * 64 + b"\n"})[4])

    def test_unpublished_commit_items(self):
        files, coid = pw.git_repo()
        repo = gitclass.repositories(files)[b""]
        (item,) = gitclass.unpublished_commit_items(repo)
        self.assertEqual(item, "i" + canonical.sha256(b"commit:" + coid.encode())[:63])
        self.assertRegex(item, canonical.KEY)


def gobj(typ, body):
    """(id, loose form) built here, not by the code under test."""
    raw = typ + b" " + str(len(body)).encode() + b"\0" + body
    return pw.hashlib.sha1(raw).hexdigest(), raw  # noqa: S324 - Git object ids


AUTHOR = b"author A U Thor <a@example.com> 1762819200 +0000\n"
COMMITTER = b"committer C O Mitter <c@example.com> 1762819200 -0130\n"
SIG = (b"gpgsig -----BEGIN PGP SIGNATURE-----\n \n iQEzBAABCAAdFiEEfixture\n =AbCd\n"
       b" -----END PGP SIGNATURE-----\n")


def object_rows():
    """{case: (objects, refs, expected (q3, q4, q5), finding, git fsck must object)}.
    Every object is hand-written bytes. `git fsck --strict` is the independent
    oracle in `test_git_fsck_agrees`; None marks a case where PR-3b is
    deliberately stricter than Git: uppercase hex, which Git parses but never
    writes; a mandatory header repeated after the ones Git reads, which Git
    ignores; a tree entry name longer than a file name can be."""
    blob, blob_raw = gobj(b"blob", b"hello\n")
    tree, tree_raw = gobj(b"tree", b"100644 README\0" + bytes.fromhex(blob))
    empty, empty_raw = gobj(b"tree", b"")
    base = {blob: blob_raw, tree: tree_raw}

    def commit(tree_id=tree, parents=(), head=None, tail=b"", msg=b"\nm\n"):
        lines = head if head is not None else (b"tree " + tree_id.encode() + b"\n"
                                               + b"".join(b"parent " + p.encode() + b"\n" for p in parents)
                                               + AUTHOR + COMMITTER)
        return gobj(b"commit", lines + tail + msg)

    root, root_raw = commit()
    child, child_raw = commit(parents=(root,))
    merge, merge_raw = commit(parents=(child, root))
    signed, signed_raw = commit(parents=(root,), tail=SIG, msg=b"\nsigned\n")
    on_empty, on_empty_raw = commit(tree_id=empty, msg=b"")
    tag, tag_raw = gobj(b"tag", b"object " + child.encode() + b"\ntype commit\ntag v1\ntagger "
                        + AUTHOR[7:] + b"\nrelease\n")
    wrong_tag, wrong_tag_raw = gobj(b"tag", tag_raw.split(b"\0", 1)[1].replace(b"type commit", b"type tree"))
    hist = {**base, root: root_raw, child: child_raw}
    ok, bad = ("VERIFIED", "COMPLETE", "ELIGIBLE"), ("INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE")

    def broken(head=None, objs=(), **kw):
        c, raw = commit(head=head, **kw)
        return {**base, **dict(objs), c: raw}, {b"refs/heads/main": c}

    def tree_case(body, extra=()):
        t, raw = gobj(b"tree", body)
        c, craw = commit(tree_id=t)
        return {**base, **dict(extra), t: raw, c: craw}, {b"refs/heads/main": c}

    def tagged(fmt, git=True):
        ident = AUTHOR[7:]
        body = fmt % ((child.encode(), ident, child.encode()) if fmt.count(b"%s") == 3 else
                      (child.encode(), ident) if fmt.count(b"%s") == 2 else (child.encode(),))
        t, raw = gobj(b"tag", body)
        return {**hist, t: raw}, {b"refs/heads/main": child, b"refs/tags/v1": t}, bad, "corrupt_object", git

    def tagged_raw(header, body, want=bad, finding="corrupt_object"):
        raw = header + b"\0" + body
        oid = pw.hashlib.sha1(raw).hexdigest()  # noqa: S324 - Git object id
        return {**hist, oid: raw}, {b"refs/heads/main": child, b"refs/tags/x": oid}, want, finding, want is bad

    def head_raw(header, body):
        raw = header + b"\0" + body
        oid = pw.hashlib.sha1(raw).hexdigest()  # noqa: S324 - Git object id
        return {**base, root: root_raw, oid: raw}, {b"refs/heads/main": oid}, bad, "corrupt_object", True

    def date_case(seconds, want=bad):
        c, raw = commit(head=b"tree " + tree.encode() + b"\nauthor A <a@example.com> " + seconds + b" +0000\n"
                        + COMMITTER)
        return {**base, c: raw}, {b"refs/heads/main": c}, want, None if want is ok else "corrupt_object", want is bad

    child_body = child_raw.split(b"\0", 1)[1]
    sigtag = (b"mergetag object " + root.encode() + b"\n type commit\n tag v0\n tagger " + AUTHOR[7:-1]
              + b"\n \n v0\n")
    mt, mt_raw = commit(parents=(child, root), tail=sigtag, msg=b"\nmerge v0\n")
    entry = b"100644 README\0" + bytes.fromhex(blob)
    missing = gobj(b"blob", b"never stored\n")[0]
    sub = "5" * 40
    rows = {
        "valid ordinary commit": (hist, {b"refs/heads/main": child}, ok, None, False),
        "valid root commit": ({**base, root: root_raw}, {b"refs/heads/main": root}, ok, None, False),
        "valid merge commit": ({**hist, merge: merge_raw}, {b"refs/heads/main": merge}, ok, None, False),
        "valid signed commit, multiline gpgsig": ({**base, root: root_raw, signed: signed_raw},
                                                  {b"refs/heads/main": signed}, ok, None, False),
        "valid empty tree, empty message": ({empty: empty_raw, on_empty: on_empty_raw},
                                            {b"refs/heads/main": on_empty}, ok, None, False),
        "valid annotated tag": ({**hist, tag: tag_raw}, {b"refs/heads/main": child, b"refs/tags/v1": tag},
                                ok, None, False),
        "valid gitlink, submodule commit not in this store": (*tree_case(entry + b"160000 sub\0"
                                                                         + bytes.fromhex(sub)), ok, "gitlink",
                                                              False),
        "commit without tree": (*broken(head=AUTHOR + COMMITTER), bad, "corrupt_object", True),
        "commit with two tree headers": (*broken(head=b"tree %s\ntree %s\n" % (tree.encode(), tree.encode())
                                                 + AUTHOR + COMMITTER), bad, "corrupt_object", True),
        "malformed tree id": (*broken(head=b"tree " + tree[:39].encode() + b"\n" + AUTHOR + COMMITTER),
                              bad, "corrupt_object", True),
        "uppercase tree id": (*broken(head=b"tree " + tree.upper().encode() + b"\n" + AUTHOR + COMMITTER),
                              bad, "corrupt_object", None),
        "tree header names a blob": (*broken(tree_id=blob), bad, "corrupt_object", True),
        "parent names a tree": (*broken(parents=(tree,)), bad, "corrupt_object", True),
        "author without email": (*broken(head=b"tree " + tree.encode() + b"\nauthor A U Thor 1762819200 +0000\n"
                                         + COMMITTER), bad, "corrupt_object", True),
        "committer missing": (*broken(head=b"tree " + tree.encode() + b"\n" + AUTHOR), bad, "corrupt_object",
                              True),
        "parent after author": (*broken(head=b"tree " + tree.encode() + b"\n" + AUTHOR + b"parent " + root.encode()
                                        + b"\n" + COMMITTER, objs=hist), bad, "corrupt_object", True),
        "unterminated header": (*broken(head=b"tree " + tree.encode() + b"\n" + AUTHOR + COMMITTER[:-1], msg=b""),
                                bad, "corrupt_object", True),
        "second tree after committer": (*broken(tail=b"tree " + tree.encode() + b"\n"), bad, "corrupt_object",
                                        None),
        "tree entry to a missing blob": (*tree_case(b"100644 README\0" + bytes.fromhex(missing)), bad,
                                         "missing_object", True),
        "invalid tree mode": (*tree_case(b"100777 README\0" + bytes.fromhex(blob)), bad, "corrupt_object", True),
        "zero-padded directory mode": (*tree_case(b"040000 d\0" + bytes.fromhex(tree)), bad, "corrupt_object",
                                       True),
        "directory entry names a blob": (*tree_case(b"40000 d\0" + bytes.fromhex(blob)), bad, "corrupt_object",
                                         True),
        "file entry names a tree": (*tree_case(b"100644 f\0" + bytes.fromhex(tree)), bad, "corrupt_object", True),
        "duplicate tree entries": (*tree_case(entry + entry), bad, "corrupt_object", True),
        "entries out of order": (*tree_case(b"100644 b\0" + bytes.fromhex(blob) + b"100644 a\0"
                                            + bytes.fromhex(blob)), bad, "corrupt_object", True),
        "truncated tree record": (*tree_case(entry[:-5]), bad, "corrupt_object", True),
        ".git entry in a tree": (*tree_case(b"100644 .git\0" + bytes.fromhex(blob)), bad, "corrupt_object", True),
        "tag declares the wrong type": ({**hist, wrong_tag: wrong_tag_raw},
                                        {b"refs/heads/main": child, b"refs/tags/v1": wrong_tag}, bad,
                                        "corrupt_object", True),
        "branch names a tag object": ({**hist, tag: tag_raw}, {b"refs/heads/main": tag}, bad, "corrupt_object",
                                      True),
        "tag type is not an object type": tagged(b"object %s\ntype bogus\ntag v1\ntagger %s\nx\n"),
        "tag with a malformed tagger": tagged(b"object %s\ntype commit\ntag v1\ntagger nobody 0 +0000\n\nx\n"),
        "tag with an empty name": tagged(b"object %s\ntype commit\ntag \ntagger %s\nx\n"),
        "tag repeats its object header": tagged(b"object %s\ntype commit\ntag v1\ntagger %s"
                                                b"object %s\n\nx\n", git=None),
        "NUL in a commit header": (*broken(tail=b"encoding UTF\0-8\n"), bad, "corrupt_object", True),
        "unterminated optional header": (*broken(tail=b"encoding UTF-8", msg=b""), bad, "corrupt_object", True),
        "continuation line before tree": (*broken(head=b" x\ntree " + tree.encode() + b"\n" + AUTHOR + COMMITTER),
                                          bad, "corrupt_object", True),
        "author with an empty name": (*broken(head=b"tree " + tree.encode() + b"\nauthor <a@example.com> 0 +0000\n"
                                              + COMMITTER), bad, "corrupt_object", True),
        "zero-padded author time": (*broken(head=b"tree " + tree.encode() + b"\nauthor A <a@example.com> 01 +0000\n"
                                            + COMMITTER), bad, "corrupt_object", True),
        "tree entry named dot": (*tree_case(b"100644 .\0" + bytes.fromhex(blob)), bad, "corrupt_object", True),
        "tree entry named dot-dot": (*tree_case(b"100644 ..\0" + bytes.fromhex(blob)), bad, "corrupt_object", True),
        "file and directory with one name": (*tree_case(b"100644 a\0" + bytes.fromhex(blob) + b"40000 a\0"
                                                        + bytes.fromhex(tree)), bad, "corrupt_object", True),
        "tree entry name over 255 bytes": (*tree_case(b"100644 " + b"n" * 256 + b"\0" + bytes.fromhex(blob)), bad,
                                           "corrupt_object", None),
        "valid empty blob": tagged_raw(b"blob 0", b"", ok, None),
        "leading-zero blob size": tagged_raw(b"blob 06", b"hello\n"),
        "size with a plus sign": tagged_raw(b"blob +6", b"hello\n"),
        "empty size": tagged_raw(b"blob ", b""),
        "declared size larger than the body": tagged_raw(b"blob 7", b"hello\n"),
        "declared size smaller than the body": tagged_raw(b"blob 5", b"hello\n"),
        "header without NUL in 32 bytes": tagged_raw(b"blob " + b"0" * 40, b""),
        "header without a size": tagged_raw(b"blob", b""),
        "leading-zero size on the HEAD commit": head_raw(b"commit 0%d" % len(child_body), child_body),
        "continuation after tree": (*broken(head=b"tree " + tree.encode() + b"\n x\n" + AUTHOR + COMMITTER), bad,
                                    "corrupt_object", True),
        "continuation after author": (*broken(head=b"tree " + tree.encode() + b"\n" + AUTHOR + b" x\n" + COMMITTER),
                                      bad, "corrupt_object", True),
        "continuation after committer": (*broken(tail=b" parent " + root.encode() + b"\n", objs=hist), bad,
                                         "corrupt_object", None),
        "continuation after an unsupported header": (*broken(tail=b"encoding UTF-8\n x\n"), bad, "corrupt_object",
                                                     None),
        "tagger time past Git's limit": tagged(b"object %s\ntype commit\ntag v1\ntagger A <a@example.com> "
                                               b"9999999999999999999 +0000\n\nx\n"),
        "continuation after a tag object": tagged(b"object %s\n x\ntype commit\ntag v1\ntagger %s\nx\n"),
        "valid mergetag on a merge": ({**hist, merge: merge_raw, mt: mt_raw}, {b"refs/heads/main": mt}, ok, None,
                                      False),
        "time 2^63-2": date_case(b"%d" % ((1 << 63) - 2), ok),
        "time 2^63-1, the largest Git accepts": date_case(b"%d" % ((1 << 63) - 1), ok),
        "time 2^63": date_case(b"%d" % (1 << 63)),
        "time 9999999999999999999": date_case(b"9" * 19),
        "time 2^64-1": date_case(b"%d" % ((1 << 64) - 1)),
        "time 2^64, 20 digits": date_case(b"%d" % (1 << 64)),
    }
    return rows


def object_files(objs, refs):
    g = {b".git/HEAD": b"ref: refs/heads/main\n",
         b".git/config": b"[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n"}
    g.update({b".git/" + name: oid.encode() + b"\n" for name, oid in refs.items()})
    g.update({pw.loose_key(oid): pw.zlib.compress(raw) for oid, raw in objs.items()})
    return g


class GitObjects(unittest.TestCase):
    """A loose object whose SHA-1 is right is not semantically verified unless
    its structure and every link's type are: mandatory commit headers, tree
    modes and names, and the type each link requires."""

    def test_matrix(self):
        for case, (objs, refs, want, finding, _) in object_rows().items():
            with self.subTest(case):
                feats, q3, q4, q5, reasons, _ = classify(object_files(objs, refs))
                self.assertEqual((q3, q4, q5), want)
                if finding:
                    self.assertIn(finding, feats)
                if want[0] != "VERIFIED":
                    self.assertEqual(reasons, [finding])

    def test_git_fsck_agrees(self):
        """Independent oracle: Git itself, in a disposable fixture repository,
        objects to every case PR-3b refuses and to none it verifies."""
        from gitfixture import GIT, Lab
        if GIT is None:
            self.skipTest("git not installed")
        lab = Lab()
        self.addCleanup(lab.cleanup)
        for n, (case, (objs, refs, _, _, objects)) in enumerate(object_rows().items()):
            if objects is None:
                continue
            with self.subTest(case):
                r = lab.path(f"r{n}")
                lab.git(lab.root, "init", "-q", r)
                for p, d in object_files(objs, refs).items():
                    if p != b".git/config":
                        lab.write(r, p.decode(), d)
                p = lab.git(r, "fsck", "--strict", "--no-dangling", "--no-progress", check=False)
                noted = p.returncode != 0 or bool(p.stderr.strip())
                self.assertEqual(noted, objects, p.stderr.decode(errors="replace"))

    def test_shared_object_checked_against_every_referrer(self):
        """One object reached first as the blob a tree entry wants, then as a
        commit's tree: the second link's type is still checked."""
        blob, blob_raw = gobj(b"blob", b"x\n")
        tree, tree_raw = gobj(b"tree", b"100644 x\0" + bytes.fromhex(blob))
        good, good_raw = gobj(b"commit", b"tree " + tree.encode() + b"\n" + AUTHOR + COMMITTER + b"\nm\n")
        bad, bad_raw = gobj(b"commit", b"tree " + blob.encode() + b"\nparent " + good.encode() + b"\n"
                            + AUTHOR + COMMITTER + b"\nm\n")
        objs = {blob: blob_raw, tree: tree_raw, good: good_raw, bad: bad_raw}
        for refs in ({b"refs/heads/main": bad}, {b"refs/heads/main": good, b"refs/tags/t": bad}):
            with self.subTest(refs):
                _, q3, _, q5, reasons, _ = classify(object_files(objs, refs))
                self.assertEqual((q3, q5, reasons), ("INCOMPLETE", "NOT_ELIGIBLE", ["corrupt_object"]))

    def test_old_tag_without_tagger_and_tag_of_a_blob(self):
        blob, blob_raw = gobj(b"blob", b"x\n")
        objs, base = object_rows()["valid ordinary commit"][:2]
        for body in (b"object " + base[b"refs/heads/main"].encode() + b"\ntype commit\ntag old\n\nv\n",
                     b"object " + blob.encode() + b"\ntype blob\ntag key\ntagger " + AUTHOR[7:] + b"\nk\n"):
            t, raw = gobj(b"tag", body)
            _, q3, _, q5, _, _ = classify(object_files({**objs, blob: blob_raw, t: raw},
                                                       {**base, b"refs/tags/t": t}))
            self.assertEqual((q3, q5), ("VERIFIED", "ELIGIBLE"), body)

    def test_repository_written_by_git(self):
        """Git's own objects: a merge, an annotated tag, a gitlink, an
        executable file, a symlink, a subdirectory and a commit of the empty
        tree are all verified."""
        from gitfixture import EMPTY_TREE, GIT, Lab
        if GIT is None:
            self.skipTest("git not installed")
        lab = Lab()
        self.addCleanup(lab.cleanup)
        r = lab.path("repo")
        lab.git(lab.root, "init", "-q", r)
        lab.write(r, "a.txt", b"a\n")
        lab.write(r, "bin/run", b"#!/bin/sh\n", mode=0o755)
        os.symlink("a.txt", os.path.join(r, "link"))
        lab.git(r, "add", "-A")
        lab.git(r, "commit", "-q", "-m", "root")
        lab.git(r, "switch", "-q", "-c", "side")
        lab.write(r, "b.txt", b"b\n")
        lab.git(r, "add", "b.txt")
        lab.git(r, "commit", "-q", "-m", "side")
        lab.git(r, "switch", "-q", "main")
        lab.git(r, "update-index", "--add", "--cacheinfo", "160000," + "5" * 40 + ",sub")
        lab.git(r, "commit", "-q", "-m", "gitlink")
        lab.git(r, "merge", "-q", "--no-ff", "-m", "merge", "side")
        lab.git(r, "tag", "-a", "-m", "release", "v1")
        self.assertEqual(lab.git(r, "hash-object", "-w", "-t", "tree", os.devnull).stdout.strip().decode(),
                         EMPTY_TREE)
        empty = lab.git(r, "commit-tree", EMPTY_TREE, "-m", "empty").stdout.strip()
        lab.git(r, "update-ref", "refs/heads/empty", empty)
        lab.git(r, "fsck", "--strict", "--no-dangling", "--no-progress")
        files = {}
        for d, _, names in os.walk(r):
            for n in names:
                p = os.path.join(d, n)
                if not os.path.islink(p):
                    with open(p, "rb") as f:
                        files[os.path.relpath(p, r).encode()] = f.read()
        feats, q3, q4, q5, reasons, _ = classify(files)
        self.assertEqual((q3, q4, q5, reasons), ("VERIFIED", "COMPLETE", "ELIGIBLE", []))
        self.assertIn("gitlink", feats)

    def test_links_parse_only_the_header(self):
        """Message lines that look like headers are not links."""
        tree, tree_raw = gobj(b"tree", b"")
        c, raw = gobj(b"commit", b"tree " + tree.encode() + b"\n" + AUTHOR + COMMITTER
                      + b"\ntree " + b"0" * 40 + b"\nparent " + b"1" * 40 + b"\n")
        _, q3, _, q5, _, _ = classify(object_files({tree: tree_raw, c: raw}, {b"refs/heads/main": c}))
        self.assertEqual((q3, q5), ("VERIFIED", "ELIGIBLE"))


def at(data, i, new):
    return data[:i] + new + data[i + len(new):]


def u32(v):
    return v.to_bytes(4, "big")


class PackIndex(unittest.TestCase):
    """B-3 / R2 K7: a reachable object that is not loose is `packed` only when
    a consistent version 2 index beside its matching pack lists it."""

    def setUp(self):
        self.files, self.coid = pw.git_repo(variant=("packed_only",))
        self.ip = next(p for p in self.files if p.endswith(b".idx"))
        self.pp = self.ip[:-4] + b".pack"
        self.idx, self.pack = self.files[self.ip], self.files[self.pp]
        self.n = int.from_bytes(self.idx[1028:1032], "big")

    def with_pair(self, idx, pack):
        return classify({**self.files, self.ip: idx, self.pp: pack})

    def unrelated_pair(self):
        oid, z = pw.gobj(b"blob", b"unrelated\n")
        pack, idx = pw.pack_pair({oid: pw.zlib.decompress(z)})
        return {b".git/objects/pack/pack-u.pack": pack, b".git/objects/pack/pack-u.idx": idx}

    def test_listed_objects_are_packed_and_not_eligible(self):
        """R4.2: index presence is not content verification."""
        _, q3, q4, q5, reasons, head = classify(self.files)
        self.assertEqual((q3, q4, q5, reasons, head),
                         ("INCOMPLETE", "COMPLETE", "NOT_ELIGIBLE", ["packed"], self.coid))
        self.assertEqual(len(gitclass._idx_names(self.idx, self.pack)), self.n)

    def test_a_pack_file_name_proves_nothing(self):
        files, _ = pw.git_repo(variant=("missing_object",))
        for extra in ({b".git/objects/pack/junk.pack": b"not a pack"}, self.unrelated_pair()):
            _, _, q4, q5, reasons, _ = classify({**files, **extra})
            self.assertEqual((q4, q5), ("INCOMPLETE", "NOT_ELIGIBLE"), extra)
            self.assertIn("missing_object", reasons)

    def test_pack_without_index_and_index_without_pack(self):
        _, _, q4, _, reasons, _ = classify({p: d for p, d in self.files.items() if p != self.ip})
        self.assertEqual((q4, reasons), ("INCOMPLETE", ["missing_object"]))
        _, _, q4, _, reasons, _ = classify({p: d for p, d in self.files.items() if p != self.pp})
        self.assertEqual((q4, reasons), ("INCOMPLETE", ["missing_object", "unknown"]))

    def test_loose_objects_beside_a_pack_stay_verified(self):
        files, _ = pw.git_repo()
        self.assertEqual(classify({**files, **self.unrelated_pair()})[1:5], ("VERIFIED", "COMPLETE", "ELIGIBLE", []))

    def test_count_above_the_bound(self):
        with mock.patch.object(gitclass, "MAX_OBJECTS", self.n - 1):
            self.assertIsNone(gitclass._idx_names(self.idx, self.pack))

    def test_every_structural_damage_is_unknown(self):
        idx, pack, n = self.idx, self.pack, self.n
        seal = pw.reseal
        names, offs = 1032, 1032 + 24 * n
        first = idx[names]
        self.assertGreater(first, 0)

        def repack(p):
            p = seal(p)
            return seal(idx[:-40] + p[-20:] + bytes(20)), p

        swapped = idx[:names] + idx[names + 20:names + 40] + idx[names:names + 20] + idx[names + 40:]
        cases = {
            "magic": (seal(at(idx, 0, b"\xfftOd")), pack),
            "version": (seal(at(idx, 4, u32(1))), pack),
            "fan-out not monotonic": (seal(at(idx, 8 + 4 * 254, u32(n + 1))), pack),
            "fan-out bucket": (seal(at(idx, 8 + 4 * (first - 1), u32(1))), pack),
            "names out of order": (seal(swapped), pack),
            "duplicate name": (seal(at(idx, names + 20, idx[names:names + 20])), pack),
            "extra bytes": (seal(idx[:-40] + bytes(8) + idx[-40:]), pack),
            "truncated": (idx[:1100], pack),
            "index checksum": (at(idx, len(idx) - 1, bytes([idx[-1] ^ 1])), pack),
            "recorded pack checksum": (seal(at(idx, len(idx) - 40, bytes(20))), pack),
            "pack trailer": (idx, at(pack, 20, bytes([pack[20] ^ 1]))),
            "pack resealed after a change": (idx, seal(at(pack, 20, bytes([pack[20] ^ 1])))),
            "pack object count": repack(at(pack, 8, u32(n + 1))),
            "pack version": repack(at(pack, 4, u32(4))),
            "pack magic": repack(at(pack, 0, b"KCAP")),
            "short pack": (idx, b"PACK"),
            "offset past the pack": (seal(at(idx, offs, u32(len(pack) - 20))), pack),
            "offset inside the header": (seal(at(idx, offs, u32(11))), pack),
            "duplicate offsets": (seal(at(idx, offs + 4, idx[offs:offs + 4])), pack),
            "large offset without a table entry": (seal(at(idx, offs, u32(0x80000000))), pack),
            "large offset below 2^31": (seal(at(idx, offs, u32(0x80000000))[:-40] + (12).to_bytes(8, "big")
                                             + idx[-40:]), pack),
        }
        for name, (bad_idx, bad_pack) in cases.items():
            with self.subTest(name):
                self.assertIsNone(gitclass._idx_names(bad_idx, bad_pack))
                _, _, q4, q5, reasons, _ = self.with_pair(bad_idx, bad_pack)
                self.assertEqual((q4, q5), ("INCOMPLETE", "NOT_ELIGIBLE"))
                self.assertIn("unknown", reasons)


def packed_rows():
    """{case: (files, q1, (q1, q3, q4, q5), reasons)} for owner amendment R4.2.
    A reachable object known only through a pack index is never content
    verified, so Q5 is NOT_ELIGIBLE whatever the pack holds; Q1 and Q4 keep
    their R2 meaning; loose objects decide whenever they are present."""
    loose, coid = pw.git_repo()
    toid, traw = pw.head_tree(loose, coid)
    no_tree = {p: d for p, d in loose.items() if p != pw.loose_key(toid)}
    raws = {p[13:].replace(b"/", b"").decode(): pw.zlib.decompress(d)
            for p, d in loose.items() if p.startswith(b".git/objects/")}
    readme = next(r for r in raws.values() if r == b"blob 6\0hello\n")
    body = b"100644 OTHER\0" + bytes(20)
    wrong_tree = b"tree %d\0" % len(body) + body
    packed = pw.git_repo(variant=("packed_only",))[0]
    ip = next(p for p in packed if p.endswith(b".idx"))
    nested = pw.git_repo(variant=("packed_only",), prefix=b"vendor/lib/")[0]
    unrelated = pw.with_pack(loose, {pw.gobj(b"blob", b"unrelated\n")[0]: b"blob 10\0unrelated\n"}, b"u")
    ok, pk = ("COMPLETE", "VERIFIED", "COMPLETE", "ELIGIBLE"), ("COMPLETE", "INCOMPLETE", "COMPLETE", "NOT_ELIGIBLE")
    gone = ("COMPLETE", "INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE")
    return {
        "01 loose only": (loose, "COMPLETE", ok, []),
        "02 packed only": (packed, "COMPLETE", pk, ["packed"]),
        "03 forged: index lists the tree, the pack holds a blob the repository has": (
            pw.with_pack(no_tree, {toid: readme}), "COMPLETE", pk, ["packed"]),
        "04 forged: index lists the tree, the pack holds an object that exists nowhere": (
            pw.with_pack(no_tree, {toid: b"blob 0\0"}), "COMPLETE", pk, ["packed"]),
        "05 forged: index lists the tree, the pack holds a different tree": (
            pw.with_pack(no_tree, {toid: wrong_tree}), "COMPLETE", pk, ["packed"]),
        "06a pack without its index": ({p: d for p, d in packed.items() if p != ip}, "COMPLETE", gone,
                                       ["missing_object"]),
        "06b corrupt index": ({**packed, ip: packed[ip][:-1] + bytes([packed[ip][-1] ^ 1])}, "COMPLETE", gone,
                              ["missing_object", "unknown"]),
        "07 index without its pack": ({p: d for p, d in packed.items() if not p.endswith(b".pack")}, "COMPLETE",
                                      gone, ["missing_object", "unknown"]),
        "09 mixed: loose commit and blobs, genuine packed tree": (
            pw.with_pack(no_tree, {toid: traw}), "COMPLETE", pk, ["packed"]),
        "09b nested repository with packed objects": ({**loose, **nested}, "COMPLETE", pk, ["packed"]),
        "10a unrelated pack, every reachable object loose": (unrelated, "COMPLETE", ok, []),
        "10b every reachable object loose and in a genuine pack": (pw.with_pack(loose, raws), "COMPLETE", ok, []),
        "10c loose object beside a forged pack entry for its id": (
            pw.with_pack(loose, {toid: readme}), "COMPLETE", ok, []),
        "11 complete raw capture, unverified packed objects": (packed, "COMPLETE", pk, ["packed"]),
        "12a incomplete raw capture, loose": (loose, "INCOMPLETE", ("INCOMPLETE", "VERIFIED", "INCOMPLETE",
                                                                    "NOT_ELIGIBLE"), []),
        "12b incomplete raw capture, packed": (packed, "INCOMPLETE", ("INCOMPLETE", "INCOMPLETE", "INCOMPLETE",
                                                                      "NOT_ELIGIBLE"), ["packed"]),
    }


class PackedEligibility(unittest.TestCase):
    def test_matrix(self):
        for case, (files, q1, want, reasons) in packed_rows().items():
            with self.subTest(case):
                _, q3, q4, q5, got, _ = gitclass.questions(gitclass.repositories(files), q1)
                self.assertEqual(((q1, q3, q4, q5), got), (want, reasons))

    def test_forged_entries_really_differ_from_the_listed_object(self):
        """The forged rows list an id whose packed bytes do not hash to it."""
        for case, (files, *_rest) in packed_rows().items():
            if case[:2] in ("03", "04", "05"):
                pack = next(d for p, d in files.items() if p.endswith(b".pack"))
                idx = next(d for p, d in files.items() if p.endswith(b".idx"))
                (listed,) = gitclass._idx_names(idx, pack)
                i = 12
                while pack[i] & 0x80:
                    i += 1
                body = pw.zlib.decompressobj().decompress(pack[i + 1:])
                typ = {1: b"commit", 2: b"tree", 3: b"blob"}[pack[12] >> 4 & 7]
                raw = typ + b" " + str(len(body)).encode() + b"\0" + body
                self.assertNotEqual(pw.hashlib.sha1(raw).hexdigest(), listed, case)  # noqa: S324


class TypedNeutralization(unittest.TestCase):
    """N-R41-9 through the real H-EST, in a full world."""

    def verdict(self, detail, extra=None):
        w = pw.World(est_detail=detail)
        try:
            w.to_p13()
            w.observe(extra=extra)
            cyc = w.cycles()[-1]
            doc = pw.o5._owner_doc(w.env, "containment-handoff", slot=pw.SLOT, op_id="op1", slot_generation=cyc.g_pre,
                                cycle_id=cyc.cycle_id, barrier_id=cyc.barrier_id)
            pre, sel = pw.freeze.basis_for(w.w, pw.SLOT, "handoff", doc=doc)
            v = w.prov.handoff_establish(pre, sel.public(), None)
            return v.passed, v.reasons
        finally:
            w.close()

    def test_typed_lists_pass(self):
        self.assertEqual(self.verdict(None), (True, []))

    def test_setuid_path_cannot_stand_in_for_a_channel(self):
        detail = dict(pw.EST_DETAIL, neutralized=list(pw.o5.NEUTRALIZED) + [pw.SETUID], neutralized_setuid=[])
        self.assertEqual(self.verdict(detail), (False, ["enforcement_record_unavailable"]))

    def test_channel_name_cannot_stand_in_for_a_setuid_path(self):
        detail = dict(pw.EST_DETAIL, neutralized_setuid=["agent_crontab"])
        self.assertEqual(self.verdict(detail), (False, ["enforcement_record_unavailable"]))

    def test_setuid_missing_from_its_own_list(self):
        detail = dict(pw.EST_DETAIL, neutralized=list(pw.o5.NEUTRALIZED), neutralized_setuid=[])
        self.assertEqual(self.verdict(detail), (False, ["enforcement_incomplete(setuid)"]))

    def test_legacy_flat_list_refuses(self):
        detail = {"enforcement": pw.o5.ENFORCEMENT, "neutralized": list(pw.o5.NEUTRALIZED) + [pw.SETUID],
                  "mounts_baseline": pw.MOUNTS}
        self.assertEqual(self.verdict(detail), (False, ["enforcement_record_unavailable"]))

    def test_wrong_types_refuse(self):
        for bad in ({"neutralized_setuid": "x"}, {"neutralized_setuid": [1]}, {"neutralized_setuid": {"x": 1}}):
            self.assertEqual(self.verdict(dict(pw.EST_DETAIL, **bad))[0], False, bad)

    def test_setuid_file_not_neutralized(self):
        self.assertEqual(self.verdict(None, {"setuid_writer_scan": {"files": [[pw.SETUID, 0o4755, False]]}}),
                         (False, ["enforcement_incomplete(setuid)"]))


if __name__ == "__main__":
    unittest.main()

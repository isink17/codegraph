"""The typed `codegraph.o5.manifest/v3` schema: closed key sets, typed
entries, the ancestor-`d` rule, deterministic chunking, totals, the path bound,
schema dispatch and the eligibility refusals. Validation only: no decision
reads a v3 manifest yet, so every v2 consumer refuses one. Inputs are built in
memory; nothing reads a live path."""

import types
import unittest
import unicodedata
from unittest import mock

import harness  # noqa: F401 - sys.path for broker
import preserve_world as pw
from broker import canonical, fdwalk, journal, o5contract as oc, preserve
from broker.journal import Mismatch
from test_preserve_flow import Base
from test_preserve_units import CID, CID2, PRE, SCTX, Mem, header

V3 = preserve.M_SCHEMA_V3
WALK = canonical.sha256(b"walk")
ORIGINALS = {a: canonical.sha256(a.encode()) for a in journal.ATTRS}
DROP = object()


def hx(path):
    return (path if type(path) is bytes else path.encode()).hex()


def d(path):
    return [hx(path), "d", "-", "-"]


def f(path, data=b"", attr="0"):
    return [hx(path), "f", attr, canonical.sha256(data)]


def link(path, target):
    return [hx(path), "l", "-", target.hex()]


def u(path, kind):
    return [hx(path), "u", kind, "-"]


def typed(files):
    """Typed entries for a {path: bytes} tree: every proper prefix is a `d`."""
    dirs = {b"/".join(p.split(b"/")[:k]) for p in files for k in range(1, p.count(b"/") + 1)}
    return sorted([d(x) for x in dirs] + [f(p, v) for p, v in files.items()])


def kind(e):
    return e[1] if type(e) is list and len(e) > 1 else None


def totals(entries, nbytes=0):
    t = {k: sum(kind(e) == t for e in entries) for t, k in preserve.V3_COUNTED.items()}
    return dict(t, entries=len(entries), bytes=nbytes)


def build(put, hdr, entries, phase="final", chunks=None, per_index=512, **fields):
    """The manifest object graph a v3 producer writes; returns the manifest digest."""
    chunks = preserve.manifest_chunks(hdr, entries) if chunks is None else chunks
    cds = [put({"header": hdr, "kind": "entries", "n": n, "entries": c}) for n, c in enumerate(chunks)]
    index = [put({"header": hdr, "kind": "entry_index", "n": n, "chunks": cds[i:i + per_index]})
             for n, i in enumerate(range(0, len(cds), per_index))]
    unsupported = any(kind(e) == "u" for e in entries)
    m = {"schema": V3, "header": hdr, "phase": phase, "entry_index": index, "totals": totals(entries),
         "walk": {"pass1": WALK, "pass2": WALK}, "result": "INCOMPLETE" if unsupported else "COMPLETE",
         "reasons": ["unsupported_entry"] if unsupported else []}
    if phase == "early":
        m["originals"] = ORIGINALS
    m.update(fields)
    return put({k: v for k, v in m.items() if v is not DROP})


class Lab:
    """One cycle's in-memory artifacts and blobs, and the `_Cycle` reading them."""

    def __init__(self, pre=PRE, cid=CID):
        self.mem, self.cid = Mem(), cid
        self.hdr = header(cid, slot=pre.slot)
        self.cy = preserve._Cycle(self.mem, pre, cid, SCTX)

    def put(self, o, cid=None):
        data = canonical.canon(o)
        h = canonical.sha256(data)
        self.mem.a[(cid or self.cid, h)] = data
        return h

    def blobs(self, contents):
        for c in contents:
            self.mem.b[canonical.sha256(c)] = c
        return sum(len(c) for c in set(contents))

    def manifest(self, entries, phase="final", **kw):
        return build(self.put, self.hdr, entries, phase, **kw)

    def binding(self, entries, phase="final", blobs=None, cycle_id=None, **kw):
        md = self.manifest(entries, phase, **kw)
        want = {e[3] for e in entries if e[1] == "f"} | (set(ORIGINALS.values()) if phase == "early" else set())
        bs = self.put({"header": self.hdr, "set": sorted(want if blobs is None else blobs)})
        return self.put({"header": self.hdr, "cycle_id": cycle_id or self.cid, "manifest": md, "blobs": bs})


SAMPLE_FILES = {b"bin/run.sh": b"#!/bin/sh\necho hi\n", b"docs/a.txt": b"alpha", b"readme": b"r"}
SAMPLE = sorted([d("bin"), f("bin/run.sh", SAMPLE_FILES[b"bin/run.sh"], "x"), d("docs"),
                 f("docs/a.txt", b"alpha"), d("docs/empty"), d("docs/sub"), d("docs/sub/deeper"),
                 f("readme", b"r")])


class Case(unittest.TestCase):
    def setUp(self):
        self.lab = Lab()

    def ok(self, entries, phase="final", **kw):
        return self.lab.cy.manifest_v3(self.lab.manifest(entries, phase, **kw), phase)

    def bad(self, fn, detail="artifact_mismatch"):
        """`detail=None`: an over-bound or headerless object, which the merged
        reader refuses before any v3 rule (`evidence_unavailable`)."""
        with self.assertRaises(Mismatch) as e:
            fn()
        self.assertEqual(e.exception.detail, detail or "evidence_unavailable")

    def bad_manifest(self, entries, phase="final", detail="artifact_mismatch", **kw):
        md = self.lab.manifest(entries, phase, **kw)
        self.bad(lambda: self.lab.cy.manifest_v3(md, phase), detail)


class ValidTrees(Case):
    def test_files_and_an_empty_directory(self):
        m, es = self.ok([d("a"), d("a/empty"), f("a/f", b"x")])
        self.assertEqual(es, [("61", "d", "-", "-"), (hx("a/empty"), "d", "-", "-"), tuple(f("a/f", b"x"))])
        self.assertEqual(m["totals"], {"entries": 3, "files": 1, "dirs": 2, "links": 0, "unsupported": 0,
                                       "bytes": 0})

    def test_nested_directories_and_both_file_modes(self):
        _, es = self.ok(SAMPLE)
        self.assertEqual([e[2] for e in es if e[1] == "f"], ["x", "0", "0"])
        self.assertIn((hx("docs/sub/deeper"), "d", "-", "-"), es)

    def test_empty_manifest(self):
        self.assertEqual(self.ok([])[1], [])

    def test_canonical_order_is_raw_byte_order(self):
        names = [b"B", b"a", b"a-b", b"a.b", b"a0", b"\xc3\xa9", b"\xff"]
        es = sorted([f(n) for n in names], key=lambda e: e[0])
        self.assertEqual([bytes.fromhex(e[0]) for e in self.ok(es)[1]], sorted(names))
        self.bad_manifest(es[::-1])

    def test_byte_exact_round_trip(self):
        """Entries come back as written, and the object graph (so the manifest
        digest) is a function of the entries and fields only."""
        names = [b"caf\xc3\xa9", b"cafe\xcc\x81", b"x\x01y", b"\xfe\xff"]
        es = sorted([d("t")] + [f(b"t/" + n, n) for n in names] + [link("w", b"../t/\xfe")])
        _, got = self.ok(es)
        self.assertEqual([list(e) for e in got], es)
        self.assertEqual(self.lab.manifest(es), Lab().manifest(es))
        self.assertEqual(bytes.fromhex(got[-1][3]), b"../t/\xfe")

    def test_binding_and_content(self):
        nbytes = self.lab.blobs(SAMPLE_FILES.values())
        b = self.lab.binding(SAMPLE, totals=totals(SAMPLE, nbytes))
        _, m, es, bs = self.lab.cy.binding_v3(b, "final")
        self.assertEqual(bs, sorted({e[3] for e in SAMPLE if e[1] == "f"}))
        self.assertTrue(self.lab.cy.content_v3(m, es))
        early = self.lab.binding(SAMPLE, "early", totals=totals(SAMPLE, nbytes))
        self.assertEqual(self.lab.cy.binding_v3(early, "early")[3],
                         sorted({e[3] for e in SAMPLE if e[1] == "f"} | set(ORIGINALS.values())))

    def test_verify_binding(self):
        md = self.lab.manifest(SAMPLE)
        bs = self.lab.put({"header": self.lab.hdr, "set": sorted({e[3] for e in SAMPLE if e[1] == "f"})})
        vb = self.lab.put({"header": self.lab.hdr, "cycle_id": CID, "manifest": md, "blobs": bs, "reverified": True,
                           "sealed_at": 1})
        self.assertEqual(self.lab.cy.binding_v3(vb, "final", verify=True)[2], [tuple(e) for e in SAMPLE])

    def test_classifier_head_is_optional_and_exact(self):
        self.ok(SAMPLE, classifier_head="a" * 40)
        for bad in ("A" * 40, "a" * 39, "a" * 64, 7):
            self.bad_manifest(SAMPLE, classifier_head=bad)


class Entries(Case):
    def test_malformed_entry_type_and_shape(self):
        for e in (["61", "x", "-", "-"], ["61", "F", "0", "0" * 64], ["61", "", "-", "-"], ["61", "d", "-"],
                  ["61", "d", "-", "-", "-"], ["61", "d", "-", 0], "61", ["61", "d", True, "-"],
                  [["61"], "d", "-", "-"], {"p": "61"}):
            with self.subTest(e):
                self.bad_manifest([e])

    def test_path_hex_encoding(self):
        for p in ("", "6", "6g", "6A", "GG", " 61", "61 ", "0x61"):
            with self.subTest(p):
                self.bad_manifest([[p, "d", "-", "-"]])

    def test_path_components_and_depth(self):
        for p in (b"/a", b"a/", b"a//b", b".", b"..", b"a/./b", b"a\0b", b"n" * 256):
            with self.subTest(p):
                self.bad_manifest([d(p)] if b"/" not in p.strip(b"/") else [d(p.split(b"/")[0]), d(p)])
        deep = [b"/".join([b"d"] * k) for k in range(1, 66)]
        self.ok([d(p) for p in deep[:65]])
        self.bad_manifest([d(p) for p in deep] + [d(b"/".join([b"d"] * 66))])

    def test_path_length_bound(self):
        """4096 raw bytes (8192 hex characters) is the bound; never truncated."""
        self.assertEqual(journal.MAX_PATH_BYTES, 4096)
        self.assertEqual(fdwalk.Limits().max_path, journal.MAX_PATH_BYTES)
        parts = [b"p" * 200] * 20
        at = b"/".join(parts + [b"z" * 76])
        self.assertEqual(len(at), 4096)
        dirs = [d(b"/".join(parts[:k])) for k in range(1, 21)]
        _, es = self.ok(dirs + [f(at)])
        self.assertEqual(bytes.fromhex(es[-1][0]), at)
        self.assertTrue(journal.path_hex(at.hex()))
        self.assertFalse(journal.path_hex((at + b"z").hex()))
        self.bad_manifest(dirs + [f(at + b"z")])

    def test_duplicate_and_conflicting_paths(self):
        self.bad_manifest([d("a"), d("a")])
        self.bad_manifest([d("a"), f("a")])
        self.bad_manifest([f("a"), d("a")])
        self.bad_manifest([f("a", b"1"), f("a", b"2")])

    def test_missing_parent_and_empty_directory_omission(self):
        self.bad_manifest([f("a/f")])
        self.bad_manifest([d("a"), f("a/b/f")])
        self.bad_manifest([d("a"), d("a/b/c")])

    def test_parent_must_be_a_directory(self):
        for parent in (f("a"), link("a", b"t"), u("a", "fifo")):
            with self.subTest(parent[1]):
                self.bad_manifest([parent, f("a/b")])
                self.bad_manifest([parent, d("a/b")])

    def test_file_modes(self):
        self.ok([f("a", attr="x"), f("b", attr="0")])
        for attr in ("1", "X", "755", "0755", "0644", "-", "", "x "):
            with self.subTest(attr):
                self.bad_manifest([f("a", attr=attr)])

    def test_directory_and_link_fields_are_fixed(self):
        for e in (["61", "d", "0", "-"], ["61", "d", "-", "0" * 64], ["61", "d", "x", "-"]):
            with self.subTest(e):
                self.bad_manifest([e])

    def test_symlink_target(self):
        self.ok([link("a", b"t" * 1024)])
        for target in ("", "7", "7A", "00", "6100", "74" * 1025, "zz"):
            with self.subTest(target[:8]):
                self.bad_manifest([[hx("a"), "l", "-", target]])
        self.bad_manifest([[hx("a"), "l", "0", "74"]])

    def test_unsupported_kinds_are_closed(self):
        for k in sorted(preserve.V3_UNSUPPORTED):
            self.ok([u("a", k)])
        for k in ("symlink", "socket", "FIFO", "-", ""):
            with self.subTest(k):
                self.bad_manifest([u("a", k)])
        self.bad_manifest([[hx("a"), "u", "fifo", "0" * 64]])

    def test_forbidden_metadata(self):
        """No ownership, mode, ACL, xattr or flag replay: no field carries them."""
        for e in ([hx("a"), "f", "0", "0" * 64, "501"], [hx("a"), "f", "0644", "0" * 64],
                  [hx("a"), "f", "0", "0" * 64, {"acl": []}], [hx("a"), "d", "0755", "-"]):
            with self.subTest(e):
                self.bad_manifest([e])
        for key in ("meta", "root", "questions", "owner", "mode", "acl", "xattr", "flags", "head_resolved"):
            with self.subTest(key):
                self.bad_manifest(SAMPLE, **{key: {}})


class ManifestFields(Case):
    def test_closed_keys(self):
        for key in ("schema", "header", "phase", "entry_index", "totals", "walk", "result", "reasons"):
            with self.subTest(key):
                self.bad_manifest(SAMPLE, detail=None if key == "header" else "artifact_mismatch", **{key: DROP})
        self.bad_manifest(SAMPLE, originals=ORIGINALS)
        self.bad_manifest(SAMPLE, "early", originals=DROP)
        self.bad_manifest(SAMPLE, "early", originals={"xattr": "0" * 64})
        md = self.lab.manifest(SAMPLE, "early")
        self.bad(lambda: self.lab.cy.manifest_v3(md, "final"))

    def test_walk(self):
        for walk in ({"pass1": WALK, "pass2": "0" * 64}, {"pass1": WALK}, {"pass1": "x", "pass2": "x"},
                     {"pass1": WALK, "pass2": WALK, "pass3": WALK}, [WALK, WALK]):
            with self.subTest(walk):
                self.bad_manifest(SAMPLE, walk=walk)

    def test_result_and_reasons(self):
        self.ok(SAMPLE, result="INCOMPLETE", reasons=["path_bound", "walk_gap"])
        for result, reasons in (("COMPLETE", ["walk_gap"]), ("INCOMPLETE", []), ("PARTIAL", []),
                                ("INCOMPLETE", ["b", "a"]), ("INCOMPLETE", ["a", "a"]), ("INCOMPLETE", ["A"]),
                                ("INCOMPLETE", ["a" * 65]), ("INCOMPLETE", [1]),
                                ("INCOMPLETE", ["r%02d" % i for i in range(33)])):
            with self.subTest((result, reasons)):
                self.bad_manifest(SAMPLE, result=result, reasons=reasons)
        self.ok(SAMPLE, result="INCOMPLETE", reasons=["r%02d" % i for i in range(32)])

    def test_unsupported_entry_is_never_complete(self):
        es = [d("a"), u("a/p", "fifo")]
        m, _ = self.ok(es)
        self.assertEqual(m["result"], "INCOMPLETE")
        self.bad_manifest(es, result="COMPLETE", reasons=[])

    def test_totals(self):
        good = totals(SAMPLE)
        for key in preserve.V3_TOTALS:
            for v in (good[key] + 1, -1, True, "1", [1]):
                if key == "bytes" and type(v) is int and v > 0:
                    continue
                with self.subTest((key, v)):
                    self.bad_manifest(SAMPLE, totals=dict(good, **{key: v}))
        self.bad_manifest(SAMPLE, totals=dict(good, extra=0))
        self.bad_manifest(SAMPLE, totals={k: v for k, v in good.items() if k != "bytes"})
        self.ok(SAMPLE, totals=dict(good, bytes=preserve.V3_MAX_BYTES))
        self.bad_manifest(SAMPLE, totals=dict(good, bytes=preserve.V3_MAX_BYTES + 1))

    def test_entry_count_bound(self):
        es = [f("f%02d" % i) for i in range(4)]
        with mock.patch.object(preserve, "V3_MAX_ENTRIES", 4):
            self.ok(es)
        with mock.patch.object(preserve, "V3_MAX_ENTRIES", 3):
            self.bad_manifest(es)

    def test_entry_bound_stops_reading(self):
        """Past the entry bound the reader refuses before it reads another chunk."""
        reads = []
        get = self.lab.mem.artifact
        self.lab.mem.artifact = lambda cid, h: reads.append(h) or get(cid, h)
        with mock.patch.object(preserve, "MAX_ENTRIES", 2):
            md = self.lab.manifest([d("%d" % i) for i in range(6)], totals=dict(totals([]), entries=3, dirs=3))
            with mock.patch.object(preserve, "V3_MAX_ENTRIES", 3):
                self.bad(lambda: self.lab.cy.manifest_v3(md, "final"))
        self.assertEqual(len(reads), 4)

    def test_header_bound(self):
        """A canonical header over 2,048 bytes refuses even when it binds."""
        for n, good in ((1500, True), (2000, False)):
            pre = types.SimpleNamespace(**dict(vars(PRE), slot="s" * n))
            lab = Lab(pre)
            md = lab.manifest(SAMPLE)
            self.assertEqual(len(canonical.canon(lab.hdr)) <= preserve.V3_MAX_HEADER, good)
            if good:
                lab.cy.manifest_v3(md, "final")
            else:
                self.bad(lambda: lab.cy.manifest_v3(md, "final"))


class Chunking(Case):
    def test_greedy_fill(self):
        es = [f("%05d" % i) for i in range(1200)]
        chunks = preserve.manifest_chunks(self.lab.hdr, es)
        self.assertGreater(len(chunks), 1)
        self.assertEqual(sum(chunks, []), es)
        for n, c in enumerate(chunks):
            size = len(canonical.canon({"header": self.lab.hdr, "kind": "entries", "n": n, "entries": c}))
            self.assertLessEqual(size, preserve.CHUNK_BYTES)
            if n + 1 < len(chunks):
                more = c + [chunks[n + 1][0]]
                self.assertGreater(len(canonical.canon({"header": self.lab.hdr, "kind": "entries", "n": n,
                                                        "entries": more})), preserve.CHUNK_BYTES)
        self.assertEqual(len(self.ok(es)[1]), 1200)

    def test_entry_limit_splits(self):
        with mock.patch.object(preserve, "MAX_ENTRIES", 3):
            es = [d("%d" % i) for i in range(7)]
            self.assertEqual([len(c) for c in preserve.manifest_chunks(self.lab.hdr, es)], [3, 3, 1])
            self.ok(es)
            self.bad_manifest(es, chunks=[es[:2], es[2:5], es[5:]])

    def test_non_greedy_chunk_split_refuses(self):
        es = [f("%05d" % i) for i in range(1200)]
        greedy = preserve.manifest_chunks(self.lab.hdr, es)
        self.assertLess(len(greedy[-1]), len(greedy[-2]))
        moved = greedy[:-2] + [greedy[-2][:-1], [greedy[-2][-1]] + greedy[-1]]
        self.assertEqual(sum(moved, []), es)
        self.bad_manifest(es, chunks=moved)
        self.bad_manifest(es, chunks=[es[:10]] + greedy)

    def test_index_split(self):
        with mock.patch.object(preserve, "MAX_INDEX_CHUNKS", 2), mock.patch.object(preserve, "MAX_ENTRIES", 1):
            es = [d("%d" % i) for i in range(5)]
            self.assertEqual(len(self.ok(es, per_index=2)[0]["entry_index"]), 3)
            self.bad_manifest(es, per_index=1)

    def test_capacity(self):
        with mock.patch.object(preserve, "MAX_INDEX", 1), mock.patch.object(preserve, "MAX_INDEX_CHUNKS", 2), \
                mock.patch.object(preserve, "MAX_ENTRIES", 1):
            self.assertEqual(len(preserve.manifest_chunks(self.lab.hdr, [d("a"), d("b")])), 2)
            self.assertIsNone(preserve.manifest_chunks(self.lab.hdr, [d("a"), d("b"), d("c")]))

    def test_worst_case_entry_fits_four_per_chunk(self):
        """§16.2.4: an `l` at 4096 path bytes with a 1024-byte target."""
        hdr = header(slot="s" * 128, op_id="o" * 128)
        worst = [[(b"%04d" % i + b"p" * 4092).hex(), "l", "-", "74" * 1024] for i in range(4)]
        self.assertEqual(len(canonical.canon(worst[0])), 10255)
        self.assertEqual(len(preserve.manifest_chunks(hdr, worst)), 1)

    def test_oversized_chunk_refuses(self):
        es = [f("%05d" % i) for i in range(1200)]
        self.bad_manifest(es, chunks=[es], detail=None)

    def test_inventory_chunker_is_separate(self):
        self.assertIsNot(preserve.manifest_chunks, preserve.chunks_of)


class Bindings(Case):
    def test_incorrect_content(self):
        nbytes = self.lab.blobs(SAMPLE_FILES.values())
        md = self.lab.manifest(SAMPLE, totals=totals(SAMPLE, nbytes))
        m, es = self.lab.cy.manifest_v3(md, "final")
        self.assertTrue(self.lab.cy.content_v3(m, es))
        self.lab.mem.b[canonical.sha256(b"alpha")] = b"alphA"
        self.assertFalse(self.lab.cy.content_v3(m, es))
        del self.lab.mem.b[canonical.sha256(b"alpha")]
        self.assertFalse(self.lab.cy.content_v3(m, es))
        self.lab.blobs([b"alpha"])
        m2, es2 = self.ok(SAMPLE, totals=totals(SAMPLE, nbytes + 1))
        self.bad(lambda: self.lab.cy.content_v3(m2, es2))

    def test_altered_object_refuses(self):
        md = self.lab.manifest(SAMPLE)
        m = canonical.canon(dict(self.lab.cy.manifest_v3(md, "final")[0], phase="final"))
        self.lab.mem.a[(CID, md)] = m.replace(b'"COMPLETE"', b'"INCOMPLETE"')
        self.bad(lambda: self.lab.cy.manifest_v3(md, "final"))

    def test_blob_set_is_the_file_values(self):
        """A symlink target is never a blob, even when it looks like a digest."""
        es = [link("a", bytes(range(1, 33))), f("b", b"x")]
        self.assertTrue(canonical.is_hex64(es[0][3]))
        self.lab.cy.binding_v3(self.lab.binding(es), "final")
        for blobs in ([es[0][3], es[1][3]], [], [es[1][3], "0" * 64]):
            with self.subTest(blobs):
                self.bad(lambda: self.lab.cy.binding_v3(self.lab.binding(es, blobs=blobs), "final"))

    def test_foreign_cycle(self):
        b = self.lab.binding(SAMPLE, cycle_id=CID2)
        self.bad(lambda: self.lab.cy.binding_v3(b, "final"), "cycle_binding")
        other = Lab(cid=CID2)
        md = other.manifest(SAMPLE)
        self.bad(lambda: self.lab.cy.manifest_v3(md, "final"), "evidence_unavailable")
        self.lab.mem.a[(CID, md)] = other.mem.a[(CID2, md)]
        self.bad(lambda: self.lab.cy.manifest_v3(md, "final"), "cycle_binding")

    def test_chunk_of_another_cycle(self):
        md = self.lab.manifest(SAMPLE)
        (idx,) = self.lab.cy.manifest_v3(md, "final")[0]["entry_index"]
        self.lab.mem.a[(CID2, idx)] = self.lab.mem.a.pop((CID, idx))
        self.bad(lambda: self.lab.cy.manifest_v3(md, "final"), "evidence_unavailable")

    def test_foreign_slot(self):
        lab = Lab(types.SimpleNamespace(**dict(vars(PRE), slot="t")))
        md = lab.manifest(SAMPLE)
        self.lab.mem.a[(CID, md)] = lab.mem.a[(CID, md)]
        self.bad(lambda: self.lab.cy.manifest_v3(md, "final"), "barrier_binding")


class Dispatch(Case):
    def test_accepted_schemas(self):
        self.assertEqual(preserve.accepted_schemas(oc.PROTECTED), (V3,))
        self.assertEqual(preserve.accepted_schemas(oc.SYNTHETIC), (preserve.M_SCHEMA, V3))
        for mode in (None, "", "synthetic", "PROTECTED ", ["SYNTHETIC"], 1):
            self.assertEqual(preserve.accepted_schemas(mode), (), mode)

    def test_v3_is_validated_but_refused_by_the_v2_consumer(self):
        b = self.lab.binding(SAMPLE)
        self.lab.cy.binding_v3(b, "final")
        self.bad(lambda: self.lab.cy.binding(b, "final"))

    def test_v2_is_not_a_v3_manifest(self):
        es = [[hx("a"), canonical.sha256(b"x")]]
        md = self.lab.put({"schema": preserve.M_SCHEMA, "header": self.lab.hdr, "phase": "final",
                           "entry_index": [self.lab.put({"header": self.lab.hdr, "kind": "entry_index", "n": 0,
                                                         "chunks": [self.lab.put({"header": self.lab.hdr,
                                                                                  "kind": "entries", "n": 0,
                                                                                  "entries": es})]})],
                           "result": "COMPLETE", "reasons": []})
        self.assertEqual(self.lab.cy.manifest(md, "final")["schema"], preserve.M_SCHEMA)
        self.bad(lambda: self.lab.cy.manifest_v3(md, "final"))
        self.bad_manifest([], schema=preserve.M_SCHEMA)

    def test_arity_follows_the_schema(self):
        self.bad_manifest([[hx("a"), canonical.sha256(b"x")]])
        md = self.lab.manifest([f("a")], schema=preserve.M_SCHEMA, totals=DROP, walk=DROP)
        self.bad(lambda: self.lab.cy.binding(self.lab.put({"header": self.lab.hdr, "cycle_id": CID, "manifest": md,
                                                           "blobs": self.lab.put({"header": self.lab.hdr,
                                                                                  "set": []})}), "final"))

    def test_protected_mode_refuses_v2_and_a_forged_mode_refuses_both(self):
        for mode, v2_ok, v3_ok in ((oc.PROTECTED, False, True), ("forged", False, False)):
            lab = Lab(types.SimpleNamespace(**dict(vars(PRE), mode=mode)))
            lab.hdr = header(mode=mode)
            md3 = lab.manifest(SAMPLE)
            md2 = lab.put({"schema": preserve.M_SCHEMA, "header": lab.hdr, "phase": "final", "entry_index": [],
                           "result": "COMPLETE", "reasons": []})
            for fn, ok in ((lambda: lab.cy.manifest(md2, "final"), v2_ok), (lambda: lab.cy.manifest_v3(md3, "final"),
                                                                            v3_ok)):
                if ok:
                    fn()
                else:
                    self.bad(fn)


class Eligibility(unittest.TestCase):
    def test_clean_tree_has_no_refusal(self):
        self.assertEqual(preserve.refusals_v3([tuple(e) for e in SAMPLE]), [])

    def test_symlinks_and_unsupported_never_become_eligible(self):
        self.assertEqual(preserve.refusals_v3([tuple(link("w", b"t"))]), ["symlink"])
        self.assertEqual(preserve.refusals_v3([tuple(d(".git")), tuple(link(".git/HEAD", b"t"))]), ["unknown"])
        lab = Lab()
        m, _ = lab.cy.manifest_v3(lab.manifest([u("p", "hardlink")]), "final")
        self.assertEqual(m["result"], "INCOMPLETE")

    def test_case_fold_collision(self):
        for a, b in ((b"README", b"readme"), (b"Dir/x", b"dir/y"), (b"stra\xc3\x9fe", b"STRASSE")):
            with self.subTest((a, b)):
                es = sorted(typed({a: b"1", b: b"2"}))
                lab = Lab()
                _, got = lab.cy.manifest_v3(lab.manifest(es), "final")
                self.assertIn("name_alias", preserve.refusals_v3(got))

    def test_unicode_alias_collision(self):
        nfc = unicodedata.normalize("NFC", "café").encode()
        nfd = unicodedata.normalize("NFD", "café").encode()
        self.assertNotEqual(nfc, nfd)
        for a, b in ((nfc, nfd), (b"ab", b"a\xe2\x80\x8cb")):
            with self.subTest((a, b)):
                self.assertEqual(preserve.refusals_v3([tuple(e) for e in typed({a: b"1", b: b"2"})]),
                                 ["name_alias"])


class V2ConsumerEndToEnd(Base):
    """A v3 EP manifest that reaches the merged consumers refuses with
    `artifact_mismatch`: never a bare exception, never eligible."""

    def test_freeze_start_with_a_v3_manifest_is_rejected(self):
        def v3_manifest(fx, s, phase, files):
            for v in files.values():
                fx.blob(v)
            es = typed(files)
            hdr = fx.header(s)
            originals = s["originals"] if phase == "early" else DROP
            md = build(lambda o: fx.obj(s, o), hdr, es, phase, originals=originals,
                       totals=totals(es, sum(len(v) for v in set(files.values()))))
            want = {e[3] for e in es if e[1] == "f"} | (set(s["originals"].values()) if phase == "early" else set())
            bs = fx.obj(s, {"header": hdr, "set": sorted(want)})
            return fx.obj(s, {"header": hdr, "cycle_id": s["cid"], "manifest": md, "blobs": bs}), md, bs

        with mock.patch.object(pw.Effects, "manifest", v3_manifest):
            self.ok("adopt", "request")
            self.rejected("freeze-start", "fresult_rejected:artifact_mismatch", "MOVE_INTENT")
        s = self.w.st()
        pre = types.SimpleNamespace(**{k: s[k] for k in ("slot", "op_id", "g_pre", "cutover_head", "barrier_id",
                                                         "root_id", "mode")})
        cy = preserve._Cycle(self.w.prov._r, pre, s["cid"], s["start_ctx"])
        _, m, es, _ = cy.binding_v3(s["early"], "early")
        self.assertTrue(cy.content_v3(m, es))


if __name__ == "__main__":
    unittest.main()

"""The typed `codegraph.o5.manifest/v3` schema: closed key sets, typed
entries, the ancestor-`d` rule, deterministic chunking, totals, the path bound,
schema dispatch and the eligibility refusals. Validation only: no decision
reads a v3 manifest yet, so every v2 consumer refuses one. Inputs are built in
memory; nothing reads a live path."""

import itertools
import types
import unittest
import unicodedata
from unittest import mock

import harness  # noqa: F401 - sys.path for broker
import preserve_world as pw
from broker import canonical, fdwalk, journal, o5contract as oc, preserve
from broker.journal import Mismatch
from broker.refusal import Code
from test_preserve_flow import Base
from test_preserve_units import CID, CID2, PRE, SCTX, Mem, header

V3 = preserve.M_SCHEMA_V3
WALK = canonical.sha256(b"walk")
ORIGINALS = {a: canonical.sha256(a.encode()) for a in journal.ATTRS}
DROP = object()
# The 16 code points HFS+ and APFS ignore in names, as CONTRACT.md lists them.
HFS_IGNORABLE = [chr(c) for c in (*range(0x200C, 0x2010), *range(0x202A, 0x202F), *range(0x206A, 0x2070), 0xFEFF)]


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
        return sum(len(c) for c in contents)

    def manifest(self, entries, phase="final", **kw):
        return build(self.put, self.hdr, entries, phase, **kw)

    def binding(self, entries, phase="final", blobs=None, cycle_id=None, **kw):
        md = self.manifest(entries, phase, **kw)
        want = {e[3] for e in entries if e[1] == "f"} | (set(ORIGINALS.values()) if phase == "early" else set())
        bs = self.put({"header": self.hdr, "set": sorted(want) if blobs is None else blobs})
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
        if e.exception.detail == "artifact_mismatch":
            self.assertIs(e.exception.code, Code.STATE_UNKNOWN)

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

    def test_classifier_head_is_optional_and_exact(self):
        self.ok(SAMPLE, classifier_head="a" * 40)
        for bad in ("A" * 40, "a" * 39, "a" * 64, 7):
            self.bad_manifest(SAMPLE, classifier_head=bad)


class Entries(Case):
    def test_malformed_entry_type_and_shape(self):
        for e in (["61", "x", "-", "-"], ["61", "F", "0", "0" * 64], ["61", "", "-", "-"], ["61", "d", "-"],
                  ["61", "d", "-", "-", "-"], ["61", "d", "-", 0], "61", ["61", "d", True, "-"],
                  [["61"], "d", "-", "-"], {"p": "61"}, ["61", "l", "-", 5], ["61", "u", ["fifo"], "-"],
                  ["61", "x", "fifo", "-"], ["61", "f", ["0"], "a" * 64], 7, True,
                  {"a": "61", "b": "d", "c": "-", "d": "-"}, ["61", "D", "-", "-"], ["61", "s", "-", "74"],
                  ["61", "U", "fifo", "-"], ["61", "L", "-", "74"]):
            with self.subTest(e):
                self.bad_manifest([e])

    def test_path_hex_encoding(self):
        for p in ("", "6", "6g", "6A", "GG", " 61", "61 ", "0x61"):
            with self.subTest(p):
                self.assertFalse(journal.path_hex(p))
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

    def test_file_digest_encoding(self):
        for v in ("-", "A" * 64, "0" * 63, "0" * 65, "g" * 64, ""):
            with self.subTest(v):
                self.bad_manifest([[hx("a"), "f", "0", v]])

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
        self.ok([link("b", b"\x10\x0a")])

    def test_unsupported_kinds_are_closed(self):
        kinds = {"fifo", "sock", "chr", "blk", "whiteout", "hardlink", "foreign_uid", "cross_device", "unreadable",
                 "unstable", "oversize", "other"}
        self.assertEqual(preserve.V3_UNSUPPORTED, kinds)
        for k in sorted(kinds):
            self.ok([u("a", k)])
        for k in ("symlink", "socket", "FIFO", "-", "", "door"):
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

    def test_phase_must_match(self):
        """An early manifest never reads as final, nor a final one as early."""
        for built, stored, read in (("final", "early", "final"), ("early", "final", "early"),
                                    ("final", "EARLY", "final"), ("final", "verify", "final"),
                                    ("final", "verify", "verify")):
            with self.subTest((built, stored)):
                lab = Lab()
                m, _ = journal.parse(lab.mem.a[(CID, lab.manifest(SAMPLE, built))])
                md = lab.put(dict(m, phase=stored))
                self.bad(lambda: lab.cy.manifest_v3(md, read))

    def test_bounds_are_pinned(self):
        self.assertEqual((preserve.V3_MAX_HEADER, preserve.V3_MAX_BYTES, preserve.V3_MAX_ENTRIES,
                          preserve.V3_MAX_REASONS, journal.MAX_PATH_BYTES, journal.MAX_PATH_DEPTH),
                         (2048, 1 << 31, 200_000, 32, 4096, 64))
        self.assertEqual((preserve.MAX_INDEX, preserve.MAX_INDEX_CHUNKS, preserve.MAX_ENTRIES, preserve.CHUNK_BYTES,
                          journal.MAX_OBJECT), (512, 512, 4096, 49152, 49152))

    def test_walk(self):
        for walk in (["pass1", "pass2"], {"pass1": WALK, "pass2": "0" * 64}, {"pass1": WALK},
                     {"pass1": "x", "pass2": "x"},
                     {"pass1": WALK, "pass2": WALK, "pass3": WALK}, [WALK, WALK],
                     {"pass1": WALK, "pass2": WALK + "0"}, {"pass1": WALK, "pass2": [WALK]}, 7,
                     {"pass1": WALK, "pass3": WALK}):
            with self.subTest(walk):
                self.bad_manifest(SAMPLE, walk=walk)

    def test_result_and_reasons(self):
        self.ok(SAMPLE, result="INCOMPLETE", reasons=["path_bound", "walk_gap"])
        self.ok(SAMPLE, result="INCOMPLETE", reasons=["a" * 64])
        for r in (["0abc"], ["_x"]):
            with self.subTest(r):
                self.bad_manifest(SAMPLE, result="INCOMPLETE", reasons=r)
        for result, reasons in (("COMPLETE", ["walk_gap"]), ("INCOMPLETE", []), ("PARTIAL", []),
                                ("INCOMPLETE", ["b", "a"]), ("INCOMPLETE", ["a", "a"]), ("INCOMPLETE", ["A"]),
                                ("INCOMPLETE", ["a" * 65]), ("INCOMPLETE", [1]),
                                ("INCOMPLETE", ["r%02d" % i for i in range(33)]), ("INCOMPLETE", 7),
                                ("INCOMPLETE", True), ("INCOMPLETE", "walk_gap"), ("INCOMPLETE", {"a": 1})):
            with self.subTest((result, reasons)):
                self.bad_manifest(SAMPLE, result=result, reasons=reasons)
        self.ok(SAMPLE, result="INCOMPLETE", reasons=["r%02d" % i for i in range(32)])

    def test_unsupported_entry_is_never_complete(self):
        es = [d("a"), u("a/p", "fifo")]
        m, _ = self.ok(es)
        self.assertEqual(m["result"], "INCOMPLETE")
        self.bad_manifest(es, result="COMPLETE", reasons=[])

    def test_unsupported_entry_named_like_git_is_valid(self):
        for es in ([u(".gitkeep", "fifo")], [d(".git"), u(".git/p", "fifo")], [u(".github", "sock")]):
            with self.subTest(es):
                m, _ = self.ok(es)
                self.assertEqual(m["result"], "INCOMPLETE")

    def test_totals(self):
        good = totals(SAMPLE)
        for key in preserve.V3_TOTALS:
            for v in (good[key] + 1, -1, True, "1", [1]):
                if key == "bytes" and type(v) is int and v > 0:
                    continue
                with self.subTest((key, v)):
                    self.bad_manifest(SAMPLE, totals=dict(good, **{key: v}))
        self.bad_manifest(SAMPLE, totals=dict(good, extra=0))
        for t in (7, True, "entries", list(preserve.V3_TOTALS), [list(kv) for kv in sorted(good.items())]):
            with self.subTest(t):
                self.bad_manifest(SAMPLE, totals=t)
        self.bad_manifest(SAMPLE, totals={k: v for k, v in good.items() if k != "bytes"})
        self.ok(SAMPLE, totals=dict(good, bytes=preserve.V3_MAX_BYTES))
        self.bad_manifest(SAMPLE, totals=dict(good, bytes=preserve.V3_MAX_BYTES + 1))

    def test_index_count_bound(self):
        """More than MAX_INDEX index digests refuse before any is read."""
        md = self.lab.put({"schema": V3, "header": self.lab.hdr, "phase": "final",
                           "entry_index": ["0" * 64] * (preserve.MAX_INDEX + 1), "totals": totals([]),
                           "walk": {"pass1": WALK, "pass2": WALK}, "result": "COMPLETE", "reasons": []})
        self.bad(lambda: self.lab.cy.manifest_v3(md, "final"))

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

    def test_header_bound_is_inclusive(self):
        """A canonical header of exactly 2,048 bytes is accepted; 2,049 refuses."""
        base = len(canonical.canon(header(slot="")))
        for n, good in ((preserve.V3_MAX_HEADER - base, True), (preserve.V3_MAX_HEADER - base + 1, False)):
            lab = Lab(types.SimpleNamespace(**dict(vars(PRE), slot="s" * n)))
            self.assertEqual(len(canonical.canon(lab.hdr)), n + base)
            md = lab.manifest(SAMPLE)
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

    def exact_at(self, n, over=0):
        """Entries whose chunk at ordinal n is exactly CHUNK_BYTES + over bytes, then one more entry."""
        hdr = self.lab.hdr
        uniform = [d("b%05d" % i) for i in range(30 * (n + 2))]
        ch = preserve.manifest_chunks(hdr, uniform)
        pre, body = sum(ch[:n], []), [ch[n][0]]
        for parity in ([], [d("c")]):
            for k in range(1, 1025):
                full = body + parity + [link("e", b"p" * k)]
                size = len(canonical.canon({"header": hdr, "kind": "entries", "n": n, "entries": full}))
                if size == preserve.CHUNK_BYTES + over:
                    return pre + full + [d("z")], len(full)
        raise AssertionError("no construction")

    @mock.patch.object(preserve, "CHUNK_BYTES", 1200)
    def test_exact_fit_at_each_ordinal_width(self):
        """An exactly full chunk stays whole and one byte more splits it, whatever
        the digit count of its ordinal."""
        for n in (0, 1, 9, 10, 11, 99, 100):
            with self.subTest(n):
                es, k = self.exact_at(n)
                self.assertEqual([len(c) for c in preserve.manifest_chunks(self.lab.hdr, es)][n:], [k, 1])
                self.ok(es)
                es, k = self.exact_at(n, over=1)
                self.assertEqual([len(c) for c in preserve.manifest_chunks(self.lab.hdr, es)][n:], [k - 1, 2])
                self.ok(es)

    def test_exact_fit(self):
        """A chunk of exactly CHUNK_BYTES is one chunk; one more entry splits it."""
        def size(es):
            return len(canonical.canon({"header": self.lab.hdr, "kind": "entries", "n": 0, "entries": es}))
        es, i = [], 0
        while size(es + [f("%05d" % i)]) <= preserve.CHUNK_BYTES - 1500:
            es.append(f("%05d" % i))
            i += 1
        tail = [d("}")] if (preserve.CHUNK_BYTES - size(es + [link("~", b"p")])) % 2 else []
        rest = preserve.CHUNK_BYTES - size(es + tail + [link("~", b"p")])
        es += tail + [link("~", b"p" * (1 + rest // 2))]
        self.assertEqual(size(es), preserve.CHUNK_BYTES)
        self.assertEqual(len(preserve.manifest_chunks(self.lab.hdr, es)), 1)
        self.ok(es)
        more = es + [f("~~")]
        self.assertEqual([len(c) for c in preserve.manifest_chunks(self.lab.hdr, more)], [len(es), 1])
        self.ok(more)

    def numbered(self, chunk_ns, index_ns):
        """Three one-entry chunks, two per index object, with the given ordinals."""
        es = [d("%d" % i) for i in range(3)]
        cds = [self.lab.put({"header": self.lab.hdr, "kind": "entries", "n": n, "entries": [e]})
               for n, e in zip(chunk_ns, es)]
        index = [self.lab.put({"header": self.lab.hdr, "kind": "entry_index", "n": n, "chunks": cds[2 * k:2 * k + 2]})
                 for k, n in enumerate(index_ns)]
        md = self.lab.put({"schema": V3, "header": self.lab.hdr, "phase": "final", "entry_index": index,
                           "totals": totals(es), "walk": {"pass1": WALK, "pass2": WALK}, "result": "COMPLETE",
                           "reasons": []})
        return lambda: self.lab.cy.manifest_v3(md, "final")

    def test_misnumbered_objects(self):
        """Ordinals are exact integers: renumbered, repeated or boolean ones refuse."""
        with mock.patch.object(preserve, "MAX_ENTRIES", 1), mock.patch.object(preserve, "MAX_INDEX_CHUNKS", 2):
            self.assertEqual(len(self.numbered([0, 1, 2], [0, 1])()[1]), 3)
            for chunk_ns, index_ns in (([0, 2, 2], [0, 1]), ([1, 1, 2], [0, 1]), ([0, 1, 2], [1, 1]),
                                       ([0, 1, 2], [0, 0]), ([False, 1, 2], [0, 1]), ([0, True, 2], [0, 1]),
                                       ([0, 1, 2], [False, 1]), ([0, 1, 2], [0, True])):
                with self.subTest((chunk_ns, index_ns)):
                    self.bad(self.numbered(chunk_ns, index_ns))

    @mock.patch.object(preserve, "MAX_INDEX_CHUNKS", 1)
    def test_empty_trailing_index_refuses(self):
        """A full index object followed by an index object with no chunks."""
        es = [d("a")]
        cd = self.lab.put({"header": self.lab.hdr, "kind": "entries", "n": 0, "entries": es})
        index = [self.lab.put({"header": self.lab.hdr, "kind": "entry_index", "n": 0, "chunks": [cd]}),
                 self.lab.put({"header": self.lab.hdr, "kind": "entry_index", "n": 1, "chunks": []})]
        md = self.lab.put({"schema": V3, "header": self.lab.hdr, "phase": "final", "entry_index": index,
                           "totals": totals(es), "walk": {"pass1": WALK, "pass2": WALK}, "result": "COMPLETE",
                           "reasons": []})
        self.bad(lambda: self.lab.cy.manifest_v3(md, "final"))

    @mock.patch.object(preserve, "CHUNK_BYTES", 1200)
    def test_every_boundary_is_exact(self):
        """Every chunk fits, and no chunk could take the next entry."""
        for pad in range(1, 100):
            es = sorted([d("b0400" + "x" * pad)] + [d("b%04d" % i) for i in range(600)])
            chunks = preserve.manifest_chunks(self.lab.hdr, es)
            self.assertEqual(sum(chunks, []), es)
            for n, c in enumerate(chunks):
                def size(c):
                    return len(canonical.canon({"header": self.lab.hdr, "kind": "entries", "n": n, "entries": c}))
                self.assertLessEqual(size(c), 1200)
                if n + 1 < len(chunks):
                    self.assertGreater(size(c + [chunks[n + 1][0]]), 1200)

    def test_one_byte_over_splits(self):
        """Entries whose single chunk would be CHUNK_BYTES + 1 split [n - 1, 1] and validate."""
        def size(es):
            return len(canonical.canon({"header": self.lab.hdr, "kind": "entries", "n": 0, "entries": es}))
        es, i = [], 0
        while size(es + [f("%05d" % i)]) <= preserve.CHUNK_BYTES - 1500:
            es.append(f("%05d" % i))
            i += 1
        tail = [d("}")] if (preserve.CHUNK_BYTES + 1 - size(es + [link("~", b"p")])) % 2 else []
        rest = preserve.CHUNK_BYTES + 1 - size(es + tail + [link("~", b"p")])
        es += tail + [link("~", b"p" * (1 + rest // 2))]
        self.assertEqual(size(es), preserve.CHUNK_BYTES + 1)
        self.assertEqual([len(c) for c in preserve.manifest_chunks(self.lab.hdr, es)], [len(es) - 1, 1])
        self.ok(es)

    def test_object_shapes(self):
        """Index and chunk objects keep their own kind, a list of chunks and a
        list of entries; the manifest's index is a list."""
        es = [d("a")]

        def graph(chunk=None, index=None, entry_index=None):
            cd = self.lab.put(dict({"header": self.lab.hdr, "kind": "entries", "n": 0, "entries": es}, **(chunk or {})))
            idx = self.lab.put(dict({"header": self.lab.hdr, "kind": "entry_index", "n": 0, "chunks": [cd]},
                                    **(index or {})))
            md = self.lab.put({"schema": V3, "header": self.lab.hdr, "phase": "final",
                               "entry_index": [idx] if entry_index is None else entry_index, "totals": totals(es),
                               "walk": {"pass1": WALK, "pass2": WALK}, "result": "COMPLETE", "reasons": []})
            return lambda: self.lab.cy.manifest_v3(md, "final")
        graph()()
        for kw in ({"chunk": {"kind": "entry_index"}}, {"index": {"kind": "entries"}}, {"chunk": {"entries": 7}},
                   {"chunk": {"entries": {"a": es[0]}}}, {"index": {"chunks": 7}}):
            with self.subTest(kw):
                self.bad(graph(**kw))
        empty = self.lab.put({"schema": V3, "header": self.lab.hdr, "phase": "final", "entry_index": {},
                              "totals": totals([]), "walk": {"pass1": WALK, "pass2": WALK}, "result": "COMPLETE",
                              "reasons": []})
        self.bad(lambda: self.lab.cy.manifest_v3(empty, "final"))

    def test_chunks_keyed_by_digest_refuse(self):
        """A dict of chunk digests is not a list, even when its keys are valid digests."""
        for i in range(64):
            es = [d("a%d" % i)]
            cd = self.lab.put({"header": self.lab.hdr, "kind": "entries", "n": 0, "entries": es})
            if cd[0] in "abcdef":
                break
        idx = self.lab.put({"header": self.lab.hdr, "kind": "entry_index", "n": 0, "chunks": {cd: 0}})
        md = self.lab.put({"schema": V3, "header": self.lab.hdr, "phase": "final", "entry_index": [idx],
                           "totals": totals(es), "walk": {"pass1": WALK, "pass2": WALK}, "result": "COMPLETE",
                           "reasons": []})
        self.bad(lambda: self.lab.cy.manifest_v3(md, "final"))

    def test_last_index_is_bounded(self):
        with mock.patch.object(preserve, "MAX_INDEX_CHUNKS", 2), mock.patch.object(preserve, "MAX_ENTRIES", 1):
            self.bad_manifest([d("%d" % i) for i in range(3)], per_index=3)

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
        with mock.patch.object(preserve, "CHUNK_BYTES", 400):
            self.assertIsNone(preserve.manifest_chunks(self.lab.hdr, [link("a", b"t" * 100)]))

    def test_worst_case_entry_fits_four_per_chunk(self):
        """The largest entry, an `l` at 4096 path bytes with a 1024-byte target."""
        hdr = header(slot="s" * 128, op_id="o" * 128)
        worst = [[(b"%04d" % i + b"p" * 4092).hex(), "l", "-", "74" * 1024] for i in range(4)]
        self.assertEqual(len(canonical.canon(worst[0])), 10255)
        self.assertEqual(len(preserve.manifest_chunks(hdr, worst)), 1)

    def test_oversized_chunk_refuses(self):
        es = [f("%05d" % i) for i in range(1200)]
        self.bad_manifest(es, chunks=[es], detail=None)


class Bindings(Case):
    def test_incorrect_content(self):
        nbytes = self.lab.blobs(SAMPLE_FILES.values())
        md = self.lab.manifest(SAMPLE, totals=totals(SAMPLE, nbytes))
        m, es = self.lab.cy.manifest_v3(md, "final")
        self.assertTrue(self.lab.cy.content_v3(m, es))
        self.lab.mem.b[canonical.sha256(b"alpha")] = b"alphA"
        self.assertIs(self.lab.cy.content_v3(m, es), False)
        del self.lab.mem.b[canonical.sha256(b"alpha")]
        self.assertIs(self.lab.cy.content_v3(m, es), False)
        self.lab.blobs([b"alpha"])
        m2, es2 = self.ok(SAMPLE, totals=totals(SAMPLE, nbytes + 1))
        self.bad(lambda: self.lab.cy.content_v3(m2, es2))

    def test_content_without_files(self):
        """No `f` entry needs no blob: the content is complete when `bytes` is 0."""
        for es in ([], [d("a"), link("a/l", b"t")]):
            with self.subTest(len(es)):
                m, got = self.ok(sorted(es))
                self.assertIs(self.lab.cy.content_v3(m, got), True)
        m, got = self.ok([d("a")], totals=dict(totals([d("a")]), bytes=1))
        self.bad(lambda: self.lab.cy.content_v3(m, got))

    def test_originals_are_not_content(self):
        self.lab.blobs([b"x"])
        es = [f("a", b"x")]
        m, got = self.ok(es, "early", totals=totals(es, 1))
        self.assertIs(self.lab.cy.content_v3(m, got), True)

    def test_symlink_target_is_never_resolved_as_content(self):
        self.lab.blobs([b"x"])
        es = [link("a", b"target"), link("c", bytes(range(1, 33))), f("b", b"x")]
        m, got = self.ok(sorted(es), totals=totals(es, 1))
        self.assertTrue(self.lab.cy.content_v3(m, got))

    def test_bytes_count_every_file_entry(self):
        """Two files with one content count its length twice."""
        self.lab.blobs([b"same"])
        es = [f("a", b"same"), f("b", b"same")]
        m, got = self.ok(es, totals=totals(es, 8))
        self.assertTrue(self.lab.cy.content_v3(m, got))
        m, got = self.ok(es, totals=totals(es, 4))
        self.bad(lambda: self.lab.cy.content_v3(m, got))

    def test_boolean_header_value_refuses(self):
        """`g_pre` spelled `true` binds `pre.g_pre == 1` under ==, so v3 checks its type."""
        lab = Lab()
        lab.hdr = header(g_pre=True)
        md = lab.manifest(SAMPLE)
        self.bad(lambda: lab.cy.manifest_v3(md, "final"))
        lab = Lab()
        md = lab.manifest(SAMPLE)
        m = lab.cy.manifest_v3(md, "final")[0]
        bad_hdr = header(g_pre=True)
        chunks = preserve.manifest_chunks(lab.hdr, SAMPLE)
        for chunk_hdr, index_hdr in ((bad_hdr, lab.hdr), (lab.hdr, bad_hdr)):
            cds = [lab.put({"header": chunk_hdr, "kind": "entries", "n": n, "entries": c})
                   for n, c in enumerate(chunks)]
            idx = lab.put({"header": index_hdr, "kind": "entry_index", "n": 0, "chunks": cds})
            md2 = lab.put(dict(m, entry_index=[idx]))
            self.bad(lambda: lab.cy.manifest_v3(md2, "final"))

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
        for blobs in ([es[0][3], es[1][3]], [], [es[1][3], "0" * 64], sorted([es[1][3], "f" * 64], reverse=True)):
            with self.subTest(blobs):
                self.bad(lambda: self.lab.cy.binding_v3(self.lab.binding(es, blobs=blobs), "final"))

    def test_set_is_sorted(self):
        es = [f("a", b"1"), f("b", b"2")]
        want = sorted(e[3] for e in es)
        self.lab.cy.binding_v3(self.lab.binding(es, blobs=want), "final")
        self.bad(lambda: self.lab.cy.binding_v3(self.lab.binding(es, blobs=want[::-1]), "final"))

    def test_binding_headers_encode_as_the_manifest(self):
        """One manifest has one accepted binding in either phase: a boolean
        `g_pre` on the binding or blob-set object refuses."""
        for phase in ("early", "final"):
            lab = Lab()
            md = lab.manifest(SAMPLE, phase)
            bad = header(g_pre=True)
            want = sorted({e[3] for e in SAMPLE if e[1] == "f"} | (set(ORIGINALS.values()) if phase == "early"
                                                                    else set()))
            for bh, sh in ((lab.hdr, lab.hdr), (bad, lab.hdr), (lab.hdr, bad), (bad, bad)):
                bs = lab.put({"header": sh, "set": want})
                b = lab.put({"header": bh, "cycle_id": CID, "manifest": md, "blobs": bs})
                with self.subTest((phase, bh is bad, sh is bad)):
                    if bh is lab.hdr and sh is lab.hdr:
                        lab.cy.binding_v3(b, phase)
                    else:
                        self.bad(lambda: lab.cy.binding_v3(b, phase))

    def test_originals_values(self):
        self.lab.cy.manifest_v3(self.lab.manifest(SAMPLE, "early"), "early")
        for o in ({}, {"owner": "0" * 64}):
            with self.subTest(o):
                self.lab.cy.manifest_v3(self.lab.manifest(SAMPLE, "early", originals=o), "early")
        for o in ({"owner": "zz"}, {"owner": "A" * 64}, {"owner": 1}, []):
            with self.subTest(o):
                self.bad_manifest(SAMPLE, "early", originals=o)

    def test_undefined_phase_refuses(self):
        b = self.lab.binding(SAMPLE)
        self.lab.cy.binding_v3(b, "final")
        for phase in ("verify", "", "Final", "EARLY", None):
            with self.subTest(phase):
                self.bad(lambda: self.lab.cy.binding_v3(b, phase))

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
    def test_schema_literals(self):
        self.assertEqual((preserve.M_SCHEMA, preserve.M_SCHEMA_V3),
                         ("codegraph.o5.manifest/v2", "codegraph.o5.manifest/v3"))

    def test_accepted_schemas(self):
        self.assertEqual(preserve.accepted_schemas(oc.PROTECTED), (V3,))
        self.assertEqual(preserve.accepted_schemas(oc.SYNTHETIC), (preserve.M_SCHEMA, V3))
        for mode in (None, "", "synthetic", "PROTECTED ", ["SYNTHETIC"], 1):
            self.assertEqual(preserve.accepted_schemas(mode), (), mode)

        class Forged(str):
            def __eq__(self, other):
                return True
            __hash__ = str.__hash__
        for mode in (Forged(oc.PROTECTED), Forged(oc.SYNTHETIC)):
            self.assertEqual(preserve.accepted_schemas(mode), ())

    def test_v2_consumer_refuses_other_schema_strings(self):
        for schema in ("", "codegraph.o5.manifest/", "codegraph.o5.manifest/v"):
            with self.subTest(schema):
                es = [[hx("a"), canonical.sha256(b"x")]]
                md = self.lab.manifest(es, schema=schema, totals=DROP, walk=DROP)
                bs = self.lab.put({"header": self.lab.hdr, "set": [canonical.sha256(b"x")]})
                b = self.lab.put({"header": self.lab.hdr, "cycle_id": CID, "manifest": md, "blobs": bs})
                self.bad(lambda: self.lab.cy.binding(b, "final"))

    def test_v3_is_validated_but_refused_by_the_v2_consumer(self):
        for es in (SAMPLE, []):
            b = self.lab.binding(es)
            self.lab.cy.binding_v3(b, "final")
            self.bad(lambda: self.lab.cy.binding(b, "final"))

    def test_v2_consumer_refuses_the_v3_schema_string(self):
        """A manifest labelled v3 but shaped like v2 is not read as v2."""
        es = [[hx("a"), canonical.sha256(b"x")]]
        md = self.lab.manifest(es, schema=V3, totals=DROP, walk=DROP)
        bs = self.lab.put({"header": self.lab.hdr, "set": [canonical.sha256(b"x")]})
        b = self.lab.put({"header": self.lab.hdr, "cycle_id": CID, "manifest": md, "blobs": bs})
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


class ClosedValues(Case):
    """Every closed value is checked against the empty string, a proper
    prefix, another case or length and a non-string, and every index split
    but the greedy one refuses."""

    def test_entry_fields_have_one_spelling(self):
        good = {"f": ("0", "0" * 64), "d": ("-", "-"), "l": ("-", "74"), "u": ("fifo", "-")}
        for t, (a, v) in good.items():
            self.ok([[hx("a"), t, a, v]])
            for e in ([[hx("a"), x, a, v] for x in ("", t.upper(), t + t)]
                      + [[hx("a"), t, x, v] for x in ("", a + a, a[:-1] or "--")]
                      + [[hx("a"), t, a, x] for x in ("", v[:-1] or "--", v + v if t != "l" else "7")]):
                with self.subTest(e):
                    self.bad_manifest([e])

    def test_digests_are_exactly_64_hex(self):
        for v in ("00", "0" * 62, "0" * 66, "0" * 128):
            with self.subTest(len(v)):
                self.bad_manifest([[hx("a"), "f", "0", v]])
                self.bad_manifest(SAMPLE, walk={"pass1": v, "pass2": v})
                self.bad_manifest(SAMPLE, "early", originals=dict(ORIGINALS, owner=v))

    def test_manifest_and_object_strings_have_one_spelling(self):
        for kw in ({"schema": ""}, {"schema": V3[:-3]}, {"schema": "codegraph.o5.manifest/"}, {"schema": 7},
                   {"result": ""}, {"result": "INCOMPLET"}, {"result": 7}):
            with self.subTest(kw):
                self.bad_manifest(SAMPLE, **kw)
        for read, stored in (("final", ""), ("final", "fin"), ("final", 7), ("early", "ear"), ("early", 7)):
            with self.subTest((read, stored)):
                m, _ = journal.parse(self.lab.mem.a[(CID, self.lab.manifest(SAMPLE, read))])
                md = self.lab.put(dict(m, phase=stored))
                self.bad(lambda: self.lab.cy.manifest_v3(md, read))
        for chunk_kind, index_kind in (("", "entry_index"), ("entr", "entry_index"), (7, "entry_index"),
                                       ("entries", ""), ("entries", "entry"), ("entries", 7)):
            with self.subTest((chunk_kind, index_kind)):
                cd = self.lab.put({"header": self.lab.hdr, "kind": chunk_kind, "n": 0, "entries": [d("a")]})
                idx = self.lab.put({"header": self.lab.hdr, "kind": index_kind, "n": 0, "chunks": [cd]})
                md = self.lab.put({"schema": V3, "header": self.lab.hdr, "phase": "final", "entry_index": [idx],
                                   "totals": totals([d("a")]), "walk": {"pass1": WALK, "pass2": WALK},
                                   "result": "COMPLETE", "reasons": []})
                self.bad(lambda: self.lab.cy.manifest_v3(md, "final"))

    def test_originals_keys_and_totals_types(self):
        for k in ("move", "own", "acls", "owner_"):
            with self.subTest(k):
                self.bad_manifest(SAMPLE, "early", originals={k: "0" * 64})
        good = totals(SAMPLE)
        for key in preserve.V3_TOTALS:
            for v in (False, "", [], {}):
                with self.subTest((key, v)):
                    self.bad_manifest(SAMPLE, totals=dict(good, **{key: v}))
            renamed = {(k + "s" if k == key else k): v for k, v in good.items()}
            with self.subTest(("renamed", key)):
                self.bad_manifest(SAMPLE, totals=renamed)

    def test_only_the_greedy_index_split_is_accepted(self):
        with mock.patch.object(preserve, "MAX_INDEX_CHUNKS", 2), mock.patch.object(preserve, "MAX_ENTRIES", 1):
            es = [d("%d" % i) for i in range(5)]
            cds = [self.lab.put({"header": self.lab.hdr, "kind": "entries", "n": n, "entries": [e]})
                   for n, e in enumerate(es)]
            for sizes in (s for k in range(1, 6) for s in itertools.product((1, 2), repeat=k) if sum(s) == 5):
                idx, at = [], 0
                for n, s in enumerate(sizes):
                    idx.append(self.lab.put({"header": self.lab.hdr, "kind": "entry_index", "n": n,
                                             "chunks": cds[at:at + s]}))
                    at += s
                md = self.lab.put({"schema": V3, "header": self.lab.hdr, "phase": "final", "entry_index": idx,
                                   "totals": totals(es), "walk": {"pass1": WALK, "pass2": WALK},
                                   "result": "COMPLETE", "reasons": []})
                with self.subTest(sizes):
                    if sizes == (2, 2, 1):
                        self.lab.cy.manifest_v3(md, "final")
                    else:
                        self.bad(lambda: self.lab.cy.manifest_v3(md, "final"))


class AcrossObjects(Case):
    """Order, ancestry, components and shapes hold across every chunk and
    index object, for every entry type, phase and result."""

    def test_one_misordered_pair_refuses(self):
        for es in ([f("b"), f("aa")], [d("b"), d("aa")], [f("ab"), f("a")], [d("a"), f("c"), f("bb")]):
            with self.subTest(es):
                self.bad_manifest(es)

    @mock.patch.object(preserve, "MAX_ENTRIES", 1)
    def test_order_is_global_across_chunks(self):
        for es in ([d("b"), d("a")], [f("a", b"1"), f("a", b"2")]):
            with self.subTest(es):
                self.assertEqual(len(preserve.manifest_chunks(self.lab.hdr, es)), 2)
                self.bad_manifest(es)

    def test_order_is_strict_across_full_chunks(self):
        es = [f("%05d" % i, b"%d" % i) for i in range(1200)]
        k = len(preserve.manifest_chunks(self.lab.hdr, es)[0])
        prev = bytes.fromhex(es[k - 1][0])
        for first in (prev, prev[:-1] + b"!"):
            with self.subTest(first):
                bad = es[:k] + [f(first, b"other")] + es[k + 1:]
                self.assertEqual(len(preserve.manifest_chunks(self.lab.hdr, bad)[0]), k)
                self.bad_manifest(bad)

    @mock.patch.object(preserve, "MAX_ENTRIES", 1)
    @mock.patch.object(preserve, "MAX_INDEX_CHUNKS", 2)
    def test_order_and_ancestors_across_index_objects(self):
        self.assertEqual(len(self.ok([d("a"), f("a/x"), f("a/y")], per_index=2)[0]["entry_index"]), 2)
        self.assertEqual(len(self.ok([d("a"), d("a/b"), d("a/b/c"), f("a/b/c/f")], per_index=2)[0]["entry_index"]), 2)
        self.ok([d("a"), d("b"), d("c")], per_index=2)
        for es in ([d("a"), d("a")], [d("b"), d("a")], [d("0"), d("1"), d("1")], [d("0"), d("2"), d("1")]):
            with self.subTest(es):
                self.bad_manifest(es, per_index=2)

    def test_malformed_entry_after_a_valid_one_refuses(self):
        for bad in (7, True, "62", ["62"], {"a": "62"}, ["62", "d", "-"], ["62", "d", "-", 0],
                    [["62"], "d", "-", "-"]):
            with self.subTest(bad):
                self.bad_manifest([d("a"), bad])

    def test_every_component_is_checked_below_a_directory(self):
        """The parent is a `d` entry, so only the component rule can refuse."""
        for p in (b"a/", b"a/.", b"a/..", b"a/x\0y", b"a/" + b"n" * 256):
            with self.subTest(p):
                self.bad_manifest([d("a"), d(p)])
        self.bad_manifest([d("a"), d("a/.."), d("a/../.."), f("a/../../x")])
        self.ok([d("a"), f(b"a/" + b"n" * 255)])

    def test_symlink_target_nul_anywhere(self):
        for target in ("0061", "610062", "006100"):
            with self.subTest(target):
                self.bad_manifest([[hx("a"), "l", "-", target]])

    def test_reason_inner_characters(self):
        for r in (["aB"], ["a-b"], ["a.b"], ["a b"], ["a,b"]):
            with self.subTest(r):
                self.bad_manifest(SAMPLE, result="INCOMPLETE", reasons=r)

    def test_unsupported_count_is_recomputed(self):
        es = [d("a"), u("a/p", "fifo")]
        self.bad_manifest(es, result="COMPLETE", reasons=[], totals=dict(totals(es), unsupported=0))
        self.bad_manifest(es, totals=dict(totals(es), unsupported=2))

    def test_rules_hold_for_every_type_phase_and_result(self):
        for es in ([u("a/p", "fifo")], [link("a/l", b"t")]):
            with self.subTest(es):
                self.bad_manifest(es)
        self.ok(SAMPLE, "early", classifier_head="a" * 40)
        for phase, result, reasons in (("final", "INCOMPLETE", ["unstable"]), ("early", "COMPLETE", [])):
            with self.subTest((phase, result)):
                self.bad_manifest(SAMPLE, phase, result=result, reasons=reasons,
                                  walk={"pass1": WALK, "pass2": "0" * 64})
        self.bad_manifest([d("a"), u("a/p", "fifo")], "early", result="COMPLETE", reasons=[])
        m, got = self.ok([d("a")], result="INCOMPLETE", reasons=["x"], totals=dict(totals([d("a")]), bytes=1))
        self.bad(lambda: self.lab.cy.content_v3(m, got))
        self.bad_manifest([[hx("a"), "d", "", ""]])

    def test_object_key_sets_are_exact(self):
        """An index, chunk, binding or blob-set object with an extra or a missing key refuses."""
        es = [f("a", b"x")]
        for obj, change in (("chunk", {"x": 1}), ("chunk", {"n": DROP}), ("index", {"x": 1}), ("index", {"n": DROP}),
                            ("binding", {"x": 1}), ("binding", {"cycle_id": DROP}), ("set", {"x": 1})):
            with self.subTest((obj, change)):
                def put(o, kind):
                    o = dict(o, **change) if kind == obj else o
                    return self.lab.put({k: v for k, v in o.items() if v is not DROP})
                cd = put({"header": self.lab.hdr, "kind": "entries", "n": 0, "entries": es}, "chunk")
                idx = put({"header": self.lab.hdr, "kind": "entry_index", "n": 0, "chunks": [cd]}, "index")
                md = self.lab.put({"schema": V3, "header": self.lab.hdr, "phase": "final", "entry_index": [idx],
                                   "totals": totals(es), "walk": {"pass1": WALK, "pass2": WALK},
                                   "result": "COMPLETE", "reasons": []})
                bs = put({"header": self.lab.hdr, "set": [es[0][3]]}, "set")
                b = put({"header": self.lab.hdr, "cycle_id": self.lab.cid, "manifest": md, "blobs": bs}, "binding")
                self.bad(lambda: self.lab.cy.binding_v3(b, "final"))


class Eligibility(unittest.TestCase):
    def test_clean_tree_has_no_refusal(self):
        self.assertEqual(preserve.refusals_v3([tuple(e) for e in SAMPLE]), [])

    def test_symlinks_and_unsupported_never_become_eligible(self):
        self.assertEqual(preserve.refusals_v3([tuple(link("w", b"t"))]), ["symlink"])
        self.assertEqual(preserve.refusals_v3([tuple(d(".git")), tuple(link(".git/HEAD", b"t"))]), ["unknown"])
        for es, want in (([d("sub"), d("sub/.git"), link("sub/.git/x", b"t")], ["unknown"]),
                         ([d(".GIT"), link(".GIT/x", b"t")], ["unknown"]),
                         ([link(".git", b"t")], ["unknown"]),
                         ([d("w"), link("w/.git", b"/x")], ["unknown"]),
                         ([link("A", b"t"), link("a", b"t")], ["name_alias", "symlink"]),
                         ([link(".gitignore", b"t")], ["symlink"]),
                         ([d("sub"), link("sub/x", b"t")], ["symlink"]),
                         ([u("p", "fifo"), u("q", "hardlink")], [])):
            with self.subTest(es):
                self.assertEqual(preserve.refusals_v3([tuple(e) for e in sorted(es)]), want)
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

    def test_nested_fold_collision(self):
        nfc = unicodedata.normalize("NFC", "caf\u00e9").encode()
        nfd = unicodedata.normalize("NFD", "caf\u00e9").encode()
        for a, b in ((b"a/X", b"a/x"), (b"t/" + nfc, b"t/" + nfd), (b"p/q/R", b"p/q/r")):
            with self.subTest((a, b)):
                self.assertEqual(preserve.refusals_v3([tuple(e) for e in typed({a: b"1", b: b"2"})]),
                                 ["name_alias"])

    def test_alias_of_every_kind_and_encoding(self):
        for es in ([f(b"\xc0"), f(b"\xe0")], [u("A", "fifo"), u("a", "sock")],
                   [d(".git"), f(".git/HEAD"), f(".git/head")]):
            with self.subTest(es):
                self.assertEqual(preserve.refusals_v3([tuple(e) for e in sorted(es)]), ["name_alias"])

    def test_symlink_below_every_gitdir_spelling_is_unknown(self):
        for cp in HFS_IGNORABLE:
            for name in (".g" + cp + "it", cp + ".git", ".GIT" + cp):
                with self.subTest((hex(ord(cp)), name.index(cp))):
                    es = [tuple(e) for e in sorted([d(name), link(name + "/x", b"t")])]
                    self.assertEqual(preserve.refusals_v3(es), ["unknown"])
                    es = [tuple(e) for e in sorted([d("w"), d("w/" + name), link("w/" + name + "/x", b"t")])]
                    self.assertEqual(preserve.refusals_v3(es), ["unknown"])

    def test_file_that_is_a_directory_elsewhere(self):
        es = [tuple(e) for e in sorted([f("A"), d("a"), d("a/x")])]
        self.assertEqual(preserve.refusals_v3(es), ["name_alias"])

    def test_same_name_in_different_directories_is_no_alias(self):
        es = [tuple(e) for e in typed({b"p/readme": b"1", b"q/readme": b"2"})]
        self.assertEqual(preserve.refusals_v3(es), [])

    def test_unicode_alias_collision(self):
        nfc = unicodedata.normalize("NFC", "café").encode()
        nfd = unicodedata.normalize("NFD", "café").encode()
        self.assertNotEqual(nfc, nfd)
        for a, b in ((nfc, nfd), (b"ab", b"a\xe2\x80\x8cb")):
            with self.subTest((a, b)):
                self.assertEqual(preserve.refusals_v3([tuple(e) for e in typed({a: b"1", b: b"2"})]),
                                 ["name_alias"])

    def test_distinct_accents_are_no_alias(self):
        for a, b in (("cafe", "caf\u00e9"), ("resume", "r\u00e9sum\u00e9"), ("a", "\u00e1"), ("e", "e\u0301")):
            with self.subTest((a, b)):
                self.assertEqual(preserve.refusals_v3([tuple(e) for e in sorted([d(a), d(b)])]), [])


def v3_effect_manifest(fx, s, phase, files):
    """The synthetic effect's manifest step, writing a valid v3 graph instead of v2."""
    for v in files.values():
        fx.blob(v)
    es = typed(files)
    hdr = fx.header(s)
    md = build(lambda o: fx.obj(s, o), hdr, es, phase, originals=s["originals"] if phase == "early" else DROP,
               totals=totals(es, sum(len(v) for v in files.values())))
    want = {e[3] for e in es if e[1] == "f"} | (set(s["originals"].values()) if phase == "early" else set())
    bs = fx.obj(s, {"header": hdr, "set": sorted(want)})
    return fx.obj(s, {"header": hdr, "cycle_id": s["cid"], "manifest": md, "blobs": bs}), md, bs


class V2ConsumerEndToEnd(Base):
    """A v3 manifest that reaches the merged consumers refuses with
    `artifact_mismatch`: never a bare exception, never eligible."""

    def cycle(self):
        s = self.w.st()
        pre = types.SimpleNamespace(**{k: s[k] for k in ("slot", "op_id", "g_pre", "cutover_head", "barrier_id",
                                                         "root_id", "mode")})
        return s, preserve._Cycle(self.w.prov._r, pre, s["cid"], s["start_ctx"])

    def test_freeze_start_with_a_v3_manifest_is_rejected(self):
        with mock.patch.object(pw.Effects, "manifest", v3_effect_manifest):
            self.ok("adopt", "request")
            self.rejected("freeze-start", "fresult_rejected:artifact_mismatch", "MOVE_INTENT")
        s, cy = self.cycle()
        _, m, es, _ = cy.binding_v3(s["early"], "early")
        self.assertTrue(cy.content_v3(m, es))

    def test_schemas_mixed_within_one_cycle_are_rejected(self):
        """A v2 EP manifest and a v3 final manifest in one cycle: the snapshot
        result is rejected, although each manifest is valid for its schema."""
        real = pw.Effects.manifest

        def mixed(fx, s, phase, files):
            return (v3_effect_manifest if phase == "final" else real)(fx, s, phase, files)
        self.w.to_p13()
        self.w.handoff()
        with mock.patch.object(pw.Effects, "manifest", mixed):
            self.rejected("snapshot", "fresult_rejected:artifact_mismatch", "SNAPSHOT_INTENT")
        s, cy = self.cycle()
        cy.binding(s["early"], "early")
        cy.binding_v3(s["fs"], "final")


if __name__ == "__main__":
    unittest.main()

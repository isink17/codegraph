"""PR-3b pure units: journal lineage and freshness (N-R41-3), manifest
reconstruction and chunk bounds (N-R41-5, N-R41-6), Git feature
classification over byte fixtures, escaped-set classification and typed
neutralization (N-R41-9). Inputs are built in memory; nothing reads a live
path."""

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
        return [["%06x" % i, canonical.sha256(str(i).encode())] for i in range(start, start + n)]

    def mismatch(self, m):
        with self.assertRaises(Mismatch):
            self.cy.entries(m)

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
        big = [["%04x" % i + "ab" * 40, canonical.sha256(str(i).encode())] for i in range(400)]
        self.assertGreater(len(canonical.canon({"header": header(), "kind": "entries", "n": 0, "entries": big})),
                           journal.MAX_OBJECT)
        self.mismatch(self.manifest([big])[0])

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
            ("packed_only",): ("INCOMPLETE", "COMPLETE", "ELIGIBLE"),
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

    def test_no_repository_is_never_eligible(self):
        _, q3, q4, q5, _, head = classify({b"README": b"x"})
        self.assertEqual((q3, q4, q5, head), ("INCOMPLETE", "INCOMPLETE", "NOT_ELIGIBLE", None))

    def test_raw_incomplete_is_never_complete(self):
        files, _ = pw.git_repo()
        self.assertEqual(gitclass.questions(gitclass.repositories(files), "INCOMPLETE")[2:4],
                         ("INCOMPLETE", "NOT_ELIGIBLE"))

    def test_object_whose_content_is_not_its_id(self):
        """Valid zlib under another object's id: integrity, not decompression, refuses."""
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

    def test_listed_objects_are_packed_and_eligible(self):
        _, q3, q4, q5, reasons, head = classify(self.files)
        self.assertEqual((q3, q4, q5, reasons, head), ("INCOMPLETE", "COMPLETE", "ELIGIBLE", ["packed"], self.coid))
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

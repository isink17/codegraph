"""R5-1 strict torn-tail classification, repair plan and repair (R5-1 §R3–§R8,
A1 §A4), with the normative byte vectors V01–V14 and the review N5-1..N5-4,
N5-7 additions. Pure, in memory."""

import hashlib
import random
import unittest

import harness  # noqa: F401  (path setup)

from broker import authz, canonical, ledger
from broker.refusal import Code, Refused
from broker.stores import Crash, FaultPlan, TxnError

L0 = (b'{"auth_digest":"505d151075517334281385ab586c7b4544935dbdf7f471f1e24505bf71c59d5f","auth_kind":"initiate",'
      b'"binding_digest":"243930e34027b51f7a470de0796d2d650af8d1ef12e6679fc0d89bc62e796cc5","confirmed_at":1762819230,'
      b'"issued_at":1762819200,"line_digest":"72eb5065c39764f2ea4edae4ef91aba8c74804a4b669d23d496b05a5a7f674ba",'
      b'"nonce_digest":"14b744a5bbd3bc8132fdafb8a2426b19135b280e321331838e28cdf945adb4fa",'
      b'"prev":"0000000000000000000000000000000000000000000000000000000000000000","repair":[],"seq":0,"v":1}')
L1 = (b'{"auth_digest":"d5f8012a164904af50c4a6dbde96427fa2b63867ade330f7788007d80d41335f","auth_kind":"initiate",'
      b'"binding_digest":"4dc53630402d2f1a3cca2a63a079e844e6575c256ccd29c85cdab1f6fa41baab","confirmed_at":1762819290,'
      b'"issued_at":1762819260,"line_digest":"d4787ee043844c7a609fcc6874b487b70963c5d95e059eb5606b702b75a29608",'
      b'"nonce_digest":"2a9af36e2abc85a233da39f6b3957d5cb7368a64b942f89e21fc0d40afbd4675",'
      b'"prev":"72eb5065c39764f2ea4edae4ef91aba8c74804a4b669d23d496b05a5a7f674ba","repair":[],"seq":1,"v":1}')
E2 = (b'{"auth_digest":"c83e6609dcacc133caba85077db8ff6078dcbacc2fc440955acd34ec6a620968","auth_kind":"resume",'
      b'"binding_digest":"9aad1f05235407ae08e423a9d40889714942f4a3ab72ea37695e254cb253abeb","confirmed_at":1762819350,'
      b'"issued_at":1762819320,"line_digest":"4783487dd92501d6955959ccfb521bd0769f41c43389d30c9d7dc60a1f66f4e2",'
      b'"nonce_digest":"b604459cb399ee29a34e100d6150a5176562dba107c470c1f45034ea446a7a33",'
      b'"prev":"d4787ee043844c7a609fcc6874b487b70963c5d95e059eb5606b702b75a29608","repair":[],"seq":2,"v":1}')
E2BADPREV = (b'{"auth_digest":"c83e6609dcacc133caba85077db8ff6078dcbacc2fc440955acd34ec6a620968","auth_kind":"resume",'
             b'"binding_digest":"9aad1f05235407ae08e423a9d40889714942f4a3ab72ea37695e254cb253abeb",'
             b'"confirmed_at":1762819350,"issued_at":1762819320,'
             b'"line_digest":"e92391a07bbd1a07692399aed4e69290d5dc377b5c4f3f6c5e11487d34056cbf",'
             b'"nonce_digest":"b604459cb399ee29a34e100d6150a5176562dba107c470c1f45034ea446a7a33",'
             b'"prev":"72eb5065c39764f2ea4edae4ef91aba8c74804a4b669d23d496b05a5a7f674ba","repair":[],"seq":2,"v":1}')
B = L0 + b"\n" + L1 + b"\n"

FIXTURE = {"L0": (L0, 501, "a61847678b648dc303e9bb64c0813126ccc38537a9b8b3f2a17da34a230afb2a"),
           "L1": (L1, 501, "6cc5f3206c6c9b35e6c20d6952eb9749febff9775d15ce6e2ceb4edb54a9cdf3"),
           "E2": (E2, 499, "820add3e6708eae530639ddfb54dbcc6ac80633c3f3df749d818202edd6b43b2"),
           "E2BADPREV": (E2BADPREV, 499, "2946ba1d106ddc81422cce989035d78bca740d272680127592175eaa817ee30d"),
           "B": (B, 1004, "91bcf17fefc89c71e69ae92922dfcd7b0013fc0b357da1cb60ed5441e34465ed")}

U = ledger.UNCLASSIFIABLE
VECTORS = [
    ("V01", B + E2 + b"*", 1504, "7ee515c3b17435a13630ef2651e3f3e5bacc3a44a88163b622f7476e9b202ed7", U,
     "COMPLETE_VALUE_EXTRA_BYTES"),
    ("V02", B + E2 + b'{"auth_digest":"', 1519, "2d7616abb6e6a7ef4299f658a3a767d6527742eb7d34fc9809d772fcdfa93922", U,
     "COMPLETE_VALUE_EXTRA_BYTES"),
    ("V03", B + E2 + b"\x00\x00", 1505, "27dd31a8737b6978d7dad5ca28f90b5b02ddd6cacc61914c4d62c9b912f614af", U,
     "NUL_EXCESS"),
    ("V04a", B + b'{"zz', 1008, "8d44225b5430f9eefffb32ed1dfb712aae6ca3fce63fc57f69876b4b6d3c3e79", U,
     "NON_CANONICAL_PREFIX"),
    ("V04b", B + b'{"v":1', 1010, "8a94288d845c29aad0d9d169064513bdf128a2d6fcfba13af030610daeb1173d", U,
     "NON_CANONICAL_PREFIX"),
    ("V04c", B + b'{ "auth_digest"', 1019, "5a9ef6f4db1ae9af5d36e43d7384de4878bd53c77a53863aaa860e9752336c17", U,
     "NON_CANONICAL_PREFIX"),
    ("V04d", B + b'{"auth_digest":01}*', 1023, "3a0d24820adbfc8d30d80d044c325098096338cea6574b24c54d7ebede534621", U,
     "NON_CANONICAL_PREFIX"),
    ("V05a", B + E2, 1503, "2236e449a665335a86c78c1175ccb35fe5227c2b13e6db3aa6016b077abe95a9",
     ledger.TAIL_UNTERMINATED_VALID, None),
    ("V05b", B + E2 + b"\x00", 1504, "a339dc4e96ab8d6efd41063c2716a35ea130b311acac6842fd5aeed0ed715491",
     ledger.TAIL_UNTERMINATED_VALID, None),
    ("V05c", B + E2 + b"\n", 1504, "ffb1a3a1ecb8dec11458bc36b575364c27a3a61fa0ca7839d43db60a764b5fbf", ledger.CLEAN,
     None),
    ("V06a", B + E2[:1], 1005, "546342f54ec189aa5c93fb26ec0ba31dda08cc7ce246c31f67fa890fd49dfff2", ledger.TAIL_PREFIX,
     None),
    ("V06b", B + E2[:16], 1020, "1fdadb0c5f34f5ba177d851ce343713da6c85a01158c97a55518bfdd486c307c",
     ledger.TAIL_PREFIX, None),
    ("V06c", B + E2[:-1], 1502, "f4f922631f56c195863a9750914805abadf4c0b14a91bc4f4046095887ef6577",
     ledger.TAIL_PREFIX, None),
    ("V06d", B + E2[:100] + b"\x00\x00\x00", 1107,
     "339eafbf193e48bf2b871898848dfc3d363e1f6bda9b3f57e37d9a75fc53914c", ledger.TAIL_PREFIX, None),
    ("V06e", B + b"\x00\x00\x00\x00", 1008, "410aac02bdbf167b72c1707b6c0e247a395b8572fb4695a06273d7f04eedf4fd",
     ledger.TAIL_PREFIX, None),
    ("V07a", B + b'{"auth_digest":"\\"}', 1023, "ed015a85dd28769107726799d0ef15737a9a9d0260688e22c433a2a276a30a80", U,
     "NON_CANONICAL_PREFIX"),
    ("V07b", B + b'{"auth_digest":"\\"{"}', 1025, "7e174d0c33fe10bdbe52a1e04a6f1ce85c522e06746b8937f2b9c7730be8eac1",
     U, "COMPLETE_INVALID"),
    ("V07c", B + b'{"auth_digest":"{"}', 1023, "df3bd1a18004f3b607948732c7aa52db1bc19e5b1c63ddb681988230c6a9d995", U,
     "COMPLETE_INVALID"),
    ("V07d", B + b'{"auth_digest":"\\\\"}*', 1025, "19c1bb11155db0c9b29ab6a215fe44bbead8bbc405245871851eb1359f0cbf40",
     U, "COMPLETE_VALUE_EXTRA_BYTES"),
    ("V08a", B + b'{"auth_digest":"\xc3\x28', 1022, "46e76ccc602abeb7e870af0a2c2b65f8c280f36819eb804de6b434d7ce362200",
     U, "NON_PRINTABLE"),
    ("V08b", B + b"\xff", 1005, "6fdebddba5ec52a450be93d9b8a039a8c091f64e9015c637beed486cbcace281", U,
     "NON_PRINTABLE"),
    ("V08c", B + E2[:10] + b"\x00" + E2[10:20], 1025,
     "241275c53d929389c52e95f0b521a450670e369e42fbc6657769387f518e5755", U, "NON_PRINTABLE"),
    ("V09a", L0[:258] + b"1" + L0[259:] + b"\n" + L1 + b"\n", 1004,
     "5d01b9c7da55cdf6d87adf30059a6314e38ef2f21fb432875d1c7305df6133d9", U, "LINE_INVALID"),
    ("V09b", L0 + b"*" + L1 + b"\n", 1004, "ab20814bc43810adad65429c118ba385ec886c76484eb9f65ba026b88520c71b", U,
     "LINE_INVALID"),
    ("V09c", L0 + b"\n" + L1 + b"*", 1004, "5ae349d9ea7b54dc542ca22be7ca10f5771c66d284a5fd192f3e66a40767570f", U,
     "COMPLETE_VALUE_EXTRA_BYTES"),
    ("V10a", B + E2BADPREV, 1503, "35c0dcab783311dfe8a055abd858d853c2ea55b7d534cea81c02140936dc03bc", U,
     "COMPLETE_INVALID"),
    ("V10b", B + E2BADPREV + b"\n", 1504, "7d21d9db057663ed84def22dd13d2b0af9f826354b0e913d0352a992a4b7db1a", U,
     "LINE_INVALID"),
    ("V10c", B + E2 + E2BADPREV, 2002, "97cccf961ed9b5ae2a210872920f7a0ff20d47746bd778a4f171248e5a7aaf44", U,
     "CONCATENATED_VALUES"),
    ("V10d", B + E2BADPREV[:429], 1433, "41ebcdc6c3c0a3b945a275e452bb645b5a51d5f518b98ed38d65a9b0ac5a651c", U,
     "NON_CANONICAL_PREFIX"),
    ("V10e", B + L1[:90], 1094, "8a7d85c1b5f3226942c532c3faecd6c12cd18cef6de33d1841b14780824af30c", U,
     "NON_CANONICAL_PREFIX"),
    ("V10f", B + E2[:20] + b"0" * 4096, 5120, "7a5e8bdaa5d044a6dd3adbb29a23e447dbbead88d93707b2c63c8755e5eead75", U,
     "TAIL_OVERSIZE"),
]


def txn():
    """A live CommitTxn obtained the only legitimate way: through revalidate."""
    doc = {"issued_at": 0, "not_after": 10, "nonce_digest": "1" * 64}
    p = authz.SyntheticOwner()
    return authz.revalidate(doc, "2" * 64, "3" * 64, "3" * 64, 5, set(), p)


def sha(b):
    return hashlib.sha256(b).hexdigest()


def gen_log(spec, n, rng):
    lines, prev = [], ledger.ZERO64
    for i in range(n):
        e = ledger.make_entry(spec, i, prev, gen_fields(spec, rng))
        lines.append(canonical.canon(e) + b"\n")
        prev = e["line_digest"]
    return lines, prev


def gen_fields(spec, rng):
    hexv = lambda: "%064x" % rng.getrandbits(256)  # noqa: E731
    if spec is ledger.LEDGER:
        return {"auth_digest": hexv(), "auth_kind": rng.choice(ledger.LEDGER_KINDS), "binding_digest": hexv(),
                "confirmed_at": rng.randrange(1 << 40), "issued_at": rng.randrange(1 << 40), "nonce_digest": hexv()}
    return {"auth_digest": hexv(), "kind": rng.choice(ledger.ANCHOR_KINDS), "post": hexv(), "pre": hexv(),
            "verb": rng.choice(ledger.ANCHOR_VERBS)}


class Fixture(unittest.TestCase):
    def test_fixture_bytes_lengths_and_digests(self):
        for name, (data, n, digest) in FIXTURE.items():
            self.assertEqual((len(data), sha(data)), (n, digest), name)

    def test_fixture_lines_are_valid_chained_entries(self):
        self.assertEqual(ledger.classify(B + E2 + b"\n", ledger.LEDGER)["cls"], ledger.CLEAN)
        self.assertEqual(L0[258:259], b"0")


class Vectors(unittest.TestCase):
    def test_normative_vectors(self):
        for vid, data, n, digest, cls, reason in VECTORS:
            with self.subTest(vid):
                self.assertEqual((len(data), sha(data)), (n, digest))
                r = ledger.classify(data, ledger.LEDGER)
                self.assertEqual((r["cls"], r.get("reason")), (cls, reason))

    def test_line_numbers(self):
        self.assertEqual(ledger.classify(VECTORS[22][1], ledger.LEDGER)["line"], 0)
        self.assertEqual(ledger.classify(VECTORS[26][1], ledger.LEDGER)["line"], 2)

    def test_unclassifiable_has_no_plan(self):
        for vid, data, *_rest, cls, _reason in VECTORS:
            if cls == U:
                with self.subTest(vid), self.assertRaises(Refused) as e:
                    ledger.plan_item("ledger", ledger.classify(data, ledger.LEDGER))
                self.assertIs(e.exception.code, Code.LEDGER_UNAVAILABLE)


class ReviewAdditions(unittest.TestCase):
    def test_n5_2_burn_rule_applies_to_complete_entries(self):
        reuse = ledger.make_entry(ledger.LEDGER, 2, ledger.classify(B, ledger.LEDGER)["entries"][-1]["line_digest"],
                                  {"auth_digest": "d5f8012a164904af50c4a6dbde96427fa2b63867ade330f7788007d80d41335f",
                                   "auth_kind": "initiate", "binding_digest": "1" * 64, "confirmed_at": 1,
                                   "issued_at": 1, "nonce_digest": "2" * 64})
        raw = canonical.canon(reuse)
        self.assertEqual(ledger.classify(B + raw, ledger.LEDGER)["reason"], "COMPLETE_INVALID")
        self.assertEqual(ledger.classify(B + raw + b"\n", ledger.LEDGER)["reason"], "LINE_INVALID")

    def test_n5_3_evidence_bytes_must_match_name(self):
        ev = {(1004, sha(b"abc"), 3): b"abd"}
        self.assertEqual(ledger.classify(B, ledger.LEDGER, ev)["reason"], "EVIDENCE_INVALID")

    def test_n5_3_referenced_evidence_must_exist(self):
        prev = ledger.classify(B, ledger.LEDGER)["entries"][-1]["line_digest"]
        e = ledger.make_entry(ledger.LEDGER, 2, prev, gen_fields(ledger.LEDGER, random.Random(1)),
                              [{"offset": 1004, "sha256": sha(b"x"), "len": 1}])
        data = B + canonical.canon(e) + b"\n"
        self.assertEqual(ledger.classify(data, ledger.LEDGER)["reason"], "EVIDENCE_INVALID")
        self.assertEqual(ledger.classify(data, ledger.LEDGER, {(1004, sha(b"x"), 1): b"x"})["cls"], ledger.CLEAN)

    def test_n5_3_evidence_offset(self):
        ev = {(1003, sha(b"x"), 1): b"x"}
        self.assertEqual(ledger.classify(B, ledger.LEDGER, ev)["reason"], "EVIDENCE_OFFSET")
        ev = {(1004, sha(b"x"), 1): b"x"}
        self.assertEqual(ledger.classify(B, ledger.LEDGER, ev)["cls"], ledger.PENDING_REPAIR_RECORD)
        self.assertEqual(ledger.classify(B + E2, ledger.LEDGER, ev)["reason"], "EVIDENCE_OFFSET")

    def test_n5_4_lexer_is_total(self):
        for tail in (b"[" * 4095, b'{"a":' * 800, b"{" * 4000):
            r = ledger.classify(B + tail, ledger.LEDGER)
            self.assertEqual(r["cls"], U)

    def test_n5_1_plan_values_per_action(self):
        trim = ledger.plan_item("ledger", ledger.classify(B + E2[:16], ledger.LEDGER))
        self.assertEqual(trim, {"log": "ledger", "class": "TAIL_PREFIX", "action": "trim", "offset": 1004,
                                "len": 16, "sha256": sha(E2[:16]), "body_sha256": FIXTURE["B"][2],
                                "evidence_pending": 0})
        term = ledger.plan_item("ledger", ledger.classify(B + E2 + b"\x00", ledger.LEDGER))
        self.assertEqual(term, {"log": "ledger", "class": "TAIL_UNTERMINATED_VALID", "action": "terminate",
                                "offset": 1503, "len": 1, "sha256": sha(E2), "body_sha256": FIXTURE["B"][2],
                                "evidence_pending": 0})
        rec = ledger.plan_item("ledger", ledger.classify(B, ledger.LEDGER, {(1004, sha(b"x"), 1): b"x"}))
        self.assertEqual(rec, {"log": "ledger", "class": "PENDING_REPAIR_RECORD", "action": "record",
                               "offset": 1004, "len": 0, "sha256": sha(b""), "body_sha256": FIXTURE["B"][2],
                               "evidence_pending": 1})
        self.assertIsNone(ledger.plan_item("ledger", ledger.classify(B, ledger.LEDGER)))
        self.assertEqual(canonical.canon(trim), canonical.canon(dict(trim)))

    def test_n5_7_no_plan_at_the_pending_limit(self):
        ev = {(1004, sha(bytes([i])), 1): bytes([i]) for i in range(16)}
        r = ledger.classify(B + E2[:10], ledger.LEDGER, ev)
        self.assertEqual(r["cls"], ledger.TAIL_PREFIX)
        with self.assertRaises(Refused):
            ledger.plan_item("ledger", r)

    def test_anchor_verb_enum(self):
        self.assertEqual(ledger.ANCHOR_VERBS, ("acquire", "clear-suspect", "mark-suspect", "migrate", "quarantine",
                                               "release"))


class Properties(unittest.TestCase):
    def test_p1_every_proper_prefix_is_a_candidate(self):
        rng = random.Random(51)
        for spec in (ledger.LEDGER, ledger.ANCHOR):
            for _ in range(40):
                lines, prev = gen_log(spec, rng.randrange(0, 3), rng)
                body = b"".join(lines)
                reps = [{"offset": rng.randrange(1 << 40), "len": rng.randrange(1, 4096),
                         "sha256": "%064x" % rng.getrandbits(256)} for _ in range(rng.randrange(0, 4))]
                e = canonical.canon(ledger.make_entry(spec, len(lines), prev, gen_fields(spec, rng), reps))
                for k in range(1, len(e)):
                    for n in (0, 1, 3):
                        self.assertEqual(ledger.classify(body + e[:k] + b"\x00" * n, spec)["cls"],
                                         ledger.TAIL_PREFIX)

    def test_p2_complete_entry_plus_bytes_is_never_a_candidate(self):
        rng = random.Random(52)
        for spec in (ledger.LEDGER, ledger.ANCHOR):
            for _ in range(30):
                lines, prev = gen_log(spec, rng.randrange(0, 3), rng)
                body = b"".join(lines)
                e = canonical.canon(ledger.make_entry(spec, len(lines), prev, gen_fields(spec, rng)))
                for j in [bytes([c]) for c in range(256) if c != 0x0A] + [b"\x00\x00", b"{", b'{"', b"{}", e]:
                    r = ledger.classify(body + e + j, spec)
                    expect = ledger.TAIL_UNTERMINATED_VALID if j == b"\x00" else U
                    self.assertEqual(r["cls"], expect)

    def test_p3_single_byte_edits_never_trim_original_bytes(self):
        rng = random.Random(53)
        for spec in (ledger.LEDGER, ledger.ANCHOR):
            for _ in range(4):
                lines, _ = gen_log(spec, 3, rng)
                g = b"".join(lines)
                for p in range(len(g) - len(lines[-1]) - len(lines[-2]), len(g) + 1):
                    edits = [g[:p] + bytes([x]) + g[p:] for x in rng.sample(range(256), 3)]
                    if p < len(g):
                        edits += [g[:p] + bytes([x]) + g[p + 1:] for x in rng.sample(range(256), 3) if x != g[p]]
                        edits.append(g[:p] + g[p + 1:])
                    for f in edits:
                        r = ledger.classify(f, spec)
                        if r["cls"] == ledger.TAIL_PREFIX:
                            self.assertEqual(f[:len(g)], g)
                            self.assertGreaterEqual(r["body_len"], len(g))

    def test_p4_truncated_committed_line_is_a_candidate(self):
        lines, _ = gen_log(ledger.LEDGER, 3, random.Random(54))
        g = b"".join(lines)
        for k in range(len(g) - len(lines[-1]) + 1, len(g) - 1):
            self.assertEqual(ledger.classify(g[:k], ledger.LEDGER)["cls"], ledger.TAIL_PREFIX)

    def test_p5_p6_parse_failure_and_brace_counting_are_not_proof(self):
        self.assertIsNone(ledger._lex(b'{"auth_digest":01}*'))
        self.assertIsNotNone(ledger._lex(b'{"auth_digest":"{"}'))
        self.assertIsNotNone(ledger._lex(b'{"auth_digest":"\\"{"}'))

    def test_fuzz_tails_never_contain_a_closed_value_or_lf(self):
        rng = random.Random(55)
        alphabet = b'{}[]",:0123456789abcdef_\\ \x00auth_digestkindrepair'
        for _ in range(3000):
            tail = bytes(rng.choice(alphabet) for _ in range(rng.randrange(1, 60)))
            if rng.random() < 0.5:
                tail = E2[:rng.randrange(1, len(E2))] + tail
            r = ledger.classify(B + tail, ledger.LEDGER)
            if r["cls"] == ledger.TAIL_PREFIX:
                core = tail.rstrip(b"\x00")
                self.assertNotIn(b"\n", tail)
                self.assertIsNone(ledger._lex(core))
                self.assertEqual(ledger.template_match(core, ledger.LEDGER, 2, ledger.classify(
                    B, ledger.LEDGER)["entries"][-1]["line_digest"], set())[0], "PREFIX")


class Repair(unittest.TestCase):
    def log(self, data, faults=None):
        log = ledger.MemLog(ledger.LEDGER, "ledger", faults or FaultPlan())
        log.data = data
        return log

    def fields(self, i):
        return {"auth_digest": sha(b"a%d" % i), "auth_kind": "initiate", "binding_digest": sha(b"b%d" % i),
                "confirmed_at": i, "issued_at": i, "nonce_digest": sha(b"n%d" % i)}

    def test_v11_i_orphan_is_checked_by_the_caller_before_any_trim(self):
        # The classifier itself is not I-ORPHAN; the invocation test covers V11 end to end.
        r = ledger.classify(B + E2[:200], ledger.LEDGER)
        self.assertEqual(r["cls"], ledger.TAIL_PREFIX)
        self.assertNotIn("c83e6609", "".join(e["auth_digest"] for e in r["entries"]))

    def test_v12_v13_crash_matrix_converges(self):
        torn = B + E2[:123] + b"\x00\x00"
        tail = torn[1004:]
        points = ["ledger:evidence:temp", "ledger:evidence:rename_lost", "ledger:evidence:rename",
                  "ledger:evidence:durable", "ledger:trim:after", "ledger:append:start", "ledger:append:partial",
                  "ledger:append:body", "ledger:append:lf"]
        for p in points:
            with self.subTest(p):
                faults = FaultPlan(p)
                log = self.log(torn, faults)
                with self.assertRaises(Crash):
                    log.append(txn(), log.plan(), self.fields(1))
                mid = log.classify()
                self.assertNotEqual(mid["cls"], U)
                self.assertEqual([canonical.canon(e) for e in mid["entries"][:2]], [L0, L1])
                if mid["cls"] != ledger.CLEAN:
                    log.append(txn(), log.plan(), self.fields(2))
                fin = log.classify()
                self.assertEqual(fin["cls"], ledger.CLEAN)
                refs = {(r["offset"], r["sha256"], r["len"]) for e in fin["entries"] for r in e["repair"]}
                self.assertIn((1004, sha(tail), len(tail)), refs)
                self.assertEqual(log.evidence[(1004, sha(tail), len(tail))], tail)
                self.assertEqual(set(log.evidence), refs)

    def test_v14_retry_with_a_stale_plan_writes_nothing(self):
        log = self.log(B + E2[:123] + b"\x00\x00")
        plan = log.plan()
        log.append(txn(), plan, self.fields(1))
        snap = log.snapshot()
        with self.assertRaises(ledger.RepairMismatch):
            log.append(txn(), plan, self.fields(2))
        self.assertEqual(log.snapshot(), snap)
        self.assertIsNone(log.plan())

    def test_v14b_later_trim_starts_at_the_committed_size(self):
        log = self.log(B + E2[:123])
        log.append(txn(), log.plan(), self.fields(1))
        size = len(log.data)
        log.data += E2[:40]
        r = log.classify()
        self.assertEqual((r["cls"], r["body_len"]), (ledger.TAIL_PREFIX, size))

    def test_terminate_keeps_the_committed_entry(self):
        log = self.log(B + E2 + b"\x00")
        log.append(txn(), log.plan(), self.fields(9))
        res = log.classify()
        self.assertEqual(res["cls"], ledger.CLEAN)
        self.assertEqual(canonical.canon(res["entries"][2]), E2)

    def test_append_needs_a_live_txn(self):
        log = self.log(B)
        with self.assertRaises(TxnError):
            log.append(None, None, self.fields(1))
        t = txn()
        t.close()
        with self.assertRaises(TxnError):
            log.append(t, None, self.fields(1))
        self.assertEqual(log.data, B)

    def test_plan_mismatch_writes_nothing(self):
        log = self.log(B + E2[:50])
        plan = dict(log.plan(), sha256="0" * 64)
        with self.assertRaises(ledger.RepairMismatch):
            log.append(txn(), plan, self.fields(1))
        self.assertEqual((log.data, log.evidence), (B + E2[:50], {}))

    def test_trim_rechecks_the_body_after_preserving_evidence(self):
        class Tamper(FaultPlan):
            def hit(self, point):
                if point == "ledger:evidence:durable":
                    log.data = log.data[:10] + b"X" + log.data[11:]

        log = self.log(B + E2[:50], Tamper())
        with self.assertRaises(ledger.RepairMismatch):
            log.append(txn(), log.plan(), self.fields(1))
        self.assertTrue(log.data.endswith(E2[:50]))

    def test_cap_refuses_ledger_full(self):
        log = ledger.MemLog(ledger.LEDGER, "ledger", cap=2)
        log.data = B
        with self.assertRaises(Refused) as e:
            log.plan()
        self.assertIs(e.exception.code, Code.LEDGER_FULL)


class Restore(unittest.TestCase):
    def test_restore_acceptance(self):
        corrupt = L0 + b"\n" + L1[:-3] + b"xx}\n"
        self.assertEqual(ledger.restore_acceptable(corrupt, B + E2 + b"\n", ledger.LEDGER), [2])
        self.assertIsNone(ledger.restore_acceptable(B, L0 + b"\n", ledger.LEDGER))
        self.assertIsNone(ledger.restore_acceptable(B, B + E2, ledger.LEDGER))
        other = canonical.canon(ledger.make_entry(ledger.LEDGER, 0, ledger.ZERO64, {
            "auth_digest": "1" * 64, "auth_kind": "initiate", "binding_digest": "2" * 64, "confirmed_at": 1,
            "issued_at": 1, "nonce_digest": "3" * 64})) + b"\n"
        self.assertIsNone(ledger.restore_acceptable(B, other, ledger.LEDGER))
        log = ledger.MemLog(ledger.LEDGER, "ledger")
        log.data = B + E2[:30]
        log.append(txn(), log.plan(), {"auth_digest": "6" * 64, "auth_kind": "initiate", "binding_digest": "6" * 64,
                                       "confirmed_at": 1, "issued_at": 1, "nonce_digest": "7" * 64})
        self.assertIsNone(ledger.restore_acceptable(B, log.data, ledger.LEDGER))
        self.assertEqual(ledger.restore_acceptable(B, log.data, ledger.LEDGER, log.evidence), [2])


if __name__ == "__main__":
    unittest.main()

"""Request parser, document parser and CANON (r3 §7.1–§7.2, r4 §4.2, C8)."""

import json
import random
import unittest

import harness  # noqa: F401

import recovery_state as rs
from broker import canonical, opclass, protocol
from broker.refusal import Code, Refused

OWNER = opclass.POOLCTL_OWNER


def req(**fields):
    return json.dumps(dict({"v": 1}, **fields)).encode()


def code(raw, entry=OWNER):
    try:
        protocol.parse_request(raw, entry)
    except Refused as e:
        return e.code
    return None


def gen_d(rng, depth=0):
    kind = rng.randrange(6 if depth < 4 else 4)
    if kind == 0:
        return rng.randrange(-canonical.MAX_INT, canonical.MAX_INT + 1)
    if kind == 1:
        return rng.choice((True, False))
    if kind == 2:
        return "".join(chr(rng.randrange(0x20, 0x7F)) for _ in range(rng.randrange(0, 12)))
    if kind == 3:
        return rng.choice((0, -1, canonical.MAX_INT, -canonical.MAX_INT, "", '"\\'))
    if kind == 4:
        return [gen_d(rng, depth + 1) for _ in range(rng.randrange(0, 4))]
    return {"k" + str(rng.randrange(100)) + "_x": gen_d(rng, depth + 1) for _ in range(rng.randrange(0, 4))}


class Canonical(unittest.TestCase):
    def test_canon_equals_library_digest_on_d(self):
        rng = random.Random(7)
        for _ in range(2000):
            obj = {"a": gen_d(rng)}
            self.assertEqual(canonical.digest(obj), rs.digest(obj))

    def test_domain_rejects(self):
        for bad in ({"A": 1}, {"a": None}, {"a": 1.5}, {"a": 2 ** 53}, {"a": "é"}, {"a": "\n"}, {"a": (1,)},
                    {"a": b"x"}, {1: 1}, {"1a": 1}):
            with self.subTest(bad), self.assertRaises(canonical.DomainError):
                canonical.canon(bad)

    def test_domain_depth_bound(self):
        obj = 1
        for _ in range(canonical.MAX_DEPTH + 2):
            obj = [obj]
        with self.assertRaises(canonical.DomainError):
            canonical.canon(obj)

    def test_secret_free(self):
        self.assertFalse(canonical.secret_free({"a": [{"Lease_Token": 1}]}))
        self.assertTrue(canonical.secret_free({"inventory": {"dirty": ["e1"]}, "evidence_refs": ["x"]}))


class Request(unittest.TestCase):
    def test_valid_request(self):
        op, fields = protocol.parse_request(req(verb="request", slot="w", op_id="op1", reason="r"), OWNER)
        self.assertEqual((op.verb, fields), ("request", {"slot": "w", "op_id": "op1", "reason": "r"}))
        protocol.parse_request(req(verb="request", slot="w", op_id="op1", reason="r") + b"\n", OWNER)

    def test_encoding_refusals(self):
        good = req(verb="stop", reason="r")
        cases = {
            Code.OVERSIZE: req(verb="stop", reason="r" * 5000),
            Code.ENCODING: [good.replace(b"r\"", b"r\x00\""), b'{"v":1,"verb":"stop","reason":"a\\u0041"}',
                            b"\xef\xbb\xbf" + good, b'{"v":1,"verb":"stop","reason":"\xc0\xaf"}',
                            b'{"v":1,"verb":"stop","reason":"\xed\xa0\x80"}', good.decode().encode("utf-16"),
                            b" " + good, good + b"\n\n", good + b"x", b"[" * 3000, b"", b"nul"],
            Code.DUPLICATE_KEY: b'{"v":1,"verb":"stop","reason":"a","reason":"b"}',
            Code.FIELD_INVALID: [b'{"v":1,"verb":"stop","reason":NaN}', b'{"v":1,"verb":"stop","reason":Infinity}',
                                 b'{"v":1,"verb":"stop","reason":1.0}', b'{"v":1,"verb":"stop","reason":1e3}',
                                 b'{"v":12345678901234567890,"verb":"stop","reason":"r"}',
                                 req(v=2, verb="stop", reason="r"),
                                 json.dumps({"v": True, "verb": "stop", "reason": "r"}).encode(),
                                 json.dumps({"verb": "stop", "reason": "r"}).encode(), b"[1]",
                                 req(verb="stop", reason=""), req(verb="stop", reason=None),
                                 req(verb="stop", reason=["r"]), req(verb="stop", reason={"a": "b"}),
                                 req(verb="request", slot="../x", op_id="o", reason="r"),
                                 req(verb="request", slot="w", op_id="o"),
                                 req(verb="quarantine", slot="w", op_id="o", reason="r", quiescence_method="x",
                                     preservation_disposition="preserve-all"),
                                 req(verb="quiescence", slot="w", op_id="o", reason="r", method="owner-quiescence"),
                                 req(verb="mark-suspect", slot="w", reason="r", attest={"note": {"x": "y"}}),
                                 req(verb="mark-suspect", slot="w", reason="r", attest={"bogus": "y"}),
                                 req(verb=1, reason="r")],
        }
        for expected, raws in cases.items():
            for raw in raws if isinstance(raws, list) else [raws]:
                with self.subTest(raw=raw[:60]):
                    self.assertIs(code(raw), expected)

    def test_operation_refusals(self):
        self.assertIs(code(req(verb="migrate", reason="r")), Code.UNKNOWN_OPERATION)
        self.assertIs(code(req(verb="bogus")), Code.UNKNOWN_OPERATION)
        self.assertIs(code(req(verb="request", slot="w", op_id="o", reason="r"), opclass.POOLCTL),
                      Code.UNKNOWN_OPERATION)

    def test_never_accepted_fields(self):
        for k in sorted(protocol.NOT_ALLOWED):
            with self.subTest(k):
                self.assertIs(code(req(verb="request", slot="w", op_id="o", reason="r", **{k: "x"})),
                              Code.FIELD_NOT_ALLOWED)
        self.assertIs(code(req(verb="request", slot="w", op_id="o", reason="r", extra="x")), Code.FIELD_UNKNOWN)

    def test_every_table_verb_has_a_field_set(self):
        self.assertEqual(set(protocol.FIELDS), {op.verb for op in opclass.TABLE})

    def test_fuzz_is_total(self):
        rng = random.Random(11)
        seeds = [req(verb="quarantine", slot="w", op_id="o", reason="r", quiescence_method="owner-quiescence",
                     preservation_disposition="preserve-all"),
                 req(verb="mark-suspect", slot="w", reason="r", attest={"note": "n"})]
        for _ in range(5000):
            raw = bytearray(rng.choice(seeds))
            for _ in range(rng.randrange(1, 4)):
                i = rng.randrange(len(raw))
                op = rng.randrange(3)
                if op == 0:
                    raw[i] = rng.randrange(256)
                elif op == 1:
                    del raw[i]
                else:
                    raw.insert(i, rng.randrange(256))
            try:
                parsed, fields = protocol.parse_request(bytes(raw), OWNER)
            except Refused as e:
                self.assertIsInstance(e.code, Code)
                continue
            req_f, opt_f = protocol.FIELDS[parsed.verb]
            self.assertTrue(set(req_f) <= set(fields) <= set(req_f) | set(opt_f))


class Document(unittest.TestCase):
    def test_round_trip(self):
        rng = random.Random(3)
        for _ in range(500):
            obj = {"a": gen_d(rng), "b": [1, {"c": "d"}]}
            data = canonical.canon(obj)
            self.assertEqual(canonical.canon(protocol.parse_doc(data)), data)

    def test_rejects_non_canonical(self):
        for raw in (b'{"b":1,"a":2}', b'{"a": 1}', b'{"a":1}\n', b'{"a":"\\u0041"}', b'{"a":1.0}', b'{"a":NaN}',
                    b'{"a":1,"a":1}', b'[1]', b'{"A":1}', b'{"a":"\xc3\xa9"}', b'{"a":null}', b"{" * 40):
            with self.subTest(raw), self.assertRaises(Refused):
                protocol.parse_doc(raw)


if __name__ == "__main__":
    unittest.main()

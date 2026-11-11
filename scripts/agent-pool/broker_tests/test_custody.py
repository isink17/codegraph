"""Custody interface and in-memory fake (r3 §3, r4 §8.1, A1 §A1.2, C3)."""

import unittest

import harness  # noqa: F401
from test_ledger_tail import txn

from broker import authz, canonical, custody
from broker._testsupport import capability
from broker.refusal import Code, Refused
from broker.stores import TxnError

KEY = custody.recovery_key("w", "op1", 3)


def store(kind=authz.SYNTHETIC):
    return custody.InMemoryCustody(is_capability=lambda p: authz.may_retrieve(p, kind))


class Custody(unittest.TestCase):
    def test_mint_is_memory_only_and_format_is_32_hex(self):
        s = store()
        h = custody.mint(KEY, lambda: "a" * 32)
        self.assertEqual((s.lookup(KEY).k, s.writes), (custody.ABSENT, []))
        self.assertEqual(h.receipt, {"v": 1, "key": KEY, "format": "recovery/v1",
                                     "check_digest": custody.check_digest(KEY, "a" * 32)})
        for bad in ("A" * 32, "a" * 31, "a" * 33, "g" * 32, 1):
            with self.assertRaises(ValueError):
                custody.mint(KEY, lambda bad=bad: bad)
        self.assertTrue(canonical.HEX32.match(custody.mint(KEY)._token))
        self.assertNotIn("a" * 32, repr(h))

    def test_check_digest_is_domain_separated(self):
        self.assertEqual(custody.check_digest(KEY, "a" * 32), canonical.sha256(
            b"codegraph-custody/v1\x00" + canonical.canon(KEY) + b"\x00" + b"a" * 32))
        self.assertFalse(canonical.SECRET_KEY.search(canonical.canon(custody.receipt(KEY, "a" * 32)).decode()))

    def test_persist_idempotent_and_conflict(self):
        s = store()
        h = custody.mint(KEY, lambda: "a" * 32)
        r1 = s.persist(txn(), h)
        self.assertEqual(s.persist(txn(), h), r1)
        with self.assertRaises(Refused) as e:
            s.persist(txn(), custody.mint(KEY, lambda: "b" * 32))
        self.assertIs(e.exception.code, Code.CUSTODY_CONFLICT)

    def test_receipt_is_state_free(self):
        s = store()
        s.persist(txn(), custody.mint(KEY, lambda: "a" * 32))
        before = canonical.digest(s.lookup(KEY).record["receipt"])
        s.mark_bound(txn(), KEY, ["1" * 64, "2" * 64])
        s.mark_terminal(txn(), KEY, {"state": "READY_FOR_REASSIGNMENT", "marker_digest": "3" * 64,
                                     "journal_digest": "4" * 64})
        self.assertEqual(canonical.digest(s.lookup(KEY).record["receipt"]), before)
        self.assertEqual(s.lookup(KEY).label, "TERMINAL(READY_FOR_REASSIGNMENT)")

    def test_transitions_and_tombstone_block_remint(self):
        s = store()
        s.persist(txn(), custody.mint(KEY, lambda: "a" * 32))
        with self.assertRaises(Refused):
            s.tombstone(txn(), KEY, 1, "5" * 64)
        s.mark_terminal(txn(), KEY, {"state": "CANCELLED", "marker_digest": "3" * 64, "journal_digest": "4" * 64})
        s.tombstone(txn(), KEY, 1, "5" * 64)
        self.assertEqual(s.lookup(KEY).k, custody.DESTROYED_PENDING)
        s.unlink(txn(), KEY)
        self.assertEqual(s.lookup(KEY).k, custody.DESTROYED)
        with self.assertRaises(Refused) as e:
            s.persist(txn(), custody.mint(KEY, lambda: "c" * 32))
        self.assertIs(e.exception.code, Code.CUSTODY_CONFLICT)
        with self.assertRaises(Refused):
            s.retrieve(KEY, authz.SyntheticOwner())

    def test_corruption_is_detected(self):
        s = store()
        s.persist(txn(), custody.mint(KEY, lambda: "a" * 32))
        kd = canonical.digest(KEY)
        good = s.records[kd]
        s.records[kd] = good.replace(b'"state":"PERSISTED"', b'"state":"BOUND"')
        self.assertEqual(s.lookup(KEY).k, custody.CORRUPT)
        with self.assertRaises(Refused) as e:
            s.retrieve(KEY, authz.SyntheticOwner())
        self.assertIs(e.exception.code, Code.CUSTODY_CORRUPT)
        s.records[kd] = good
        s.tombstones[kd] = canonical.canon({"key": KEY, "receipt_digest": "0" * 64, "terminal_ref": {},
                                            "destroyed_at": 1, "auth_digest": "1" * 64})
        self.assertEqual(s.lookup(KEY).k, custody.CORRUPT)

    def test_checksum_alone_is_checked(self):
        s = store()
        s.persist(txn(), custody.mint(KEY, lambda: "a" * 32))
        kd = canonical.digest(KEY)
        rec = custody.parse_doc(s.records[kd])
        s.records[kd] = canonical.canon(dict(rec, checksum="0" * 64))
        self.assertEqual(s.lookup(KEY).k, custody.CORRUPT)

    def test_retrieve_needs_the_right_principal(self):
        for kind, ok_principals, bad in ((authz.SYNTHETIC, [authz.SyntheticOwner(), capability.owner_capability()],
                                          [None, object()]),
                                         (authz.PROTECTED, [capability.owner_capability()],
                                          [authz.SyntheticOwner(), None])):
            s = store(kind)
            s.persist(txn(), custody.mint(KEY, lambda: "a" * 32))
            for p in ok_principals:
                self.assertEqual(s.retrieve(KEY, p), "a" * 32)
            for p in bad:
                with self.assertRaises(Refused) as e:
                    s.retrieve(KEY, p)
                self.assertIs(e.exception.code, Code.OWNER_REQUIRED)

    def test_verify(self):
        s = store()
        s.persist(txn(), custody.mint(KEY, lambda: "a" * 32))
        self.assertTrue(s.verify(KEY, "a" * 32))
        self.assertFalse(s.verify(KEY, "b" * 32))
        self.assertFalse(s.verify(KEY, None))

    def test_every_write_needs_a_live_txn(self):
        s = store()
        h = custody.mint(KEY, lambda: "a" * 32)
        t = txn()
        t.close()
        for call in (lambda x: s.persist(x, h), lambda x: s.mark_bound(x, KEY, []),
                     lambda x: s.mark_terminal(x, KEY, {}), lambda x: s.tombstone(x, KEY, 1, "0" * 64),
                     lambda x: s.unlink(x, KEY)):
            for bad in (None, t):
                with self.assertRaises(TxnError):
                    call(bad)
        self.assertEqual((s.records, s.tombstones, s.writes), ({}, {}, []))


if __name__ == "__main__":
    unittest.main()

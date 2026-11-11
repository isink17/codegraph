"""Token custody (r3 §3.1, r4 §8, A1 §A1). An abstract interface plus an
in-memory fake. The token format stays exactly 32 lowercase hex. Tokens never
leave custody except through retrieve() with an OwnerCapability."""

import hmac
import secrets

from . import canonical
from .protocol import parse_doc
from .refusal import Code, Refused
from .stores import Gated

FORMATS = {"recovery": "recovery/v1", "lease": "lease/v1"}
CHECK_DOMAIN = b"codegraph-custody/v1\x00"

ABSENT = "ABSENT"
PERSISTED = "PERSISTED"
BOUND = "BOUND"
TERMINAL = "TERMINAL"
DESTROYED = "DESTROYED"
DESTROYED_PENDING = "DESTROYED*"
CORRUPT = "CORRUPT"


def recovery_key(slot, op_id, g_pre):
    return {"slot": slot, "id": op_id, "generation": g_pre, "kind": "recovery"}


def lease_key(slot, lease_id, generation):
    return {"slot": slot, "id": lease_id, "generation": generation, "kind": "lease"}


def _valid_key(key):
    return (type(key) is dict and sorted(key) == ["generation", "id", "kind", "slot"] and key["kind"] in FORMATS
            and type(key["generation"]) is int and key["generation"] >= 0)


def check_digest(key, token):
    return canonical.sha256(CHECK_DOMAIN + canonical.canon(key) + b"\x00" + token.encode("ascii"))


def receipt(key, token):
    return {"v": 1, "key": key, "format": FORMATS[key["kind"]], "check_digest": check_digest(key, token)}


class Handle:
    """A minted token in memory only (k1). Nothing is written until persist(k2)."""

    __slots__ = ("key", "_token", "receipt")

    def __init__(self, key, token):
        self.key, self._token = key, token
        self.receipt = receipt(key, token)

    def __repr__(self):
        return f"Handle({self.key!r})"


def mint(key, token_source=None):
    """k1. token_source is injectable for tests; production uses secrets."""
    if not _valid_key(key):
        raise Refused(Code.FIELD_INVALID, "custody key")
    token = (token_source or (lambda: secrets.token_hex(16)))()
    if type(token) is not str or not canonical.HEX32.match(token):
        raise ValueError("token source must yield 32 lowercase hex")
    return Handle(key, token)


class State:
    """What lookup() reports: the K value plus the parsed record (never shown)."""

    __slots__ = ("k", "terminal_state", "record", "tombstone", "record_digest")

    def __init__(self, k, record=None, tombstone=None, record_digest=None):
        self.k = k
        self.record, self.tombstone, self.record_digest = record, tombstone, record_digest
        self.terminal_state = record["terminal_ref"]["state"] if k == TERMINAL else None

    @property
    def label(self):
        return f"TERMINAL({self.terminal_state})" if self.k == TERMINAL else self.k


class CustodyStore:
    """Interface every backend implements (PR-4 chooses E, OD-1)."""

    def lookup(self, key):
        raise NotImplementedError

    def retrieve(self, key, cap):
        raise NotImplementedError

    def persist(self, txn, handle):
        raise NotImplementedError

    def mark_bound(self, txn, key, bound):
        raise NotImplementedError

    def mark_terminal(self, txn, key, terminal_ref):
        raise NotImplementedError

    def tombstone(self, txn, key, destroyed_at, auth_digest):
        raise NotImplementedError

    def unlink(self, txn, key):
        raise NotImplementedError


def _checksum(rec):
    return canonical.digest({k: v for k, v in rec.items() if k != "checksum"})


class InMemoryCustody(Gated, CustodyStore):
    name = "custody"

    def __init__(self, faults=None, is_capability=None):
        super().__init__(faults)
        self.records = {}
        self.tombstones = {}
        self.temps = 0
        self._is_capability = is_capability

    # -------------------------------------------------------- reads (no write)

    @staticmethod
    def _parse(raw):
        try:
            return parse_doc(raw)
        except Refused:
            return None

    def lookup(self, key):
        kd = canonical.digest(key)
        raw, tomb_raw = self.records.get(kd), self.tombstones.get(kd)
        rec = tomb = None
        if raw is not None:
            rec = self._parse(raw)
            if not self._record_ok(rec, key):
                return State(CORRUPT)
        if tomb_raw is not None:
            tomb = self._parse(tomb_raw)
            if not self._tomb_ok(tomb, key):
                return State(CORRUPT)
        if tomb is not None:
            if rec is not None and canonical.digest(rec["receipt"]) != tomb["receipt_digest"]:
                return State(CORRUPT)
            return State(DESTROYED_PENDING if rec is not None else DESTROYED, rec, tomb)
        if rec is None:
            return State(ABSENT)
        return State(rec["state"], rec, None, canonical.sha256(raw))

    @staticmethod
    def _record_ok(rec, key):
        if type(rec) is not dict or sorted(rec) != ["bound", "checksum", "receipt", "state", "terminal_ref", "token"]:
            return False
        tok = rec["token"]
        if type(tok) is not str or not canonical.HEX32.match(tok) or rec["receipt"] != receipt(key, tok):
            return False
        if rec["checksum"] != _checksum(rec):
            return False
        st, bound, tref = rec["state"], rec["bound"], rec["terminal_ref"]
        bound_ok = type(bound) is list and len(bound) == 2 and all(
            type(x) is str and canonical.HEX64.match(x) for x in bound)
        if st == PERSISTED:
            return bound == [] and tref == {}
        if st == BOUND:
            return bound_ok and tref == {}
        if st == TERMINAL:
            return (bound == [] or bound_ok) and type(tref) is dict and sorted(tref) == [
                "journal_digest", "marker_digest", "state"] and tref["state"] in (
                "READY_FOR_REASSIGNMENT", "CANCELLED")
        return False

    @staticmethod
    def _tomb_ok(tomb, key):
        return (type(tomb) is dict and sorted(tomb) == ["auth_digest", "destroyed_at", "key", "receipt_digest",
                                                         "terminal_ref"] and tomb["key"] == key
                and canonical.HEX64.match(str(tomb["receipt_digest"])))

    def retrieve(self, key, cap):
        if self._is_capability is None or not self._is_capability(cap):
            raise Refused(Code.OWNER_REQUIRED)
        st = self.lookup(key)
        if st.k == CORRUPT:
            raise Refused(Code.CUSTODY_CORRUPT)
        if st.k not in (PERSISTED, BOUND, TERMINAL):
            raise Refused(Code.CUSTODY_ABSENT)
        token = st.record["token"]
        if not hmac.compare_digest(check_digest(key, token), st.record["receipt"]["check_digest"]):
            raise Refused(Code.CUSTODY_MISMATCH)
        return token

    def verify(self, key, presented):
        st = self.lookup(key)
        if st.record is None or type(presented) is not str or not canonical.HEX32.match(presented):
            return False
        return hmac.compare_digest(check_digest(key, presented), st.record["receipt"]["check_digest"])

    # -------------------------------------------------------- writes (CommitTxn)

    def _store(self, key, rec):
        rec = dict(rec)
        rec["checksum"] = _checksum(rec)
        self.records[canonical.digest(key)] = canonical.canon(rec)

    def persist(self, txn, handle):
        """k2: ABSENT -> PERSISTED; idempotent for the same token."""
        key = handle.key
        self._begin(txn, "persist")
        st = self.lookup(key)
        if st.k != ABSENT:
            if st.record is not None and hmac.compare_digest(st.record["token"], handle._token):
                self._end(txn, "persist")
                return handle.receipt
            raise Refused(Code.CUSTODY_CONFLICT)
        self.temps += 1
        self.faults.hit(f"{self.name}:persist:temp")
        self._store(key, {"receipt": handle.receipt, "token": handle._token, "state": PERSISTED,
                          "bound": [], "terminal_ref": {}})
        self.temps -= 1
        self._end(txn, "persist")
        return handle.receipt

    def mark_bound(self, txn, key, bound):
        """k4: PERSISTED -> BOUND; idempotent when already bound to the same pair."""
        self._begin(txn, "mark_bound")
        st = self.lookup(key)
        if st.k == BOUND and st.record["bound"] == list(bound):
            self._end(txn, "mark_bound")
            return
        if st.k != PERSISTED:
            raise Refused(Code.CUSTODY_CONFLICT)
        self._store(key, dict(st.record, state=BOUND, bound=list(bound)))
        self._end(txn, "mark_bound")

    def mark_terminal(self, txn, key, terminal_ref):
        """k5 (BOUND) or k6 (PERSISTED) -> TERMINAL."""
        self._begin(txn, "mark_terminal")
        st = self.lookup(key)
        if st.k == TERMINAL and st.record["terminal_ref"] == terminal_ref:
            self._end(txn, "mark_terminal")
            return
        if st.k not in (PERSISTED, BOUND):
            raise Refused(Code.CUSTODY_CONFLICT)
        self._store(key, dict(st.record, state=TERMINAL, terminal_ref=dict(terminal_ref)))
        self._end(txn, "mark_terminal")

    def tombstone(self, txn, key, destroyed_at, auth_digest):
        """k7: TERMINAL -> DESTROYED*."""
        self._begin(txn, "tombstone")
        st = self.lookup(key)
        if st.k in (DESTROYED, DESTROYED_PENDING):
            self._end(txn, "tombstone")
            return
        if st.k != TERMINAL:
            raise Refused(Code.CUSTODY_CONFLICT)
        tomb = {"key": key, "receipt_digest": canonical.digest(st.record["receipt"]),
                "terminal_ref": st.record["terminal_ref"], "destroyed_at": destroyed_at, "auth_digest": auth_digest}
        self.tombstones[canonical.digest(key)] = canonical.canon(tomb)
        self._end(txn, "tombstone")

    def unlink(self, txn, key):
        """k8: DESTROYED* -> DESTROYED."""
        self._begin(txn, "unlink")
        st = self.lookup(key)
        if st.k not in (DESTROYED, DESTROYED_PENDING):
            raise Refused(Code.CUSTODY_CONFLICT)
        self.records.pop(canonical.digest(key), None)
        self._end(txn, "unlink")

    def snapshot(self):
        return canonical.serialize({"records": {k: v.decode() for k, v in sorted(self.records.items())},
                                "tombstones": {k: v.decode() for k, v in sorted(self.tombstones.items())}})

    def references(self):
        """auth_digests that custody binds (I-ORPHAN): bound pairs and tombstones."""
        out = set()
        for kd in self.records:
            rec = self._parse(self.records[kd])
            if type(rec) is dict and type(rec.get("bound")) is list and len(rec["bound"]) == 2:
                out.add(rec["bound"][1])
        for kd in self.tombstones:
            tomb = self._parse(self.tombstones[kd])
            if type(tomb) is dict and "auth_digest" in tomb:
                out.add(tomb["auth_digest"])
        return out

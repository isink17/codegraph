"""Write gating shared by every store interface (A1 §A2.3 Invariant WB), plus the
in-memory fakes PR-1 tests drive. A fake proves ordering and the write barrier,
never filesystem durability: real stores are PR-4 and must satisfy the same
interface with temp, F_FULLFSYNC, renameatx_np(RENAME_EXCL) and directory sync."""

from . import canonical, fscap
from .authz_txn import CommitTxn, StoreCap, TxnError
from .refusal import Code, Refused


class Crash(BaseException):
    """A simulated stop of the process at a named fault point (test fakes only).
    BaseException, so no handler inside the library or the broker can absorb it."""


class FaultPlan:
    """Raises Crash (or `error`, an ordinary failure) once at a named point.
    Points are '<store>:<step>:<when>'."""

    def __init__(self, point=None, error=None):
        self.point, self.error = point, error
        self.seen = []

    def hit(self, point):
        self.seen.append(point)
        if self.point is not None and point == self.point:
            self.point = None
            raise (self.error or Crash)(point)


class Gated:
    """Base for every store whose writes need the live CommitTxn."""

    name = "store"

    def __init__(self, faults=None):
        self.faults = faults if faults is not None else FaultPlan()
        self.writes = []

    def _begin(self, txn, step):
        if not isinstance(txn, CommitTxn):
            raise TxnError(f"{self.name} write without a CommitTxn")
        txn.require()
        self.faults.hit(f"{self.name}:{step}:before")

    def _end(self, txn, step):
        self.writes.append((txn.txn_id, step))
        self.faults.hit(f"{self.name}:{step}:after")

    def snapshot(self):
        raise NotImplementedError


class AuthzStore(Gated):
    """owner/authz/<digest>.json: content-addressed canonical documents (LB2)."""

    name = "authz"

    def __init__(self, faults=None):
        super().__init__(faults)
        self.docs = {}

    def put(self, txn, data):
        d = canonical.sha256(data)
        self._begin(txn, "put")
        if d in self.docs and self.docs[d] != data:
            raise TxnError("content-addressed document differs")
        self.docs[d] = bytes(data)
        self._end(txn, "put")
        return d

    def get(self, d):
        data = self.docs.get(d)
        if data is None or canonical.sha256(data) != d:
            return None
        return data

    def snapshot(self):
        return canonical.serialize({k: v.decode("ascii") for k, v in sorted(self.docs.items())})


class TierC(Gated):
    """Tier C slot records (BROKER_RECORD). Written by the _cgpool child in PR-4."""

    name = "tierc"

    def __init__(self, faults=None):
        super().__init__(faults)
        self.records = {}

    def read(self, slot):
        return self.records.get(slot)

    def write(self, txn, slot, text):
        self._begin(txn, "write")
        self.records[slot] = text
        self._end(txn, "write")

    def snapshot(self):
        return canonical.serialize({k: v for k, v in sorted(self.records.items())})


class FWal(Gated):
    """Tier P freeze and preservation WAL per slot (PR-3 defines effects).
    Each entry: {op_id, kind: intent|complete, f_from, f_to, auth_digest}."""

    name = "fwal"

    def __init__(self, faults=None):
        super().__init__(faults)
        self.entries = {}

    def append(self, txn, slot, entry):
        self._begin(txn, "append")
        self.entries.setdefault(slot, []).append(dict(entry))
        self._end(txn, "append")

    def log(self, slot):
        return [dict(e) for e in self.entries.get(slot, [])]

    def snapshot(self):
        return canonical.serialize({k: v for k, v in sorted(self.entries.items())})


class GateRecords(Gated):
    """Tier P gate records (stop/resume), each binding an auth_digest."""

    name = "gates"

    def __init__(self, faults=None):
        super().__init__(faults)
        self.entries = []

    def append(self, txn, entry):
        self._begin(txn, "append")
        self.entries.append(dict(entry))
        self._end(txn, "append")

    def stopped(self):
        return bool(self.entries) and self.entries[-1]["kind"] == "stop"

    def snapshot(self):
        return canonical.serialize(self.entries)


class RecordStore(Gated):
    """Immutable O5 records (cutover, head, enumeration, barrier, journal and
    artifact stores; PR-3a contract R2 §8 C20). put is exclusive-create; only a
    CommitTxn writes. PR-3a tests seed the fakes directly; PR-4 owns the real
    stores and their writers."""

    def __init__(self, name, faults=None):
        super().__init__(faults)
        self.name = name
        self.items = {}

    def put(self, txn, key, data):
        self._begin(txn, "put")
        if key in self.items:
            raise TxnError(f"{self.name}: {key} exists")
        self.items[key] = bytes(data)
        self._end(txn, "put")

    def get(self, key):
        return self.items.get(key)

    def keys(self):
        return sorted(self.items)

    def snapshot(self):
        return canonical.serialize({k: v.decode("ascii", "replace") for k, v in sorted(self.items.items())})


class CapStore:
    """A capability-gated O5 store: the journal, artifact and blob namespaces
    (P-B1(b) A). It refuses the CommitTxn and accepts only the store
    capability, inside that capability's own hold of the authority lock. A put
    is an exclusive create, or a content-addressed one that accepts identical
    existing bytes; nothing is overwritten or deleted. In-memory model: a key
    whose put raised after the write stays in `pending` until `make_durable`,
    so existence is never durability. A put of identical bytes over a pending
    key does not make it durable (durable restricted stores are PR-4d). Tests
    seed `items` directly (test setup, not a write API)."""

    def __init__(self, name, faults=None):
        self.name = name
        self.faults = faults if faults is not None else FaultPlan()
        self.items, self.pending, self.writes = {}, set(), []

    def put(self, cap, key, data, exclusive):
        if not isinstance(cap, StoreCap) or not cap._holds():
            raise TxnError(f"{self.name} accepts only the store capability")
        self.faults.hit(f"{self.name}:put:before")
        old = self.items.get(key)
        if old is not None:
            if exclusive or old != data:
                raise fscap.Refused(f"{self.name}: {key} exists")
            return
        self.items[key] = bytes(data)
        self.pending.add(key)
        self.writes.append((cap._txn_id, "put"))
        self.faults.hit(f"{self.name}:put:after")
        self.pending.discard(key)

    def get(self, key):
        self.faults.hit(f"{self.name}:get:before")
        return self.items.get(key)

    def durable(self, key):
        return key in self.items and key not in self.pending

    def make_durable(self, key):
        self.faults.hit(f"{self.name}:sync:before")
        self.pending.discard(key)
        return key in self.items

    def keys(self):
        """The complete key listing, or Refused(STATE_UNKNOWN, artifact_inventory); never a partial list."""
        try:
            self.faults.hit(f"{self.name}:keys:before")
        except Exception:
            raise Refused(Code.STATE_UNKNOWN, "artifact_inventory") from None
        return sorted(self.items)

    def snapshot(self):
        return canonical.serialize({k: v.decode("ascii", "replace") for k, v in sorted(self.items.items())})


class EvidenceStore:
    """Content-addressed, append-only evidence objects (F4 §5.1). Not Gated: a
    CommitTxn cannot write it and an IngestCapability cannot write any Gated
    store. evidence.ingest is the only writer."""

    name = "evidence"

    def __init__(self):
        self.objects = {}
        self.by_kind = {}

    def _add(self, digest, data, kind):
        if self.objects.get(digest, data) != data:
            raise TxnError("content-addressed evidence differs")
        if digest not in self.objects:
            self.by_kind.setdefault(kind, []).append(digest)
        self.objects[digest] = bytes(data)

    def get(self, digest):
        data = self.objects.get(digest)
        if data is None or canonical.sha256(data) != digest:
            return None
        return data

    def digests(self):
        return sorted(self.objects)

"""CommitTxn (A1 §A2.3): the single-use write permission that only
authz.revalidate issues. An AST test keeps _issue out of every other module.

From PR-4c also the store capability (P-B1(b) A) and `_issue_effect`, the one
mint of the effect's two capabilities, referenced only by
invocation._f_complete."""

import itertools

_COUNTER = itertools.count(1)


class TxnError(AssertionError):
    """A write was attempted without the live transaction: a defect, never a refusal."""


class CommitTxn:
    __slots__ = ("txn_id", "doc_digest", "ctx_digest", "_live", "_effect")

    def __init__(self, *_a, **_k):
        raise TypeError("a CommitTxn is issued only by authz.revalidate")

    def require(self):
        if not self._live:
            raise TxnError("CommitTxn is closed")

    def close(self):
        self._live = False

    @property
    def live(self):
        return self._live


def _issue(doc_digest, ctx_digest):
    txn = object.__new__(CommitTxn)
    txn.txn_id = next(_COUNTER)
    txn.doc_digest = doc_digest
    txn.ctx_digest = ctx_digest
    txn._live = True
    txn._effect = False
    return txn


# Below `_issue`: authz imports it, and these modules import authz.
from . import canonical, fscap, journal, o5contract as oc  # noqa: E402

_RESERVED = frozenset({"schema", "header", "seq", "prev", "kind", "early"})


class StoreCap:
    """The store capability: exclusive or content-addressed creates in this
    cycle's `journal/<cycle_id>/`, `artifact/<cycle_id>/` and `blob/`
    namespaces only, each check and put in one hold of the FsCap authority's
    lock (LP-PUT). The capability supplies the cycle's header; a store failure
    seals the authority in that hold. Only `_issue_effect` creates it."""
    __slots__ = ("_a", "_c", "_stores", "_txn_id")

    def __init__(self, *_a, **_k):
        raise TypeError("StoreCap is created only by the mint")

    def __copy__(self):
        raise TypeError("not copyable")

    def _not_serializable(self, *a):
        raise TypeError("not serializable")

    __deepcopy__ = __reduce__ = __reduce_ex__ = __getstate__ = _not_serializable

    def _holds(self):
        return self._a.lock.mine() and self._a.state is fscap.FSCAP_OPEN

    def _failed(self, kind, e):
        a = self._a
        a.store_failure = f"{kind}: {e.strerror}"
        a.log.append(("store_failure", kind))
        a._seal_held()

    def put(self, kind, **fields):
        """Append the next journal record of this cycle: `seq` and `prev` from the re-read journal, this cycle's
        header, and the schema of `seq 0` (a `start` may name `journal/v2` in synthetic mode)."""
        a, c = self._a, self._c
        with a.lock:                                        # LP-PUT: check and write under one lock
            a.require_open()                                # LP-PUT check
            a._hit("store_checked")
            try:
                recs = c.records()
                schema = fields.pop("schema", journal.J_SCHEMA_V3) if kind == "start" and not recs else \
                    (recs[0]["schema"] if recs else journal.J_SCHEMA_V3)
                v3 = schema == journal.J_SCHEMA_V3
                if schema not in journal.accepted_journals(c.header["mode"]) \
                        or set(fields) & (_RESERVED if v3 else _RESERVED - {"early"}):
                    raise fscap.Refused("journal_mismatch")
                if kind in ("move.done", "thaw.move.done", "revoke.done", "thaw.done") \
                        and (not recs or a.observed_done != (recs[-1]["kind"], canonical.digest(recs[-1]))):
                    raise fscap.Refused("completion not observed by the paired filesystem capability")
                seq = len(recs)
                prev = canonical.digest(recs[-1]) if recs else oc.ZERO64
                r = {"schema": schema, "header": c.header, "seq": seq, "prev": prev, "kind": kind, **fields}
                if v3:
                    _, _, early = fscap.walk(recs, c.header["root_id"])
                    if seq:
                        r["early"] = fields.get("binding") if kind == "ep.done" else (early if seq > 1 else oc.ZERO64)
                    fscap.walk(recs + [r], c.header["root_id"])
                elif not _v2_ok(recs + [r]):
                    raise fscap.Refused("journal_mismatch")
                data = canonical.canon(r)
                self._stores["journals"].put(self, f"journal/{c.binding['cycle_id']}/{seq:08d}", data, exclusive=True)
            except OSError as e:                            # store failure (read or write): seal in this hold
                self._failed(kind, e)
                raise
            a.log.append(("put", kind))
            return canonical.sha256(data)

    def put_artifact(self, obj):
        """A header-carrying object at `artifact/<cycle_id>/<sha256 of its bytes>`."""
        a, c = self._a, self._c
        with a.lock:
            a.require_open()
            try:
                self._started(c)
                if type(obj) is not dict or c.header is None or obj.get("header") != c.header:
                    raise fscap.Refused("artifact header")
                data = _canon(obj)
                if len(data) > journal.MAX_OBJECT:
                    raise fscap.Refused("artifact too large")
                d = canonical.sha256(data)
                self._stores["artifacts"].put(self, f"artifact/{c.binding['cycle_id']}/{d}", data, exclusive=False)
            except OSError as e:
                self._failed("artifact", e)
                raise
            a.log.append(("put", "artifact"))
            return d

    def put_blob(self, data):
        """Raw bytes at `blob/<sha256 of the bytes>` in the blob store, which `_traces` never scans."""
        a, c = self._a, self._c
        with a.lock:
            a.require_open()
            try:
                self._started(c)
                if type(data) is not bytes:
                    raise fscap.Refused("blob bytes")
                d = canonical.sha256(data)
                self._stores["blobs"].put(self, f"blob/{d}", data, exclusive=False)
            except OSError as e:
                self._failed("blob", e)
                raise
            a.log.append(("put", "blob"))
            return d

    def _started(self, c):
        """No blob or artifact precedes the durability of `journal/<cycle_id>/00000000`."""
        if c.header is None or c.reader.journal(0) is None:
            raise fscap.Refused("journal start not durable")


def _canon(obj):
    try:
        return canonical.canon(obj)
    except canonical.DomainError:
        raise fscap.Refused("artifact encoding") from None


def _v2_ok(recs):
    prev, early = oc.ZERO64, None
    for seq, r in enumerate(recs):
        if not journal._record_ok(r, seq, prev, early) or (seq and recs[seq - 1]["kind"] == "end") \
                or (r["kind"] == "revoked" and any(x["kind"] == "revoked" for x in recs[:seq])):
            return False
        early, prev = r.get("early", early), canonical.digest(r)
    return True


def _issue_effect(txn, binding, header, mode, stores, faults):
    """The one mint of the effect's store capability and filesystem capability, with their shared FsCap
    authority and reader, from the invocation's live CommitTxn. At most one pair per CommitTxn; a second
    attempt refuses and changes nothing. Protected mode refuses (the mode is the invocation's own trust)."""
    if not isinstance(txn, CommitTxn):
        raise TxnError("mint without a CommitTxn")
    if txn._effect:
        raise fscap.Refused("second mint in this invocation")
    txn.require()
    a, c = fscap._mint(binding, header, mode, stores, faults)
    txn._effect = True
    cap = fscap._wrap(StoreCap, a, c)
    cap._stores, cap._txn_id = stores, txn.txn_id
    return cap, fscap._wrap(fscap.FsCap, a, c), a

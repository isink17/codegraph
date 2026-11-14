"""Protected evidence ingestion and selection (F4 §5.1–§5.3; PR-3a contract R2
§3.6, R4 §4.1). Ingestion is the only writer of the EvidenceStore and needs an
IngestCapability, which is not a CommitTxn: it cannot burn an authorization,
touch custody, Tier C, the F WAL or any record store. In PR-3a the capability
is constructible only in _testsupport; PR-4 gives it to the root producer."""

from . import canonical, o5contract as oc, protocol
from .refusal import Code, Refused

PRODUCER = "pr4-root-producer"
ROOT_KINDS = oc.TMB_KINDS + oc.WX_KINDS + oc.QUIES_KINDS + (
    "boot_session", "source_identity", "setuid_writer_scan", "mounts_external", "claimed_set_observation",
    "writer_capabilities", "artifact_isolation", "holder_scan_protected", "protected_mounts", "final_observation")
KINDS = {k: PRODUCER for k in dict.fromkeys(ROOT_KINDS)}
POOL_KINDS = oc.TMB_KINDS + oc.WX_KINDS
FORBIDDEN = ("auth_digest", "token", "custody", "nonce")
OBJECT_KEYS = ("schema", "kind", "subject", "observed_at", "producer", "provenance", "role", "mode", "data")
SUBJECT_KEYS = ("scope", "slot", "slot_generation", "op_id", "cycle", "barrier_id")

_ISEAL = object()


class IngestCapability:
    __slots__ = ()

    def __init__(self, *_a, **_k):
        raise TypeError("an IngestCapability belongs to the PR-4 root producer")


def _new_ingest(seal):
    if seal is not _ISEAL:
        raise TypeError("an IngestCapability belongs to the PR-4 root producer")
    return object.__new__(IngestCapability)


def _forbidden(obj):
    if type(obj) is dict:
        return any(k in FORBIDDEN or _forbidden(v) for k, v in obj.items())
    if type(obj) is list:
        return any(_forbidden(v) for v in obj)
    return False


def validate(obj):
    """Schema of one evidence object; refuses anything outside the producer table."""
    if type(obj) is not dict or sorted(obj) != sorted(OBJECT_KEYS) or obj["schema"] != oc.S_EVIDENCE:
        raise Refused(Code.PROVENANCE_INSUFFICIENT, "evidence schema")
    s = obj["subject"]
    if type(s) is not dict or sorted(s) != sorted(SUBJECT_KEYS) or s["scope"] not in ("slot", "pool") \
            or type(s["slot_generation"]) is not int or any(type(s[k]) is not str for k in SUBJECT_KEYS
                                                            if k != "slot_generation"):
        raise Refused(Code.PROVENANCE_INSUFFICIENT, "evidence subject")
    if KINDS.get(obj["kind"]) != obj["producer"] or obj["provenance"] != "ROOT_OBSERVED" \
            or obj["role"] not in ("decisive", "supporting") or obj["mode"] not in (oc.SYNTHETIC, oc.PROTECTED) \
            or type(obj["observed_at"]) is not int or type(obj["data"]) is not dict:
        raise Refused(Code.PROVENANCE_INSUFFICIENT, "evidence producer")
    if _forbidden(obj) or not canonical.secret_free(obj):
        raise Refused(Code.PROVENANCE_INSUFFICIENT, "evidence field")


def ingest(cap, store, obj):
    """Content-addressed, append-only; a duplicate is idempotent. Returns sha256."""
    if not isinstance(cap, IngestCapability):
        raise TypeError("ingestion needs an IngestCapability")
    validate(obj)
    try:
        data = canonical.canon(obj)
    except canonical.DomainError:
        raise Refused(Code.PROVENANCE_INSUFFICIENT, "evidence outside D") from None
    d = canonical.sha256(data)
    store._add(d, data, obj["kind"])
    return d


_PARSED = {}


def load(store, digest):
    """The validated object, or None. Content-addressed, so a parse is cached
    by (digest, bytes); a replaced or missing object is re-read and refused."""
    data = store.get(digest)
    if data is None:
        return None
    hit = _PARSED.get(digest)
    if hit is not None and hit[0] == data:
        return hit[1]
    try:
        obj = protocol.parse_doc(data)
        validate(obj)
    except Refused:
        return None
    _PARSED[digest] = (bytes(data), obj)
    return obj


def _matches(obj, kind, want):
    s = obj["subject"]
    if obj["kind"] != kind or obj["mode"] != want["mode"]:
        return False
    if kind in POOL_KINDS:
        return s["scope"] == "pool" and s["barrier_id"] == (want["barrier_id"] if kind in oc.WX_KINDS else "")
    return (s["scope"], s["slot"], s["slot_generation"], s["op_id"], s["cycle"]) == (
        "slot", want["slot"], want["gen"], want["op_id"], want["cycle"])


def select(store, kinds, *, slot, gen, op_id, cycle, barrier_id, now, mode):
    """F4 §5.3 step 3: the newest object per kind for this exact subject,
    tie-broken by observed_at then sha256. Returns (picked, missing, stale);
    picked maps kind -> (sha256, object). Only fresh objects are picked."""
    want = {"slot": slot, "gen": gen, "op_id": op_id or "", "cycle": cycle or "", "barrier_id": barrier_id or "",
            "mode": mode}
    picked, missing, stale = {}, [], []
    for kind in dict.fromkeys(kinds):
        objs = [(d, load(store, d)) for d in store.by_kind.get(kind, ())]
        cands = sorted(((o["observed_at"], d), o) for d, o in objs if o is not None and _matches(o, kind, want))
        if not cands:
            missing.append(kind)
            continue
        (t, d), o = cands[-1]
        if not now - oc.FRESH <= t <= now:
            stale.append(kind)
            continue
        picked[kind] = (d, o)
    return picked, missing, stale

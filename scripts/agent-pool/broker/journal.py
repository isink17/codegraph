"""PR-3b cycle-bound artifact headers and the revocation journal (PR-3b
contract R4.1 §3.1–§3.3, §7, §8). Pure: every read goes through the
digest- and key-addressed resolver PR-3b is constructed with, every object is
re-hashed and parsed strictly, and nothing is listed or written.

A journal is walked forward from `journal/<cycle_id>/00000000` by exact key
until the bound head digest, so lineage, contiguity and cycle scope are
proven by construction and no record of another cycle can enter the chain."""

import re

from . import canonical, o5contract as oc, protocol
from .canonical import is_hex64
from .refusal import Code, Refused

J_SCHEMA = "codegraph.o5.journal/v2"
HEADER_KEYS = ("slot", "op_id", "g_pre", "cycle_id", "start_ctx", "cutover_head", "barrier_id", "root_id", "mode")
BOUND_KEYS = ("slot", "op_id", "g_pre", "cutover_head", "barrier_id", "root_id", "mode")
MAX_OBJECT = 49152
MAX_RECORDS = 4096
MAX_ESCAPED = 4096
ATTRS = ("owner", "mode", "acl", "flags")
_PATH_HEX = re.compile(r"^(?:[0-9a-f]{2}){1,2048}\Z")

# kind -> keys beyond the common {schema, header, seq, prev, kind} (+ `early` from seq 1 on).
KINDS = {"start": (), "move.done": ("linked",), "revoke.done": ("attr", "original"), "revoked": ("escaped",),
         "thaw.done": ("attr",), "end": ()}


class Mismatch(Exception):
    """A refusal raised by the pure PR-3b checks; the provider maps it to its
    own result type (decision, verdict, None or a check_fresult raise)."""

    def __init__(self, code, detail):
        super().__init__(detail)
        self.code, self.detail = code, detail


def need(cond, code, detail):
    if not cond:
        raise Mismatch(code, detail)


def path_hex(v):
    return type(v) is str and bool(_PATH_HEX.match(v))


def parse(data, code=Code.PROVENANCE_INSUFFICIENT, detail="evidence_unavailable"):
    """Strict canonical parse of one effect-produced object, bounded by the
    R4.1 §3.4 per-object limit. Returns (object, sha256)."""
    need(type(data) is bytes and len(data) <= MAX_OBJECT, code, detail)
    try:
        obj = protocol.parse_doc(data)
    except Refused:
        raise Mismatch(code, detail) from None
    return obj, canonical.sha256(data)


def check_header(pre, header, cid, sctx):
    """§3.1 / §3.5: the exact header set, every bound field equal to `pre`,
    the cycle identity and the freeze-start context of this cycle."""
    need(type(header) is dict, Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")
    need(sorted(header) == sorted(HEADER_KEYS), Code.STATE_UNKNOWN, "barrier_binding")
    need(all(header[k] == getattr(pre, k) for k in BOUND_KEYS), Code.STATE_UNKNOWN, "barrier_binding")
    need(is_hex64(header["cycle_id"]) and header["cycle_id"] == cid, Code.STATE_UNKNOWN, "cycle_binding")
    need(is_hex64(header["start_ctx"]) and header["start_ctx"] == sctx, Code.STATE_UNKNOWN, "cycle_binding")


class View:
    """A verified journal prefix ending at the bound head."""

    def __init__(self, recs, head):
        self.recs, self.head = recs, head
        self.early = recs[1]["early"] if len(recs) > 1 else None
        kinds = [r["kind"] for r in recs]
        self.revoked = "revoked" in kinds
        self.end = kinds[-1] == "end"
        rv = [r for r in recs if r["kind"] == "revoked"]
        self.escaped = rv[0]["escaped"] if rv else None
        self.moved = "move.done" in kinds
        self.linked = next((r["linked"] for r in recs if r["kind"] == "move.done"), None)
        self.revoked_attrs = {r["attr"]: r["original"] for r in recs if r["kind"] == "revoke.done"}
        self.thawed = {r["attr"] for r in recs if r["kind"] == "thaw.done"}


def _record_ok(rec, seq, prev, early):
    kind = rec.get("kind")
    if kind not in KINDS or rec.get("schema") != J_SCHEMA or rec.get("seq") != seq or rec.get("prev") != prev:
        return False
    want = {"schema", "header", "seq", "prev", "kind", *KINDS[kind]} | ({"early"} if seq else set())
    if set(rec) != want:
        return False
    if seq == 0:
        return kind == "start"
    if kind == "start" or not is_hex64(rec["early"]) or early is not None and rec["early"] != early:
        return False
    if kind == "move.done":
        return type(rec["linked"]) is list and all(path_hex(x) for x in rec["linked"]) \
            and rec["linked"] == sorted(set(rec["linked"]))
    if kind == "revoke.done":
        return rec["attr"] in ATTRS and is_hex64(rec["original"])
    if kind == "thaw.done":
        return rec["attr"] in ATTRS + ("move",)
    return True


def walk(res, pre, head, cid, sctx):
    """§3.2/§8 lineage. Raises Mismatch(journal_mismatch) on any gap, broken
    `prev`, foreign header, unknown kind, record after `end`, second
    `revoked` or partial (non-canonical) record."""
    need(is_hex64(head), Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")
    recs, prev, early = [], oc.ZERO64, None
    for seq in range(MAX_RECORDS):
        data = res.journal(cid, seq)
        if data is None:
            break
        rec, d = parse(data, Code.PROVENANCE_INSUFFICIENT, "journal_mismatch")
        check_header(pre, rec.get("header"), cid, sctx)
        need(_record_ok(rec, seq, prev, early), Code.PROVENANCE_INSUFFICIENT, "journal_mismatch")
        need(not recs or recs[-1]["kind"] != "end", Code.PROVENANCE_INSUFFICIENT, "journal_mismatch")
        need(rec["kind"] != "revoked" or all(r["kind"] != "revoked" for r in recs), Code.PROVENANCE_INSUFFICIENT,
             "journal_mismatch")
        early = rec.get("early", early)
        recs.append(rec)
        if d == head:
            return View(recs, d)
        prev = d
    raise Mismatch(Code.PROVENANCE_INSUFFICIENT, "journal_mismatch")


def require_revoked(j):
    """N-R41-3: a binding after REVOKED must name a journal point at or after
    this cycle's `revoked` record; an older point of the same cycle refuses."""
    need(j.revoked, Code.PROVENANCE_INSUFFICIENT, "journal_mismatch")


def escaped_state(j, ep_paths):
    """R4.1 §7.1: `empty`, `present` or `unclassifiable` (no revoked record,
    escaped section malformed, oversize, duplicated or naming a path outside
    the EP manifest)."""
    e = j.escaped
    if not j.revoked or type(e) is not list or len(e) > MAX_ESCAPED or not all(path_hex(x) for x in e) \
            or e != sorted(set(e)) or any(x not in ep_paths for x in e):
        return "unclassifiable"
    return "present" if e else "empty"


def require_no_escaped(j, ep_paths):
    st = escaped_state(j, ep_paths)
    need(st != "unclassifiable", Code.PROVENANCE_INSUFFICIENT, "escaped_unclassifiable")
    need(st == "empty", Code.QUIESCENCE_NOT_ESTABLISHED, "escaped_present")


def escaped_digest(j):
    return canonical.digest({"escaped": j.escaped if type(j.escaped) is list else []})


def thaw_complete(j):
    """(thaw, NONE): the journal ends in `end` and every committed step of this
    cycle (the move and each revoked attribute) has its own `thaw.done`."""
    committed = set(j.revoked_attrs) | ({"move"} if j.moved else set())
    return j.end and committed <= j.thawed

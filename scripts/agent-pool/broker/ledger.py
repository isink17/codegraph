"""Hash-chained Tier P logs: the replay ledger and the per-slot anchor logs.
Line format and append (A1 §A4.2–§A4.3), strict torn-tail classification,
owner-confirmed repair plan and repair (R5-1 §R2–§R6), restore acceptance
(A1 §A4.7). The classifier and the plan are pure functions over bytes; MemLog is
the in-memory store with fault hooks. Real file I/O is PR-4."""

import json

from . import canonical
from .refusal import Code, Refused
from .stores import Gated, TxnError

L_MAX = 4096
CAP = 10 ** 6
ZERO64 = "0" * 64
MAX_PENDING = 16
_HEX = frozenset(b"0123456789abcdef")
_DIG = frozenset(b"0123456789")

LEDGER_KINDS = ("initiate", "resume")
ANCHOR_KINDS = ("abort", "complete", "pending", "stable")
# R5-1 §R2 hole W, enumerated (review N5-5). "migrate" is the seed entry's verb;
# complete and abort entries carry the verb of the pending entry they resolve.
ANCHOR_VERBS = ("acquire", "clear-suspect", "mark-suspect", "migrate", "quarantine", "release")

REPAIR_ITEM = (("len", "uint"), ("offset", "uint"), ("sha256", "hex"))


class Spec:
    __slots__ = ("kind", "keys", "burns")

    def __init__(self, kind, keys, burns):
        self.kind, self.keys, self.burns = kind, keys, burns


LEDGER = Spec("ledger", (("auth_digest", "hex"), ("auth_kind", ("enum", LEDGER_KINDS)), ("binding_digest", "hex"),
                         ("confirmed_at", "uint"), ("issued_at", "uint"), ("line_digest", "hex"),
                         ("nonce_digest", "hex"), ("prev", "prev"), ("repair", "repair"), ("seq", "seq"),
                         ("v", "v")), burns=("auth_digest", "nonce_digest"))
ANCHOR = Spec("anchor", (("auth_digest", "hex"), ("kind", ("enum", ANCHOR_KINDS)), ("line_digest", "hex"),
                         ("post", "hex"), ("pre", "hex"), ("prev", "prev"), ("repair", "repair"), ("seq", "seq"),
                         ("v", "v"), ("verb", ("enum", ANCHOR_VERBS))), burns=())

CLEAN = "CLEAN"
TAIL_UNTERMINATED_VALID = "TAIL_UNTERMINATED_VALID"
TAIL_PREFIX = "TAIL_PREFIX"
PENDING_REPAIR_RECORD = "PENDING_REPAIR_RECORD"
UNCLASSIFIABLE = "UNCLASSIFIABLE"


def line_digest(entry):
    return canonical.digest({k: v for k, v in entry.items() if k != "line_digest"})


def make_entry(spec, seq, prev, fields, repair=()):
    entry = dict(fields, seq=seq, prev=prev, v=1,
                 repair=sorted((dict(r) for r in repair), key=lambda r: r["sha256"]))
    if sorted(k for k, _ in spec.keys if k != "line_digest") != sorted(entry):
        raise ValueError("entry fields do not match the log template")
    entry["line_digest"] = line_digest(entry)
    return entry


# ------------------------------------------------------------ strict entry validation (step 3, 8c)

def _reject(_):
    raise ValueError("not in D")


def _value_ok(v, t, seq, prev):
    if t == "hex":
        return type(v) is str and bool(canonical.HEX64.match(v))
    if t == "uint":
        return type(v) is int and 0 <= v <= canonical.MAX_INT
    if t == "prev":
        return v == prev
    if t == "seq":
        return type(v) is int and v == seq
    if t == "v":
        return type(v) is int and v == 1
    if t == "repair":
        if type(v) is not list or len(v) > MAX_PENDING:
            return False
        for it in v:
            if type(it) is not dict or sorted(it) != ["len", "offset", "sha256"]:
                return False
            if not all(_value_ok(it[k], tt, seq, prev) for k, tt in REPAIR_ITEM):
                return False
        return [it["sha256"] for it in v] == sorted(it["sha256"] for it in v)
    return type(v) is str and v in t[1]


def entry_valid(raw, spec, seq, prev, burned):
    """raw is a line without LF: canonical bytes, D, template, chain, digest and,
    for the ledger, no reuse of a burned auth_digest or nonce_digest."""
    if len(raw) + 1 > L_MAX:
        return None
    try:
        e = json.loads(raw.decode("ascii"), parse_float=_reject, parse_constant=_reject)
        if type(e) is not dict or canonical.canon(e) != raw:
            return None
    except Exception:
        return None
    if [k for k, _ in spec.keys] != sorted(e):
        return None
    if not all(_value_ok(e[k], t, seq, prev) for k, t in spec.keys):
        return None
    if e["line_digest"] != line_digest(e):
        return None
    if any(e[k] in burned for k in spec.burns):
        return None
    return e


# ------------------------------------------------------------ canonical-prefix recognizer (step 9)

class _Eoi(Exception):
    pass


class _Mismatch(Exception):
    pass


class _Cur:
    __slots__ = ("b", "i")

    def __init__(self, b):
        self.b, self.i = b, 0

    def peek(self):
        if self.i >= len(self.b):
            raise _Eoi
        return self.b[self.i]

    def lit(self, s):
        for c in s:
            if self.peek() != c:
                raise _Mismatch(self.i)
            self.i += 1


def _uint(cur):
    start = cur.i
    if cur.peek() == 0x30:
        cur.i += 1
        return 0
    while True:
        try:
            c = cur.peek()
        except _Eoi:
            digits = cur.b[start:cur.i]
            if digits and (len(digits) > 16 or int(digits) > canonical.MAX_INT):
                raise _Mismatch(start) from None
            raise
        if c not in _DIG:
            break
        cur.i += 1
    digits = cur.b[start:cur.i]
    if not digits or len(digits) > 16 or int(digits) > canonical.MAX_INT:
        raise _Mismatch(start)
    return int(digits)


def _hole(cur, t, seq, prev):
    if t == "prev":
        cur.lit(b'"' + prev.encode() + b'"')
        return prev
    if t == "hex":
        cur.lit(b'"')
        start = cur.i
        for _ in range(64):
            if cur.peek() not in _HEX:
                raise _Mismatch(cur.i)
            cur.i += 1
        val = cur.b[start:cur.i].decode()
        cur.lit(b'"')
        return val
    if t == "seq":
        cur.lit(str(seq).encode())
        return seq
    if t == "v":
        cur.lit(b"1")
        return 1
    if t == "uint":
        return _uint(cur)
    if t == "repair":
        cur.lit(b"[")
        if cur.peek() == 0x5D:
            cur.i += 1
            return []
        n = 0
        while True:
            n += 1
            if n > MAX_PENDING:
                raise _Mismatch(cur.i)
            cur.lit(b"{")
            for j, (k, tt) in enumerate(REPAIR_ITEM):
                cur.lit((b"," if j else b"") + b'"' + k.encode() + b'":')
                _hole(cur, tt, seq, prev)
            cur.lit(b"}")
            if cur.peek() == 0x5D:
                cur.i += 1
                return n
            cur.lit(b",")
    cur.lit(b'"')
    start = cur.i
    members = [m.encode() for m in t[1]]
    while True:
        try:
            c = cur.peek()
        except _Eoi:
            if not any(m.startswith(cur.b[start:cur.i]) for m in members):
                raise _Mismatch(start) from None
            raise
        if c == 0x22:
            if cur.b[start:cur.i] not in members:
                raise _Mismatch(start)
            cur.i += 1
            return cur.b[start:cur.i - 1].decode()
        cur.i += 1
        if not any(m.startswith(cur.b[start:cur.i]) for m in members):
            raise _Mismatch(start)


def template_match(core, spec, seq, prev, burned):
    """("PREFIX", None) for a proper prefix of the template at seq/prev,
    ("COMPLETE", end) for a full instance, ("MISMATCH", pos) otherwise."""
    cur = _Cur(core)
    try:
        cur.lit(b"{")
        for j, (k, t) in enumerate(spec.keys):
            cur.lit((b"," if j else b"") + b'"' + k.encode() + b'":')
            start = cur.i
            val = _hole(cur, t, seq, prev)
            if k in spec.burns and val in burned:
                raise _Mismatch(start)
        cur.lit(b"}")
        return ("COMPLETE", cur.i)
    except _Eoi:
        return ("PREFIX", None)
    except _Mismatch as m:
        return ("MISMATCH", m.args[0])


def _lex(core):
    """Escape-aware stdlib JSON scan. Any exception counts as no value (N5-4)."""
    try:
        _, end = json.JSONDecoder().raw_decode(core.decode("ascii"))
        return end
    except Exception:
        return None


# ------------------------------------------------------------ classification (R5-1 §R3)

def _bad(reason, **extra):
    return dict(extra, cls=UNCLASSIFIABLE, reason=reason)


def classify(data, spec, evidence=None, cap=CAP):
    """Pure, total classification of one log file. `evidence` maps
    (offset, sha256, len) to the stored evidence bytes."""
    evidence = evidence or {}
    if len(data) > cap * L_MAX + L_MAX:
        return _bad("FILE_OVERSIZE")
    cut = data.rfind(b"\n") + 1
    body, tail = data[:cut], data[cut:]
    lines = body.split(b"\n")[:-1]
    if len(lines) > cap:
        return _bad("CAP")
    entries, prev, burned = [], ZERO64, set()
    for i, raw in enumerate(lines):
        e = entry_valid(raw, spec, i, prev, burned)
        if e is None:
            return _bad("LINE_INVALID", line=i)
        entries.append(e)
        prev = e["line_digest"]
        burned.update(e[k] for k in spec.burns)
    res = {"body_len": len(body), "body_sha256": canonical.sha256(body)}
    tail_entry, core, nul = None, b"", 0
    if not tail:
        cls = CLEAN
    elif len(tail) > L_MAX:
        return _bad("TAIL_OVERSIZE", **res)
    else:
        core = tail.rstrip(b"\x00")
        nul = len(tail) - len(core)
        if any(not 0x20 <= c <= 0x7E for c in core):
            return _bad("NON_PRINTABLE", **res)
        end = _lex(core)
        if end is not None:
            if end < len(core):
                second = _lex(core[end:])
                return _bad("CONCATENATED_VALUES" if second is not None else "COMPLETE_VALUE_EXTRA_BYTES", **res)
            tail_entry = entry_valid(core, spec, len(entries), prev, burned)
            if tail_entry is None:
                return _bad("COMPLETE_INVALID", **res)
            if nul > 1:
                return _bad("NUL_EXCESS", **res)
            cls = TAIL_UNTERMINATED_VALID
        else:
            kind, pos = template_match(core, spec, len(entries), prev, burned)
            if kind == "COMPLETE":
                return _bad("RECOGNIZER_DISAGREE", **res)
            if kind == "MISMATCH":
                return _bad("NON_CANONICAL_PREFIX", pos=pos, **res)
            if len(core) > L_MAX - 2 or len(tail) > L_MAX - 1:
                return _bad("TAIL_OVERSIZE", **res)
            cls = TAIL_PREFIX
    committed = entries + ([tail_entry] if tail_entry is not None else [])
    referenced = {(r["offset"], r["sha256"], r["len"]) for e in committed for r in e["repair"]}
    for name, raw in evidence.items():
        if canonical.sha256(raw) != name[1] or len(raw) != name[2]:
            return _bad("EVIDENCE_INVALID", **res)
    if any(ref not in evidence for ref in referenced):
        return _bad("EVIDENCE_INVALID", **res)
    pending = sorted(n for n in evidence if n not in referenced)
    if pending and (tail_entry is not None or any(n[0] != len(body) for n in pending) or len(pending) > MAX_PENDING):
        return _bad("EVIDENCE_OFFSET", **res)
    if cls == CLEAN and pending:
        cls = PENDING_REPAIR_RECORD
    res.update(cls=cls, reason=None, entries=committed, pending=pending,
               committed_size=len(body) + len(core) if tail_entry is not None else len(body),
               core_len=len(core), nul=nul, tail_len=len(tail), tail_sha256=canonical.sha256(tail),
               core_sha256=canonical.sha256(core))
    return res


def plan_item(log_name, res):
    """The owner-visible repair plan item for one log (R5-1 §R4.3; values per
    review N5-1). None when nothing is to repair. Raises for a log no plan can fix."""
    cls = res["cls"]
    if cls == UNCLASSIFIABLE:
        raise Refused(Code.LEDGER_UNAVAILABLE, res["reason"])
    if cls == CLEAN:
        return None
    base = {"log": log_name, "class": cls, "body_sha256": res["body_sha256"],
            "evidence_pending": len(res["pending"])}
    if cls == TAIL_PREFIX:
        if len(res["pending"]) >= MAX_PENDING:
            raise Refused(Code.LEDGER_UNAVAILABLE, "EVIDENCE_PENDING_LIMIT")
        return dict(base, action="trim", offset=res["body_len"], len=res["tail_len"], sha256=res["tail_sha256"])
    if cls == TAIL_UNTERMINATED_VALID:
        return dict(base, action="terminate", offset=res["body_len"] + res["core_len"], len=res["nul"],
                    sha256=res["core_sha256"])
    return dict(base, action="record", offset=res["body_len"], len=0, sha256=canonical.sha256(b""))


def restore_acceptable(current, backup, spec, backup_evidence=None):
    """A1 §A4.7 conditions for an out-of-band restore from backup B (with its
    own repair evidence). Returns the list of extra backup line indexes the
    owner must confirm (review N-10), or None."""
    b = classify(backup, spec, backup_evidence)
    if b["cls"] != CLEAN:
        return None
    blines = backup.split(b"\n")[:-1]
    clines = current.split(b"\n")
    lf_terminated = len(clines) - 1
    if len(blines) < lf_terminated:
        return None
    for i, raw in enumerate(clines[:lf_terminated]):
        try:
            e = json.loads(raw.decode("ascii"), parse_float=_reject, parse_constant=_reject)
            self_valid = type(e) is dict and canonical.canon(e) == raw and e.get("line_digest") == line_digest(e)
        except Exception:
            self_valid = False
        if self_valid and blines[i] != raw:
            return None
    return list(range(lf_terminated, len(blines)))


class RepairMismatch(TxnError):
    """Phase W found a file state other than the confirmed plan: a defect, since
    the Tier P lock is held from V to W."""


class MemLog(Gated):
    """One append-only log in memory, with the R5-1 §R5 repair procedure and
    fault points for every crash row of §R6."""

    def __init__(self, spec, name, faults=None, cap=CAP):
        super().__init__(faults)
        self.spec, self.name, self.cap = spec, name, cap
        self.data = b""
        self.evidence = {}

    def classify(self):
        return classify(self.data, self.spec, self.evidence, self.cap)

    def entries(self):
        res = self.classify()
        if res["cls"] == UNCLASSIFIABLE:
            raise Refused(Code.LEDGER_UNAVAILABLE if self.spec is LEDGER else Code.STATE_UNKNOWN, res["reason"])
        return res["entries"]

    def plan(self):
        res = self.classify()
        if res["cls"] == UNCLASSIFIABLE:
            raise Refused(Code.LEDGER_UNAVAILABLE if self.spec is LEDGER else Code.STATE_UNKNOWN, res["reason"])
        if len(res["entries"]) >= self.cap:
            raise Refused(Code.LEDGER_FULL)
        return plan_item(self.name, res)

    def _point(self, step):
        self.faults.hit(f"{self.name}:{step}")

    def append(self, txn, confirmed_plan, fields):
        """Phase W: the confirmed repair, then the append (A1 §A4.3)."""
        self._begin(txn, "append")
        res = self.classify()
        if res["cls"] == UNCLASSIFIABLE or plan_item(self.name, res) != confirmed_plan:
            raise RepairMismatch(f"{self.name} changed since the confirmed plan")
        if res["cls"] == TAIL_PREFIX:
            self._trim(res, confirmed_plan)
            res = self.classify()
        if res["cls"] == TAIL_UNTERMINATED_VALID:
            self.data = self.data[:res["committed_size"]] + b"\n"
            self._point("lf:after")
            res = self.classify()
        if res["cls"] not in (CLEAN, PENDING_REPAIR_RECORD) or len(res["entries"]) >= self.cap:
            raise RepairMismatch(f"{self.name} not appendable after repair")
        size = len(self.data)
        if size != res["committed_size"] or (size and self.data[-1:] != b"\n"):
            raise RepairMismatch(f"{self.name} committed size")
        entries = res["entries"]
        prev = entries[-1]["line_digest"] if entries else ZERO64
        repair = [{"offset": o, "sha256": s, "len": n} for (o, s, n) in res["pending"]]
        entry = make_entry(self.spec, len(entries), prev, fields, repair)
        body = canonical.canon(entry)
        self._point("append:start")
        self.data += body[:len(body) // 2]
        self._point("append:partial")
        self.data += body[len(body) // 2:]
        self._point("append:body")
        self.data += b"\n"
        self._point("append:lf")
        check = self.classify()
        if check["cls"] != CLEAN or check["entries"][-1] != entry:
            raise RepairMismatch(f"{self.name} append did not verify")
        self._end(txn, "append")
        return entry

    def _trim(self, res, plan):
        offset, tail = res["body_len"], self.data[res["body_len"]:]
        name = (offset, canonical.sha256(tail), len(tail))
        if (offset, len(tail), name[1], res["body_sha256"]) != (
                plan["offset"], plan["len"], plan["sha256"], plan["body_sha256"]):
            raise RepairMismatch(f"{self.name} trim differs from the confirmed plan")
        self._point("evidence:temp")
        if name in self.evidence:
            if self.evidence[name] != tail:
                raise RepairMismatch(f"{self.name} evidence name holds other bytes")
        else:
            self._point("evidence:rename_lost")
            self.evidence[name] = bytes(tail)
            self._point("evidence:rename")
        self._point("evidence:durable")
        if len(self.data) != offset + len(tail) or canonical.sha256(self.data[:offset]) != plan["body_sha256"] \
                or (offset and self.data[offset - 1:offset] != b"\n"):
            raise RepairMismatch(f"{self.name} changed before the trim")
        self.data = self.data[:offset]
        self._point("trim:after")
        if len(self.data) != offset or canonical.sha256(self.data) != plan["body_sha256"]:
            raise RepairMismatch(f"{self.name} trim did not verify")

    def snapshot(self):
        return canonical.serialize({"data": self.data.hex(),
                                "evidence": {f"{o}-{s}-{n}": v.hex() for (o, s, n), v in sorted(self.evidence.items())}})

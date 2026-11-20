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
J_SCHEMA_V3 = "codegraph.o5.journal/v3"
HEADER_KEYS = ("slot", "op_id", "g_pre", "cycle_id", "start_ctx", "cutover_head", "barrier_id", "root_id", "mode")
BOUND_KEYS = ("slot", "op_id", "g_pre", "cutover_head", "barrier_id", "root_id", "mode")
MAX_OBJECT = 49152
MAX_RECORDS = 4096
MAX_ESCAPED = 4096
ATTRS = ("owner", "mode", "acl", "flags")
# One bound for every O5 path, equal to `fdwalk.Limits.max_path`.
MAX_PATH_BYTES = 4096
_PATH_HEX = re.compile(r"^(?:[0-9a-f]{2}){1,%d}\Z" % MAX_PATH_BYTES)

# kind -> keys beyond the common {schema, header, seq, prev, kind} (+ `early` from seq 1 on).
KINDS = {"start": (), "move.done": ("linked",), "revoke.done": ("attr", "original"), "revoked": ("escaped",),
         "thaw.done": ("attr",), "end": ()}
_MOVE = ("src_parent", "src_name", "dst_parent", "dst_name", "root")
_RANGE = ("attr", "lo", "hi")
KINDS_V3 = {"start": (), "ep.done": ("binding",), "move.intent": _MOVE, "move.done": ("intent", "linked"),
            "move.abort": ("intent", "reason"), "revoke.intent": _RANGE + ("originals",), "revoke.done": _RANGE,
            "revoked": ("escaped",), "thaw.intent": _RANGE, "thaw.done": _RANGE, "thaw.move.intent": _MOVE,
            "thaw.move.done": ("intent",), "end": ("outcome", "reason")}
REVOKE_ORDER = ("flags", "acl", "mode", "owner")
THAW_ORDER = REVOKE_ORDER[::-1]
MAX_RANGE = 4096
ABORT_REASONS = ("rename_failed", "target_exists", "early_unstable")
_REASON = re.compile(r"^[a-z][a-z_]{0,63}\Z")


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


# The capture walk's default `fdwalk.Limits.max_depth`: no captured path has
# more separators.
MAX_PATH_DEPTH = 64


def valid_component(c):
    """One path component as the capture walk (`fdwalk.valid_name`) accepts
    it (R2 §3.2): a manifest path with an empty, `.` or `..` component names a
    file another spelling also names."""
    return 0 < len(c) <= 255 and c not in (b".", b"..") and b"\0" not in c


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
    v3 = False

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


def accepted_journals(mode):
    """Journal schemas a mode accepts: protected v3 only, synthetic v2 and v3."""
    if type(mode) is not str:
        return ()
    return {oc.SYNTHETIC: (J_SCHEMA, J_SCHEMA_V3), oc.PROTECTED: (J_SCHEMA_V3,)}.get(mode, ())


def _name(v):
    return path_hex(v) and valid_component(bytes.fromhex(v)) and b"/" not in bytes.fromhex(v)


def _ordinal(v):
    return type(v) is int and 0 <= v


def _record_ok_v3(rec, seq, prev, early):
    """The common fields and one kind's own fields of a `journal/v3` record
    (D2.2 §17.3.2); `early` is fixed by `seq 1` and constant after it."""
    kind = rec.get("kind")
    if type(kind) is not str or kind not in KINDS_V3 or rec.get("schema") != J_SCHEMA_V3 \
            or type(rec.get("seq")) is not int or rec["seq"] != seq or rec.get("prev") != prev:
        return False
    if set(rec) != {"schema", "header", "seq", "prev", "kind", *KINDS_V3[kind]} | ({"early"} if seq else set()):
        return False
    if seq == 0:
        return kind == "start"
    if not is_hex64(rec["early"]) or kind == "start":
        return False
    if seq == 1:
        if rec["early"] != (rec["binding"] if kind == "ep.done" else oc.ZERO64):
            return False
    elif rec["early"] != early:
        return False
    if kind == "ep.done":
        return is_hex64(rec["binding"])
    if kind in ("move.intent", "thaw.move.intent"):
        return all(is_hex64(rec[k]) for k in ("src_parent", "dst_parent", "root")) \
            and _name(rec["src_name"]) and _name(rec["dst_name"])
    if kind in ("move.done", "move.abort", "thaw.move.done") and not is_hex64(rec["intent"]):
        return False
    if kind == "move.done":
        return rec["linked"] == [] and type(rec["linked"]) is list
    if kind == "move.abort":
        return type(rec["reason"]) is str and rec["reason"] in ABORT_REASONS
    if kind in ("revoke.intent", "revoke.done", "thaw.intent", "thaw.done"):
        return type(rec["attr"]) is str and rec["attr"] in ATTRS and _ordinal(rec["lo"]) and _ordinal(rec["hi"]) \
            and rec["lo"] <= rec["hi"] < rec["lo"] + MAX_RANGE \
            and (kind != "revoke.intent" or is_hex64(rec["originals"]))
    if kind == "end":
        o, r = rec["outcome"], rec["reason"]
        return type(o) is str and type(r) is str and ((o, r) == ("thawed", "") or o == "none" and bool(_REASON.match(r)))
    return True


class _Grammar:
    """The `journal/v3` grammar of D2.2 §17.3.2, checked record by record. The
    rules that need the EP entry count `N` and its `originals` are checked by
    `check_v3` once the EP is read."""

    def __init__(self, root_id):
        self.root_id, self.last = root_id, None
        self.mi = self.mi_d = self.md = self.abort = self.tmi = self.tmi_d = self.revoked = self.end = None
        self.rev, self.thaw, self.orig = {a: [] for a in ATTRS}, {a: [] for a in ATTRS}, {}
        self.open = self.cur = self.plan = None
        self.thaw_moved = False

    def _thaw_plan(self):
        """Frozen at the first `thaw.*` record: every revoke range, done or
        open, owner first, each attribute's ranges ascending."""
        if self.plan is None:
            self.plan = [(a, r[0], r[1]) for a in THAW_ORDER for r in self.rev[a]]
        return self.plan

    def _thaw_done(self):
        return sum(1 for a in ATTRS for r in self.thaw[a] if r[2])

    def ok(self, rec, d):
        k, last = rec["kind"], self.last
        # After `move.abort` only its `end` can follow: every other kind needs another predecessor.
        if self.end is not None or (last is None) != (k == "start"):
            return False
        if last == "start" and k not in ("ep.done", "end") or k == "ep.done" and last != "start":
            return False
        if k == "move.intent":
            if last != "ep.done" or rec["root"] != self.root_id:
                return False
            self.mi, self.mi_d = rec, d
        elif k in ("move.done", "move.abort"):
            if last != "move.intent" or rec["intent"] != self.mi_d:
                return False
            if k == "move.done":
                self.md = rec
            else:
                self.abort = rec
        elif k == "revoke.intent":
            a, i = rec["attr"], REVOKE_ORDER.index(rec["attr"])
            if self.md is None or self.open is not None or self.revoked is not None or self.plan is not None \
                    or self.orig.get(a, rec["originals"]) != rec["originals"]:
                return False
            if a == self.cur:
                if rec["lo"] != self.rev[a][-1][1] + 1:
                    return False
            elif rec["lo"] != 0 or i != (0 if self.cur is None else REVOKE_ORDER.index(self.cur) + 1):
                return False
            self.rev[a].append([rec["lo"], rec["hi"], False])
            self.orig[a], self.cur, self.open = rec["originals"], a, ("revoke", a, rec["lo"], rec["hi"])
        elif k == "revoke.done":
            if self.open != ("revoke", rec["attr"], rec["lo"], rec["hi"]) or self.plan is not None:
                return False
            self.rev[rec["attr"]][-1][2], self.open = True, None
        elif k == "revoked":
            if last != "revoke.done" or self.cur != "owner" or self.open is not None:
                return False
            self.revoked = rec
        elif k == "thaw.intent":
            # A thaw may start while one revoke range is open (thaw after FREEZE_INCOMPLETE).
            if self.md is None or self.open is not None and (self.open[0] != "revoke" or self.plan is not None):
                return False
            plan, n = self._thaw_plan(), self._thaw_done()
            if n >= len(plan) or plan[n] != (rec["attr"], rec["lo"], rec["hi"]):
                return False
            self.thaw[rec["attr"]].append([rec["lo"], rec["hi"], False])
            self.open = ("thaw", rec["attr"], rec["lo"], rec["hi"])
        elif k == "thaw.done":
            if self.open != ("thaw", rec["attr"], rec["lo"], rec["hi"]):
                return False
            self.thaw[rec["attr"]][-1][2], self.open = True, None
        elif k == "thaw.move.intent":
            mi = self.mi
            if self.md is None or self.open is not None or self.tmi is not None \
                    or self._thaw_done() != len(self._thaw_plan()) \
                    or (rec["src_parent"], rec["src_name"], rec["dst_parent"], rec["dst_name"], rec["root"]) \
                    != (mi["dst_parent"], mi["dst_name"], mi["src_parent"], mi["src_name"], mi["root"]):
                return False
            self.tmi, self.tmi_d = rec, d
        elif k == "thaw.move.done":
            if last != "thaw.move.intent" or rec["intent"] != self.tmi_d:
                return False
            self.thaw_moved = True
        elif k == "end":
            if rec["outcome"] == "thawed" and last != "thaw.move.done" or rec["outcome"] == "none" and (
                    last not in ("start", "ep.done", "move.abort")
                    or last == "move.abort" and rec["reason"] != self.abort["reason"]):
                return False
            self.end = rec
        self.last = k
        return True


class View3:
    """A verified `journal/v3` prefix ending at the bound head (D2.2 §17.3.2),
    with the merged reader fields in their merged meanings."""
    v3 = True

    def __init__(self, g, recs, head):
        self.recs, self.head = recs, head
        self.early = recs[1]["early"] if len(recs) > 1 else None
        self.ep = recs[1]["binding"] if len(recs) > 1 and recs[1]["kind"] == "ep.done" else None
        self.move_intent, self.moved, self.aborted = g.mi, g.md is not None, g.abort
        self.ranges = {a: [tuple(r) for r in g.rev[a]] for a in ATTRS}
        self.thaw_ranges = {a: [tuple(r) for r in g.thaw[a]] for a in ATTRS}
        self.revoked = g.revoked is not None
        self.escaped = g.revoked["escaped"] if g.revoked is not None else None
        self.thaw_moved = g.thaw_moved
        self.end = recs[-1]["kind"] == "end"
        self.end_rec = recs[-1] if self.end else None
        self.linked = g.md["linked"] if g.md is not None else None
        self.revoked_attrs = dict(g.orig)
        self.thawed = {a for a in ATTRS if _thawed(self, a)} | ({"move"} if g.thaw_moved else set())


def _thawed(j, a):
    return [r[:2] for r in j.thaw_ranges[a]] == [r[:2] for r in j.ranges[a]] and all(r[2] for r in j.thaw_ranges[a])


def check_v3(j, n, originals):
    """The rules of §17.3.2 that need the EP: every ordinal is in [0, N], an
    attribute is covered to N before the next one starts, `revoked` follows
    full coverage of all four, and every `revoke.intent` names the EP
    manifest's `originals[attr]`."""
    started = [a for a in REVOKE_ORDER if j.ranges[a]]
    need(all(r[1] <= n for a in ATTRS for r in j.ranges[a] + j.thaw_ranges[a])
         and all(j.ranges[a][-1][1] == n for a in started[:-1])
         and (not j.revoked or len(started) == len(ATTRS) and j.ranges[started[-1]][-1][1] == n)
         and all(originals.get(a) == o for a, o in j.revoked_attrs.items()),
         Code.PROVENANCE_INSUFFICIENT, "journal_mismatch")


def walk(res, pre, head, cid, sctx):
    """§3.2/§8 lineage. Raises Mismatch(journal_mismatch) on any gap, broken
    `prev`, foreign header, unknown kind, record after `end`, second
    `revoked` or partial (non-canonical) record. The schema of `seq 0` is the
    cycle's: every record has it, and a `journal/v3` journal is also held to
    its grammar."""
    need(is_hex64(head), Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")
    recs, prev, early, g = [], oc.ZERO64, None, None
    for seq in range(MAX_RECORDS):
        data = res.journal(cid, seq)
        if data is None:
            break
        rec, d = parse(data, Code.PROVENANCE_INSUFFICIENT, "journal_mismatch")
        check_header(pre, rec.get("header"), cid, sctx)
        if seq == 0:
            need(type(rec.get("schema")) is str and rec["schema"] in accepted_journals(pre.mode),
                 Code.PROVENANCE_INSUFFICIENT, "journal_mismatch")
            g = _Grammar(pre.root_id) if rec["schema"] == J_SCHEMA_V3 else None
        if g is None:
            need(_record_ok(rec, seq, prev, early), Code.PROVENANCE_INSUFFICIENT, "journal_mismatch")
            need(not recs or recs[-1]["kind"] != "end", Code.PROVENANCE_INSUFFICIENT, "journal_mismatch")
            need(rec["kind"] != "revoked" or all(r["kind"] != "revoked" for r in recs), Code.PROVENANCE_INSUFFICIENT,
                 "journal_mismatch")
        else:
            need(_record_ok_v3(rec, seq, prev, early) and g.ok(rec, d), Code.PROVENANCE_INSUFFICIENT,
                 "journal_mismatch")
        early = rec.get("early", early)
        recs.append(rec)
        if d == head:
            return View(recs, d) if g is None else View3(g, recs, d)
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
    cycle (the move and each revoked attribute) has its own `thaw.done`. In
    v3 the end is `end{thawed}`, every attribute's thaw ranges are its revoke
    ranges and done, and the move and its reversal both exist; the caller
    proves the head final."""
    if j.v3:
        return j.end and j.end_rec["outcome"] == "thawed" and j.moved and j.thaw_moved \
            and all(_thawed(j, a) for a in ATTRS)
    committed = set(j.revoked_attrs) | ({"move"} if j.moved else set())
    return j.end and committed <= j.thawed

"""The PR-3b preservation and recovery provider (PR-3b contract R2 as amended
by R3, R4 and R4.1). Installed by wiring as `world.o5.preservation`; PR-3a
calls it through `freeze.check`, `freeze.basis_for`, `barrier.handoff_allowed`,
`freeze.release_check` and `invocation._f_complete`, and accepts a result only
through its own guard and bindings.

Pure decision logic. It reads only what PR-3a selected (by digest) or what a
selected, re-hashed object names, through a read-only resolver; it never
lists a store, never writes, never selects records and never creates
authority. Unknown or incomplete evidence refuses. Protected mode refuses
before anything else."""

import re

import recovery_state as rs

from . import barrier, canonical, evidence, freeze, gitclass, journal, libview, o5contract as oc, protocol
from .journal import Mismatch, hex64, need, path_hex
from .refusal import Code, Refused

M_SCHEMA = "codegraph.o5.manifest/v2"
MAX_INDEX = 512
MAX_INDEX_CHUNKS = 512
MAX_ENTRIES = 4096
CHUNK_BYTES = journal.MAX_OBJECT
MAX_CHUNKS = 128
INV_DOMAIN = "codegraph.o5.inventory/v1"
TCB_UIDS = (0,)
_CHANNEL = re.compile(r"^[a-z][a-z0-9_]{0,63}\Z")
_PROTECTED = "barrier_unavailable(protected_mode_unsupported)"
_RESUME_INTENT = {"D-THAW-R": ("THAW_INTENT", ("D-THAW", "D-THAW-R")),
                  "D-SNAP-R": ("SNAPSHOT_INTENT", ("D-SNAP", "D-SNAP-R")),
                  "D-VERIFY-R": ("VERIFY_INTENT", ("D-VERIFY", "D-VERIFY-R"))}
_FAILURE_ROWS = (("freeze-start", "NONE"), ("freeze-start", "FREEZE_BLOCKED"), ("freeze-continue", "FREEZE_INCOMPLETE"),
                 ("freeze-continue", "FREEZE_BLOCKED"), ("thaw", "FREEZE_BLOCKED"), ("snapshot", "SNAPSHOT_FAILED"),
                 ("verify-preservation", "VERIFY_FAILED"))


# ------------------------------------------------------------------ resolver (R4 §3.2, R4.1 §4)

class WorldResolver:
    """Read-only, exact-key reader over the in-process O5 stores, the burned
    documents, the verified ledger, the library, Tier C and the F WAL. It
    returns raw bytes or parsed PR-1 views; PR-3b re-hashes and binds every
    value itself. Synthetic mode only: protected mode refuses on every read
    (PR-4 supplies the protected resolver under its own review). It holds no
    CommitTxn and has no write path."""

    def __init__(self, world):
        self._w = world

    def _store(self, name):
        oc.require_synthetic(oc.mode(self._w))
        return self._w.o5.stores[name]

    def journal(self, cid, seq):
        if not hex64(cid) or type(seq) is not int or seq < 0:
            return None
        return self._store("journals").get(f"journal/{cid}/{seq:08d}")

    def artifact(self, cid, d):
        if not hex64(cid) or not hex64(d):
            return None
        return self._store("artifacts").get(f"artifact/{cid}/{d}")

    def blob(self, d):
        return self._store("artifacts").get(f"blob/{d}") if hex64(d) else None

    def barrier(self, bid, seq):
        if not hex64(bid) or type(seq) is not int or seq < 0:
            return None
        return self._store("barriers").get(f"barrier/{bid}/{seq:010d}")

    def evidence(self, d):
        return evidence.load(self._store("evidence"), d) if hex64(d) else None

    def burned_doc(self, d):
        oc.require_synthetic(oc.mode(self._w))
        if not hex64(d) or d not in {e["auth_digest"] for e in self._w.ledger.entries()}:
            return None
        data = self._w.authz.get(d)
        if data is None or canonical.sha256(data) != d:
            return None
        try:
            return protocol.parse_doc(data)
        except Refused:
            return None

    def library(self, slot, op_id):
        oc.require_synthetic(oc.mode(self._w))
        return self._w.library.read(slot, op_id)

    def tierc(self, slot):
        oc.require_synthetic(oc.mode(self._w))
        return self._w.tierc.read(slot)

    def fwal(self, slot):
        oc.require_synthetic(oc.mode(self._w))
        return self._w.fwal.log(slot)


# ------------------------------------------------------------------ helpers

def _cell(doc):
    o5 = doc.get("o5") if type(doc) is dict else None
    dec = o5.get("decision") if type(o5) is dict else None
    return dec.get("cell") if type(dec) is dict else None


def _digest_of(selection, kind):
    for k, d in selection.get("evidence", []) + selection.get("provider_inputs", []):
        if k == kind:
            return d
    return None


def _inputs(selection, kinds):
    return {k: _digest_of(selection, k) for k in kinds}


def _mode(pre):
    need(type(pre.mode) is str and pre.mode == oc.SYNTHETIC, Code.QUIESCENCE_NOT_ESTABLISHED, _PROTECTED)


def _bb(detail):
    return "barrier_binding" if detail == "cycle_binding" else detail


def decision_cell(pre):
    """The decision cell of PR-3a's own total table (R4 §3.1: no second list)."""
    spec = freeze.cell_spec(pre.verb, pre.row, pre.f, False)
    return spec.decision if spec is not None else None


class Assessment:
    """Q1–Q5 recomputed from a manifest's entries and the captured bytes."""
    __slots__ = ("q1", "q2", "q3", "q4", "q5", "reasons", "head", "top")


# ------------------------------------------------------------------ inventory items (R4 §5.3, R4.1 §3.4, N-R41-5)

def _chunk_obj(header, category, manifest, n, members):
    return {"header": header, "domain": INV_DOMAIN, "category": category, "manifest": manifest, "chunk": n,
            "members": members}


def chunks_of(header, category, manifest, members, limit):
    """Members in canonical (sorted) order; a chunk grows while its canonical
    encoding with the verified manifest's header stays <= CHUNK_BYTES and it
    has <= MAX_ENTRIES members. Members are hex paths or item ids, so each
    adds its length + 2 quotes (+ 1 comma). None when more than `limit`
    chunks are needed or one member cannot fit."""
    out, cur, size = [], [], 0
    for m in sorted(members):
        add = len(m) + 2
        if cur and (len(cur) == MAX_ENTRIES or size + add + 1 > CHUNK_BYTES):
            out.append(cur)
            cur = []
        if not cur:
            if len(out) >= limit:
                return None
            size = len(canonical.canon(_chunk_obj(header, category, manifest, len(out), [])))
            if size + add > CHUNK_BYTES:
                return None
            cur, size = [m], size + add
        else:
            cur.append(m)
            size += add + 1
    if cur:
        out.append(cur)
    for n, c in enumerate(out):
        if len(canonical.canon(_chunk_obj(header, category, manifest, n, c))) > CHUNK_BYTES:
            return None
    return out


def item_id(category, manifest, n, members):
    """The id preimage excludes the header (N-R41-5): anyone can recompute it
    from the verified manifest digest and the member list."""
    return "i" + canonical.digest({"domain": INV_DOMAIN, "category": category, "manifest": manifest, "chunk": n,
                                   "members": members})[:63]


def inventory_items(manifest, header, members_by_category):
    """(inventory, preserved) for the preservation document, or (None, None)
    past the 128-chunk cap (`preservation_incomplete(inventory_size)`)."""
    inv, preserved, total = {}, {}, 0
    for c in rs.INVENTORY:
        members = members_by_category.get(c, [])
        if any(type(m) is not str or not re.match(r"^[0-9a-fi][0-9a-f]*\Z", m) for m in members):
            return None, None
        chunks = chunks_of(header, c, manifest, members, MAX_CHUNKS - total)
        if chunks is None:
            return None, None
        total += len(chunks)
        inv[c] = []
        for n, members_n in enumerate(chunks):
            i = item_id(c, manifest, n, members_n)
            inv[c].append(i)
            preserved[i] = {"verified": True, "to": "raw:" + manifest}
    return inv, preserved


# ------------------------------------------------------------------ one cycle's artifacts (R4.1 §3)

class _Cycle:
    """Reads this cycle's effect-produced objects by exact key, re-hashed,
    strictly parsed, size-bounded and header-bound to `pre`."""

    def __init__(self, r, pre, cid, sctx):
        self.r, self.pre, self.cid, self.sctx = r, pre, cid, sctx

    def obj(self, d, keys=None, code=Code.PROVENANCE_INSUFFICIENT, detail="evidence_unavailable"):
        need(hex64(d), code, detail)
        data = self.r.artifact(self.cid, d)
        need(data is not None, code, detail)
        o, h = journal.parse(data, code, detail)
        need(h == d, Code.STATE_UNKNOWN, "artifact_mismatch")
        journal.check_header(self.pre, o.get("header"), self.cid, self.sctx)
        if keys is not None:
            need(set(o) == set(keys), Code.STATE_UNKNOWN, "artifact_mismatch")
        return o

    def journal(self, head):
        return journal.walk(self.r, self.pre, head, self.cid, self.sctx)

    def entries(self, m):
        """R4.1 §3.4 reconstruction: ordered index objects, ordered chunks,
        strictly increasing paths (no duplicate, overlap or conflict), every
        bound respected. Any gap refuses; an incomplete list is never complete."""
        idx = m.get("entry_index")
        need(type(idx) is list and len(idx) <= MAX_INDEX, Code.STATE_UNKNOWN, "artifact_mismatch")
        out, n, files = [], 0, set()
        for i, d in enumerate(idx):
            io = self.obj(d, ("header", "kind", "n", "chunks"))
            ch = io["chunks"]
            need(io["kind"] == "entry_index" and io["n"] == i and type(ch) is list
                 and 1 <= len(ch) <= MAX_INDEX_CHUNKS, Code.STATE_UNKNOWN, "artifact_mismatch")
            for cd in ch:
                co = self.obj(cd, ("header", "kind", "n", "entries"))
                es = co["entries"]
                need(co["kind"] == "entries" and co["n"] == n and type(es) is list and 1 <= len(es) <= MAX_ENTRIES,
                     Code.STATE_UNKNOWN, "artifact_mismatch")
                for e in es:
                    need(type(e) is list and len(e) == 2 and path_hex(e[0]) and hex64(e[1])
                         and (not out or e[0] > out[-1][0]), Code.STATE_UNKNOWN, "artifact_mismatch")
                    p = bytes.fromhex(e[0])
                    need(not any(p[:i] in files for i, c in enumerate(p) if c == 0x2F), Code.STATE_UNKNOWN,
                         "artifact_mismatch")
                    files.add(p)
                    out.append((e[0], e[1]))
                n += 1
        return out

    def manifest(self, d, phase):
        keys = {"schema", "header", "phase", "entry_index", "result", "reasons"} | (
            {"originals"} if phase == "early" else set())
        m = self.obj(d)
        need(keys <= set(m) <= keys | {"questions", "head_resolved"} and m["schema"] == M_SCHEMA
             and m["phase"] == phase and m["result"] in ("COMPLETE", "INCOMPLETE") and type(m["reasons"]) is list
             and all(type(x) is str for x in m["reasons"]) and (m["result"] == "INCOMPLETE") == bool(m["reasons"]),
             Code.STATE_UNKNOWN, "artifact_mismatch")
        if phase == "early":
            o = m["originals"]
            need(type(o) is dict and all(k in journal.ATTRS and hex64(v) for k, v in o.items()), Code.STATE_UNKNOWN,
                 "artifact_mismatch")
        return m

    def binding(self, d, phase, verify=False):
        """(binding, manifest, entries, blob set) of an `early`, `final` or
        `verify` binding object of this cycle (§3.2)."""
        keys = ("header", "cycle_id", "manifest", "blobs") + (("reverified", "sealed_at") if verify else ())
        b = self.obj(d, keys)
        need(b["cycle_id"] == self.cid, Code.STATE_UNKNOWN, "cycle_binding")
        if verify:
            need(b["reverified"] is True and type(b["sealed_at"]) is int, Code.PROVENANCE_INSUFFICIENT,
                 "evidence_unavailable")
        m = self.manifest(b["manifest"], phase)
        es = self.entries(m)
        bs = self.obj(b["blobs"], ("header", "set"))
        want = {x for _, x in es} | (set(m["originals"].values()) if phase == "early" else set())
        need(bs["set"] == sorted(want), Code.STATE_UNKNOWN, "artifact_mismatch")
        return b, m, es, bs["set"]

    def blobs(self, digests):
        """Every digest resolves and re-hashes; returns {digest: bytes} or None."""
        out = {}
        for d in digests:
            data = self.r.blob(d)
            if type(data) is not bytes or canonical.sha256(data) != d:
                return None
            out[d] = data
        return out

    def assess(self, m, es, full):
        """Q1 from the raw capture (stored INCOMPLETE is trusted only to refuse;
        COMPLETE needs every needed blob); Q2–Q5 from the captured Git bytes."""
        a = Assessment()
        git = [(bytes.fromhex(p), b) for p, b in es if gitclass.is_git_path(bytes.fromhex(p))]
        got = self.blobs(sorted({b for _, b in es}) if full else sorted({b for _, b in git}))
        a.q1 = "COMPLETE" if m["result"] == "COMPLETE" and got is not None else "INCOMPLETE"
        files = {p: got[b] for p, b in git} if got is not None else {}
        repos = gitclass.repositories(files)
        a.q2, a.q3, a.q4, a.q5, a.reasons, a.head = gitclass.questions(repos, a.q1)
        a.top = repos.get(b"")
        if "head_resolved" in m:
            need(m["head_resolved"] == a.head, Code.STATE_UNKNOWN, "artifact_mismatch")
        return a

    def reversible(self, m, blobset):
        o = m["originals"]
        return set(o) == set(journal.ATTRS) and all(v in blobset for v in o.values()) \
            and self.blobs(sorted(o.values())) is not None


def _checks(data, keys):
    return type(data) is dict and all(k in data for k in keys)


def _holders_ok(data):
    ex, hs = (data.get("uids_excluded"), data.get("holders")) if type(data) is dict else (None, None)
    return type(ex) is list and all(type(u) is int and u in TCB_UIDS for u in ex) and type(hs) is list \
        and all(type(x) is list and len(x) == 4 and all(type(v) is int for v in x) and x[0] in ex for x in hs)


def _protected_entry(e):
    """uid, mode, acl entries, nlink of a protected object: root-owned, no
    write bit, no ACL grant, a single link."""
    return type(e) is list and len(e) >= 4 and all(type(v) is int for v in e[-4:]) \
        and e[-4] == 0 and e[-3] & 0o222 == 0 and e[-2] == 0 and e[-1] == 1


def _claimed_separated(d):
    """E-1 and the per-entry part of E-2 / V-3 over `claimed_set_observation`."""
    if not _checks(d, ("parent", "ancestors_root_owned", "entries", "linked")):
        return False
    return d["parent"] == [0, 0o700, 0] and d["ancestors_root_owned"] is True and type(d["entries"]) is list \
        and all(type(e) is list and len(e) == 5 and path_hex(e[0]) and _protected_entry(e) for e in d["entries"])


def _isolated(d, required):
    """E-4 / V-4 over `artifact_isolation`: every protected object isolated, and
    the bound artifacts among them."""
    objs = d.get("objects") if type(d) is dict else None
    return type(objs) is list and bool(objs) and all(
        type(o) is list and len(o) == 6 and hex64(o[0]) and o[5] is True and _protected_entry(o[:5]) for o in objs) \
        and set(required) <= {o[0] for o in objs}


# ------------------------------------------------------------------ the provider

class Preservation:
    """`Wiring.preservation`. Constructed with a read-only resolver."""

    def __init__(self, resolver):
        self._r = resolver

    # ---- shared

    def _ev(self, inputs, kind, pre, decisive=True):
        o = self._r.evidence(inputs.get(kind))
        need(o is not None, Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")
        need(o.get("mode") == pre.mode and o.get("kind") == kind, Code.STATE_UNKNOWN, "barrier_binding")
        need(not decisive or o.get("role") == "decisive", Code.PROVENANCE_INSUFFICIENT, "evidence_not_decisive")
        need(type(o.get("data")) is dict, Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")
        return o

    def _ids(self, pre):
        """(cycle_id, start_ctx): the cycle identity re-derived from the burned
        freeze-start document PR-3a named, and that document's `o5.ctx`."""
        need(hex64(pre.cycle_id) and hex64(pre.freeze_start_auth)
             and pre.cycle_id == oc.cycle_id(pre.slot, pre.g_pre, pre.op_id, pre.freeze_start_auth),
             Code.STATE_UNKNOWN, "barrier_binding")
        doc = self._r.burned_doc(pre.freeze_start_auth)
        need(doc is not None and doc.get("verb") == "freeze-start" and doc.get("slot") == pre.slot
             and doc.get("op_id") == pre.op_id and doc.get("slot_generation") == pre.g_pre
             and type(doc.get("o5")) is dict and hex64(doc["o5"].get("ctx")), Code.STATE_UNKNOWN, "barrier_binding")
        return pre.cycle_id, doc["o5"]["ctx"]

    def _cycle(self, pre):
        cid, sctx = self._ids(pre)
        return _Cycle(self._r, pre, cid, sctx)

    def _ep(self, cy, j, ep_d, full=None):
        """The EP binding of this cycle, consistent with the journal's `early`;
        assessed (Git bytes only, or every blob) when `full` is not None."""
        need(j.early == ep_d, Code.STATE_UNKNOWN, "barrier_binding")
        b, m, es, bs = cy.binding(ep_d, "early")
        return m, es, bs, cy.assess(m, es, full) if full is not None else None

    def _library(self, pre, d, states):
        try:
            view = self._r.library(pre.slot, pre.op_id)
        except Refused:
            view = None
        need(view is not None and hex64(d) and canonical.digest(view.rec) == d, Code.PROVENANCE_INSUFFICIENT,
             "evidence_unavailable")
        need(view.rec.get("state") in states and view.rec.get("slot_generation") == pre.g_pre + 1,
             Code.VERB_NOT_ALLOWED_IN_STATE, "library_state")
        return view

    def _pending(self, pre, d):
        try:
            view = self._r.library(pre.slot, pre.op_id)
        except Refused:
            view = None
        p = view.pending if view is not None else None
        need(p is not None and hex64(d) and canonical.digest({k: p[k] for k in rs.ENTRY_KEYS if k in p}) == d,
             Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")
        return view

    def _barrier_record(self, pre, d):
        """R4.1 §4: `d` is the digest the current selection names (callers pass
        `_inputs(selection, ...)`), and only a record of the holder chain, read
        by exact key, matches it."""
        need(hex64(d), Code.QUIESCENCE_NOT_ESTABLISHED, "enforcement_record_unselected")
        for seq in range(4096):
            data = self._r.barrier(pre.holder_chain, seq)
            if data is None:
                break
            if type(data) is bytes and canonical.sha256(data) == d:
                try:
                    return protocol.parse_doc(data)
                except Refused:
                    break
        raise Mismatch(Code.QUIESCENCE_NOT_ESTABLISHED, "enforcement_record_unavailable")

    # ---- decide (R4 §8.1, R4.1 §7)

    def decide(self, ctx, selection, view):
        pre = ctx.pre
        cell = decision_cell(pre)
        inputs = _inputs(selection, oc.DECISION_CELLS[cell][2]) if cell in oc.DECISION_CELLS else {}
        try:
            _mode(pre)
            need(cell in oc.DECISION_CELLS, Code.PROVENANCE_INSUFFICIENT, "no_decision_cell")
            basis = oc.DECISION_CELLS[cell][1]
            need(ctx.basis == basis or basis == "barrier_or_handoff" and ctx.basis in ("barrier", "handoff"),
                 Code.STATE_UNKNOWN, "barrier_binding")
            need(all(hex64(v) for v in inputs.values()), Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")
            if ctx.basis == "handoff":
                v = ctx.verdict
                need(v is not None and v.cell == "H-VAL" and v.phase == "validate" and v.passed is True,
                     Code.QUIESCENCE_NOT_ESTABLISHED, "handoff_invalid")
            fields = getattr(self, "_" + cell.replace("-", "_"))(pre, inputs, selection)
            return oc.PreservationDecision(oc._PSEAL, cell=cell, ctx=ctx.digest, inputs=inputs, result="PROCEED",
                                           code=None, detail=None, doc_fields=fields)
        except (Mismatch, Refused) as e:
            return oc.PreservationDecision(oc._PSEAL, cell=cell, ctx=ctx.digest, inputs=inputs, result="REFUSE",
                                           code=e.code, detail=_bb(e.detail), doc_fields={})

    def _resume_intent(self, cell, inputs, pre):
        intent, cells = _RESUME_INTENT[cell]
        e = next((x for x in self._r.fwal(pre.slot) if canonical.digest(x) == inputs["f_intent"]), None)
        need(e is not None and e.get("kind") == "intent" and e.get("value") == intent and e.get("op_id") == pre.op_id,
             Code.STATE_UNKNOWN, "barrier_binding")
        doc = self._r.burned_doc(e.get("auth_digest"))
        need(doc is not None and _cell(doc) in cells and doc.get("slot") == pre.slot and doc.get("op_id") == pre.op_id
             and (doc.get("o5") or {}).get("cycle_id") == pre.cycle_id, Code.STATE_UNKNOWN, "original_mismatch")

    def _D_THAW(self, pre, inputs, selection):
        need(pre.stop is False, Code.STOPPED, None)
        cy = self._cycle(pre)
        try:
            j = cy.journal(inputs["journal_head"])
        except Mismatch as e:
            if e.detail == "journal_mismatch":
                raise Mismatch(Code.PROVENANCE_INSUFFICIENT, "thaw_not_safe(journal_mismatch)") from None
            raise
        need(j.early == inputs["ep_manifest"] == inputs["original_blobs"], Code.STATE_UNKNOWN, "barrier_binding")
        _, m, es, bs = cy.binding(inputs["ep_manifest"], "early")
        originals = m["originals"]
        need(all(originals.get(a) == o for a, o in j.revoked_attrs.items()), Code.PROVENANCE_INSUFFICIENT,
             "thaw_not_safe(journal_mismatch)")
        need(set(originals.values()) <= set(bs) and cy.blobs(bs) is not None, Code.PROVENANCE_INSUFFICIENT,
             "thaw_not_safe(data_unavailable)")
        return {}

    def _D_THAW_R(self, pre, inputs, selection):
        self._resume_intent("D-THAW-R", inputs, pre)
        return self._D_THAW(pre, inputs, selection)

    def _D_QUAR(self, pre, inputs, selection):
        cy = self._cycle(pre)
        j = cy.journal(inputs["journal_head"])
        journal.require_revoked(j)
        m, es, bs, a = self._ep(cy, j, inputs["ep_manifest"], True)
        journal.require_no_escaped(j, {p for p, _ in es})
        rev = cy.reversible(m, bs)
        if not (rev and a.q4 == "COMPLETE" and a.q5 == "ELIGIBLE"):
            why = sorted(set(a.reasons) | ({"irreversible"} if not rev else set()) | (
                {"raw"} if a.q1 != "COMPLETE" else set()) | ({"head"} if a.head is None else set()))
            raise Mismatch(Code.PROVENANCE_INSUFFICIENT, f"not_preservable({','.join(why)})")
        return {}

    def _D_QUIES(self, pre, inputs, selection):
        cy = self._cycle(pre)
        j = cy.journal(inputs["journal_head"])
        need(j.recs[-1]["kind"] == "revoked", Code.PROVENANCE_INSUFFICIENT, "journal_mismatch")
        return {}

    def _snap_common(self, pre, inputs):
        cy = self._cycle(pre)
        j = cy.journal(inputs["journal_head"])
        journal.require_revoked(j)
        _, es, bs, _ = self._ep(cy, j, inputs["ep_manifest"])
        journal.require_no_escaped(j, {p for p, _ in es})
        need(cy.blobs(bs) is not None, Code.STATE_UNKNOWN, "artifact_mismatch")
        return cy, j, es

    def _D_SNAP(self, pre, inputs, selection):
        self._snap_common(pre, inputs)
        src = self._ev(inputs, "source_identity", pre)
        need(src["data"].get("root_id") == pre.root_id, Code.STATE_UNKNOWN, "barrier_binding")
        return {}

    def _D_SNAP_R(self, pre, inputs, selection):
        self._resume_intent("D-SNAP-R", inputs, pre)
        return self._D_SNAP(pre, inputs, selection)

    def _D_VERIFY(self, pre, inputs, selection):
        cy, _, _ = self._snap_common(pre, inputs)
        cy.binding(inputs["fs_manifest"], "final")
        return {}

    def _D_VERIFY_R(self, pre, inputs, selection):
        self._resume_intent("D-VERIFY-R", inputs, pre)
        return self._D_VERIFY(pre, inputs, selection)

    def _pres_fields(self, pre, cy, ep_d, v_d):
        _, em, es_ep, _ = cy.binding(ep_d, "early")
        vb, vm, ves, _ = cy.binding(v_d, "final", verify=True)
        ep_head = cy.assess(em, es_ep, False).head
        a = cy.assess(vm, ves, True)
        need(a.q4 == "COMPLETE", Code.PROVENANCE_INSUFFICIENT, "preservation_incomplete(raw)")
        need(a.q5 == "ELIGIBLE", Code.PROVENANCE_INSUFFICIENT, "preservation_incomplete(recovery)")
        need(a.head is not None and a.head == ep_head, Code.PROVENANCE_INSUFFICIENT, "preservation_incomplete(head)")
        members = {"dirty": [p for p, _ in ves if not gitclass.is_git_path(bytes.fromhex(p))],
                   "untracked": [], "ignored": [], "unpublished_commits": gitclass.unpublished_commit_items(a.top)}
        inv, preserved = inventory_items(vb["manifest"], vm["header"], members)
        need(inv is not None, Code.PROVENANCE_INSUFFICIENT, "preservation_incomplete(inventory_size)")
        return {"head": a.head, "complete": True, "inventory": inv, "preserved": preserved,
                "artifact": {"manifest": vb["manifest"], "blobs": vb["blobs"]}}

    def _D_PRES(self, pre, inputs, selection):
        need(pre.f == "VERIFIED", Code.VERB_NOT_ALLOWED_IN_STATE, "f")
        cy, _, _ = self._snap_common(pre, inputs)
        return self._pres_fields(pre, cy, inputs["ep_manifest"], inputs["fs_manifest_verified"])

    def _original(self, pre, inputs, verb, cell):
        view = self._pending(pre, inputs["library_pending"])
        need(view.pending.get("evidence") == inputs["original_doc"], Code.STATE_UNKNOWN, "original_mismatch")
        doc = self._r.burned_doc(inputs["original_doc"])
        need(doc is not None and doc.get("verb") == verb and _cell(doc) == cell, Code.STATE_UNKNOWN,
             "original_mismatch")
        need((doc.get("slot"), doc.get("op_id"), doc.get("slot_generation")) == (pre.slot, pre.op_id, pre.g_pre + 1)
             and (doc.get("o5") or {}).get("cycle_id") == pre.cycle_id, Code.STATE_UNKNOWN, "barrier_binding")
        return doc, view

    def _D_PRES_R(self, pre, inputs, selection):
        doc, _ = self._original(pre, inputs, "preservation", "D-PRES")
        ep = _digest_of(selection, "ep_manifest")
        need(hex64(ep), Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")
        cy = self._cycle(pre)
        now = self._pres_fields(pre, cy, ep, inputs["fs_manifest_verified"])
        need(all(doc.get(k) == v for k, v in now.items()), Code.STATE_UNKNOWN, "original_mismatch")
        return {}

    def _ready_check(self, pre, inputs, rec, cy):
        """R1, R3–R7 (§8.2); R2 is the handoff verdict PR-3a and `decide` checked."""
        pdoc = self._r.burned_doc(rec.get("gates", {}).get("preservation"))
        need(pdoc is not None and pdoc.get("verb") == "preservation" and _cell(pdoc) == "D-PRES",
             Code.STATE_UNKNOWN, "preservation_binding")
        try:
            vb, vm, ves, _ = cy.binding(inputs["fs_manifest_verified"], "final", verify=True)
        except Mismatch as e:
            if e.detail in ("cycle_binding", "barrier_binding"):
                raise
            raise Mismatch(Code.STATE_UNKNOWN, "artifact_mismatch") from None
        need(cy.blobs(sorted({b for _, b in ves})) is not None, Code.STATE_UNKNOWN, "artifact_mismatch")
        need(pdoc.get("artifact") == {"manifest": vb["manifest"], "blobs": vb["blobs"]}, Code.STATE_UNKNOWN,
             "preservation_binding")
        obs = self._ev(inputs, "final_observation", pre)
        d = obs["data"]
        need(d.get("root_id") == pre.root_id, Code.STATE_UNKNOWN, "barrier_binding")
        need(type(obs.get("observed_at")) is int and obs["observed_at"] >= vb["sealed_at"],
             Code.PROVENANCE_INSUFFICIENT, "evidence_stale")
        need(d.get("projection") == canonical.digest({"entries": [list(e) for e in ves]}), Code.NOT_CLEAN, "mutated")
        head = cy.assess(vm, ves, False).head
        need(head is not None and d.get("head_resolved") == head == rec.get("preserved_head"), Code.NOT_CLEAN, "head")
        text = self._r.tierc(pre.slot)
        need(text is not None and canonical.sha256(text.encode()) == inputs["tierc"], Code.NOT_CLEAN, "lease")
        comp = libview.component(text, pre.slot)
        need(comp is not None and comp.kind == libview.QUARANTINED and comp.g == pre.g_pre + 1
             and comp.op == pre.op_id, Code.NOT_CLEAN, "lease")
        return {"clean": True, "lease_absent": True, "head": rec["preserved_head"],
                "preservation": rec["gates"]["preservation"]}

    def _D_READY(self, pre, inputs, selection):
        need(pre.f == "VERIFIED", Code.VERB_NOT_ALLOWED_IN_STATE, "f")
        view = self._library(pre, inputs["library_record"], (rs.PRESERVED,))
        f = self._ready_check(pre, inputs, view.rec, self._cycle(pre))
        return {"clean": f["clean"], "lease_absent": f["lease_absent"], "head": f["head"]}

    def _D_READY_R(self, pre, inputs, selection):
        doc, view = self._original(pre, inputs, "ready", "D-READY")
        need(view.rec.get("state") == rs.PRESERVED and view.rec.get("slot_generation") == pre.g_pre + 1,
             Code.VERB_NOT_ALLOWED_IN_STATE, "library_state")
        f = self._ready_check(pre, inputs, view.rec, self._cycle(pre))
        need(all(doc.get(k) == f[k] for k in ("clean", "lease_absent", "head", "preservation")), Code.STATE_UNKNOWN,
             "original_mismatch")
        return {}

    def _D_READY_T(self, pre, inputs, selection):
        view = self._library(pre, inputs["library_record"], (rs.READY,))
        need(view.marker is None, Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")
        need(view.rec.get("gates", {}).get("ready") == inputs["ready_doc"], Code.STATE_UNKNOWN, "original_mismatch")
        doc = self._r.burned_doc(inputs["ready_doc"])
        need(doc is not None and doc.get("verb") == "ready" and _cell(doc) == "D-READY", Code.STATE_UNKNOWN,
             "original_mismatch")
        need((doc.get("o5") or {}).get("cycle_id") == pre.cycle_id and doc.get("slot") == pre.slot
             and doc.get("op_id") == pre.op_id, Code.STATE_UNKNOWN, "barrier_binding")
        f = self._ready_check(pre, inputs, view.rec, self._cycle(pre))
        need(all(doc.get(k) == f[k] for k in ("clean", "lease_absent", "head", "preservation")), Code.STATE_UNKNOWN,
             "original_mismatch")
        return {}

    # ---- handoff verdicts (R4 §6, R4.1 §5)

    def _verdict(self, cell, phase, pre, inputs, passed, reasons):
        return oc.HandoffVerdict(oc._PSEAL, cell=cell, phase=phase, pre=pre.digest, slot=pre.slot, op_id=pre.op_id,
                                 cycle_id=pre.cycle_id, g_pre=pre.g_pre, inputs=inputs, passed=passed,
                                 reasons=reasons)

    def _e5(self, pre, selection, inputs):
        """R4.1 §5.2 E-5 with N-R41-9 typed neutralization: channel names come
        only from `detail.neutralized`, setuid paths only from
        `detail.neutralized_setuid`; neither can satisfy the other."""
        h1, h2 = self._ev(inputs, "holder_scan_1", pre), self._ev(inputs, "holder_scan_2", pre)
        need(type(h1.get("observed_at")) is int and type(h2.get("observed_at")) is int
             and abs(h2["observed_at"] - h1["observed_at"]) >= 2, Code.QUIESCENCE_NOT_ESTABLISHED, "holder_interval")
        need(_holders_ok(h1["data"]) and _holders_ok(h2["data"]), Code.QUIESCENCE_NOT_ESTABLISHED, "holder")
        ups = self._ev(inputs, "uid_process_scan", pre)["data"]
        need(ups.get("processes") == [] and type(ups.get("uids")) is list and ups["uids"]
             and all(type(u) is int for u in ups["uids"]), Code.QUIESCENCE_NOT_ESTABLISHED, "uid_process")
        rec = self._barrier_record(pre, inputs["enforcement_record"])
        need(rec.get("kind") == "established" and rec.get("barrier_id") == pre.barrier_id == pre.holder_chain,
             Code.STATE_UNKNOWN, "barrier_binding")
        det = rec.get("detail") if type(rec.get("detail")) is dict else {}
        chans, setuid = det.get("neutralized"), det.get("neutralized_setuid")
        need(type(chans) is list and all(type(x) is str and _CHANNEL.match(x) and not hex64(x) for x in chans)
             and type(setuid) is list and all(hex64(x) for x in setuid), Code.QUIESCENCE_NOT_ESTABLISHED,
             "enforcement_record_unavailable")
        need(pre.scope in ("SLOT_ISOLATED", "POOL_MAINTENANCE"), Code.STATE_UNKNOWN, "barrier_binding")
        ch = self._ev(inputs, "agent_channels", pre)["data"].get("channels")
        need(type(ch) is dict and bool(ch) and all(s == "neutralized" and n in chans for n, s in ch.items()),
             Code.QUIESCENCE_NOT_ESTABLISHED, "enforcement_incomplete(channel)")
        files = self._ev(inputs, "setuid_writer_scan", pre)["data"].get("files")
        need(type(files) is list and all(type(f) is list and len(f) == 3 and hex64(f[0]) and type(f[1]) is int
                                         and f[2] is True and f[0] in setuid for f in files),
             Code.QUIESCENCE_NOT_ESTABLISHED, "enforcement_incomplete(setuid)")
        eo = self._r.evidence(_digest_of(selection, "enforcement_observation"))
        need(eo is not None and eo.get("mode") == pre.mode and type(eo.get("data")) is dict
             and type(det.get("enforcement")) is str and eo["data"].get("digest") == det["enforcement"],
             Code.QUIESCENCE_NOT_ESTABLISHED, "enforcement_incomplete(observation)")
        mnt = self._ev(inputs, "mounts_external", pre)["data"]
        need(hex64(det.get("mounts_baseline")) and mnt.get("baseline") == det["mounts_baseline"]
             and mnt.get("mounts") == det["mounts_baseline"], Code.QUIESCENCE_NOT_ESTABLISHED,
             "enforcement_incomplete(mounts_baseline)")

    def handoff_establish(self, pre, selection, view):
        inputs = _inputs(selection, oc.H_EST_KINDS)
        try:
            _mode(pre)
            need(pre.release_target is None and pre.holder_chain == pre.barrier_id and pre.cycle_id is not None,
                 Code.STATE_UNKNOWN, "barrier_binding")
            need(all(hex64(v) for v in inputs.values()), Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")
            cy = self._cycle(pre)
            self._e5(pre, selection, inputs)
            cso = self._ev(inputs, "claimed_set_observation", pre)["data"]
            need(_claimed_separated(cso), Code.QUIESCENCE_NOT_ESTABLISHED, "claimed_set")
            need(self._ev(inputs, "writer_capabilities", pre)["data"].get("grants") == [],
                 Code.QUIESCENCE_NOT_ESTABLISHED, "writer_capabilities")
            src = self._ev(inputs, "source_identity", pre)
            need(src["data"].get("root_id") == pre.root_id, Code.STATE_UNKNOWN, "barrier_binding")
            j = cy.journal(inputs["journal_head"])
            journal.require_revoked(j)
            _, es, _, _ = self._ep(cy, j, inputs["ep_manifest"])
            journal.require_no_escaped(j, {p for p, _ in es})
            need(type(cso["linked"]) is list and len(cso["linked"]) == 2 and cso["linked"][0] == 0o700
                 and j.linked is not None and cso["linked"][1] == j.linked, Code.QUIESCENCE_NOT_ESTABLISHED, "linked")
            iso = self._ev(inputs, "artifact_isolation", pre)["data"]
            need(_isolated(iso, (inputs["ep_manifest"], inputs["journal_head"])), Code.QUIESCENCE_NOT_ESTABLISHED,
                 "artifact_isolation")
            self._library(pre, inputs["library_record"], (rs.QUIESCENT,))
            return self._verdict("H-EST", "establish", pre, inputs, True, [])
        except (Mismatch, Refused) as e:
            return self._verdict("H-EST", "establish", pre, inputs, False, [_bb(e.detail or e.code.value)])

    def _validate(self, pre, selection, kinds, sup_digest=None):
        """H-VAL V-1..V-7 (R4 §6.3, R4.1 §5.3): protected evidence only; nothing
        an agent can write after an authorized release is consulted."""
        inputs = _inputs(selection, kinds)
        _mode(pre)
        need(all(hex64(v) for v in inputs.values()), Code.PROVENANCE_INSUFFICIENT, "evidence_unavailable")
        cy = self._cycle(pre)
        need(sup_digest is None or inputs["superseded"] == sup_digest, Code.STATE_UNKNOWN, "barrier_binding")
        sup = self._barrier_record(pre, inputs["superseded"])
        d = sup.get("detail") if type(sup.get("detail")) is dict else {}
        need(sup.get("kind") == "superseded" and sup.get("barrier_id") == pre.holder_chain
             and (d.get("slot"), d.get("cycle_id"), d.get("g_pre"), d.get("root_id"))
             == (pre.slot, pre.cycle_id, pre.g_pre, pre.root_id), Code.STATE_UNKNOWN, "barrier_binding")
        hdoc = self._r.burned_doc(sup.get("owner_auth"))
        need(hdoc is not None and hdoc.get("verb") == "containment-handoff" and hdoc.get("cycle_id") == pre.cycle_id,
             Code.STATE_UNKNOWN, "barrier_binding")
        cso = self._ev(inputs, "claimed_set_observation", pre)["data"]
        need(_claimed_separated(cso), Code.QUIESCENCE_NOT_ESTABLISHED, "claimed_set")
        need(_holders_ok(self._ev(inputs, "holder_scan_protected", pre)["data"]), Code.QUIESCENCE_NOT_ESTABLISHED,
             "holder_protected")
        need(self._ev(inputs, "protected_mounts", pre)["data"].get("below") == [], Code.QUIESCENCE_NOT_ESTABLISHED,
             "protected_mounts")
        src = self._ev(inputs, "source_identity", pre)
        need(src["data"].get("root_id") == pre.root_id, Code.STATE_UNKNOWN, "barrier_binding")
        j = cy.journal(inputs["journal_head"])
        journal.require_revoked(j)
        protected = set(self._ep(cy, j, inputs["ep_manifest"])[2])
        bound = [inputs["ep_manifest"], inputs["journal_head"]]
        if "fs_manifest" in kinds:
            protected |= set(cy.binding(inputs["fs_manifest"], "final")[3])
            bound.append(inputs["fs_manifest"])
        if "fs_manifest_verified" in kinds:
            protected |= set(cy.binding(inputs["fs_manifest_verified"], "final", verify=True)[3])
            bound.append(inputs["fs_manifest_verified"])
        # V-2 / R2 §3.2: every protected EP and FS blob (entries and originals) is re-read and re-hashed on every call.
        need(cy.blobs(sorted(protected)) is not None, Code.STATE_UNKNOWN, "artifact_mismatch")
        need(_isolated(self._ev(inputs, "artifact_isolation", pre)["data"], bound), Code.QUIESCENCE_NOT_ESTABLISHED,
             "artifact_isolation")
        lv = self._library(pre, inputs["library_record"], (rs.QUIESCENT, rs.PRESERVED, rs.READY))
        return inputs, lv

    def handoff_validate(self, pre, selection, view):
        kinds = oc.h_val_kinds(pre.f)
        try:
            inputs, _ = self._validate(pre, selection, kinds)
            return self._verdict("H-VAL", "validate", pre, inputs, True, [])
        except (Mismatch, Refused) as e:
            return self._verdict("H-VAL", "validate", pre, _inputs(selection, kinds), False,
                                 [_bb(e.detail or e.code.value)])

    # ---- discharge (R4 §8.4)

    def discharge(self, view, holder, pre, selection):
        cur = getattr(view, "current", None)
        try:
            _mode(pre)
            need(cur is not None and pre.holder_chain == holder.id == cur.barrier_id and pre.cycle_id == cur.cycle_id
                 and pre.g_pre == cur.g_pre and pre.slot == view.slot, Code.STATE_UNKNOWN, "barrier_binding")
            if view.row in ("P13", "P14", "P15", "P16", "P17"):
                need(cur.closure in ("open", "ready_pending"), Code.STATE_UNKNOWN, "closure")
                kind = "D3"
            elif view.row == "P18":
                need(cur.closure == "ready", Code.STATE_UNKNOWN, "closure")
                kind = "D4"
            else:
                return None
            if holder.id != pre.release_target:
                need(holder.state == barrier.RELEASED, Code.STATE_UNKNOWN, "holder")
                kind += "H"
            sup = holder.superseded.get(view.slot)
            need(sup is not None, Code.STATE_UNKNOWN, "superseded")
            inputs, lv = self._validate(pre, selection, oc.discharge_kinds(kind, view.f), sup_digest=sup[1])
            need(inputs["holder_record"] == holder.head, Code.STATE_UNKNOWN, "holder_record")
            if kind in ("D4", "D4H"):
                need(lv.rec.get("state") == rs.READY and lv.marker is not None, Code.STATE_UNKNOWN, "ready")
                rdoc = self._r.burned_doc(inputs["ready_doc"])
                need(rdoc is not None and rdoc.get("verb") == "ready" and _cell(rdoc) == "D-READY"
                     and inputs["ready_doc"] == lv.rec["gates"].get("ready")
                     and rdoc.get("preservation") == inputs["preservation_doc"] == lv.rec["gates"].get("preservation"),
                     Code.STATE_UNKNOWN, "preservation_binding")
            verdict = self._verdict("H-VAL", "validate", pre, inputs, True, [])
            return oc.Discharge(oc._PSEAL, cell=oc.DISCHARGE_CELLS[kind], pre=pre.digest, slot=view.slot,
                                cycle_id=cur.cycle_id, g_pre=cur.g_pre, holder=holder.id, superseded=sup[1],
                                kind=kind, inputs=inputs, verdict=verdict.digest)
        except (Mismatch, Refused):
            return None

    # ---- check_fresult (R4.1 §8, closed)

    def check_fresult(self, ctx, verb, res, cycle_id):
        try:
            self._check_fresult(ctx, verb, res, cycle_id)
        except (Mismatch, Refused) as e:
            raise ValueError(f"fresult_rejected:{e.detail or e.code.value}") from None

    def _check_fresult(self, ctx, verb, res, cycle_id):
        pre = ctx.pre
        _mode(pre)
        need(hex64(cycle_id), Code.STATE_UNKNOWN, "cycle_binding")
        if pre.cycle_id is not None:
            need(cycle_id == pre.cycle_id, Code.STATE_UNKNOWN, "cycle_binding")
            cy = self._cycle(pre)
        else:
            need(verb == "freeze-start", Code.STATE_UNKNOWN, "cycle_binding")
            cy = _Cycle(self._r, pre, cycle_id, ctx.digest)
        b = res.bindings
        need(type(b) is dict, Code.STATE_UNKNOWN, "fresult_unlisted")
        if "reconcile" in b:
            rc = cy.obj(b["reconcile"], ("header", "reconcile"), Code.STATE_UNKNOWN, "artifact_mismatch")
            need(type(rc["reconcile"]) is list, Code.STATE_UNKNOWN, "artifact_mismatch")
        row = (verb, res.value)
        if row == ("freeze-start", "MOVED"):
            need("early" in b and "journal" in b, Code.STATE_UNKNOWN, "journal_required")
            j = cy.journal(b["journal"])
            need(j.moved and not j.revoked, Code.STATE_UNKNOWN, "journal_mismatch")
            m, es, bs, a = self._ep(cy, j, b["early"], True)
            need(cy.reversible(m, bs) and a.q4 == "COMPLETE" and a.q5 == "ELIGIBLE", Code.STATE_UNKNOWN,
                 "not_preservable")
        elif row == ("freeze-continue", "REVOKED"):
            j = cy.journal(b["journal"])
            journal.require_revoked(j)
            need(j.recs[-1]["kind"] == "revoked" and j.early == b["early"], Code.STATE_UNKNOWN, "journal_mismatch")
            need(b.get("escaped") == journal.escaped_digest(j), Code.STATE_UNKNOWN, "escaped")
        elif row == ("thaw", "NONE"):
            j = cy.journal(b["journal"])
            need(journal.thaw_complete(j), Code.STATE_UNKNOWN, "journal_mismatch")
        elif row == ("snapshot", "SNAPSHOTTED"):
            need("journal" in b, Code.STATE_UNKNOWN, "journal_required")
            j = cy.journal(b["journal"])
            journal.require_revoked(j)
            _, ees, _, _ = self._ep(cy, j, b["early"])
            _, fm, fes, _ = cy.binding(b["final"], "final")
            a = cy.assess(fm, fes, True)
            need(fm["result"] == "COMPLETE" and a.q4 == "COMPLETE", Code.STATE_UNKNOWN, "raw_incomplete")
            need(fes == ees, Code.STATE_UNKNOWN, "mutated")
        elif row == ("verify-preservation", "VERIFIED"):
            need("journal" in b, Code.STATE_UNKNOWN, "journal_required")
            j = cy.journal(b["journal"])
            journal.require_revoked(j)
            _, ees, ebs, _ = self._ep(cy, j, b["early"])
            fb, fm, fes, fbs = cy.binding(b["final"], "final")
            vb, vm, ves, vbs = cy.binding(b["verify"], "final", verify=True)
            need(vb["manifest"] == fb["manifest"] and vb["blobs"] == fb["blobs"], Code.STATE_UNKNOWN,
                 "artifact_mismatch")
            need(cy.blobs(sorted(set(ebs) | set(fbs))) is not None, Code.STATE_UNKNOWN, "artifact_mismatch")
            need(cy.assess(vm, ves, True).q4 == "COMPLETE", Code.STATE_UNKNOWN, "raw_incomplete")
        elif row in _FAILURE_ROWS:
            if "journal" in b:
                j = cy.journal(b["journal"])
                if verb in ("snapshot", "verify-preservation"):
                    journal.require_revoked(j)
        else:
            raise Mismatch(Code.STATE_UNKNOWN, "fresult_unlisted")

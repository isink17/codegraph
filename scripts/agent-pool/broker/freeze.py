"""The O5 containment gate and freeze-state rules (PR-3a contract R2 §3, §5.4,
§5.8, R3 §3, R4 §2–§4, R4.1 §3.3/§3.7/§4.6). Pure: reads authenticated PR-1
data, the O5 record stores and selected evidence; never writes. The
protected-mode branch is a constant refusal until PR-4 replaces it."""

from collections import namedtuple

import recovery_state as rs

from . import barrier, canonical, cutover, evidence, o5contract as oc, protocol
from .canonical import is_hex64
from .refusal import Code, Refused
from .stores import EvidenceStore, RecordStore

STORES = ("cutovers", "cutover_heads", "enumerations", "barriers", "journals", "artifacts")

# ------------------------------------------------------------------ F relation and fold (F1 §4.3–§4.4, C1′, C2)

RELATION = {
    "freeze-start": (("NONE",), "MOVE_INTENT", {
        "MOVED": (("",), ("early",)),
        "NONE": (("early_unstable", "early_cap", "unsupported_layout", "mount", "store_space", "store_error",
                  "identity", "rename_failed", "target_exists", "not_preservable"), ()),
        "FREEZE_BLOCKED": (("identity_swap", "identity"), ())}),
    "freeze-continue": (("MOVED", "FREEZE_INCOMPLETE", "FREEZE_BLOCKED"), "REVOKE_INTENT", {
        "REVOKED": (("",), ("early", "journal")),
        "FREEZE_INCOMPLETE": (("flags", "acl", "cap", "io", "entry_set_changed", "store_error"), ("journal",)),
        "FREEZE_BLOCKED": (("identity", "journal_mismatch"), ())}),
    "thaw": (("MOVED", "REVOKED", "FREEZE_INCOMPLETE", "FREEZE_BLOCKED"), "THAW_INTENT", {
        "NONE": (("",), ("journal",)),
        "FREEZE_BLOCKED": (("thaw_target_occupied", "journal_mismatch", "identity"), ())}),
    "snapshot": (("REVOKED", "SNAPSHOT_FAILED", "VERIFY_FAILED"), "SNAPSHOT_INTENT", {
        "SNAPSHOTTED": (("",), ("early", "final", "reconcile")),
        "SNAPSHOT_FAILED": (("incomplete", "unstable", "cap", "dataless", "mount", "missing_tip", "identity",
                             "io", "store_error"), ())}),
    "verify-preservation": (("SNAPSHOTTED", "VERIFY_FAILED"), "VERIFY_INTENT", {
        "VERIFIED": (("",), ("early", "final", "reconcile", "verify")),
        "VERIFY_FAILED": (("changed", "missing", "mismatch", "identity", "io"), ())}),
}
INTENT_VERB = {spec[1]: v for v, spec in RELATION.items()}


def check_result(verb, res):
    """C1′: an effect result outside the relation raises in W (the invocation
    ends UNFINISHED with the intent pending)."""
    spec = RELATION[verb][2].get(getattr(res, "value", None))
    if spec is None or res.reason not in spec[0] or type(res.bindings) is not dict \
            or any(not is_hex64(res.bindings.get(b)) for b in spec[1]) \
            or any(not is_hex64(v) for v in res.bindings.values()):
        raise ValueError("effect result outside the F relation")


def fold(log, doc_of):
    """C2: the whole F log of one operation. Returns (F, last entry) or
    ("UNKNOWN", None). `doc_of(d)` is a burned, stored document or None."""
    f, pending, last, cyc = "NONE", None, None, {}
    for e in log:
        try:
            canonical.check(e)
        except canonical.DomainError:
            return "UNKNOWN", None
        if e.get("kind") == "intent":
            verb = INTENT_VERB.get(e.get("value"))
            doc = doc_of(e.get("auth_digest"))
            if pending is not None or verb is None or f not in RELATION[verb][0] or doc is None \
                    or doc.get("verb") != verb or doc.get("f_from") != f or doc.get("op_id") != e.get("op_id"):
                return "UNKNOWN", None
            if verb == "freeze-continue" and "early" not in cyc:
                return "UNKNOWN", None
            if verb == "freeze-start":
                cyc = {}
            pending = e
        elif e.get("kind") == "complete" and pending is not None:
            verb = INTENT_VERB[pending["value"]]
            spec = RELATION[verb][2].get(e.get("value"))
            b = e.get("bindings")
            if spec is None or e.get("reason") not in spec[0] or type(b) is not dict \
                    or any(not is_hex64(b.get(k)) for k in spec[1]) or e.get("op_id") != pending["op_id"]:
                return "UNKNOWN", None
            if e.get("auth_digest") != pending["auth_digest"]:
                rdoc = doc_of(e.get("auth_digest"))
                if rdoc is None or rdoc.get("verb") != "resume-step" \
                        or rdoc.get("target_digest") != canonical.digest(pending):
                    return "UNKNOWN", None
            for k in ("early", "final", "reconcile"):
                if k in b and k in cyc and cyc[k] != b[k] and e["value"] != "MOVED":
                    return "UNKNOWN", None
            cyc.update(b)
            f, pending = e["value"], None
        else:
            return "UNKNOWN", None
        last = e
    return (pending["value"], pending) if pending is not None else (f, last)


# ------------------------------------------------------------------ documents, heads, chains

def burned_docs(world, entries):
    out = {}
    for e in entries:
        d = e["auth_digest"]
        data = world.authz.get(d)
        if data is None:
            continue
        try:
            out[d] = protocol.parse_doc(data)
        except Refused:
            continue
    return out


def new_stores(faults):
    st = {n: RecordStore(n, faults) for n in STORES}
    st["evidence"] = EvidenceStore()
    return st


def head_of(world, entries, docs):
    witnesses = {d["o5"]["cutover_head"] for d in docs.values()
                 if d.get("verb") not in oc.CUTOVER_VERBS and type(d.get("o5")) is dict and "cutover_head" in d["o5"]}
    return cutover.head(entries, docs.get, world.o5.stores, witnesses, world.o5.floor, oc.mode(world))


def all_cycles(world, docs):
    """Cycle facts for chain verification: built from burned freeze-start
    documents and a classifiable F WAL only (an unclassifiable log yields none)."""
    return {c.cycle_id: c for s in world.slots() for c in cycles(world, s, docs) if c.closure != "STATE_UNKNOWN(fwal)"}


def chains_of(world, docs, cycle_facts):
    st = world.o5.stores["barriers"]
    ids = {}
    for k in st.keys():
        parts = k.split("/")
        ids.setdefault(parts[1] if len(parts) == 3 else "", []).append(k)
    out = {}
    for bid, keys in ids.items():
        raw = [st.get(f"barrier/{bid}/{i:010d}") for i in range(len(keys))]
        if None in raw or not is_hex64(bid):
            out[bid or "?"] = barrier._invalid(bid, "gap")
            continue
        out[bid] = barrier.verify_chain(bid, raw, lambda d: (docs.get(d) or {}).get("verb"), cycle_facts,
                                        oc.mode(world))
    return out


# ------------------------------------------------------------------ cycles and closure (R2 §5.4, R4.1 §3.3)

Cycle = namedtuple("Cycle", "cycle_id slot op_id g_pre freeze_start_auth doc barrier_id cutover_head enumeration "
                            "closure entries bindings closed_utc")
CLOSED = ("none_no_move", "thaw_complete", "ready")


def _ready_closure(world, slot, op_id, docs):
    try:
        view = world.library.read(slot, op_id)
    except Refused:
        return "STATE_UNKNOWN(library)"
    if view is None:
        return "open"
    rec, pending = view.rec, view.pending
    if pending is not None:
        return "ready_pending" if pending["to"] == rs.READY else "open"
    if rec["state"] != rs.READY:
        return "open"
    if view.marker is None:
        return "ready_pending"
    if view.marker != rs._marker(rec):
        return "STATE_UNKNOWN(marker)"
    gate = docs.get(rec["gates"].get("ready"))
    if gate is None:
        return "STATE_UNKNOWN(auth_missing)"
    if gate.get("verb") != "ready" or _cell(gate) != "D-READY":
        return "STATE_UNKNOWN(auth_binding)"
    for d in docs.values():
        if d.get("verb") == "resume-step" and d.get("op_id") == op_id and d.get("slot") == slot \
                and d.get("transition") == "ready" and _cell(d) != oc.READY_CELLS.get(d.get("mode")):
            return "STATE_UNKNOWN(auth_binding)"
    return "ready"


def _cell(doc):
    o5 = doc.get("o5")
    dec = o5.get("decision") if type(o5) is dict else None
    return dec.get("cell") if type(dec) is dict else None


def refreshed_records(world):
    """Every parseable `refreshed` barrier record. An unparseable record makes
    its chain INVALID in verify_chain; it is never trusted here."""
    st, out = world.o5.stores["barriers"], []
    for k in st.keys():
        try:
            r = protocol.parse_doc(st.get(k))
        except Refused:
            continue
        d = r.get("detail")
        if r.get("kind") == "refreshed" and type(d) is dict and type(d.get("cycle_id")) is str \
                and type(d.get("completion")) is str:
            out.append(r)
    return out


def _refreshed_consistent(cid, slot, barrier_id, seg, refreshed):
    """R4 §2.2 last row and §2.4: a `refreshed` naming this cycle must bind
    exactly one completion of this cycle, written under the record's own
    authorization, in the cycle's own chain and for the same slot; one
    completion carries at most one `refreshed`. A completion without its
    `refreshed` (crash between the two writes) is consistent."""
    comps = {canonical.digest(e): e for e in seg if e.get("kind") == "complete"}
    seen = set()
    for r in refreshed:
        d = r["detail"]
        if d.get("cycle_id") != cid:
            continue
        done = comps.get(d.get("completion"))
        if done is None or d["completion"] in seen or d.get("slot") != slot or r.get("barrier_id") != barrier_id \
                or r.get("owner_auth") != done.get("auth_digest"):
            return False
        seen.add(d["completion"])
    return True


def cycles(world, slot, docs):
    """Every cycle of the slot in F WAL order (R2 §5.4 with R4.1 §3.3)."""
    log = world.fwal.log(slot)
    refreshed = refreshed_records(world)
    out = []
    for op_id in dict.fromkeys(e.get("op_id") for e in log):
        oplog = [e for e in log if e.get("op_id") == op_id]
        f, _ = fold(oplog, docs.get)
        starts = [i for i, e in enumerate(oplog) if e.get("kind") == "intent" and e.get("value") == "MOVE_INTENT"]
        for n, i in enumerate(starts):
            seg = oplog[i:starts[n + 1] if n + 1 < len(starts) else len(oplog)]
            auth = seg[0].get("auth_digest")
            doc = docs.get(auth)
            if doc is None or doc.get("verb") != "freeze-start" or doc.get("slot") != slot:
                continue
            o5 = doc.get("o5") if type(doc.get("o5")) is dict else {}
            binds, closed = {}, None
            for e in seg:
                if e.get("kind") == "complete" and type(e.get("bindings")) is dict:
                    binds.update({k: v for k, v in e["bindings"].items() if k != "early" or e["value"] == "MOVED"})
            last_intent = [e for e in seg if e.get("kind") == "intent"][-1]
            done = seg[-1].get("kind") == "complete"
            cid = oc.cycle_id(slot, doc.get("slot_generation"), op_id, auth)
            if f == "UNKNOWN":
                closure = "STATE_UNKNOWN(fwal)"
            elif not _refreshed_consistent(cid, slot, o5.get("barrier_id"), seg, refreshed):
                closure = "STATE_UNKNOWN(cycle_inconsistent)"
            elif len(seg) == 2 and done and seg[1]["value"] == "NONE":
                closure, closed = "none_no_move", seg[1].get("utc")
            elif done and last_intent["value"] == "THAW_INTENT" and seg[-1]["value"] == "NONE":
                tdoc = docs.get(last_intent["auth_digest"])
                ok = tdoc is not None and _cell(tdoc) in ("D-THAW", "D-THAW-R") and _cell(
                    docs.get(seg[-1]["auth_digest"]) or tdoc) in ("D-THAW", "D-THAW-R")
                closure, closed = ("thaw_complete", seg[-1].get("utc")) if ok else ("open", None)
            elif n + 1 < len(starts):
                closure = "STATE_UNKNOWN(cycle_inconsistent)"
            else:
                closure = _ready_closure(world, slot, op_id, docs)
            g_pre = doc.get("slot_generation")
            out.append(Cycle(cid, slot, op_id, g_pre, auth, doc,
                             o5.get("barrier_id"), o5.get("cutover_head"), o5.get("enumeration"), closure, seg,
                             binds, closed))
    return out


def is_open(c):
    return c.closure in ("open", "ready_pending")


def _burn_bound(world, slot, doc, entry):
    """R4 §2.3 condition 4: the burn and its document bind exactly this slot's
    operation at its pre-quarantine generation. The expected binding key is
    computed from the library record (authoritative), never from the document."""
    from . import authz, lifecycle
    op_id = doc.get("op_id")
    if entry["auth_kind"] != "initiate" or doc.get("verb") != "freeze-start" or doc.get("slot") != slot:
        return False
    try:
        view = world.library.read(slot, op_id)
    except Refused:
        return False
    if view is None:
        return False
    g = lifecycle.g_pre(view.rec)
    o5 = doc.get("o5")
    return (doc.get("slot_generation") == g and type(o5) is dict and is_hex64(o5.get("barrier_id"))
            and is_hex64(o5.get("cutover_head"))
            and entry["binding_digest"] == authz.binding_digest({"verb": "freeze-start", "slot": slot,
                                                                 "op_id": op_id, "slot_generation": g}))


def classify_burn(world, slot, docs, entries, auth, cycle_list):
    """R4 §2.2 for one burned freeze-start document (pure)."""
    mine = [e for e in entries if e["auth_digest"] == auth]
    if len(mine) != 1:
        return "STATE_UNKNOWN(fork)"
    doc = docs.get(auth)
    if doc is None:
        return "STATE_UNKNOWN(auth_missing)"
    if not _burn_bound(world, slot, doc, mine[0]):
        return "STATE_UNKNOWN(barrier_binding)"
    log = world.fwal.log(slot)
    f, _ = fold([e for e in log if e.get("op_id") == doc.get("op_id")], docs.get)
    if f == "UNKNOWN":
        return "STATE_UNKNOWN(fwal)"
    intents = [e for e in log if e.get("kind") == "intent" and e.get("auth_digest") == auth]
    if intents:
        cyc = next((c for c in cycle_list if c.freeze_start_auth == auth), None)
        if cyc is not None and cyc.closure.startswith("STATE_UNKNOWN"):
            return cyc.closure
        if cyc is None or cyc.entries[-1].get("kind") == "intent" and len(cyc.entries) == 1:
            return "INTENT_PENDING"
        return "COMPLETED"
    cid = oc.cycle_id(slot, doc.get("slot_generation"), doc.get("op_id"), auth)
    if _traces(world, cid, auth):
        return "STATE_UNKNOWN(cycle_inconsistent)"
    return "ABANDONED_PRE_INTENT"


def _traces(world, cid, auth):
    st = world.o5.stores
    if any(k.split("/")[1:2] == [cid] for k in st["journals"].keys()):
        return True
    for k in st["artifacts"].keys():
        data = st["artifacts"].get(k)
        if cid.encode() in data or auth.encode() in data:
            return True
    for k in st["barriers"].keys():
        data = st["barriers"].get(k)
        if b'"refreshed"' in data and cid.encode() in data:
            return True
    return False


def freeze_start_burns(world, docs, entries, slot):
    """Every burned freeze-start of the slot. A burn whose document is missing is
    still found by its binding key (R4 §2.2, the R2 §4.3 membership technique)."""
    from . import authz, lifecycle
    out = [d for d, doc in docs.items() if doc.get("verb") == "freeze-start" and doc.get("slot") == slot]
    keys = set()
    for op_id in world.library.operations(slot):
        try:
            view = world.library.read(slot, op_id)
        except Refused:
            continue
        if view is not None:
            keys.add(authz.binding_digest({"verb": "freeze-start", "slot": slot, "op_id": op_id,
                                           "slot_generation": lifecycle.g_pre(view.rec)}))
    for e in entries:
        if e["binding_digest"] in keys and e["auth_digest"] not in out:
            out.append(e["auth_digest"])
    return out


# ------------------------------------------------------------------ cells (R2 §3.4, R3 §3.4, R3 §7.9)

Spec = namedtuple("Spec", "cls heads cycle basis decision fverb")
_A, _AR = ("ACTIVE",), ("ACTIVE", "REVOKED")
_CELL = {
    ("freeze-start", "P6"): (("NONE",), Spec("C", _A, "none", "barrier", None, True)),
    ("freeze-continue", "P7"): (("MOVED", "FREEZE_INCOMPLETE", "FREEZE_BLOCKED"),
                                Spec("C", _A, "open", "barrier", None, True)),
    ("thaw", "P7"): (("MOVED", "REVOKED", "FREEZE_INCOMPLETE", "FREEZE_BLOCKED"),
                     Spec("C", _AR, "open", "thaw", "D-THAW", True)),
    ("quarantine", "P7"): (("REVOKED",), Spec("C", _A, "open", "barrier", "D-QUAR", False)),
    ("quiescence", "P11"): (None, Spec("C", _A, "open", "barrier", "D-QUIES", False)),
    ("snapshot", "P13"): (("REVOKED", "SNAPSHOT_FAILED", "VERIFY_FAILED"),
                          Spec("P", _A, "open", "barrier_or_handoff", "D-SNAP", True)),
    ("verify-preservation", "P13"): (("SNAPSHOTTED", "VERIFY_FAILED"),
                                     Spec("P", _A, "open", "barrier_or_handoff", "D-VERIFY", True)),
    ("preservation", "P13"): (("VERIFIED",), Spec("PC", _A, "open", "handoff", "D-PRES", False)),
    ("ready", "P15"): (None, Spec("PC", _A, "open", "handoff", "D-READY", False)),
}
_RESUME = {
    ("P7", "MOVE_INTENT"): Spec("C", _A, "open", "barrier", None, True),
    ("P7", "REVOKE_INTENT"): Spec("C", _A, "open", "barrier", None, True),
    ("P7", "THAW_INTENT"): Spec("C", _AR, "open", "thaw", "D-THAW-R", True),
    ("P13", "SNAPSHOT_INTENT"): Spec("P", _A, "open", "barrier_or_handoff", "D-SNAP-R", True),
    ("P13", "VERIFY_INTENT"): Spec("P", _A, "open", "barrier_or_handoff", "D-VERIFY-R", True),
    ("P14", None): Spec("PC", _A, "open", "handoff", "D-PRES-R", False),
    ("P16", None): Spec("PC", _A, "open", "handoff", "D-READY-R", False),
    ("P17", None): Spec("PC", _A, "open", "handoff", "D-READY-T", False),
    ("P7a", None): Spec("C", _A, "open", "barrier", None, False),
    ("P8", None): Spec("C", _A, "open", "barrier", None, False),
    ("P9", None): Spec("C", _A, "open", "barrier", None, False),
    ("P10", None): Spec("C", _A, "open", "barrier", None, False),
    ("P10a", None): Spec("C", _A, "open", "barrier", None, False),
    ("P12", None): Spec("C", _A, "open", "barrier", None, False),
}


def cell_spec(verb, row, f, n):
    """R2 §3.4 total table; N cells use the strictest cell of the row (R3 §3.4)."""
    if n:
        basis = {"P7": "barrier", "P7a": "barrier", "P8": "barrier", "P9": "barrier", "P10": "barrier",
                 "P10a": "barrier", "P11": "barrier", "P12": "barrier",
                 "P13": "handoff" if f == "VERIFIED" else "barrier_or_handoff"}.get(row, "handoff")
        return Spec("N", _A, "ready" if row == "P18" else "open", basis, None, False)
    if verb == "resume-step":
        return _RESUME.get((row, f if row in ("P7", "P13") else None))
    fs, spec = _CELL.get((verb, row), (None, None))
    if spec is None or fs is not None and f not in fs:
        return None
    return spec


def is_o5(verb, row):
    return verb in oc.O5_VERBS or verb == "resume-step" and row in oc.O5_ROWS


# ------------------------------------------------------------------ the gate (R2 §3.3 G0–G8, R3 G7a/G7b/G8)

Gate = namedtuple("Gate", "spec head chain cycle pre ctx decision selection sel_digest digest picked doc basis")


def _protected():
    raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(protected_mode_unsupported)")


def _g2(h, heads):
    if h.state in heads:
        return
    if h.state in ("ABSENT", "REVOKED", "ACTIVE"):
        raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(model_a)")
    if h.state == "PENDING":
        raise Refused(Code.STATE_UNKNOWN, f"cutover_invalid(pending_{h.reason})")
    raise Refused(Code.STATE_UNKNOWN, f"cutover_invalid({h.reason})")


def c18(world, plan, facts, docs):
    """R2 §5.4 C18: Tier C writer verbs refuse while a live chain covers the slot."""
    verb = plan.op.verb
    if verb == "resume-step" and facts.a.kind == "PENDING":
        verb = facts.a.verb
    if verb not in ("acquire", "release", "clear-suspect"):
        return
    chains = chains_of(world, docs, all_cycles(world, docs))
    if any(barrier.covers(c, facts.slot) for c in barrier.live(chains)):
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use")


def gen_of(facts):
    return facts.op.gen if facts.op is not None else facts.l.g


def check(world, plan, c, facts, n, row):
    """Returns a Gate for an O5 cell, or None for every other verb."""
    entries, verb = plan.entries, plan.op.verb
    docs = burned_docs(world, entries)
    if not is_o5(verb, row):
        c18(world, plan, facts, docs)
        return None
    spec = cell_spec(verb, row, facts.f, n)
    if spec is None:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "o5_row")
    if oc.mode(world) != oc.SYNTHETIC:
        _protected()
    now = world.clock()
    h = head_of(world, entries, docs)
    _g2(h, spec.heads)
    slot = facts.slot
    cyc_list = cycles(world, slot, docs)
    cycle = _g4(world, spec, facts, cyc_list, docs, entries)
    if h.state == "REVOKED":
        enum, uids = _enum_from_cycle(world, cycle), None
    else:
        enum, uids = h.enumeration, h.record["agent"]["writer_uids"]
    entry = next((s for s in (enum or {}).get("slots", []) if s["slot"] == slot), None)
    if entry is None:
        raise Refused(Code.FIELD_INVALID, "slot_unknown")
    if uids is not None and entry["writer_uid"] not in uids:
        raise Refused(Code.STATE_UNKNOWN, "cutover_invalid(identity)")
    if cycle is not None and spec.basis != "thaw":
        if is_open(cycle) and h.digest != cycle.cutover_head:
            raise Refused(Code.STATE_UNKNOWN, "cutover_changed")
        if cycle.closure == "ready" and cycle.cutover_head not in h.history:
            raise Refused(Code.STATE_UNKNOWN, "cutover_invalid(history)")
    chains = chains_of(world, docs, all_cycles(world, docs))
    if spec.cycle == "none":
        chain = barrier.resolve(chains, slot, plan.fields.get("barrier_id"))
    else:
        chain = chains.get(cycle.barrier_id)
    if chain is None:
        raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(none)")
    basis = spec.basis
    if basis == "barrier_or_handoff":
        basis = "handoff" if slot in chain.superseded else "barrier"
    sel = _Selection(world, facts, cycle, chain, h, now)
    if spec.basis != "thaw":
        sel.want(oc.TMB_KINDS)
    st = None
    if basis == "barrier":
        wx = sel.pool(oc.WX_KINDS)
        st = barrier.state(chain, wx)
        if st != barrier.ESTABLISHED:
            barrier.refuse_state(st)
        if slot not in chain.slots or chain.slot_generations.get(slot) != gen_of(facts):
            raise Refused(Code.STATE_UNKNOWN, "barrier_binding")
        if chain.identity_baseline != h.digest or (
                chain.scope == "POOL_MAINTENANCE" and list(chain.writer_uids) != list(uids)):
            raise Refused(Code.STATE_UNKNOWN, "barrier_binding")
        src = sel.want(("source_identity",))
        if src and src["source_identity"][1]["data"].get("root_id") != chain.root_ids.get(slot):
            raise Refused(Code.STATE_UNKNOWN, "barrier_binding")
    elif basis == "thaw":
        st = chain.state
        if st == barrier.INVALID:
            raise Refused(Code.STATE_UNKNOWN, "barrier_invalid")
        if plan.g.stop:
            raise Refused(Code.STOPPED)
    if verb == "quiescence":
        method = (facts.view.rec or {}).get("quiescence_method") if facts.view is not None else None
        sel.want(oc.QUIES_KINDS + (("boot_session",) if method == "reboot-runbook" else ()))
    dec_kinds = oc.DECISION_CELLS[spec.decision][2] if spec.decision else ()
    verdict, sup = None, None
    if basis == "handoff":
        if chain.state == barrier.INVALID:
            raise Refused(Code.STATE_UNKNOWN, "barrier_invalid")
        sup = chain.superseded.get(slot)
        if sup is None:
            raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "handoff_unavailable")
        if sup[0]["detail"].get("cycle_id") != cycle.cycle_id or sup[0]["detail"].get(
                "g_pre") != cycle.g_pre or sup[0]["detail"].get("root_id") != chain.root_ids.get(slot):
            raise Refused(Code.STATE_UNKNOWN, "barrier_binding")
        prov = world.o5.preservation
        if prov is None:
            raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "handoff_unavailable")
        sel.inputs(oc.h_val_kinds(facts.f), sup=sup)
        sel.finish()
        hpre = _pre(world, plan, facts, row, cycle, h, chain, None, sel, "validate")
        verdict = prov.handoff_validate(hpre, sel.public(), _view(facts, row, sel))
        guard(verdict, "H-VAL", hpre.digest, sel, oc.h_val_kinds(facts.f))
        if verdict.phase != "validate" or verdict.passed is not True:
            raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "handoff_invalid")
    sel.inputs(dec_kinds, sup=sup)
    sel.finish()
    if spec.basis != "thaw":
        cutover.model_b_gate(h.record, sel.picked)
    pre = _pre(world, plan, facts, row, cycle, h, chain, st, sel, "check")
    ctx = oc.ContainmentContext(oc._CSEAL, pre=pre, basis=basis, superseded=sup[1] if sup else None,
                                verdict=verdict)
    decision = None
    prov = world.o5.preservation
    if spec.decision or spec.fverb:
        if prov is None:
            raise Refused(Code.PROVENANCE_INSUFFICIENT, "preservation_unavailable")
    if spec.decision:
        decision = prov.decide(ctx, sel.public(), _view(facts, row, sel))
        guard(decision, spec.decision, ctx.digest, sel, dec_kinds, field="ctx")
        if decision.result != "PROCEED":
            code = decision.code if isinstance(decision.code, Code) else Code.STATE_UNKNOWN
            raise Refused(code, decision.detail)
    digest = canonical.sha256(canonical.canon({"ctx": ctx.digest,
                                               "decision": decision.digest if decision is not None else ""}))
    doc = {"cutover_head": h.digest, "enumeration": h.record["slot_enumeration"] if h.record else cycle.enumeration,
           "barrier_id": chain.id, "barrier_head": chain.head, "pre": pre.digest, "ctx": ctx.digest,
           "selection": sel.digest, "g_pre": gen_of(facts)}
    if cycle is not None:
        doc["cycle_id"] = cycle.cycle_id
    if decision is not None:
        doc["decision"] = {"cell": decision.cell, "digest": decision.digest}
    doc = {k: v for k, v in doc.items() if v is not None}
    return Gate(spec, h, chain, cycle, pre, ctx, decision, sel.public(), sel.digest, digest, sel.picked, doc, basis)


def _enum_from_cycle(world, cycle):
    if cycle is None or not cycle.enumeration:
        return None
    data = world.o5.stores["enumerations"].get(cycle.enumeration)
    try:
        return protocol.parse_doc(data) if data is not None else None
    except Refused:
        return None


def _g4(world, spec, facts, cyc_list, docs, entries):
    """G4 with the R4.1 open-cycle classification."""
    if spec.cycle == "none":
        for x in cyc_list:
            if x.closure.startswith("STATE_UNKNOWN"):
                raise Refused(Code.STATE_UNKNOWN, x.closure)
            if is_open(x):
                raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "cycle_open")
        known = {x.cycle_id for x in cyc_list}
        if any(r["detail"].get("slot") == facts.slot and r["detail"].get("cycle_id") not in known
               for r in refreshed_records(world)):
            raise Refused(Code.STATE_UNKNOWN, "cycle_inconsistent")
        for auth in freeze_start_burns(world, docs, entries, facts.slot):
            k = classify_burn(world, facts.slot, docs, entries, auth, cyc_list)
            if k.startswith("STATE_UNKNOWN"):
                raise Refused(Code.STATE_UNKNOWN, k)
        return None
    mine = [x for x in cyc_list if x.op_id == facts.op_id]
    if not mine:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "cycle_absent")
    cur = mine[-1]
    if facts.op is None or cur.g_pre != facts.op.gen:
        raise Refused(Code.STATE_UNKNOWN, "barrier_binding")
    if cur.closure.startswith("STATE_UNKNOWN"):
        raise Refused(Code.STATE_UNKNOWN, cur.closure)
    if spec.cycle == "open" and not is_open(cur) or spec.cycle == "ready" and cur.closure != "ready":
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "cycle_absent")
    return cur


def guard(obj, cell, bound, sel, kinds, field="pre"):
    """R3 §7.9 / R4 §4.4 structural guard over a PR-3b object."""
    if obj is None or getattr(obj, "cell", None) != cell or getattr(obj, field, None) != bound:
        raise Refused(Code.STATE_UNKNOWN, "decision_incomplete")
    inputs = obj.inputs
    if type(inputs) is not dict or any(k not in inputs for k in kinds):
        raise Refused(Code.STATE_UNKNOWN, "decision_incomplete")
    if any(inputs[k] != sel.digest_of(k) for k in kinds):
        raise Refused(Code.STATE_UNKNOWN, "decision_incomplete(digest)")


def _view(facts, row, sel):
    return oc.View(facts.slot, facts.op_id, row, facts.f, sel.public(), dict(sel.objects), None)


def _pre(world, plan, facts, row, cycle, h, chain, st, sel, phase, holder=None, target=None, verb=None):
    cyc_head = cycle.cutover_head if cycle is not None else h.digest
    holder = holder if holder is not None else chain
    return oc.PreBasis(
        oc._CSEAL, mode=oc.mode(world), verb=verb or plan.op.verb, row=row, f=facts.f, slot=facts.slot,
        op_id=facts.op_id, g_pre=cycle.g_pre if cycle is not None else gen_of(facts), gen_now=facts.l.g,
        cycle_id=cycle.cycle_id if cycle is not None else None,
        freeze_start_auth=cycle.freeze_start_auth if cycle is not None else None, cycle_cutover_head=cyc_head,
        current_cutover_head=h.digest, holder_chain=holder.id if holder is not None else None,
        release_target=target, cutover_head=cyc_head, barrier_id=holder.id if holder is not None else None,
        barrier_head=holder.head if holder is not None else None, barrier_state=st,
        scope=holder.scope if holder is not None else None,
        root_id=holder.root_ids.get(facts.slot) if holder is not None else None,
        enumeration=h.record["slot_enumeration"] if h.record else None,
        writer_uid=None, stop=bool(plan.g.stop) if plan is not None else world.gates.stopped(),
        custody_label=facts.k,
        selection=sel.digest if sel is not None else None, phase=phase, checked_at=None)


# ------------------------------------------------------------------ selection (R2 §3.6, R4 §4.1)

class _Selection:
    def __init__(self, world, facts, cycle, chain, h, now):
        self.world, self.facts, self.cycle, self.chain, self.h, self.now = world, facts, cycle, chain, h, now
        self.picked, self.derived, self.missing, self.stale, self.objects = {}, {}, [], [], {}
        self.digest = None

    def _select(self, kinds):
        f = self.facts
        picked, missing, stale = evidence.select(
            self.world.o5.stores["evidence"], kinds, slot=f.slot, gen=gen_of(f), op_id=f.op_id,
            cycle=self.cycle.cycle_id if self.cycle is not None else None, barrier_id=self.chain.id,
            now=self.now, mode=oc.mode(self.world))
        for k, v in picked.items():
            self.picked[k] = v
            self.objects[k] = v[0]
        return picked, missing, stale

    def pool(self, kinds):
        picked, missing, stale = self._select(kinds)
        return picked if not missing and not stale else None

    def want(self, kinds):
        picked, missing, stale = self._select([k for k in kinds if k in evidence.KINDS])
        self.missing += missing
        self.stale += stale
        return picked

    def inputs(self, kinds, sup=None):
        ev = [k for k in kinds if k in evidence.KINDS]
        self.want(ev)
        for k in kinds:
            if k in evidence.KINDS or k in self.derived:
                continue
            d = derived_input(self.world, self.facts, self.cycle, self.chain, k, sup)
            if d is None:
                self.missing.append(k)
            else:
                self.derived[k] = d

    def digest_of(self, k):
        if k in self.picked:
            return self.picked[k][0]
        return self.derived.get(k)

    def finish(self):
        if self.missing:
            raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "evidence_missing")
        if self.stale:
            raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(stale)")
        self.digest = canonical.digest(self.public())

    def public(self):
        f = self.facts
        out = {"cutover_head": self.h.digest, "barrier_id": self.chain.id, "barrier_head": self.chain.head,
               "custody_label": f.k, "evidence": sorted([k, v[0]] for k, v in self.picked.items()),
               "provider_inputs": sorted([k, v] for k, v in self.derived.items())}
        if self.cycle is not None:
            out["cycle_id"] = self.cycle.cycle_id
        return {k: v for k, v in out.items() if v is not None}


def derived_input(world, facts, cycle, chain, kind, sup=None):
    """Provider inputs selected by digest from authenticated PR-1 and O5 data
    (R3 §7.9, R4 §4.1). PR-3a never interprets their content."""
    view = facts.view
    rec = view.rec if view is not None else None
    pending = view.pending if view is not None else None
    b = cycle.bindings if cycle is not None else {}
    if kind in ("ep_manifest", "original_blobs"):
        return b.get("early")
    if kind == "journal_head":
        return b.get("journal")
    if kind == "fs_manifest":
        return b.get("final")
    if kind == "fs_manifest_verified":
        return b.get("verify")
    if kind == "f_intent":
        return canonical.digest(facts.f_entry) if facts.f_entry is not None and facts.f_entry.get(
            "kind") == "intent" else None
    if kind == "original_doc":
        return pending["evidence"] if pending is not None else None
    if kind == "library_pending":
        return canonical.digest({k: pending[k] for k in rs.ENTRY_KEYS if k in pending}) if pending else None
    if kind == "library_record":
        return canonical.digest(rec) if rec is not None else None
    if kind in ("ready_doc", "preservation_doc"):
        return rec["gates"].get(kind[:-4] if kind == "ready_doc" else "preservation") if rec else None
    if kind == "tierc":
        return canonical.sha256(facts.text.encode()) if facts.text is not None and facts.a.kind == "STABLE" \
            else None
    if kind == "superseded":
        return sup[1] if sup is not None else None
    if kind == "holder_record":
        return chain.head if chain is not None else None
    if kind == "enforcement_record":
        if chain is None or chain.established is None:
            return None
        return canonical.digest(chain.established)
    return None


# ------------------------------------------------------------------ PreBasis for PR-4 B purposes (R4.1 §4.6)

def basis_for(world, slot, purpose, doc=None, release_target=None, kind=None):
    """The PreBasis of a PR-4 B verb (purpose `handoff` (H-EST), `release`).
    For `handoff`, `doc` is the current containment-handoff document (N-R4.1-1):
    its schema and bindings are validated without requiring a burn; every
    historical document still must be burned. Returns (pre, selection)."""
    from . import invocation
    entries = world.ledger.entries()
    docs = burned_docs(world, entries)
    facts = invocation._read_slot(world, slot, entries)
    if oc.mode(world) != oc.SYNTHETIC:
        _protected()
    h = head_of(world, entries, docs)
    if h.state != "ACTIVE":
        _g2(h, ("ACTIVE",))
    cyc_list = cycles(world, slot, docs)
    mine = [x for x in cyc_list if x.op_id == facts.op_id]
    cycle = mine[-1] if mine else None
    chains = chains_of(world, docs, all_cycles(world, docs))
    now = world.clock()
    if purpose == "handoff":
        if release_target is not None:
            raise Refused(Code.STATE_UNKNOWN, "barrier_binding")
        if cycle is None or not is_open(cycle) or facts.op is None or cycle.g_pre != facts.op.gen:
            raise Refused(Code.STATE_UNKNOWN, "barrier_binding")
        if h.digest != cycle.cutover_head:
            raise Refused(Code.STATE_UNKNOWN, "cutover_changed")
        holder = chains.get(cycle.barrier_id) if cycle.barrier_id else None
        _check_handoff_doc(doc, slot, cycle)
        if holder is None:
            raise Refused(Code.STATE_UNKNOWN, "barrier_binding")
        if holder.state == barrier.INVALID:
            raise Refused(Code.STATE_UNKNOWN, "barrier_invalid")
        sel = _Selection(world, facts, cycle, holder, h, now)
        st = barrier.state(holder, sel.pool(oc.WX_KINDS))
        if st != barrier.ESTABLISHED:
            barrier.refuse_state(st)
        if slot not in holder.slots or holder.slot_generations.get(slot) != cycle.g_pre \
                or facts.l.g != cycle.g_pre + 1 or holder.identity_baseline != cycle.cutover_head:
            raise Refused(Code.STATE_UNKNOWN, "barrier_binding")
        if slot in holder.superseded:
            raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "superseded_exists")
        sel.inputs(oc.H_EST_KINDS)
        sel.finish()
        pre = _pre(world, None, facts, None, cycle, h, holder, st, sel, "establish", verb="containment-handoff")
        return pre, sel
    target = chains.get(release_target)
    if target is None:
        raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(none)")
    holder = chains.get(cycle.barrier_id) if cycle is not None else None
    sel = _Selection(world, facts, cycle, holder or target, h, now)
    if cycle is not None and holder is not None:
        sel.inputs(oc.discharge_kinds(kind, facts.f), sup=holder.superseded.get(slot))
    sel.finish()
    pre = _pre(world, None, facts, None, cycle, h, holder, None, sel, "release", target=target.id,
               verb="barrier-release")
    return pre, sel


def _check_handoff_doc(doc, slot, cycle):
    """N-R4.1-1: the proposed document (not yet burned) binds exactly this cycle."""
    if type(doc) is not dict or doc.get("verb") != "containment-handoff":
        raise Refused(Code.FIELD_INVALID, "handoff document")
    want = {"slot": slot, "op_id": cycle.op_id, "cycle_id": cycle.cycle_id, "barrier_id": cycle.barrier_id}
    if any(doc.get(k) != v for k, v in want.items()):
        raise Refused(Code.STATE_UNKNOWN, "barrier_binding")


# ------------------------------------------------------------------ quiescence_check rules 1–6 (R2 §5.8)

def quiescence_check(components, picked, store, doc, revoked_utc, confirmed_at, method):
    kinds = oc.QUIES_KINDS + (("boot_session",) if method == "reboot-runbook" else ())
    names = {c["name"]: c["sha256"] for c in components}
    q = Code.QUIESCENCE_NOT_ESTABLISHED
    for k in kinds:
        if k not in picked or k not in names:
            raise Refused(q, "missing kind")
    for k in kinds:
        d, obj = picked[k]
        if obj["data"].get("result") == "contradicts":
            raise Refused(q, "contradicts")
        s = obj["subject"]
        if (s["slot"], s["op_id"]) != (doc["slot"], doc["op_id"]) or s["slot_generation"] != doc["o5"]["g_pre"]:
            raise Refused(q, "subject")
        if obj["observed_at"] < confirmed_at - oc.FRESH or revoked_utc is None or obj["observed_at"] < revoked_utc:
            raise Refused(q, "stale")
        data = store.get(names[k])
        if data is None or names[k] != d or canonical.sha256(data) != names[k]:
            raise Refused(q, "details")
    for a, b in (("uid_process_scan_1", "uid_process_scan_2"), ("holder_scan_1", "holder_scan_2"),
                 ("tree_vector_1", "tree_vector_2")):
        if picked[a][1]["data"] != picked[b][1]["data"]:
            raise Refused(q, "disagree")


def revoked_utc(cycle):
    if cycle is None:
        return None
    done = [e for e in cycle.entries if e.get("kind") == "complete" and e.get("value") == "REVOKED"]
    return done[-1].get("utc") if done else None


# ------------------------------------------------------------------ release (PR-4 barrier-release V/R; R3 §6, R4 §3.5)

def release_views(world, target, docs, entries, cyc_list_by_slot):
    from . import invocation, lifecycle
    views = {}
    for slot in target.slots:
        facts = invocation._read_slot(world, slot, entries)
        c = lifecycle.Components(invocation._g(world), facts.l, facts.a, facts.f, facts.r, facts.k, facts.op)
        unknown, row = None, None
        try:
            invocation._precheck(c)
            row = lifecycle.row(c)
        except Refused as e:
            unknown = f"{e.code.value}"
        if row is None and unknown is None:
            unknown = "row"
        cyc_list = cyc_list_by_slot[slot]
        mine = [x for x in cyc_list if facts.op_id is not None and x.op_id == facts.op_id]
        cur = mine[-1] if mine else None
        relevant = [x for x in cyc_list if x.barrier_id == target.id or x.op_id == facts.op_id]
        burns = [classify_burn(world, slot, docs, entries, a, cyc_list)
                 for a in freeze_start_burns(world, docs, entries, slot)
                 if a not in docs or (docs[a].get("o5") or {}).get("barrier_id") == target.id]
        known = {x.cycle_id for x in cyc_list}
        refreshed_unknown = any(r["detail"].get("slot") == slot and r["detail"].get("cycle_id") not in known
                                for r in target.refreshed)
        views[slot] = barrier.SlotView(slot, row, unknown, facts.a.kind == "STABLE", facts.f, relevant, cur, burns,
                                       refreshed_unknown)
        if cur is not None and cur.closure.startswith("STATE_UNKNOWN") and unknown is None:
            views[slot] = views[slot]._replace(unknown=cur.closure)
    return views


def release_check(world, barrier_id, resume=False, halt=False):
    """The full PR-3a release precondition for one target chain (pure)."""
    if oc.mode(world) != oc.SYNTHETIC:
        _protected()
    entries = world.ledger.entries()
    docs = burned_docs(world, entries)
    by_slot = {s: cycles(world, s, docs) for s in world.slots()}
    chains = chains_of(world, docs, all_cycles(world, docs))
    target = chains.get(barrier_id)
    if target is None:
        raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(none)")
    h = head_of(world, entries, docs)
    for s in target.slots:
        by_slot.setdefault(s, [])
    views = release_views(world, target, docs, entries, by_slot)
    for v in views.values():
        cur = v.current
        if v.unknown is None and cur is not None and h.state == "ACTIVE":
            if is_open(cur) and cur.cutover_head != h.digest:
                views[v.slot] = v._replace(unknown="cutover_changed")
            elif cur.closure == "ready" and cur.cutover_head not in h.history:
                views[v.slot] = v._replace(unknown="cutover_invalid(history)")
    st = target.state

    def discharge(view, kind, holder, cyc):
        prov = world.o5.preservation
        if prov is None:
            return None
        pre, sel = basis_for(world, view.slot, "release", release_target=target.id, kind=kind)
        d = prov.discharge(view, holder, pre, sel.public())
        if d is None:
            return None
        guard(d, oc.DISCHARGE_CELLS[kind], pre.digest, sel, oc.discharge_kinds(kind, view.f))
        sup = holder.superseded.get(view.slot)
        if d.kind != kind or d.slot != view.slot or d.cycle_id != cyc.cycle_id or d.g_pre != cyc.g_pre \
                or d.holder != holder.id or sup is None or d.superseded != sup[1]:
            return None
        return d

    return barrier.release_allowed(oc.mode(world), target, st, h, views, chains, world.gates.stopped(), halt, resume,
                                   discharge)


def refreshed_record(chain, auth, slot, cid, completion, picked, utc, mode):
    """R4 §2.4: appended after the F completion, barrier basis only."""
    a = chain.authorized
    rec = {k: a[k] for k in barrier.IMMUTABLE}
    rec.update(schema=oc.S_BARRIER, barrier_id=chain.id, seq=len(chain.records), prev=chain.head, kind="refreshed",
               owner_auth=auth, evidence=sorted(v[0] for k, v in picked.items() if k in oc.WX_KINDS), utc=utc,
               mode=mode, detail={"slot": slot, "cycle_id": cid, "completion": canonical.digest(completion)})
    return f"barrier/{chain.id}/{rec['seq']:010d}", canonical.canon(rec)


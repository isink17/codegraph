"""Writer barrier chains (F4 §4, PR-3a contract R2 §5–§6, R3 §5–§6, R4 §3.5,
R4.1 §3.7/§4.6): record verification, derived state, overlap, the structural
handoff precondition and the release decision table. Pure: the records are
written only by PR-4 owner verbs (and, in PR-3a tests, by raw fixtures)."""

from collections import namedtuple

from . import canonical, o5contract as oc, protocol
from .refusal import Code, Refused

Chain = namedtuple("Chain", "id state reason records authorized established superseded refreshed head slots "
                            "writer_uids scope identity_baseline slot_generations root_ids")
NONE, INVALID, AUTHORIZED, ACTIVATING, FAILED = "NONE", "INVALID", "AUTHORIZED", "ACTIVATING", "FAILED"
ESTABLISHED, STALE, BROKEN, RELEASING, RELEASED = "ESTABLISHED", "STALE", "BROKEN", "RELEASING", "RELEASED"
COMMON = ("schema", "barrier_id", "seq", "prev", "kind", "scope", "slots", "slot_generations", "writer_uids",
          "owner_auth", "root_ids", "identity_baseline", "enumeration", "capability", "evidence", "utc", "mode",
          "detail")
IMMUTABLE = ("scope", "slots", "slot_generations", "writer_uids", "root_ids", "identity_baseline", "enumeration",
             "capability")
NEXT = {None: ("authorized",), "authorized": ("activation_attempted",),
        "activation_attempted": ("established", "activation_failed"),
        "established": ("refreshed", "superseded", "release_requested"),
        "refreshed": ("refreshed", "superseded", "release_requested"),
        "superseded": ("refreshed", "superseded", "release_requested"),
        "activation_failed": ("release_requested",), "release_requested": ("release_authorized",),
        "release_authorized": ("released",), "released": ()}
OWNER_VERB = {"authorized": ("barrier-establish",), "activation_attempted": ("barrier-establish",),
              "established": ("barrier-establish",), "activation_failed": ("barrier-establish",),
              "refreshed": oc.F_VERBS + ("resume-step",), "superseded": ("containment-handoff",),
              "release_requested": ("barrier-release",), "release_authorized": ("barrier-release",),
              "released": ("barrier-release",)}
STATIC = {"authorized": AUTHORIZED, "activation_attempted": ACTIVATING, "activation_failed": FAILED,
          "release_requested": RELEASING, "release_authorized": RELEASING, "released": RELEASED}


def _invalid(bid, reason):
    return Chain(bid, INVALID, reason, (), None, None, {}, (), None, (), (), None, None, {}, {})


def _parse(data):
    try:
        return protocol.parse_doc(data) if data is not None else None
    except Refused:
        return None


def verify_chain(bid, raw, burned_verb, cycle_ids, mode):
    """F4 §4.3 INVALID conditions plus R2 §5.1. `burned_verb(d)` is the verb of a
    burned, stored document or None; `cycle_ids` are the cycles freeze.cycles knows."""
    recs, prev_d, last = [], oc.ZERO64, None
    supers, refreshed = {}, []
    for i, data in enumerate(raw):
        r = _parse(data)
        if r is None or sorted(r) != sorted(COMMON) or r["schema"] != oc.S_BARRIER or r["seq"] != i \
                or r["barrier_id"] != bid or r["prev"] != prev_d or type(r["detail"]) is not dict:
            return _invalid(bid, "record")
        if r["mode"] != mode:
            return _invalid(bid, "mode")
        if r["kind"] not in NEXT.get(last, ()):
            return _invalid(bid, "order")
        if burned_verb(r["owner_auth"]) not in OWNER_VERB[r["kind"]]:
            return _invalid(bid, "owner_auth")
        if recs and any(r[k] != recs[0][k] for k in IMMUTABLE):
            return _invalid(bid, "binding")
        named = r["detail"].get("slot")
        if named is not None and (not recs or named not in recs[0]["slots"]):
            return _invalid(bid, "slot")
        if r["kind"] == "superseded":
            if named in supers or r["detail"].get("cycle_id") not in cycle_ids:
                return _invalid(bid, "superseded")
            supers[named] = (r, canonical.sha256(data))
        if r["kind"] == "refreshed":
            refreshed.append(r)
        recs.append(r)
        prev_d, last = canonical.sha256(data), r["kind"]
    if not recs:
        return Chain(bid, NONE, None, (), None, None, {}, (), None, (), (), None, None, {}, {})
    a = recs[0]
    slots, uids = a["slots"], a["writer_uids"]
    gens, roots = _pairs(a["slot_generations"]), _pairs(a["root_ids"])
    if bid != oc.barrier_id(a["owner_auth"]) or a["scope"] not in ("SLOT_ISOLATED", "POOL_MAINTENANCE") \
            or type(slots) is not list or slots != sorted(set(slots)) or not slots or type(uids) is not list \
            or not uids or gens is None or roots is None or sorted(gens) != slots or sorted(roots) != slots:
        return _invalid(bid, "authorized")
    est = next((r for r in recs if r["kind"] == "established"), None)
    if est is not None and (type(est["detail"].get("enforcement")) is not str
                            or type(est["detail"].get("neutralized")) is not list):
        return _invalid(bid, "established")
    return Chain(bid, STATIC.get(last, ESTABLISHED), None, tuple(recs), a, est, supers, tuple(refreshed),
                 prev_d, tuple(slots), tuple(uids), a["scope"], a["identity_baseline"], gens, roots)


def _pairs(v):
    """Slot-keyed maps are sorted [slot, value] pairs (slot names are not D keys)."""
    if type(v) is not list or any(type(p) is not list or len(p) != 2 or type(p[0]) is not str for p in v) \
            or [p[0] for p in v] != sorted({p[0] for p in v}):
        return None
    return {p[0]: p[1] for p in v}


def wx_ok(chain, picked):
    """G7b writer exclusion (R3 §3.3) over fresh pool observations of this chain."""
    d = {k: picked[k][1]["data"] for k in oc.WX_KINDS}
    neutral = set(chain.established["detail"]["neutralized"])
    chans = d["agent_channels"].get("channels")
    return (d["uid_process_scan"].get("processes") == [] and d["holder_scan"].get("holders") == []
            and d["enforcement_observation"].get("digest") == chain.established["detail"]["enforcement"]
            and type(chans) is dict and all(s == "neutralized" and n in neutral for n, s in chans.items())
            and not any(x.get("result") == "contradicts" for x in d.values()))


def state(chain, picked):
    """F4 §4.3 derived state. `picked` holds the fresh WX observations or None."""
    if chain.state != ESTABLISHED:
        return chain.state
    if picked is None:
        return STALE
    return ESTABLISHED if wx_ok(chain, picked) else BROKEN


def covers(chain, slot, uids=()):
    return chain.state == INVALID or slot in chain.slots or bool(set(uids) & set(chain.writer_uids))


def live(chains):
    return [c for c in chains.values() if c.state != RELEASED]


def establish_allowed(chains, scope, slots, writer_uids, enum_uids, pending_slots):
    """R2 §5.2, F5 §3.5: shared uid, overlap (INVALID covers everything), pending Tier C step."""
    if scope != "POOL_MAINTENANCE" or sorted(slots) != sorted(s for s, u in enum_uids.items() if u in writer_uids):
        raise Refused(Code.FIELD_INVALID, "barrier_scope")
    if any(covers(c, s, writer_uids) for c in live(chains) for s in slots):
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use")
    if set(slots) & set(pending_slots):
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "pending_step")


def resolve(chains, slot, named):
    """R4 §6 (N7): an omitted barrier_id resolves only to exactly one live chain
    covering the slot while no INVALID chain exists."""
    if named is not None:
        return chains.get(named)
    cands = [c for c in live(chains) if slot in c.slots or c.state == INVALID]
    if len(cands) != 1 or cands[0].state == INVALID:
        raise Refused(Code.FIELD_INVALID, "barrier_ambiguous")
    return cands[0]


def refuse_state(st):
    if st == BROKEN:
        raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_broken")
    if st == INVALID:
        raise Refused(Code.STATE_UNKNOWN, "barrier_invalid")
    raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, f"barrier_unavailable({st.lower()})")


def handoff_allowed(chain, st, row, slot, cycle, stop, verdicts, pre_digests, guard):
    """R2 §5.5 structural part (PR-4 containment-handoff V and R). `verdicts`
    are the V and R establishment verdicts; `guard(v)` is the R4 §4.4 guard."""
    if stop:
        raise Refused(Code.STOPPED)
    if chain is None or chain.id != cycle.barrier_id:
        raise Refused(Code.STATE_UNKNOWN, "barrier_binding")
    if st != ESTABLISHED:
        refuse_state(st)
    if row != "P13":
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "o5_row")
    if slot in chain.superseded:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "superseded_exists")
    if any(v is None for v in verdicts):
        raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "handoff_unavailable")
    for v, pd in zip(verdicts, pre_digests):
        guard(v)
        if v.phase != "establish" or v.pre != pd:
            raise Refused(Code.STATE_UNKNOWN, "decision_incomplete")
        if v.passed is not True:
            raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "handoff_failed")


# ------------------------------------------------------------------ release (R3 §6, R4 §3.5)

SlotView = namedtuple("SlotView", "slot row unknown anchor_stable f cycles current burn_classes refreshed_unknown")
D_KINDS = ("D3", "D4", "D3H", "D4H")


def release_case(target, view, chains):
    """One slot's case under R3 §6.2 as amended by R4 §3.5 / R4.1 §3.7. Returns
    ("D1"|"D2", None) or ("DIS", (kind, holder, cycle)); refuses otherwise."""
    if view.unknown:
        raise Refused(Code.STATE_UNKNOWN, view.unknown)
    bad = [c for c in view.burn_classes if c.startswith("STATE_UNKNOWN")]
    if bad:
        raise Refused(Code.STATE_UNKNOWN, bad[0])
    if view.refreshed_unknown:
        raise Refused(Code.STATE_UNKNOWN, "cycle_inconsistent")
    row = view.row
    if row in ("P5'", "P19", "P20", "P21"):
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(pending_step)")
    if row in ("P1", "P2", "P3", "P4", "P5", "P6"):
        if not view.anchor_stable or view.f != "NONE":
            raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(preservation_dependent)")
        if "INTENT_PENDING" in view.burn_classes:
            raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(preservation_dependent)")
        for c in view.cycles:
            if c.closure not in ("none_no_move", "thaw_complete"):
                raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(preservation_dependent)")
            if c.closure == "thaw_complete" and c.barrier_id == target.id:
                return "D2", None
        return "D1", None
    cur = view.current
    if row in ("P7", "P7a", "P8", "P9", "P10", "P10a", "P11", "P12"):
        if cur is not None and cur.barrier_id != target.id:
            raise Refused(Code.STATE_UNKNOWN, "barrier_binding")
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(preservation_dependent)")
    if row in ("P13", "P14", "P15", "P16", "P17", "P18") and cur is not None:
        holder = chains.get(cur.barrier_id)
        want_open = row != "P18"
        if holder is None or holder.state == INVALID or cur.slot not in holder.superseded \
                or (cur.closure in ("open", "ready_pending")) != want_open \
                or (not want_open and cur.closure != "ready"):
            raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(preservation_dependent)")
        if holder.id != target.id and holder.state != RELEASED:
            raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(preservation_dependent)")
        kind = ("D3" if want_open else "D4") + ("" if holder.id == target.id else "H")
        return "DIS", (kind, holder, cur)
    raise Refused(Code.STATE_UNKNOWN, "row")


def release_allowed(target, st, head, views, chains, stop, halt, resume, discharge):
    """R2 §6.1/§6.4 global conditions, then every slot of the enforcement scope
    needs exactly one case. `discharge(view, kind, holder, cycle)` returns an
    accepted Discharge or None (PR-3b; R3 §6.3). Returns [(slot, case)]."""
    if stop:
        raise Refused(Code.STOPPED)
    if halt:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "halt")
    if head.state != "ACTIVE":
        if head.state in ("PENDING", "INVALID"):
            raise Refused(Code.STATE_UNKNOWN, f"cutover_invalid({head.reason})")
        raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(model_a)")
    if st == INVALID:
        refuse_state(st)
    if head.digest != target.identity_baseline:
        raise Refused(Code.STATE_UNKNOWN, "cutover_invalid(identity)")
    if st == RELEASING and not resume:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "release_in_progress")
    if st not in (ESTABLISHED, FAILED, RELEASING):
        refuse_state(st)
    out = []
    for slot in target.slots:
        case, dis = release_case(target, views[slot], chains)
        if case == "DIS":
            if st == FAILED:
                raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(preservation_dependent)")
            kind, holder, cyc = dis
            if discharge(views[slot], kind, holder, cyc) is None:
                raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use(preservation_dependent)")
            case = kind
        out.append((slot, case))
    return out

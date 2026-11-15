"""Test-only O5 fixtures (PR-3a contract R2 §7.4 rule 4, R3 A-N1). Every record
here is seeded straight into the in-memory fakes (no CommitTxn, no producer):
the owner verbs that write them are PR-4. Nothing here is importable by
production code (AST test), and protected mode refuses before any of it is
consulted. Synthetic evidence never provides root authority."""

import hashlib
import json

from .. import (authz, barrier, canonical, evidence, freeze, invocation, ledger, o5contract as oc)

AGENT_UID = 601
NEUTRALIZED = ["agent_crontab", "agent_launchagents", "agent_login"]
ENFORCEMENT = hashlib.sha256(b"enforcement").hexdigest()
BASELINES = {k: hashlib.sha256(f"baseline-{k}".encode()).hexdigest() for k in ("root", "owner", "principals",
                                                                               "channels")}


def h(text):
    return hashlib.sha256(str(text).encode()).hexdigest()


def ingest_capability():
    return evidence._new_ingest(evidence._ISEAL)


def fixture_mode(env):
    return oc.SYNTHETIC if env.ctx.store == authz.SYNTHETIC else oc.PROTECTED


def _trust(env):
    return authz.TRUST_SYNTHETIC if env.ctx.store == authz.SYNTHETIC else authz.TRUST_OWNER


def _next(env):
    """A per-Env counter: fixtures stay deterministic across worlds."""
    env.o5_counter = getattr(env, "o5_counter", 0) + 1
    return env.o5_counter


def burn(env, doc, ledger_line=True, binding=None, auth_kind="initiate"):
    """Stores the document and appends its burn (raw seeding). `binding` and
    `auth_kind` override the ledger fields for crafted-burn tests."""
    w = env.world
    data = canonical.canon(doc)
    d = canonical.sha256(data)
    w.authz.docs[d] = data
    if ledger_line:
        res = w.ledger.classify()
        prev = res["entries"][-1]["line_digest"] if res["entries"] else ledger.ZERO64
        n = _next(env)
        e = ledger.make_entry(ledger.LEDGER, len(res["entries"]), prev, {
            "auth_digest": d, "auth_kind": auth_kind, "binding_digest": binding or authz.binding_digest(doc),
            "confirmed_at": env.world.clock(), "issued_at": env.world.clock(),
            "nonce_digest": h(f"fixture-nonce-{d}-{n}")})
        w.ledger.data += canonical.canon(e) + b"\n"
    return d


def _owner_doc(env, verb, **fields):
    return dict({"v": 1, "doc": verb, "verb": verb, "trust": _trust(env), "actor_uid": 501,
                 "nonce_digest": h(f"fixture-doc-{_next(env)}")}, **fields)


# ------------------------------------------------------------------ Model B cutover

def enumeration(env, slots, uid=AGENT_UID):
    obj = {"schema": oc.S_ENUM, "mode": fixture_mode(env),
           "slots": [{"slot": s, "writer_uid": uid, "provisioned_root": "/Users/o5-agent/" + s} for s in sorted(slots)]}
    data = canonical.canon(obj)
    env.world.o5.stores["enumerations"].items[canonical.sha256(data)] = data
    return canonical.sha256(data)


def cutover_step(env, verb="model-b-cutover", slots=None, record=True, head=True, floor=None, agent=None,
                 enum=None, seq=None):
    """One PR-4 cutover verb W, seeded: burn (+ document), then record, head, floor.
    Returns (auth, record digest)."""
    w = env.world
    st = w.o5.stores
    seq = len(st["cutovers"].keys()) if seq is None else seq
    prev_data = st["cutovers"].get(f"cutover/{seq - 1:010d}") if seq else None
    prev = canonical.sha256(prev_data) if prev_data else oc.ZERO64
    if enum is None:
        enum = enumeration(env, slots) if slots is not None else (
            json.loads(prev_data)["slot_enumeration"] if prev_data else "")
    kind = oc.CUTOVER_KIND[verb]
    body = {"schema": oc.S_CUTOVER, "seq": seq, "prev": prev, "kind": kind,
            "effective": "MODEL_A" if kind == "revoked" else "MODEL_B",
            "agent": agent or {"writer_uids": [AGENT_UID], "account": "o5-agent", "primary_gid": AGENT_UID,
                               "scope": "pool"},
            "owner_principal": dict(oc.OWNER_PRINCIPAL), "slot_enumeration": enum, "baselines": dict(BASELINES),
            "tests": h("tests"), "preconditions": [], "supersedes": "" if seq == 0 else prev, "utc": env.world.clock(),
            "mode": fixture_mode(env)}
    doc = _owner_doc(env, verb, slot=oc.CUTOVER_SUBJECT, slot_generation=seq, cutover_record=canonical.digest(body),
                     cutover_prev=prev, enumeration=enum)
    auth = burn(env, doc)
    rec = dict(body, owner_auth=auth)
    data = canonical.canon(rec)
    if record:
        st["cutovers"].items[f"cutover/{seq:010d}"] = data
    if head:
        st["cutover_heads"].items[f"head/{seq:010d}"] = canonical.canon(
            {"schema": oc.S_HEAD, "seq": seq, "record": canonical.sha256(data), "owner_auth": auth,
             "mode": fixture_mode(env)})
    if floor:
        w.o5.floor = (seq, canonical.sha256(data))
    return auth, canonical.sha256(data)


def synthetic_model_b(env, slots, floor=False):
    return cutover_step(env, "model-b-cutover", slots=slots, floor=floor)


# ------------------------------------------------------------------ barriers

def barrier_record(env, bid, seq, prev, kind, auth, base, detail=None):
    rec = dict(base, schema=oc.S_BARRIER, barrier_id=bid, seq=seq, prev=prev, kind=kind, owner_auth=auth,
               evidence=[], utc=env.world.clock(), mode=fixture_mode(env), detail=detail or {})
    data = canonical.canon(rec)
    env.world.o5.stores["barriers"].items[f"barrier/{bid}/{seq:010d}"] = data
    return canonical.sha256(data)


def synthetic_barrier(env, slots, gens=None, upto="established", head_digest=None):
    """`barrier-establish` W, seeded: authorized, activation_attempted, established."""
    st = env.world.o5.stores
    if head_digest is None:
        keys = st["cutovers"].keys()
        head_digest = canonical.sha256(st["cutovers"].get(keys[-1])) if keys else oc.ZERO64
    doc = _owner_doc(env, "barrier-establish", slot="o5-barrier", slot_generation=len(st["barriers"].keys()),
                     slots=sorted(slots))
    auth = burn(env, doc)
    bid = oc.barrier_id(auth)
    base = {"scope": "POOL_MAINTENANCE", "slots": sorted(slots),
            "slot_generations": [[s, (gens or {}).get(s, 1)] for s in sorted(slots)],
            "writer_uids": [AGENT_UID], "root_ids": [[s, h(f"root|{s}")] for s in sorted(slots)],
            "identity_baseline": head_digest, "enumeration": "", "capability": h("capability")}
    prev = oc.ZERO64
    kinds = ["authorized", "activation_attempted"] + ([upto] if upto in ("established", "activation_failed") else [])
    for i, kind in enumerate(kinds):
        detail = {"enforcement": ENFORCEMENT, "neutralized": list(NEUTRALIZED)} if kind == "established" else None
        prev = barrier_record(env, bid, i, prev, kind, auth, base, detail)
    return bid


def chain(env, bid):
    w = env.world
    docs = freeze.burned_docs(w, w.ledger.entries())
    return freeze.chains_of(w, docs, freeze.all_cycles(w, docs))[bid]


def append_record(env, bid, kind, auth=None, detail=None, verb=None):
    """Appends one record to a chain; burns an owner document of `verb` unless `auth` is given."""
    c = chain(env, bid)
    if auth is None:
        auth = burn(env, _owner_doc(env, verb or {"superseded": "containment-handoff"}.get(kind, "barrier-release"),
                                    slot="o5-barrier", slot_generation=len(c.records) + 1000 * _next(env)))
    base = {k: c.authorized[k] for k in barrier.IMMUTABLE}
    return barrier_record(env, bid, len(c.records), c.head, kind, auth, base, detail)


def release_chain(env, bid):
    for kind in ("release_requested", "release_authorized", "released"):
        append_record(env, bid, kind)


# ------------------------------------------------------------------ evidence

def _ev(env, kind, scope, slot="", gen=0, op_id="", cycle="", barrier_id="", data=None, at=None):
    obj = {"schema": oc.S_EVIDENCE, "kind": kind, "observed_at": at if at is not None else env.world.clock(),
           "subject": {"scope": scope, "slot": slot, "slot_generation": gen, "op_id": op_id, "cycle": cycle,
                       "barrier_id": barrier_id},
           "producer": evidence.PRODUCER, "provenance": "ROOT_OBSERVED",
           "role": "supporting" if kind in oc.QUIES_KINDS else "decisive", "mode": fixture_mode(env),
           "data": data if data is not None else {"result": "consistent"}}
    return evidence.ingest(ingest_capability(), env.world.o5.stores["evidence"], obj)


def subject(env, slot):
    w = env.world
    entries = w.ledger.entries()
    facts = invocation._read_slot(w, slot, entries)
    docs = freeze.burned_docs(w, entries)
    mine = [c for c in freeze.cycles(w, slot, docs) if c.op_id == facts.op_id]
    cyc = mine[-1].cycle_id if mine and freeze.is_open(mine[-1]) or mine and mine[-1].closure == "ready" else ""
    return {"slot": slot, "gen": freeze.gen_of(facts), "op_id": facts.op_id or "", "cycle": cyc}


def observe(env, slot, at=None, overrides=None, skip=()):
    """Fresh synthetic ROOT_OBSERVED objects for every kind the gate may select."""
    overrides = overrides or {}
    at = at if at is not None else env.world.clock()
    pool = {"agent_identity_root": {"writer_uid": AGENT_UID, "primary_gid": AGENT_UID, "groups": ["o5-agent"]},
            "principals": {"digest": BASELINES["principals"]}, "no_agent_as_owner": {"result": "absent"},
            "root_channels": {"digest": BASELINES["channels"]}}
    for k, data in pool.items():
        if k not in skip:
            _ev(env, k, "pool", data=overrides.get(k, data), at=at)
    for bid in {key.split("/")[1] for key in env.world.o5.stores["barriers"].keys()}:
        wx = {"uid_process_scan": {"processes": []}, "holder_scan": {"holders": []},
              "agent_channels": {"channels": {n: "neutralized" for n in NEUTRALIZED}},
              "enforcement_observation": {"digest": ENFORCEMENT}}
        for k, data in wx.items():
            if k not in skip:
                _ev(env, k, "pool", barrier_id=bid, data=overrides.get(k, data), at=at)
    try:
        s = subject(env, slot)
    except Exception:  # noqa: BLE001 - an unreadable world: the gate itself refuses
        return
    if type(s["gen"]) is not int:
        return
    slot_kinds = [k for k in evidence.ROOT_KINDS if k not in evidence.POOL_KINDS]
    for k in slot_kinds:
        if k in skip:
            continue
        data = {"root_id": h(f"root|{slot}")} if k == "source_identity" else (
            {"vector": h("tree")} if k.startswith("tree_vector") else {"result": "consistent"})
        _ev(env, k, "slot", slot, s["gen"], s["op_id"], s["cycle"], data=overrides.get(k, data), at=at)


# ------------------------------------------------------------------ handoff (exercises the H-EST interface)

def maybe_handoff(env, slot):
    """A-N1 auto_handoff: once a slot is QUIESCENT at P13 with no `superseded`
    for its open cycle, write the synthetic handoff."""
    from .. import lifecycle
    w = env.world
    try:
        entries = w.ledger.entries()
        facts = invocation._read_slot(w, slot, entries)
        c = lifecycle.Components(invocation._g(w), facts.l, facts.a, facts.f, facts.r, facts.k, facts.op)
        if lifecycle.row(c) != "P13":
            return None
        docs = freeze.burned_docs(w, entries)
        cyc = [x for x in freeze.cycles(w, slot, docs) if x.op_id == facts.op_id][-1]
        if slot in chain(env, cyc.barrier_id).superseded:
            return None
    except Exception:  # noqa: BLE001 - an unreadable world: the gate itself refuses
        return None
    observe(env, slot)
    return synthetic_handoff(env, slot)


def synthetic_handoff(env, slot):
    """PR-4 containment-handoff, seeded through the PR-3a H-EST interface: the
    proposed document is validated unburned (N-R4.1-1), two establishment
    verdicts pass handoff_allowed, then the document is burned and
    `superseded` written."""
    w = env.world
    entries = w.ledger.entries()
    docs = freeze.burned_docs(w, entries)
    facts = invocation._read_slot(w, slot, entries)
    cyc = [c for c in freeze.cycles(w, slot, docs) if c.op_id == facts.op_id][-1]
    doc = _owner_doc(env, "containment-handoff", slot=slot, op_id=facts.op_id, slot_generation=cyc.g_pre,
                     cycle_id=cyc.cycle_id, barrier_id=cyc.barrier_id)
    prov = w.o5.preservation
    pres, verdicts = [], []
    for _ in ("V", "R"):
        pre, sel = freeze.basis_for(w, slot, "handoff", doc=doc)
        pres.append(pre.digest)
        verdicts.append(prov.handoff_establish(pre, sel.public(), None) if prov is not None else None)
        last_sel = sel
    c = chain(env, cyc.barrier_id)
    barrier.handoff_allowed(oc.mode(w), c, barrier.ESTABLISHED, "P13", slot, cyc, w.gates.stopped(), verdicts, pres,
                            lambda v: freeze.guard(v, "H-EST", v.pre, last_sel, oc.H_EST_KINDS))
    auth = burn(env, doc)
    return append_record(env, cyc.barrier_id, "superseded", auth=auth, detail={
        "slot": slot, "cycle_id": cyc.cycle_id, "g_pre": cyc.g_pre, "root_id": c.root_ids[slot],
        "verdicts": [v.digest for v in verdicts]})


# ------------------------------------------------------------------ PR-3b doubles

def _digest_of(selection, kind):
    for k, d in selection.get("evidence", []) + selection.get("provider_inputs", []):
        if k == kind:
            return d
    return None


class SyntheticPreservation:
    """Every Wiring.preservation function (R3 A-N1). Decisions PROCEED citing the
    selection; handoff verdicts pass; discharge is always None."""

    def __init__(self, observer, refuse=None):
        self.observer, self.refuse = observer, refuse or {}
        self.calls = []

    def inputs(self, verb, row):
        return ()

    def decide(self, ctx, selection, view):
        pre = ctx.pre
        spec = freeze.cell_spec(pre.verb, pre.row, pre.f, False)
        cell = spec.decision
        self.calls.append(("decide", cell))
        fields = {}
        if cell == "D-PRES":
            fields = self.observer.preservation(pre.slot, pre.op_id)
        elif cell == "D-READY":
            fields = self.observer.final_check(pre.slot, pre.op_id)
        code, detail = self.refuse.get(cell, (None, None))
        return oc.PreservationDecision(
            oc._PSEAL, cell=cell, ctx=ctx.digest,
            inputs={k: _digest_of(selection, k) for k in oc.DECISION_CELLS[cell][2]},
            result="REFUSE" if code else "PROCEED", code=code, detail=detail, doc_fields=fields)

    def _verdict(self, cell, phase, pre, selection, kinds):
        self.calls.append((cell,))
        return oc.HandoffVerdict(oc._PSEAL, cell=cell, phase=phase, pre=pre.digest, slot=pre.slot, op_id=pre.op_id,
                                 cycle_id=pre.cycle_id, g_pre=pre.g_pre,
                                 inputs={k: _digest_of(selection, k) for k in kinds},
                                 passed=cell not in self.refuse, reasons=[])

    def handoff_establish(self, pre, selection, view):
        return self._verdict("H-EST", "establish", pre, selection, oc.H_EST_KINDS)

    def handoff_validate(self, pre, selection, view):
        return self._verdict("H-VAL", "validate", pre, selection, oc.h_val_kinds(pre.f))

    def discharge(self, view, holder, pre, selection):
        self.calls.append(("discharge",))
        return None

    def check_fresult(self, ctx, verb, fresult, cycle_id):
        """Accepts only a result stamped by the effect with this exact cycle
        identity, and, past freeze-start, only the context's own cycle."""
        self.calls.append(("check_fresult", verb, cycle_id))
        if "check_fresult" in self.refuse:
            raise ValueError("synthetic check_fresult rejection")
        pre = ctx.pre
        if pre.cycle_id is not None and cycle_id != pre.cycle_id:
            raise ValueError("synthetic check_fresult: cycle identity is not the context's cycle")
        if fresult.bindings != stamp(fresult.bindings, pre.slot, pre.op_id, cycle_id):
            raise ValueError("synthetic check_fresult: result not stamped with this cycle identity")


class DischargeDouble(SyntheticPreservation):
    """Returns a well-formed D3/D4/D3H/D4H for the pure release unit tests (A-N6)."""

    def discharge(self, view, holder, pre, selection):
        self.calls.append(("discharge",))
        row = view.row
        kind = ("D4" if row == "P18" else "D3") + ("" if holder.id == pre.release_target else "H")
        sup = holder.superseded[view.slot]
        return oc.Discharge(oc._PSEAL, cell=oc.DISCHARGE_CELLS[kind], pre=pre.digest, slot=view.slot,
                            cycle_id=view.current.cycle_id, g_pre=view.current.g_pre, holder=holder.id,
                            superseded=sup[1], kind=kind,
                            inputs={k: _digest_of(selection, k) for k in oc.discharge_kinds(kind, view.f)},
                            verdict="")


def stamp(names, slot, op, cycle_id):
    return {b: h(f"{b}|{slot}|{op}|{cycle_id}") for b in names}


def expect_cycle(world, slot, op, cycle_id):
    """The synthetic effect's own check: `cycle_id` must name the cycle that
    PR-3a's F WAL derivation gives for the operation's pending intent."""
    mine = [c for c in freeze.cycles(world, slot, freeze.burned_docs(world, world.ledger.entries()))
            if c.op_id == op]
    if not mine or mine[-1].cycle_id != cycle_id:
        raise ValueError("synthetic effect: cycle identity mismatch")
    return cycle_id


def fresult(value, verb, slot, op, cycle_id):
    """A harness effect result inside the F relation, with deterministic bindings
    stamped with the cycle identity."""
    reasons, binds = freeze.RELATION[verb][2][value]
    # The effect's first durable write is the journal header (F4 §7.3).
    return oc.FResult(value, reasons[0], stamp(binds + ("journal",), slot, op, cycle_id))

"""One owner invocation: phases V, P, C, R, W (A1 §A2). V, P, C and R write
nothing and are the only places a refusal can come from; W writes in the
order of A1 §A2.2 under the CommitTxn that R issues, and every failure there is
UNFINISHED. Stores are the interfaces of stores.py, custody.py, ledger.py and
library.py; PR-1 drives them only with in-memory fakes and synthetic library
stores."""

import json

import recovery_state as rs

from . import anchor, authz, canonical, custody, ledger, libview, lifecycle, opclass, protocol, provenance
from .refusal import Code, Outcome, Refused, Result
from .stores import AuthzStore, Crash, FaultPlan, FWal, GateRecords, TierC

LIBRARY_STEP = {"request": "request", "cancel": "cancel", "quarantine": "quarantine",
                "quiescence": "verify_quiescence", "preservation": "record_preservation", "ready": "mark_ready"}
TOKEN_STEPS = ("quarantine", "quiescence", "preservation", "ready")
F_STEP = {"freeze-start": ("MOVE_INTENT",), "freeze-continue": ("REVOKE_INTENT",), "thaw": ("THAW_INTENT",),
          "snapshot": ("SNAPSHOT_INTENT",), "verify-preservation": ("VERIFY_INTENT",)}
F_INTENT_VERB = {"MOVE_INTENT": "freeze-start", "REVOKE_INTENT": "freeze-continue", "THAW_INTENT": "thaw",
                 "SNAPSHOT_INTENT": "snapshot", "VERIFY_INTENT": "verify-preservation"}


class World:
    """Every store one invocation reads or writes, plus injected producers:
    observer (PR-2/PR-3 evidence), freeze (PR-3 effects), backups (OD-6),
    and the id, token and nonce sources."""

    def __init__(self, library, ctx, *, clock, observer, freeze, backups, ids, tokens, nonces, faults=None,
                 principals_ok=True, one_way=True, window=300, cap=ledger.CAP):
        self.faults = faults if faults is not None else FaultPlan()
        self.library = library
        self.library.faults = self.faults
        self.ctx = ctx
        self.clock, self.observer, self.freeze, self.backups = clock, observer, freeze, backups
        self.ids, self.tokens, self.nonces = ids, tokens, nonces
        self.principals_ok, self.one_way, self.window, self.cap = principals_ok, one_way, window, cap
        self.ledger = ledger.MemLog(ledger.LEDGER, "ledger", self.faults, cap)
        self.anchors = {}
        self.authz = AuthzStore(self.faults)
        self.custody = custody.InMemoryCustody(self.faults,
                                               is_capability=lambda p: authz.may_retrieve(p, ctx.store))
        self.tierc = TierC(self.faults)
        self.fwal = FWal(self.faults)
        self.gates = GateRecords(self.faults)

    def anchor_log(self, slot, create=False):
        log = self.anchors.get(slot)
        if log is None:
            log = ledger.MemLog(ledger.ANCHOR, f"anchor:{slot}", self.faults, self.cap)
            if create:
                self.anchors[slot] = log
        return log

    def slots(self):
        return sorted(set(self.tierc.records) | set(self.anchors))

    def snapshot(self):
        snap = {"ledger": self.ledger.snapshot(), "authz": self.authz.snapshot(), "custody": self.custody.snapshot(),
                "tierc": self.tierc.snapshot(), "fwal": self.fwal.snapshot(), "gates": self.gates.snapshot()}
        for slot, log in sorted(self.anchors.items()):
            snap[f"anchor:{slot}"] = log.snapshot()
        snap["library"] = canonical.serialize({s: self.library.snapshot_slot(s) for s in self.slots()})
        return snap

    def writes(self):
        return [(s.name, w) for s in [self.ledger, self.authz, self.custody, self.tierc, self.fwal, self.gates,
                                      self.library, *self.anchors.values()] for w in s.writes]


# ------------------------------------------------------------------ reads shared by V and R

def _doc(world, digest):
    data = world.authz.get(digest) if canonical.HEX64.match(str(digest)) else None
    if data is None:
        return None
    try:
        return protocol.parse_doc(data)
    except Refused:
        return None


def _journal_entries(view):
    return [json.loads(line) for line in view.journal.decode().splitlines()]


def _latest_op(world, slot, entries):
    ops = set(world.library.operations(slot))
    for e in reversed(entries):
        d = _doc(world, e["auth_digest"])
        if d and d.get("verb") == "adopt" and d.get("slot") == slot and d.get("op_id") in ops:
            return d["op_id"]
    return None


def i_orphan(world, entries):
    """A1 §A4.9: every auth_digest a durable effect binds is a committed burn,
    and every library operation has a committed adopt burn."""
    burned = {e["auth_digest"] for e in entries}
    adopt_docs = [d for d in (_doc(world, e["auth_digest"]) for e in entries) if d and d.get("verb") == "adopt"]
    refs = set()
    for slot in world.slots():
        log = world.anchor_log(slot)
        res = log.classify()
        if res["cls"] != ledger.UNCLASSIFIABLE:
            refs |= {e["auth_digest"] for e in res["entries"]}
        adopts = {d["op_id"] for d in adopt_docs if d.get("slot") == slot}
        for op_id in world.library.operations(slot):
            if op_id not in adopts:
                raise Refused(Code.LEDGER_UNAVAILABLE, "I-ORPHAN")
            try:
                view = world.library.read(slot, op_id)
            except Refused:
                continue
            refs |= set(view.rec["gates"].values())
            refs |= {e["evidence"] for e in _journal_entries(view) if e.get("from") is not None}
        refs |= {e["auth_digest"] for e in world.fwal.log(slot)}
    refs |= world.custody.references()
    refs |= {e["auth_digest"] for e in world.gates.entries}
    if refs - burned:
        raise Refused(Code.LEDGER_UNAVAILABLE, "I-ORPHAN")


def _f_component(world, slot, op_id):
    log = [e for e in world.fwal.log(slot) if e.get("op_id") == op_id]
    if not log:
        return "NONE", None
    last = log[-1]
    value = last.get("value")
    if value not in lifecycle.F_VALUES or (last.get("kind") == "intent") != (value in lifecycle.F_INTENTS):
        return "UNKNOWN", None
    return value, last


class Facts:
    """Everything phase V read for one slot verb (an immutable snapshot S)."""

    def __init__(self, **kw):
        self.__dict__.update(kw)


def _read_slot(world, slot, entries, op_override=None):
    text = world.tierc.read(slot)
    l_comp = libview.component(text, slot)
    alog = world.anchor_log(slot)
    a_comp = anchor.component(alog.data, alog.evidence)
    op_id = op_override or _latest_op(world, slot, entries)
    view, r, op, k_state, f, f_entry = None, lifecycle.R_NONE, None, None, "NONE", None
    if op_id is not None:
        try:
            view = world.library.read(slot, op_id)
        except Refused as e:
            r = lifecycle.R_BUSY if e.code is Code.BUSY else lifecycle.R_CORRUPT
        if view is not None:
            rec, pending = view.rec, view.pending
            reconcile = None
            if pending is not None and pending["to"] == rs.QUARANTINED and rec["seq"] != pending["seq"]:
                img = libview.token_free_image(text) if text is not None else None
                reconcile = {pending["effect"]["pre"]: "pre", pending["effect"]["post"]: "post"}.get(img)
                if reconcile is None:
                    r = lifecycle.R_CORRUPT
            if r is lifecycle.R_NONE:
                r = lifecycle.r_component(rec, pending, view.marker, reconcile)
            op = lifecycle.OpInfo(op_id, rec["identity"], lifecycle.g_pre(rec),
                                  "request" in rec["gates"], lifecycle.qi(rec, pending))
            k_state = world.custody.lookup(custody.recovery_key(slot, op_id, op.gen))
        f, f_entry = _f_component(world, slot, op_id)
    k = k_state.label if k_state is not None else "ABSENT"
    return Facts(slot=slot, text=text, l=l_comp, a=a_comp, alog=alog, op_id=op_id, view=view, r=r, op=op,
                 k=k, k_state=k_state, f=f, f_entry=f_entry)


def _g(world):
    return lifecycle.G(world.gates.stopped(), world.principals_ok, True, world.one_way)


def _is_burned_doc(world, entries):
    burned = {e["auth_digest"] for e in entries}
    return lambda d: d in burned and world.authz.get(d) is not None


def _lease_token(world, facts):
    key = custody.lease_key(facts.slot, facts.l.lid, facts.l.g)
    st = world.custody.lookup(key)
    if st.k == custody.CORRUPT:
        raise Refused(Code.CUSTODY_CORRUPT)
    if st.k not in (custody.PERSISTED, custody.BOUND):
        raise Refused(Code.CUSTODY_ABSENT)
    token = world.custody.retrieve(key, world.ctx.principal)
    return token, libview.view(facts.text, facts.slot, token)


def _recovery_token(world, facts, *, verifier_written):
    key = custody.recovery_key(facts.slot, facts.op_id, facts.op.gen)
    token = world.custody.retrieve(key, world.ctx.principal)
    rec = facts.view.rec
    if verifier_written and not libview.hmac.compare_digest(libview.verifier(rec["salt"], token), rec["verifier"]):
        raise Refused(Code.CUSTODY_MISMATCH)
    return key, token


def _no_remint(world, entries, slot, op_id, gen):
    """A1 §A1.7 I-NOREMINT for a quarantine mint."""
    target = canonical.digest({"verb": "quarantine", "slot": slot, "op_id": op_id, "slot_generation": gen})
    if any(e["binding_digest"] == target for e in entries):
        raise Refused(Code.CUSTODY_ABSENT, "I-NOREMINT")


# ------------------------------------------------------------------ phase V

class Plan:
    def __init__(self, **kw):
        self.__dict__.update(kw)


def validate(world, op, fields):
    """Phase V. Returns a Plan; raises Refused; writes nothing."""
    g = _g(world)
    if not g.principals_ok:
        raise Refused(Code.GATE_ADMIN)
    ledger_plan = world.ledger.plan()
    entries = world.ledger.entries()
    if g.stop and op.verb not in ("resume", "resume-step", "stop"):
        raise Refused(Code.STOPPED)
    i_orphan(world, entries)
    plan = Plan(op=op, fields=fields, g=g, entries=entries, ledger_plan=ledger_plan, anchor_plan=None,
                facts=None, mode=None, row=None, resume_mode=None, already=False, extra={})
    if op.verb in ("stop", "resume"):
        plan.already = (op.verb == "stop") == world.gates.stopped()
    else:
        _validate_slot(world, plan)
    plan.ctx = {"verb": op.verb, "fields": fields, "g": list(g),
                "stores": {k: canonical.sha256(v) for k, v in world.snapshot().items()}}
    plan.ctx_digest = canonical.sha256(canonical.serialize(plan.ctx))
    return plan


def _validate_slot(world, plan):
    op, fields, entries = plan.op, plan.fields, plan.entries
    slot = fields["slot"]
    named = fields.get("op_id") if op.verb == "custody-destroy" else None
    facts = _read_slot(world, slot, entries, named)
    plan.facts = facts
    c = lifecycle.Components(plan.g, facts.l, facts.a, facts.f, facts.r, facts.k, facts.op)
    if op.verb == "custody-destroy":
        return _validate_destroy(world, plan, facts)
    if op.verb in LIBRARY_STEP and facts.view is not None:
        if fields["op_id"] != facts.op_id:
            raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "not the slot's current operation")
        _precheck(c)
        mode = lifecycle.completion_mode(facts.view.rec, facts.view.pending, facts.view.marker, op.verb,
                                         _is_burned_doc(world, entries))
        if mode == lifecycle.ALREADY_COMPLETE:
            plan.already = True
            return
        if mode != lifecycle.INITIATE:
            raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "only resume-step finishes it")
        plan.mode = mode
    if op.verb == "resume-step" and _nothing_to_resume(c):
        plan.already = True
        return
    if op.verb == "adopt":
        done = _adopt_already(world, entries, slot, fields["op_id"])
        if done:
            plan.already = True
            return
    plan.row, plan.resume_mode = lifecycle.decide(c, op.verb)
    if op.verb == "adopt" and fields["op_id"] in world.library.operations(slot):
        raise Refused(Code.STATE_UNKNOWN, "operation exists")
    if "op_id" in fields and op.verb not in ("adopt", "resume-step") and fields["op_id"] != facts.op_id:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "not the slot's current operation")
    if op.verb == "resume-step" and fields.get("op_id") not in (None, facts.op_id):
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "not the slot's current operation")
    writes_anchor = op.verb in lifecycle.TIER_C_VERBS or op.verb == "quarantine" or (
        op.verb == "resume-step" and (plan.resume_mode == "anchor_pending" or (
            plan.resume_mode == "library_pending" and facts.r.state == rs.QUARANTINED)))
    if writes_anchor:
        plan.anchor_plan = facts.alog.plan()
    _replica(world, plan, facts)


def _nothing_to_resume(c):
    """A second resume after completion writes nothing (A1 §A3.3)."""
    return (c.r.mode in ("none", "done", "marked") and c.a.kind == anchor.STABLE
            and c.f not in lifecycle.F_INTENTS and c.l.kind != libview.UNKNOWN)


def _precheck(c):
    """A1 §A2.6 steps 3–5 for a slot verb before completion_mode."""
    if c.r.mode == "busy":
        raise Refused(Code.BUSY)
    if c.r.mode == "corrupt" or c.a.kind in (anchor.ABSENT, anchor.CORRUPT) or c.l.kind == libview.UNKNOWN:
        raise Refused(Code.STATE_UNKNOWN)
    if (c.a.kind == anchor.STABLE and c.l.digest != c.a.digest) or (
            c.a.kind == anchor.PENDING and c.l.digest not in (c.a.pre, c.a.post)):
        raise Refused(Code.ANCHOR_MISMATCH)
    lifecycle.check_k(c.r, c.k, c.op)


def _adopt_already(world, entries, slot, op_id):
    """r4 §5.2 adopt row: ALREADY_COMPLETE, or STATE_UNKNOWN for other content."""
    if op_id not in world.library.operations(slot):
        return False
    view = world.library.read(slot, op_id)
    for e in reversed(entries):
        d = _doc(world, e["auth_digest"])
        if d and d.get("verb") == "adopt" and d.get("slot") == slot and d.get("op_id") == op_id:
            rec = view.rec
            if (rec["state"], rec["seq"], view.pending) == (rs.ACTIVE, 0, None) and (
                    rec["identity_kind"], rec["identity"], rec["slot_generation"]) == (
                    d["identity_kind"], d["identity"], d["slot_generation"]):
                return True
            break
    raise Refused(Code.STATE_UNKNOWN, "operation exists")


def _replica(world, plan, facts):
    """The pure replica of every library check for this call (r4 §8.2), plus
    custody preconditions. Runs in V and R; refuses before any write."""
    verb, entries = plan.op.verb, plan.entries
    trust = authz.trust_of(world.ctx)
    rec = facts.view.rec if facts.view is not None else None
    target = verb
    if verb == "resume-step" and plan.resume_mode in ("library_pending", "complete_terminal"):
        target = rs.TRANSITION[facts.view.pending["to"] if plan.resume_mode == "library_pending"
                               else rec["state"]]
        original_digest = facts.view.pending["evidence"] if plan.resume_mode == "library_pending" \
            else rec["gates"][target]
        original = _doc(world, original_digest)
        if original is None or not _is_burned_doc(world, entries)(original_digest) or \
                canonical.digest(original) != original_digest:
            raise Refused(Code.ORIGINAL_AUTHZ_UNAVAILABLE)
        plan.extra["original"], plan.extra["transition"] = original, target
    if target in ("request", "cancel", "quarantine") and trust == authz.TRUST_OWNER:
        raise Refused(Code.TRUST_INSUFFICIENT, "needs RS-1")
    if verb in ("adopt", "request", "cancel", "quarantine") or (
            verb == "resume-step" and target in ("request", "cancel")) or (
            verb == "resume-step" and target == "quarantine" and facts.r.reconcile == "pre"):
        lease_token, view_text = _lease_token(world, facts)
        plan.extra["view"] = view_text
        if rec is not None and target in ("request", "quarantine") and (
                facts.r.mode != "post") and not libview.hmac.compare_digest(
                libview.verifier(rec["salt"], lease_token), rec["verifier"]):
            raise Refused(Code.CUSTODY_MISMATCH)
        plan.extra["lease_token"] = lease_token
    if verb == "quarantine":
        key = custody.recovery_key(facts.slot, facts.op_id, facts.op.gen)
        if facts.k == "ABSENT":
            if facts.op.qi:
                raise Refused(Code.CUSTODY_ABSENT)
            _no_remint(world, entries, facts.slot, facts.op_id, facts.op.gen)
            plan.extra["mint_key"] = key
        else:
            token = world.custody.retrieve(key, world.ctx.principal)
            if libview.hmac.compare_digest(libview.verifier(rec["salt"], token), rec["verifier"]):
                raise Refused(Code.CUSTODY_CONFLICT, "recovery token equals the lease token")
            plan.extra["reuse_key"] = key
    if verb == "resume-step" and target in TOKEN_STEPS and plan.resume_mode in ("library_pending",
                                                                                 "complete_terminal"):
        if facts.k not in ("PERSISTED", "BOUND"):
            raise Refused(Code.CUSTODY_ABSENT)
        written = not (target == "quarantine" and facts.r.mode == "pre")
        plan.extra["key"], _ = _recovery_token(world, facts, verifier_written=written)
        if target == "quarantine":
            receipt = world.custody.lookup(plan.extra["key"]).record["receipt"]
            if plan.extra["original"].get("custody_receipt_digest") != canonical.digest(receipt):
                raise Refused(Code.CUSTODY_MISMATCH, "custody does not hold the authorized token")
    if verb in ("quiescence", "preservation", "ready"):
        plan.extra["key"], _ = _recovery_token(world, facts, verifier_written=True)
    if verb == "quiescence":
        if facts.view.rec.get("quiescence_method") != plan.fields["method"]:
            raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "method differs from the authorized one")
        if not all(plan.fields[f"accounted_{c}"] for c in protocol.QUIESCENCE_ACCOUNTED):
            raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED)
    if verb == "ready":
        final = world.observer.final_check(facts.slot, facts.op_id)
        if final["head"] != rec.get("preserved_head") or final["clean"] is not True \
                or final["lease_absent"] is not True:
            raise Refused(Code.NOT_CLEAN)


def _validate_destroy(world, plan, facts):
    if facts.view is None:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE)
    if facts.r.mode in ("corrupt", "busy"):
        raise Refused(Code.STATE_UNKNOWN if facts.r.mode == "corrupt" else Code.BUSY)
    terminal = world.library.terminal(facts.slot, facts.op_id)
    action = lifecycle.custody_row(facts.r, facts.k, facts.op, terminal)
    if action == "done":
        plan.already = True
        return
    if facts.a.kind == anchor.PENDING and (_doc(world, facts.a.auth_digest) or {}).get("op_id") == facts.op_id:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "unresolved anchor intent")
    rec = facts.view.rec
    key = custody.recovery_key(facts.slot, facts.op_id, facts.op.gen)
    st = facts.k_state
    if action == "full":
        world.custody.retrieve(key, world.ctx.principal)
        if facts.r.state == rs.READY:
            q = [e for e in _journal_entries(facts.view) if e["to"] == rs.QUARANTINED and e["kind"] == "complete"]
            intent = canonical.digest({k: q[-1][k] for k in rs.ENTRY_KEYS if k in q[-1]})
            if st.k == custody.BOUND and st.record["bound"] != [intent, rec["gates"]["quarantine"]]:
                raise Refused(Code.CUSTODY_CONFLICT)
        elif st.record["bound"] != []:
            raise Refused(Code.CUSTODY_CONFLICT)
    marker = facts.view.marker
    backup = world.backups(canonical.sha256(marker))
    if not backup:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "no backup newer than the marker")
    plan.row, plan.mode = "CD-R" if facts.r.state == rs.READY else "CD-C", action
    receipt_digest = canonical.digest(st.record["receipt"]) if st.record else st.tombstone["receipt_digest"]
    plan.extra.update(key=key, backup=backup, receipt_digest=receipt_digest,
                      terminal_ref={"state": facts.r.state, "marker_digest": canonical.sha256(marker),
                                    "journal_digest": canonical.sha256(facts.view.journal)})


# ------------------------------------------------------------------ phase P

def _components(world, verb, facts):
    """Evidence components from the injected producers (PR-2/PR-3)."""
    getter = getattr(world.observer, "components", None)
    return getter(verb, facts.slot, facts.op_id) if getter else []


def prepare(world, plan):
    """Phase P: in memory only. Returns (doc, doc_bytes, nonce, handle)."""
    op, fields, facts = plan.op, plan.fields, plan.facts
    nonce = world.nonces()
    issued_at = world.clock()
    handle = None
    if op.verb == "acquire":
        plan.extra["lease_id"] = world.ids()
        plan.extra["mint_key"] = custody.lease_key(facts.slot, plan.extra["lease_id"], facts.l.g + 1)
        if world.custody.lookup(plan.extra["mint_key"]).k != custody.ABSENT:
            raise Refused(Code.CUSTODY_CONFLICT)
    if "mint_key" in plan.extra:
        handle = custody.mint(plan.extra["mint_key"], world.tokens)
    comps = _components(world, op.verb, facts) if facts is not None else []
    repair = [p for p in (plan.ledger_plan, plan.anchor_plan) if p is not None]
    a_digest = canonical.digest({k: v for k, v in facts.a._asdict().items() if v is not None}) \
        if facts is not None else canonical.digest({})
    name = op.verb
    doc = authz.header(world.ctx, op, reason=fields["reason"], evidence=provenance.evidence(comps),
                       anchor_digest=a_digest, tierp_state_digest=plan.ctx_digest,
                       nonce_digest=canonical.sha256(nonce.encode("ascii")), issued_at=issued_at,
                       window=world.window, repair_plan=repair, doc=name)
    if facts is not None:
        doc["slot"] = facts.slot
        _doc_fields(world, plan, doc, facts, handle)
    data = authz.finish(doc)
    return doc, data, nonce, handle


def _binding(rec, transition):
    b = rs.RecoveryStore._binding(rec, transition, False)
    return {k: b[k] for k in authz.BINDING_KEYS}


def _doc_fields(world, plan, doc, facts, handle):
    verb, fields, rec = plan.op.verb, plan.fields, facts.view.rec if facts.view is not None else None
    if verb in LIBRARY_STEP:
        doc.update(_binding(rec, verb))
    if verb == "quarantine":
        receipt = handle.receipt if handle else world.custody.lookup(plan.extra["reuse_key"]).record["receipt"]
        doc.update(quiescence_method=fields["quiescence_method"],
                   preservation_disposition=fields["preservation_disposition"],
                   assessment_digest=world.observer.assessment(facts.slot, facts.op_id),
                   custody_receipt_digest=canonical.digest(receipt))
    elif verb == "quiescence":
        names = [c["name"] for c in doc["evidence"]["components"]]
        if not 1 <= len(names) <= 32:
            raise Refused(Code.PROVENANCE_INSUFFICIENT, "quiescence needs evidence references")
        doc.update(method=fields["method"], evidence_refs=names,
                   accounted={c: True for c in protocol.QUIESCENCE_ACCOUNTED})
    elif verb == "preservation":
        doc.update(world.observer.preservation(facts.slot, facts.op_id))
    elif verb == "ready":
        final = world.observer.final_check(facts.slot, facts.op_id)
        doc.update(clean=final["clean"], lease_absent=final["lease_absent"], head=rec["preserved_head"],
                   preservation=rec["gates"]["preservation"])
    elif verb == "adopt":
        doc.update(op_id=fields["op_id"], slot_generation=facts.l.g, lease_id=facts.l.lid, identity_kind="v2",
                   identity=facts.l.lid, tierc_digest=facts.l.digest)
    elif verb == "acquire":
        doc.update(slot_generation=facts.l.g + 1, lease_id=plan.extra["lease_id"], task=fields["task"],
                   custody_receipt_digest=canonical.digest(handle.receipt), tierc_digest=facts.l.digest)
    elif verb in ("release", "mark-suspect", "clear-suspect"):
        doc.update(slot_generation=facts.l.g, tierc_digest=facts.l.digest)
        if facts.l.lid:
            doc["lease_id"] = facts.l.lid
    elif verb in F_STEP:
        doc.update(op_id=facts.op_id, slot_generation=facts.op.gen, f_from=facts.f)
    elif verb == "custody-destroy":
        doc.update(op_id=facts.op_id, key_digest=canonical.digest(plan.extra["key"]),
                   receipt_digest=plan.extra["receipt_digest"], terminal_ref=plan.extra["terminal_ref"],
                   backup_ref=plan.extra["backup"])
    elif verb == "resume-step":
        _resume_fields(world, plan, doc, facts)


def _resume_fields(world, plan, doc, facts):
    mode = plan.resume_mode
    doc["mode"] = mode
    if mode in ("library_pending", "complete_terminal"):
        rec, original = facts.view.rec, plan.extra["original"]
        t = plan.extra["transition"]
        doc["class"] = opclass.lookup(opclass.POOLCTL_OWNER, original["verb"]).cls
        if mode == "library_pending":
            pending = facts.view.pending
            target = canonical.digest({k: pending[k] for k in rs.ENTRY_KEYS if k in pending})
            seq = pending["seq"]
        else:
            target, seq = rec["gates"][t], rec["seq"]
        doc.update(op_id=facts.op_id, transition=t, slot_generation=original["slot_generation"], seq=seq,
                   target_digest=target, journal_digest=canonical.sha256(facts.view.journal),
                   original_authorization_digest=canonical.digest(original))
    elif mode == "anchor_pending":
        entries = facts.alog.classify()["entries"]
        doc.update(target_digest=entries[-1]["line_digest"], slot_generation=facts.l.g,
                   transition=facts.a.verb)
        if facts.op_id:
            doc["op_id"] = facts.op_id
    else:
        doc.update(op_id=facts.op_id, slot_generation=facts.op.gen, transition=F_INTENT_VERB[facts.f],
                   target_digest=canonical.digest(facts.f_entry))


# ------------------------------------------------------------------ the invocation

def invoke(world, raw, confirmer, entry=opclass.POOLCTL_OWNER):
    """Runs one owner invocation and returns a Result. Crash (a simulated
    process stop) propagates."""
    try:
        op, fields = protocol.parse_request(raw, entry)
        opclass.authorize(entry, op.verb, owner=authz.is_owner(world.ctx))
        if op.cls == opclass.READ_ONLY:
            return Result(Outcome.COMMITTED, detail=_report(world, op, fields))
        authz.trust_of(world.ctx)
        plan = validate(world, op, fields)
        if plan.already:
            return Result(Outcome.ALREADY_COMPLETE)
        doc, data, nonce, handle = prepare(world, plan)
        digest = authz.confirm(data, nonce, confirmer, world.ctx.principal)
        confirmed_at = world.clock()
        again = validate(world, op, fields)
        if again.already:
            raise Refused(Code.CONTEXT_CHANGED)
        if handle is not None and world.custody.lookup(handle.key).k != custody.ABSENT:
            raise Refused(Code.CONTEXT_CHANGED)
        burned = {e["auth_digest"] for e in again.entries} | {e["nonce_digest"] for e in again.entries}
        txn = authz.revalidate(doc, digest, plan.ctx_digest, again.ctx_digest, confirmed_at, burned,
                               world.ctx.principal)
    except Refused as e:
        return Result(Outcome.REFUSED, e.code, detail=e.detail)
    done = []
    try:
        _write(world, plan, doc, data, digest, handle, txn, confirmed_at, done)
    except Crash:
        raise
    except Exception:
        return Result(Outcome.UNFINISHED, durable_ref=done)
    finally:
        txn.close()
    return Result(Outcome.COMMITTED)


def _report(world, op, fields):
    """READ_ONLY verbs: classes, offsets and component kinds; never a token."""
    out = {"ledger": world.ledger.classify()["cls"]}
    if "slot" in fields:
        log = world.anchor_log(fields["slot"])
        out["anchor"] = log.classify()["cls"]
        out["tierc"] = libview.component(world.tierc.read(fields["slot"]), fields["slot"]).kind
    return json.dumps(out, sort_keys=True)


# ------------------------------------------------------------------ phase W

def _write(world, plan, doc, data, digest, handle, txn, confirmed_at, done):
    verb, facts = plan.op.verb, plan.facts
    if handle is not None:
        receipt = world.custody.persist(txn, handle)
        done.append(f"custody:{canonical.digest(receipt)}")
    world.authz.put(txn, data)
    done.append(f"authz:{digest}")
    burn = world.ledger.append(txn, plan.ledger_plan, {
        "auth_digest": digest, "auth_kind": "resume" if verb == "resume-step" else "initiate",
        "binding_digest": authz.binding_digest(doc), "confirmed_at": confirmed_at, "issued_at": doc["issued_at"],
        "nonce_digest": doc["nonce_digest"]})
    done.append(f"ledger:{burn['line_digest']}")
    anchor_plan = [plan.anchor_plan]

    def anchor_append(kind, pre, post, averb):
        log = world.anchor_log(facts.slot, create=True)
        e = log.append(txn, anchor_plan[0], anchor.entry_fields(kind, pre, post, averb, digest))
        anchor_plan[0] = None
        done.append(f"anchor:{e['line_digest']}")

    if verb in ("stop", "resume"):
        world.gates.append(txn, {"kind": verb, "auth_digest": digest})
    elif verb in lifecycle.TIER_C_VERBS:
        _tierc_verb(world, plan, facts, txn, anchor_append, handle)
    elif verb == "adopt":
        world.library.call(txn, "adopt", facts.slot, plan.fields["op_id"], plan.extra["view"])
    elif verb in F_STEP:
        _f_step(world, facts, txn, verb, digest)
    elif verb == "custody-destroy":
        _destroy(world, plan, txn, digest, done)
    elif verb == "resume-step":
        _resume(world, plan, facts, txn, anchor_append, digest, done)
    else:
        _library_step(world, plan, facts, txn, anchor_append, doc, done)
    done.append(f"{verb}:complete")


def _tierc_verb(world, plan, facts, txn, anchor_append, handle):
    verb, text, slot = plan.op.verb, facts.text, facts.slot
    if verb == "acquire":
        token = handle._token
        lines = ["version=2", f"slot={slot}", f"generation={facts.l.g + 1}", "state=leased",
                 f"lease_id={plan.extra['lease_id']}", f"task={plan.fields['task']}",
                 f"utc={authz.utc(world.clock())}", f"lease_token={token}"]
        new = libview.tierc_from_view("\n".join(lines) + "\n", world.ids())
    elif verb == "release":
        new = f"version=2\nslot={slot}\ngeneration={facts.l.g + 1}\nstate=free\n"
    else:
        new = libview.with_suspect(text, verb == "mark-suspect")
    pre, post = canonical.sha256(text.encode()), canonical.sha256(new.encode())
    anchor_append("pending", pre, post, verb)
    world.tierc.write(txn, slot, new)
    anchor_append("complete", pre, post, verb)


def _f_step(world, facts, txn, verb, digest):
    intent = F_STEP[verb][0]
    world.fwal.append(txn, facts.slot, {"op_id": facts.op_id, "kind": "intent", "value": intent,
                                        "auth_digest": digest})
    result = world.freeze(verb, facts.slot, facts.op_id)
    world.fwal.append(txn, facts.slot, {"op_id": facts.op_id, "kind": "complete", "value": result,
                                        "auth_digest": digest})


def _quarantine_hook(world, txn, slot, expected_post):
    def hook(pre, post):
        if post != expected_post or libview.token_free_image(world.tierc.read(slot)) != rs.image_digest(pre):
            raise ValueError("live Tier C record is not the pre-image")
        world.tierc.write(txn, slot, post)
        return post
    return hook


def _intent_digest(world, slot, op_id):
    view = world.library.read(slot, op_id)
    q = [e for e in _journal_entries(view) if e["to"] == rs.QUARANTINED and e["kind"] == "complete"]
    return canonical.digest({k: q[-1][k] for k in rs.ENTRY_KEYS if k in q[-1]}), view.rec["gates"]["quarantine"]


def _call_library(world, txn, facts, transition, document, token, record, hook=None):
    method = LIBRARY_STEP[transition]
    kw = {"op_id": facts.op_id, "record": record}
    if transition in ("request", "cancel", "quarantine"):
        kw["authorization"] = document
    elif transition == "ready":
        kw["final_check"] = document
    else:
        kw["evidence"] = document
    if transition in TOKEN_STEPS:
        kw["recovery_token"] = token
    if transition == "quarantine":
        kw["replace_record"] = hook
    world.library.call(txn, method, facts.slot, **kw)
    return method


def _library_step(world, plan, facts, txn, anchor_append, document, done):
    slot, verb = facts.slot, plan.op.verb
    if verb == "quarantine":
        key = plan.extra.get("mint_key") or plan.extra["reuse_key"]
        token = world.custody.retrieve(key, world.ctx.principal)
        pre_rec = rs.parse_record(plan.extra["view"], slot)
        post_text = rs.quarantined_record(slot, facts.op_id, facts.view.rec["slot_generation"] + 1, pre_rec)
        pre_d, post_d = canonical.sha256(facts.text.encode()), canonical.sha256(post_text.encode())
        anchor_append("pending", pre_d, post_d, "quarantine")
        _call_library(world, txn, facts, verb, document, token, plan.extra["view"],
                      _quarantine_hook(world, txn, slot, post_text))
        done.append("library:quarantine")
        anchor_append("complete", pre_d, post_d, "quarantine")
        world.custody.mark_bound(txn, key, list(_intent_digest(world, slot, facts.op_id)))
        done.append("custody:bound")
        return
    token = None
    if verb in TOKEN_STEPS:
        token = world.custody.retrieve(plan.extra["key"], world.ctx.principal)
    if verb == "quiescence" and facts.k == "PERSISTED":
        world.custody.mark_bound(txn, plan.extra["key"], list(_intent_digest(world, slot, facts.op_id)))
        done.append("custody:bound")
    record = plan.extra["view"] if verb in ("request", "cancel") else facts.text
    _call_library(world, txn, facts, verb, document, token, record)
    done.append(f"library:{verb}")


def _resume(world, plan, facts, txn, anchor_append, digest, done):
    mode, slot = plan.resume_mode, facts.slot
    if mode == "anchor_pending":
        a = facts.a
        if plan.row == "P10a" and facts.k == "PERSISTED":
            world.custody.mark_bound(txn, custody.recovery_key(slot, facts.op_id, facts.op.gen),
                                     list(_intent_digest(world, slot, facts.op_id)))
            done.append("custody:bound")
        complete = plan.row == "P10a" or (plan.row == "P21" and facts.l.digest == a.post)
        anchor_append("complete" if complete else "abort", a.pre, a.post, a.verb)
        return
    if mode == "f_intent":
        verb = F_INTENT_VERB[facts.f]
        result = world.freeze(verb, slot, facts.op_id)
        world.fwal.append(txn, slot, {"op_id": facts.op_id, "kind": "complete", "value": result,
                                      "auth_digest": digest})
        return
    t, original = plan.extra["transition"], plan.extra["original"]
    token = None
    if t in TOKEN_STEPS:
        token = world.custody.retrieve(plan.extra["key"], world.ctx.principal)
    if t == "quarantine":
        if facts.r.reconcile == "pre":
            pre_rec = rs.parse_record(plan.extra["view"], slot)
            post_text = rs.quarantined_record(slot, facts.op_id, facts.view.rec["slot_generation"] + 1, pre_rec)
            record, hook = plan.extra["view"], _quarantine_hook(world, txn, slot, post_text)
        else:
            record, hook = facts.text, (lambda pre, post: post)
        _call_library(world, txn, facts, t, original, token, record, hook)
        done.append("library:quarantine")
        a = facts.a
        anchor_append("complete", a.pre, a.post, "quarantine")
        key = plan.extra["key"]
        world.custody.mark_bound(txn, key, list(_intent_digest(world, slot, facts.op_id)))
        done.append("custody:bound")
        return
    if t in ("request", "cancel"):
        record = plan.extra["view"]
    else:
        record = facts.text
    _call_library(world, txn, facts, t, original, token, record)
    done.append(f"library:{t}")


def _destroy(world, plan, txn, digest, done):
    key = plan.extra["key"]
    if plan.mode == "full":
        st = world.custody.lookup(key)
        if not (st.k == custody.TERMINAL and st.record["terminal_ref"] == plan.extra["terminal_ref"]):
            world.custody.mark_terminal(txn, key, plan.extra["terminal_ref"])
            done.append("custody:terminal")
        world.custody.tombstone(txn, key, world.clock(), digest)
        done.append("custody:tombstone")
    world.custody.unlink(txn, key)
    done.append("custody:unlinked")

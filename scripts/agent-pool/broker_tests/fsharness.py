"""Test-only bridge from the approved R2 inert-model test API (fsmodel.py,
sha256 c10cecba...) to the production PR-4c capabilities.

Durable state lives in production stores of a synthetic world: the F WAL,
the capability-gated journal store, burned documents (authz store + ledger)
and the barrier store. Every effect runs through the production call site
`invocation._f_complete`: the one mint (`authz_txn._issue_effect`), the
seven-argument effect, the seal, LP-RESULT (`fscap.reconcile`),
`freeze.check_result`, the PR-3b check (here the model's stand-in rows), the F
WAL completion and the `refreshed` record. The "filesystem" is the model's
JSON world, served by `Backend`, which only tests install in
`fscap._BACKEND`. Nothing here touches a real filesystem object; the model's
faults and pause hooks arrive through the production fault points
(`FaultPlan.hit`). Lives outside the broker package, so it is never part of an
installed broker."""

import hashlib
import json
import threading
import time
import types

import harness  # noqa: F401  (puts the agent-pool directory on sys.path)

from broker import (authz, authz_txn, canonical, fscap, invocation, journal as J, ledger, o5contract as oc,
                    freeze as F)
from broker.stores import AuthzStore, Crash, FaultPlan, FWal

FSCAP_OPEN, FSCAP_POISONED, FSCAP_SEALED = fscap.FSCAP_OPEN, fscap.FSCAP_POISONED, fscap.FSCAP_SEALED
Refused, Unfinished = fscap.Refused, fscap.Unfinished
PROTECTED, SUCCESS, LOCATION, WALKS = fscap.PROTECTED, fscap.SUCCESS, fscap.LOCATION, fscap.WALKS
ROOT, X = "a" * 64, "b" * 64                 # root_id and a substitute object key
SP, QD = "1" * 64, "2" * 64                  # slot parent and quarantine/<op_id> keys (the pinned layout)
SN, DN = b"slot".hex(), b"tree".hex()
EARLY = "e" * 64
A_START, A_CONT, A_THAW, A_THAW2 = "c1" * 32, "c4" * 32, "c2" * 32, "c3" * 32   # authorization tokens
ROOT_DEV = 1
SLOT, OP, GEN, BID, HEAD = "codegraph-worker-x", "op", 1, "b0" * 32, "d0" * 32
HOOKS = {}                                   # test-only pause points: name -> callable
FAULTS = set()                               # test-only injected failures (model names)
CRASH = {"at": None}                         # in-process crash point (replaces the model's os._exit child)
GUARD = 30                                   # deadlock guard for barriers; never a timing assumption
EVENTS = threading.Condition()


def h(x):
    return canonical.digest(x)


def await_(pred, what):
    """Poll a predicate that may depend on production state (the authority lock) that never notifies EVENTS."""
    end = time.monotonic() + GUARD
    with EVENTS:
        while not pred():
            if time.monotonic() > end:
                raise AssertionError(f"barrier not reached: {what}")
            EVENTS.wait(0.002)


def signal():
    with EVENTS:
        EVENTS.notify_all()


class Gate:
    """Deterministic pause point: arrive() blocks until release(); tests await `arrived`."""

    def __init__(self):
        self.arrived = self.released = False

    def arrive(self):
        with EVENTS:
            self.arrived = True
            EVENTS.notify_all()
            if not EVENTS.wait_for(lambda: self.released, GUARD):
                raise AssertionError("gate never released")

    def release(self):
        with EVENTS:
            self.released = True
            EVENTS.notify_all()


# ---------------------------------------------------------------- faults and hooks through production fault points

_ERRORS = {
    "fwal:append:before": ("fwal:append:before", 5, "fwal append failed before the write"),
    "fwal:append:after": ("fwal:append:after", 5, "fwal append raised after the write"),
    "journals:get:before": ("journal:read", 5, "journal read failed"),
    "journals:put:before": ("journal:put:before", 28, "journal put failed before the write"),
    "journals:put:after": ("journal:put:after", 5, "journal put raised after the write"),
    "barriers:put:before": ("barrier:put:before", 5, "refreshed put failed before the write"),
    "barriers:put:after": ("barrier:put:after", 5, "refreshed put raised after the write"),
}
_CRASH_POINTS = {"fscap:poisoned": "poisoned", "fscap:seal_enter": "effect_returned", "fscap:sealed": "sealed",
                 "fwal:append:before": "completion_prepared", "fwal:append:after": "completion_durable"}


class Faults(FaultPlan):
    def hit(self, point):
        if CRASH["at"] is not None and _CRASH_POINTS.get(point) == CRASH["at"]:
            raise Crash(point)
        if point.startswith("fscap:"):
            hook = HOOKS.get(point[6:])
            if hook:
                hook()
            return
        if point == "fwal:append:before" and "completion_prepared" in FAULTS:
            raise RuntimeError("call site stopped before the completion append")
        name, code, msg = _ERRORS.get(point, (None, 0, ""))
        if name in FAULTS:
            raise OSError(code, msg)


class _FWal(FWal):
    """The production F WAL; remembers the last entry the call site tried to append (the model's `prepared`)."""

    def __init__(self, faults):
        super().__init__(faults)
        self.prepared = None

    def append(self, txn, slot, entry):
        if entry.get("kind") == "complete":
            self.prepared = dict(entry)
        super().append(txn, slot, entry)


class _Stub:
    """The model's check_fresult stand-in for the PR-3b rows it reaches (the real check needs the PR-3b reader)."""

    def __init__(self, disk):
        self.disk = disk

    def check_fresult(self, ctx, verb, res, cid):
        _check_fresult(self.disk, verb, res.value, res.reason, res.bindings)


# ---------------------------------------------------------------- the world (durable state)

def _pre(verb, cycle_id, start_auth):
    f = dict.fromkeys(oc.PreBasis.FIELDS)
    f.update(mode=oc.SYNTHETIC, verb=verb, slot=SLOT, op_id=OP, g_pre=GEN, cycle_id=cycle_id,
             freeze_start_auth=start_auth, cutover_head=HEAD, barrier_id=BID, root_id=ROOT)
    return oc.PreBasis(oc._CSEAL, **f)


def _ctx(pre):
    return oc.ContainmentContext(oc._CSEAL, pre=pre, basis="barrier", superseded=None, verdict=None)


START_CTX = _ctx(_pre("freeze-start", None, None))


def _doc(verb, f_from, token, **extra):
    d = {"verb": verb, "f_from": f_from, "op_id": OP, "slot": SLOT, "slot_generation": GEN, "token": token, **extra}
    if verb == "freeze-start":
        d["o5"] = {"ctx": START_CTX.digest}
    return d


START_AUTH = canonical.sha256(canonical.canon(_doc("freeze-start", "NONE", A_START)))
CID = oc.cycle_id(SLOT, GEN, OP, START_AUTH)
HEADER = {"slot": SLOT, "op_id": OP, "g_pre": GEN, "cycle_id": CID, "start_ctx": START_CTX.digest,
          "cutover_head": HEAD, "barrier_id": BID, "root_id": ROOT, "mode": oc.SYNTHETIC}
CTX = {"freeze-start": START_CTX, "other": _ctx(_pre("thaw", CID, START_AUTH))}


class Disk:
    def __init__(self, fs):
        self.faults = Faults()
        st = F.new_stores(self.faults)
        self.world = types.SimpleNamespace(
            ctx=authz.Ctx(authz.SyntheticOwner(), authz.SYNTHETIC), faults=self.faults,
            ledger=ledger.MemLog(ledger.LEDGER, "ledger", self.faults), authz=AuthzStore(self.faults),
            fwal=_FWal(self.faults), o5=oc.Wiring(st, _Stub(self)), clock=iter(range(1, 10 ** 9)).__next__,
            freeze=None)
        self._fs = json.dumps(fs)

    def fs(self):
        return json.loads(self._fs)

    def set_fs(self, fs):
        self._fs = json.dumps(fs)

    def fwal(self):
        return self.world.fwal.log(SLOT)

    def fwal_append(self, e):                 # test setup
        self.world.fwal.entries.setdefault(SLOT, []).append(dict(e))

    def journal(self):
        items, out = self.world.o5.stores["journals"].items, []
        while f"journal/{CID}/{len(out):08d}" in items:
            out.append(json.loads(items[f"journal/{CID}/{len(out):08d}"]))
        return out

    def journal_seed(self, kind, **fields):  # test setup: direct seeding of the test-owned store
        recs = self.journal()
        r = {"schema": J.J_SCHEMA_V3, "header": HEADER, "seq": len(recs),
             "prev": h(recs[-1]) if recs else oc.ZERO64, "kind": kind, **fields}
        if recs:
            _, _, early = fscap.walk(recs, ROOT)
            r["early"] = fields.get("binding") if kind == "ep.done" else (early if len(recs) > 1 else oc.ZERO64)
        fscap.walk(recs + [r], ROOT)
        self.world.o5.stores["journals"].items[f"journal/{CID}/{len(recs):08d}"] = canonical.canon(r)
        return h(r)

    def docs(self):
        burned = {e["auth_digest"] for e in self.world.ledger.entries()}
        return {d: json.loads(v) for d, v in self.world.authz.docs.items() if d in burned}

    def burn(self, doc, kind="initiate"):
        data = canonical.canon(doc)
        d = canonical.sha256(data)
        self.world.authz.docs[d] = data
        entries = self.world.ledger.entries()
        prev = entries[-1]["line_digest"] if entries else ledger.ZERO64
        e = ledger.make_entry(ledger.LEDGER, len(entries), prev, {
            "auth_digest": d, "auth_kind": kind, "binding_digest": canonical.digest({"d": d}),
            "confirmed_at": 1, "issued_at": 1, "nonce_digest": canonical.sha256(d.encode())})
        self.world.ledger.data += canonical.canon(e) + b"\n"
        return d

    def barrier(self):
        st = self.world.o5.stores["barriers"]
        return [{"kind": "refreshed", "completion": json.loads(st.items[k])["detail"]["completion"]}
                for k in st.keys()]


class Backend:
    """The model's filesystem world as an fscap backend: raw facts only; every classification is fscap's."""

    def __init__(self, disk):
        self.disk = disk

    def pin(self):
        return {"slot_parent": SP, "qdir": QD, "slot_name": SN, "dst_name": DN, "root_dev": ROOT_DEV}

    def observe(self, key):
        fs = self.disk.fs()
        if key in fs.get("io", ()):
            raise OSError(5, f"{key} observation failed")
        return fs

    def walk(self, key):
        objects = self.disk.fs().get("objects", "ok")
        if objects == "io":
            raise OSError(5, f"walk below quarantine/<op_id> failed ({key})")
        return objects

    def content(self, what):
        return self.disk.fs().get("content", {}).get(what, "ok")

    def rename(self, back):
        fs = self.disk.fs()
        err = fs.get("rename_error")
        if not err:
            src, dst = ("dst", "slot") if back else ("slot", "dst")
            fs[dst], fs[src] = fs[src], None
            self.disk.set_fs(fs)
        return err

    def peek(self, what):
        fs = self.disk.fs()
        return {"slot": fs["slot"], "slot_root": fs["slot"], "quarantine": [fs["dst"], fs.get("extra", [])],
                "walk_q": fs["dst"]}[what]


def walk(recs):
    """Merged journal/v3 record and grammar checks over the whole journal."""
    return fscap.walk(recs, ROOT)


def fstate(disk):
    return F.fold(disk.fwal(), disk.docs().get)


def cycle_bindings(disk):
    b = {}
    for e in disk.fwal():
        if e["kind"] == "intent" and e["value"] == "MOVE_INTENT":
            b = {}
        elif e["kind"] == "complete":
            b.update(e["bindings"])
    return b


# ---------------------------------------------------------------- invocation (drives the production call site)

_CURRENT = {}


class Invocation:
    def __init__(self, disk, verb, auth, resume_auth=None):
        self.disk, self.verb, self.auth, self.resume_auth = disk, verb, auth, resume_auth
        self.authority = self.fscap = self.prepared = self.txn = None

    def mint(self):
        """A second mint attempt through the production mint on this invocation's CommitTxn."""
        return authz_txn._issue_effect(self.txn, {}, None, oc.SYNTHETIC, {}, self.disk.faults)

    def complete(self, effect):
        w = self.disk.world
        self.txn = authz_txn._issue(self.resume_auth or self.auth, "0" * 64)

        def freeze(verb, slot, op, ctx, cid, cap, fs):
            self.authority, self.fscap = fs._a, fs
            out = effect(cap, fs)
            return oc.FResult(*out) if type(out) is tuple and len(out) == 3 else out
        w.freeze, w.fwal.prepared = freeze, None
        recs = [json.loads(w.o5.stores["barriers"].items[k]) for k in w.o5.stores["barriers"].keys()]
        chain = types.SimpleNamespace(state="ESTABLISHED", id=BID, records=recs,
                                      head=h(recs[-1]) if recs else oc.ZERO64,
                                      authorized=dict.fromkeys(F.barrier.IMMUTABLE, ""))
        gate = types.SimpleNamespace(ctx=CTX["freeze-start" if self.verb == "freeze-start" else "other"],
                                     chain=chain, basis="barrier", picked={})
        _CURRENT["disk"], fscap._BACKEND = self.disk, Backend(self.disk)
        try:
            invocation._f_complete(w, types.SimpleNamespace(o5=gate), types.SimpleNamespace(slot=SLOT, op_id=OP),
                                   self.txn, self.verb, self.resume_auth or self.auth, CID)
        finally:
            self.prepared = w.fwal.prepared
            self.txn.close()
            fscap._BACKEND = None
        return self.prepared["value"], self.prepared["reason"]


def FsCap(authority, inv):
    """A further wrapper of an invocation's authority (the model's `FsCap(authority, inv)`)."""
    assert inv.fscap._a is authority
    return inv.fscap.view()


def invoke(inv, effect):
    """Merged `invoke` semantics for phase W: any exception from the completion path is UNFINISHED."""
    try:
        value, reason = inv.complete(effect)
    except Exception as e:
        return "UNFINISHED", f"{type(e).__name__}: {e}"
    return "COMMITTED", value, reason


def _check_fresult(disk, verb, value, reason, bindings):
    if "check_fresult" in FAULTS:
        raise ValueError("fresult_rejected:injected")
    recs = disk.journal()
    tail = recs[-1] if recs else {}
    need = {("freeze-start", "MOVED"): tail.get("kind") == "move.done" and (bindings or {}).get("journal") == h(tail),
            ("freeze-start", "NONE"): (not recs and not bindings) if reason in ("store_space", "store_error")
            else tail.get("kind") == "end" and tail.get("reason") == reason,
            ("thaw", "NONE"): tail.get("kind") == "end" and tail.get("outcome") == "thawed"}
    if not need.get((verb, value), True):
        raise ValueError("fresult_rejected:journal_mismatch")


def check_fresult(inv, value, reason, bindings=None):
    _check_fresult(inv.disk, inv.verb, value, reason, bindings)


def inspect_durable(inv):
    """Independent read-only inspection (OD-S3), outside `invoke` and its result."""
    last = inv.disk.fwal()[-1] if inv.disk.fwal() else None
    durable = inv.prepared is not None and last == inv.prepared
    refreshed = durable and {"kind": "refreshed", "completion": canonical.digest(inv.prepared)} in inv.disk.barrier()
    return {"fwal": "completion_durable" if durable else "intent_pending", "refreshed": refreshed}


def refreshed_consistent(disk):
    done = {canonical.digest(e) for e in disk.fwal() if e["kind"] == "complete"}
    named = [r["completion"] for r in disk.barrier()]
    return set(named) <= done and len(named) == len(set(named))


# ---------------------------------------------------------------- selection (phase V) and W entry

def _intent(value, auth):
    return {"op_id": OP, "kind": "intent", "value": value, "auth_digest": auth}


def start_invocation(disk, verb, auth=None):
    """Select and, for a new verb, append its F WAL intent; refuse what the merged relation refuses."""
    f, last = fstate(disk)
    if verb == "resume-step":
        if f not in F.INTENT_VERB or last is None or last.get("kind") != "intent":
            raise Refused("nothing pending")
        target = F.INTENT_VERB[last["value"]]
        rdoc = disk.burn({"verb": "resume-step", "slot": SLOT, "op_id": OP, "transition": target,
                          "target_digest": canonical.digest(last), "n": len(disk.world.authz.docs)}, "resume")
        return Invocation(disk, target, last["auth_digest"], rdoc)
    if f not in F.RELATION[verb][0]:
        raise Refused(f"{verb} not selectable from {f}")
    if verb == "thaw" and not {"early", "journal"} <= set(cycle_bindings(disk)):
        raise Refused("evidence_missing")
    d = disk.burn(_doc(verb, f, auth))
    e = _intent(F.RELATION[verb][1], d)
    if F.fold(disk.fwal() + [e], disk.docs().get)[0] == "UNKNOWN":
        raise Refused("fold refuses the intent")
    disk.fwal_append(e)
    return Invocation(disk, verb, d)


# ---------------------------------------------------------------- effects (test code; the trusted effect is PR-4f)

def _disk():
    return _CURRENT["disk"]


def journal_through_move_intent(store):
    store.put("start")
    store.put("ep.done", binding=EARLY)
    store.put("move.intent", src_parent=SP, src_name=SN, dst_parent=QD, dst_name=DN, root=ROOT)


def finish_move(store, out):
    j = _disk().journal()
    if out == "completed":
        jd = store.put("move.done", intent=h(j[-1]), linked=[])
        return "MOVED", "", {"early": EARLY, "journal": jd}
    reason = out.split(":")[1]
    store.put("move.abort", intent=h(j[-1]), reason=reason)
    store.put("end", outcome="none", reason=reason)
    return "NONE", reason, {}


def effect_freeze_start(store, fs):
    journal_through_move_intent(store)
    return finish_move(store, fs.move())


def effect_resume_move(store, fs):
    """D2.2.1 resume of MOVE_INTENT: tail move.done -> MOVED unchanged; tail move.intent -> observe, then move."""
    if _disk().journal()[-1]["kind"] == "move.done":
        return "MOVED", "", {"early": EARLY, "journal": h(_disk().journal()[-1])}
    fs.observe("slot")
    return finish_move(store, fs.move())


def effect_move_back(store, fs):
    out = fs.move_back()
    if out != "completed":
        raise Unfinished(out)
    store.put("thaw.move.done", intent=h(_disk().journal()[-1]))
    return "NONE", "", {"journal": store.put("end", outcome="thawed", reason="")}


def thaw_precheck(store, fs):
    """D2.2.1 §17.3.5 thaw pre-check (zero writes) at tail `thaw.move.intent`; returns "A", "B" or a veto reason."""
    cases = {fs.observe(what)[0] for what in LOCATION}
    if cases == {"B"}:
        return "B"
    if cases not in ({"A"}, {"mismatch"}):
        raise Unfinished(f"pre-check location {sorted(cases)}")
    for what in ("originals", "attrs"):
        verdict = fs.read_content(what)
        if verdict != "ok":
            return verdict
    return cases.pop()


def effect_thaw(store, fs):
    tail = _disk().journal()[-1]
    if tail["kind"] == "end":
        return "NONE", "", {"journal": h(tail)}
    if tail["kind"] == "thaw.move.done":
        return "NONE", "", {"journal": store.put("end", outcome="thawed", reason="")}
    verdict = thaw_precheck(store, fs)
    if verdict not in ("A", "B", "mismatch"):
        return "FREEZE_BLOCKED", verdict, {}
    return effect_move_back(store, fs)


def seed_thaw(disk, auth=A_THAW):
    """Test setup: a cycle MOVED, revoked and thawed up to `thaw.move.intent`, seeded directly (merged checks)."""
    a_start = disk.burn(_doc("freeze-start", "NONE", A_START))
    disk.fwal_append(_intent("MOVE_INTENT", a_start))
    disk.fwal_append({"op_id": OP, "kind": "complete", "value": "MOVED", "reason": "", "bindings": {"early": EARLY},
                      "auth_digest": a_start, "utc": 1})
    a_cont = disk.burn(_doc("freeze-continue", "MOVED", A_CONT))
    disk.fwal_append(_intent("REVOKE_INTENT", a_cont))
    disk.fwal_append({"op_id": OP, "kind": "complete", "value": "REVOKED", "reason": "",
                      "bindings": {"early": EARLY, "journal": "d" * 64}, "auth_digest": a_cont, "utc": 1})
    disk.journal_seed("start")
    disk.journal_seed("ep.done", binding=EARLY)
    mi = disk.journal_seed("move.intent", src_parent=SP, src_name=SN, dst_parent=QD, dst_name=DN, root=ROOT)
    fs = disk.fs()
    fs["dst"], fs["slot"] = fs["slot"], None
    disk.set_fs(fs)
    disk.journal_seed("move.done", intent=mi, linked=[])
    orig = {a: hashlib.sha256(a.encode()).hexdigest() for a in J.ATTRS}
    for a in J.REVOKE_ORDER:
        disk.journal_seed("revoke.intent", attr=a, lo=0, hi=0, originals=orig[a])
        disk.journal_seed("revoke.done", attr=a, lo=0, hi=0)
    disk.journal_seed("revoked", escaped=[])
    for a in J.THAW_ORDER:
        disk.journal_seed("thaw.intent", attr=a, lo=0, hi=0)
        disk.journal_seed("thaw.done", attr=a, lo=0, hi=0)
    disk.journal_seed("thaw.move.intent", src_parent=QD, src_name=DN, dst_parent=SP, dst_name=SN, root=ROOT)


def new_world(d=None, **fs):
    base = {"slot_parent": {"key": SP, "dev": ROOT_DEV}, "qdir": {"key": QD, "dev": ROOT_DEV},
            "slot": {"key": ROOT, "dev": ROOT_DEV}, "dst": None, "extra": []}
    base.update(fs)
    return Disk(base)


def reset():
    HOOKS.clear()
    FAULTS.clear()
    CRASH["at"] = None
    fscap._BACKEND = None


def crash_child(disk, crash_at, how):
    """The model's crash child, in process: a substitution (case (c)), a lying effect, and a simulated process stop
    (stores.Crash) at `crash_at`. The world's stores keep only what was written; the authority is discarded."""
    inv = start_invocation(disk, "freeze-start", A_START)

    def eff(store, fs):
        journal_through_move_intent(store)
        f = disk.fs()
        f["slot"] = {"key": X, "dev": ROOT_DEV}
        disk.set_fs(f)
        try:
            fs.observe("slot") if how == "observe" else fs.move()
        except Refused:
            pass
        return "MOVED", "", {"early": EARLY}
    CRASH["at"] = crash_at
    try:
        inv.complete(eff)
        return 0
    except Crash:
        return 70
    except Exception:
        return 1
    finally:
        CRASH["at"] = None

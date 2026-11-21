"""The filesystem capability, its read-only reader and the FsCap authority
(P-B1(b) B, PR-4c). Inert: every observation and effect goes through the one
backend installed in `_BACKEND`, and an installed broker has none; in PR-4c
only the test-support model is installed there, by tests. The authority's
shared lock, monotonic state, classification, poison, content veto, sealing
and result reconciliation are in-process properties of trusted code; they
prove no OS isolation, durability or atomic rename (contract "Model limits").

One authority per invocation, created only by the mint and shared by every
alias and wrapper of both capabilities. State FSCAP_OPEN -> FSCAP_POISONED ->
FSCAP_SEALED (or FSCAP_OPEN -> FSCAP_SEALED), forward only, plus two
irreversible flags: `claimed` (the one move/move_back call) and
`rename_issued`. One lock serializes every observation, content read,
move/move_back call, store put and the seal: LP-OBS, LP-POISON, LP-CALL,
LP-RENAME, LP-PUT, LP-SEAL; LP-RESULT reconciles from the immutable record."""

import collections
import errno
import threading

from . import canonical, journal, o5contract as oc

FSCAP_OPEN, FSCAP_POISONED, FSCAP_SEALED = "FSCAP_OPEN", "FSCAP_POISONED", "FSCAP_SEALED"
_FORWARD = {(FSCAP_OPEN, FSCAP_POISONED), (FSCAP_OPEN, FSCAP_SEALED), (FSCAP_POISONED, FSCAP_SEALED)}
PROTECTED = {"freeze-start": "move.intent", "thaw": "thaw.move.intent"}
SUCCESS = {"freeze-start": "MOVED", "thaw": "NONE"}
DONE = {"freeze-start": "move.done", "thaw": "thaw.move.done"}
OBSERVATIONS = ("slot", "slot_root", "quarantine", "walk_q")
LOCATION = ("slot", "quarantine", "walk_q")      # the thaw pre-check's location and identity observations
WALKS = ("walk_q", "call_pre", "call_post")      # observations that walk below quarantine/<op_id> (rows 1-7)
CONTENT_VERDICT = {"originals": "identity", "attrs": "journal_mismatch"}   # the only content reasons, by source
CAP_STORES = ("journals", "artifacts", "blobs")  # the only stores the pair can reach

# The one place a filesystem backend comes from. None in an installed broker.
_BACKEND = None


class Refused(Exception):
    """A capability operation refused; it changes nothing."""


class Unfinished(Exception):
    """No result can be established in this invocation (W reports UNFINISHED)."""


Record = collections.namedtuple("Record", "poison claimed rename_issued outcome renames observations log "
                                           "store_failure veto io")


class _Lock:
    """A non-reentrant mutex that counts blocked acquirers; a holder that acquires it again raises."""
    __slots__ = ("_c", "held", "waiting", "owner")

    def __init__(self):
        self._c = threading.Condition()
        self.held, self.waiting, self.owner = False, 0, None

    def __enter__(self):
        with self._c:
            if self.held and self.owner is threading.current_thread():
                raise AssertionError("non-reentrant authority lock re-acquired by its holder")
            self.waiting += 1
            self._c.wait_for(lambda: not self.held)
            self.waiting -= 1
            self.held, self.owner = True, threading.current_thread()

    def __exit__(self, *exc):
        with self._c:
            self.held, self.owner = False, None
            self._c.notify_all()

    def mine(self):
        return self.held and self.owner is threading.current_thread()


class _Sealed:
    __slots__ = ()

    def __init__(self, *_a, **_k):
        raise TypeError(f"{type(self).__name__} is created only by the mint")

    def __copy__(self):
        raise TypeError("not copyable")

    def _not_serializable(self, *a):
        raise TypeError("not serializable")

    __deepcopy__ = __reduce__ = __reduce_ex__ = __getstate__ = _not_serializable


class _Authority(_Sealed):
    """The one trusted FsCap authority of an invocation. Its state lives only here."""
    __slots__ = ("lock", "state", "binding", "poison", "claimed", "rename_issued", "outcome",
                 "observed_done", "renames", "observations", "log", "record", "store_failure", "veto", "io",
                 "content_ok", "_faults")

    def _hit(self, name):
        self._faults.hit(f"fscap:{name}")

    def _to(self, new):
        if (self.state, new) not in _FORWARD:
            raise AssertionError(f"authority state {self.state} -> {new} is not forward")
        self.state = new

    def require_open(self):
        if self.state is not FSCAP_OPEN:
            raise Refused(f"authority {self.state}")

    def seal(self):
        """Call site only. Terminal and idempotent; the lock waits for every in-flight call."""
        self._hit("seal_enter")
        with self.lock:                                     # LP-SEAL
            self._seal_held()
        self._hit("sealed")
        return self.record

    def _seal_held(self):
        """LP-SEAL body; the caller already holds the lock (the call site's seal, or a failed LP-PUT)."""
        if self.state is not FSCAP_SEALED:
            self._to(FSCAP_SEALED)
            self.record = Record(self.poison, self.claimed, self.rename_issued, self.outcome,
                                 self.renames, self.observations, tuple(self.log), self.store_failure,
                                 self.veto, self.io)


class Reader(_Sealed):
    """The read-only reader minted with the pair; only the capabilities' own
    operations use it. Exact-key reads of this cycle's journal and artifact
    keys and of `blob/<sha256>` keys in its allow-set, which starts empty and
    gains a key only from a binding already validated through this reader. A
    present key not yet reported durable is relied on only after the store
    makes it durable. It writes, lists and mints nothing."""
    __slots__ = ("_a", "_stores", "_cid", "_allow")

    def _open(self):
        if self._a.state is not FSCAP_OPEN:
            raise Refused(f"reader closed ({self._a.state})")

    def _durable(self, store, key):
        data = store.get(key)
        if data is not None and not store.durable(key) and not store.make_durable(key):
            raise OSError(errno.EIO, f"{store.name}: {key} not durable")
        return data

    def journal(self, seq):
        self._open()
        return self._durable(self._stores["journals"], f"journal/{self._cid}/{seq:08d}")

    def artifact(self, d):
        self._open()
        if not canonical.is_hex64(d):
            raise Refused("artifact key")
        data = self._durable(self._stores["artifacts"], f"artifact/{self._cid}/{d}")
        if data is not None and canonical.sha256(data) != d:
            raise Refused("artifact digest")
        return data

    def blob(self, d):
        self._open()
        if d not in self._allow:
            raise Refused("blob not in the reader's allow-set")
        data = self._durable(self._stores["blobs"], f"blob/{d}")
        if data is not None and canonical.sha256(data) != d:
            raise Refused("blob digest")
        return data


def walk(recs, root_id):
    """Merged journal/v3 record and grammar checks over the whole journal."""
    g, prev, early = journal._Grammar(root_id), oc.ZERO64, None
    for seq, r in enumerate(recs):
        if not journal._record_ok_v3(r, seq, prev, early) or not g.ok(r, canonical.digest(r)):
            raise Refused("journal_mismatch")
        early = r["binding"] if r["kind"] == "ep.done" else early
        prev = canonical.digest(r)
    return g, prev, early


class _Core(_Sealed):
    """The trusted operations behind both capabilities: the binding, the cycle header, the layout pinned at mint,
    the reader and the backend. Every operation runs under its authority's lock."""
    __slots__ = ("verb", "binding", "header", "layout", "reader", "backend")

    def records(self):
        """The cycle's journal re-read by exact key from `seq 0` through the reader, each record canonical and
        carrying this cycle's header, up to its last record. A store failure raises OSError."""
        if self.header is None:
            raise Refused("no cycle header")
        recs = []
        for seq in range(journal.MAX_RECORDS + 1):
            data = self.reader.journal(seq)
            if data is None:
                return recs
            try:
                rec, _ = journal.parse(data)
            except journal.Mismatch:
                raise Refused("journal_mismatch") from None
            if rec.get("header") != self.header:
                raise Refused("journal_mismatch")
            recs.append(rec)
        raise Refused("journal_mismatch")

    def _journal(self, a):
        """`records` inside a capability operation: a store read failure seals in this hold."""
        try:
            return self.records()
        except OSError as e:
            a.store_failure = f"journal read: {e.strerror}"
            a.log.append(("store_failure", "journal read"))
            a._seal_held()
            raise

    def _require_backend(self):
        if self.backend is None or self.layout is None:
            raise Refused("no filesystem backend")

    def _authentic_tail(self, a):
        recs = self._journal(a)
        walk(recs, self.header["root_id"])
        tail = recs[-1] if recs else None
        return tail, a.binding is self.binding and tail is not None \
            and tail["kind"] == PROTECTED.get(self.verb) and self._intent_ok(tail, tail["kind"])

    def _intent_ok(self, r, want):
        lay, root = self.layout, self.header["root_id"]
        move = dict(src_parent=lay["slot_parent"], src_name=lay["slot_name"], dst_parent=lay["qdir"],
                    dst_name=lay["dst_name"], root=root)
        back = dict(src_parent=lay["qdir"], src_name=lay["dst_name"], dst_parent=lay["slot_parent"],
                    dst_name=lay["slot_name"], root=root)
        return r["kind"] == want and all(r.get(k) == v for k, v in (back if want == "thaw.move.intent" else move).items())

    def _classify(self, a, what, back, phase=None):
        """Observation under the lock (LP-OBS); case C poisons at once (LP-POISON); I/O seals (OD-S6)."""
        a.observations += 1
        a._hit("observe")
        key = phase or what
        try:
            fs = self.backend.observe(key)                  # the identity observation itself
        except OSError:
            self._observation_io(a, key)
        case, reason = self._location(fs, back)
        if back and key in WALKS and case in ("A", "mismatch"):
            try:
                objects = self.backend.walk(key)            # the walk below a verified Q
            except OSError:
                self._observation_io(a, key)                # the walk below a verified Q failed: no C established
            if objects != "ok":
                case, reason = "C", "identity"              # D2.2.1 thaw pre-check matrix rows 1-7
        a.log.append(("obs", what, case))
        if case == "C":
            a.poison = reason
            a._to(FSCAP_POISONED)
            a.log.append(("poison", reason))
            a._hit("poisoned")
            raise Refused(f"FREEZE_BLOCKED({reason})")
        return case

    def _observation_io(self, a, key):
        """No classification, no poison, no rename: seal in this hold; a rename already issued stays an
        uncertain effect, never a pre-effect failure."""
        a.io = key
        a.log.append(("obs_io", key))
        if a.claimed and a.outcome is None:
            a.outcome = ("uncertain" if a.rename_issued else "unavailable", "observation I/O")
        a._seal_held()
        raise Unfinished(f"observation I/O ({key})")

    def _observation(self, a, what):
        if what not in OBSERVATIONS:
            raise Refused("unknown observation")
        a._hit("pre_acquire")
        with a.lock:                                        # LP-OBS
            a.require_open()
            self._require_backend()
            tail, authentic = self._authentic_tail(a)
            if tail is None or tail["kind"] not in PROTECTED.values():
                return "unprotected", self.backend.peek(what)   # outside the protected rule: no classification
            if not authentic:
                raise Refused("intent authority")
            case = self._classify(a, what, tail["kind"] == "thaw.move.intent")  # protected classification
            return case, self.backend.peek(what)

    def _content(self, a, what):
        if what not in CONTENT_VERDICT:
            raise Refused("unknown content read")
        a._hit("pre_acquire")
        with a.lock:
            a.require_open()
            self._require_backend()
            tail, authentic = self._authentic_tail(a)
            if self.verb != "thaw" or tail is None or tail["kind"] != "thaw.move.intent" or not authentic:
                raise Refused("content read authority")
            if a.claimed:                                   # a verdict after the call can authorize nothing
                raise Refused("content read after the move call")
            for w in LOCATION:                              # OD-S7: location and identity before content
                if not any(e[:2] == ("obs", w) for e in a.log):
                    self._classify(a, w, True)              # C poisons and raises; I/O seals and raises
            if {e[2] for e in a.log if e[0] == "obs" and e[1] in LOCATION} not in ({"A"}, {"mismatch"}):
                raise Refused("content read needs every location observation A")   # identities established
            try:
                fact = self.backend.content(what)
            except OSError:
                fact = None
            if fact == "ok":
                a.log.append(("content", what, "ok"))
                a.content_ok[what] = canonical.digest(tail)
                return "ok"
            if fact == CONTENT_VERDICT[what]:               # definite mismatch: terminal content veto (OD-S1)
                a.veto = fact
                a.log.append(("content", what, fact))
                a.log.append(("veto", fact))
                a._seal_held()
                return fact
            a.io = f"content {what}"                        # I/O or unclassifiable: no verdict (row 10)
            a.log.append(("content_io", what))
            a._seal_held()
            raise OSError(errno.EIO, f"{what} read failed")

    def _rename_call(self, a, want):
        a._hit("pre_acquire")
        with a.lock:                                        # LP-CALL
            a.require_open()
            if a.claimed:
                raise Refused("move call already claimed in this invocation")
            a._hit("claim")
            a.claimed = True
            a.log.append(("claim", want))
            try:
                return self._rename_body(a, want)
            finally:
                if a.outcome is None:
                    a.outcome = ("unavailable", "call ended without an outcome")

    def _unavailable(self, a, cause):
        a.outcome = ("unavailable", cause)
        a.log.append(("unavailable", cause))
        raise Unfinished(cause)

    def _rename_body(self, a, want):
        if self.backend is None or self.layout is None:
            return self._unavailable(a, "no filesystem backend")
        tail, authentic = self._authentic_tail(a)
        if not authentic or tail["kind"] != want:
            return self._unavailable(a, "intent authority")
        back = want == "thaw.move.intent"
        case = self._classify(a, want, back, "call_pre")
        if case == "B":
            return self._completed(a, tail)
        if case == "mismatch":
            return self._unavailable(a, "clean device mismatch")
        if back and any(a.content_ok.get(w) != canonical.digest(tail) for w in CONTENT_VERDICT):
            return self._unavailable(a, "content checks not passed for this intent")   # RF-2 content gate
        a._hit("before_rename")
        a.require_open()                                    # LP-RENAME
        if a.rename_issued:
            raise Refused("rename already issued in this invocation")
        a.rename_issued = True
        a.log.append(("rename",))
        a.renames += 1
        err = self.backend.rename(back)
        case = self._classify(a, want, back, "call_post")
        if case == "B":
            return self._completed(a, tail)
        if back:
            return self._unavailable(a, "rename back not observed")
        reason = "target_exists" if err == "EEXIST" else "rename_failed"
        a.outcome = ("abort", reason)
        return "abort:" + reason

    def _completed(self, a, tail):
        a.outcome = ("completed",)
        a.observed_done = (tail["kind"], canonical.digest(tail))
        return "completed"

    def _location(self, fs, back):
        slot, dst, extra = fs["slot"], fs["dst"], fs.get("extra", [])
        sp, qd = fs["slot_parent"], fs["qdir"]
        lay, root = self.layout, self.header["root_id"]
        if sp["key"] != lay["slot_parent"] or qd["key"] != lay["qdir"] or extra:
            return "C", "identity" if back else "identity_swap"

        def is_root(o, parent):
            return o is not None and o["key"] == root and o["dev"] == lay["root_dev"] == parent["dev"]
        here, there = (dst, slot) if back else (slot, dst)
        here_p = qd if back else sp
        there_p = sp if back else qd
        if is_root(here, here_p) and there is None:
            return ("mismatch", None) if sp["dev"] != qd["dev"] else ("A", None)
        if is_root(there, there_p) and here is None:
            return "B", None
        if back and is_root(dst, qd) and slot is not None:
            return "C", "thaw_target_occupied"
        return "C", "identity" if back else "identity_swap"


class FsCap(_Sealed):
    """The filesystem capability; every wrapper of one authority shares its state."""
    __slots__ = ("_a", "_c")

    def view(self):
        return _wrap(FsCap, self._a, self._c)

    def observe(self, what="slot"):
        """Read-only observation: `slot` (no-follow fstatat of the slot name), `slot_root`, `quarantine`
        (quarantine/<op_id> and its entries) or `walk_q` (read walk below it)."""
        return self._c._observation(self._a, what)

    def read_content(self, what):
        """Trusted content read (`originals` blobs, `attrs` values) at tail `thaw.move.intent`: the capability
        authenticates its inputs, establishes location first, and computes the verdict itself; a definite
        mismatch is a terminal content veto (sealed), never a poison."""
        return self._c._content(self._a, what)

    def move(self):
        return self._c._rename_call(self._a, "move.intent")

    def move_back(self):
        return self._c._rename_call(self._a, "thaw.move.intent")


def _wrap(cls, a, c):
    w = object.__new__(cls)
    w._a, w._c = a, c
    return w


def _mint(binding, header, mode, stores, faults):
    """The authority, its core and reader for `authz_txn._issue_effect`; refuses protected mode."""
    oc.require_synthetic(mode)
    a = object.__new__(_Authority)
    a.lock, a.state, a.binding, a._faults = _Lock(), FSCAP_OPEN, binding, faults
    a.poison = a.outcome = a.observed_done = a.record = a.store_failure = None
    a.veto = a.io = None
    a.content_ok = {}                                       # source -> digest of the intent it passed at
    a.claimed = a.rename_issued = False
    a.renames, a.observations, a.log = 0, 0, []
    r = object.__new__(Reader)
    r._a, r._stores, r._cid, r._allow = a, stores, binding["cycle_id"], frozenset()
    c = object.__new__(_Core)
    c.verb, c.binding, c.header, c.reader, c.backend = binding["verb"], binding, header, r, _BACKEND
    c.layout = _BACKEND.pin() if _BACKEND is not None else None
    return a, c


# ---------------------------------------------------------------- LP-RESULT

def journal_kinds(store, cid):
    """The call site's re-read of the cycle's journal kinds: every present record, including one whose put raised
    after the write (conservative for R7 and the `seq 0` rule). An unparsable record reads as kind None."""
    kinds = []
    for seq in range(journal.MAX_RECORDS + 1):
        data = store.get(f"journal/{cid}/{seq:08d}")
        if data is None:
            break
        try:
            kinds.append(journal.parse(data)[0].get("kind"))
        except journal.Mismatch:
            kinds.append(None)
    return kinds


def reconcile(verb, rec, kinds, res, err):
    """LP-RESULT: the only result the call site may complete, from the immutable sealed record and the
    journal. None means UNFINISHED."""
    out = _rule(verb, rec, kinds[-1] if kinds else None, res, err, bool(kinds))
    if out is not None and out[0] == "FREEZE_BLOCKED" and verb in DONE \
            and (DONE[verb] in kinds or None in kinds or ("put", DONE[verb]) in rec.log):
        return None                                         # R7 (OD-S5): no FREEZE_BLOCKED over a durable done move
    return out


def _rule(verb, rec, tail, res, err, seq0):
    if rec.poison:                                          # R1
        return "FREEZE_BLOCKED", rec.poison, {}
    if rec.veto:                                            # R1b: terminal content veto (OD-S1)
        return "FREEZE_BLOCKED", rec.veto, {}
    if rec.io:                                              # R2: observation or content I/O (OD-S6)
        return None
    if rec.store_failure:                                   # R2 (store failure, "Failure")
        ok = not seq0 and err is None and verb == "freeze-start" and res is not None \
            and tuple(res[:2]) in (("NONE", "store_space"), ("NONE", "store_error"))
        return res if ok else None
    if err is not None:                                     # R2
        return None
    if rec.claimed and rec.outcome[0] not in ("completed", "abort"):   # R3: unavailable or no outcome
        return None
    value, reason, _ = res
    if rec.claimed:                                         # R4, R5
        if rec.outcome[0] == "abort":
            ok = (value, reason) == ("NONE", rec.outcome[1])
        elif rec.rename_issued:
            ok = value == SUCCESS[verb]
        elif any(e[0] == "obs" and e[2] == "A" for e in rec.log):
            ok = False                                      # R5 (OD-S2): B after an A without a rename contradicts
        else:
            ok = value == SUCCESS[verb] or (verb == "freeze-start" and (value, reason) == ("FREEZE_BLOCKED", "identity"))
        return res if ok else None
    if tail == PROTECTED.get(verb) and value == "FREEZE_BLOCKED":   # R6
        return None
    return res

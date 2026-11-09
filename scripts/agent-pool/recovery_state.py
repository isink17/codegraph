"""Orphan lease recovery state machine (CG-150 phases 2a and 2b-1).

It has no command-line entry point. Only recovery_cli.py, which pool.sh runs
as `pool.sh recover` against marked synthetic pools only, creates or advances
an operation; the coordinator and the campaign never do, and nothing
reassigns a slot. pool_record.py uses parse_record, the durable-write helpers
and, for the recovery gate, only the read-only queries of RecoveryStore. It
records recovery decisions per slot and operation in a store
directory the caller names, and reads the live slot record only as text the
caller passes in. Policy and limits: docs/agent-pool.md, "Recovery state
machine".

The only path to READY_FOR_REASSIGNMENT is

    ACTIVE -> RECOVERY_REQUESTED -> QUARANTINED -> QUIESCENCE_VERIFIED
           -> PRESERVED -> READY_FOR_REASSIGNMENT

with one side exit to CANCELLED from ACTIVE or RECOVERY_REQUESTED. Each step needs its own
evidence. Every unreadable, inconsistent or half-written record is refused,
never read as available.
"""

import hashlib
import hmac
import json
import os
import re
import secrets
import stat
import sys
from collections import namedtuple
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path

ACTIVE = "ACTIVE"
REQUESTED = "RECOVERY_REQUESTED"
QUARANTINED = "QUARANTINED"
QUIESCENT = "QUIESCENCE_VERIFIED"
PRESERVED = "PRESERVED"
READY = "READY_FOR_REASSIGNMENT"
CANCELLED = "CANCELLED"
ORDER = (ACTIVE, REQUESTED, QUARANTINED, QUIESCENT, PRESERVED, READY)
STATES = ORDER + (CANCELLED,)
TERMINAL_STATES = (READY, CANCELLED)
# States whose live slot record is the quarantined one this operation wrote.
AFTER_QUARANTINE = (QUARANTINED, QUIESCENT, PRESERVED, READY)

# The transition that leads into each state; its name is also the evidence gate it records.
TRANSITION = {REQUESTED: "request", CANCELLED: "cancel", QUARANTINED: "quarantine", QUIESCENT: "quiescence",
              PRESERVED: "preservation", READY: "ready"}
STEPS = {None: (ACTIVE,), ACTIVE: (REQUESTED, CANCELLED), REQUESTED: (QUARANTINED, CANCELLED), QUARANTINED: (QUIESCENT,),
         QUIESCENT: (PRESERVED,), PRESERVED: (READY,)}
# A cancelled operation was cancelled from ACTIVE or from RECOVERY_REQUESTED.
CANCEL_GATES = (("cancel",), ("cancel", "request"))
GATES = {ACTIVE: (), REQUESTED: ("request",), CANCELLED: ("cancel", "request"),
         QUARANTINED: ("request", "quarantine"), QUIESCENT: ("request", "quarantine", "quiescence"),
         PRESERVED: ("request", "quarantine", "quiescence", "preservation"),
         READY: ("request", "quarantine", "quiescence", "preservation", "ready")}
# The fields a completion must share with its intent. A resumed completion
# adds "resumed" and its own time; both are ignored when they are compared.
ENTRY_KEYS = ("seq", "op_id", "from", "to", "slot_generation", "evidence", "effect")

SCHEMA = 2
SUPPORTED = os.name != "nt"
IDENTITY_KINDS = ("v1", "v2", "legacy")
# Holder fields a lease may carry and a quarantined record keeps; never a token.
HOLDER = ("task", "agent", "pr", "owner", "host", "pid", "utc")
# The only trust level this module accepts. It records that some process of
# the owner's uid attested the decision; it is not proof of owner approval.
TRUST = "same-uid-attestation"
QUIESCENCE_METHODS = ("containment-teardown", "owner-quiescence", "reboot-runbook")
# Every class of writer the owner must have accounted for. A dead PID, an empty
# process group, no process cwd or a stale heartbeat accounts for none of them.
QUIESCENCE_ACCOUNTED = ("old_coordinators", "wrappers", "escaped_descendants", "remote_writers",
                        "automatic_restarts")
INVENTORY = ("dirty", "untracked", "ignored", "unpublished_commits")
AUTHORIZATION_FIELDS = ("actor", "reason", "utc")
QUARANTINE_FIELDS = ("assessment_digest", "quiescence_method", "preservation_disposition")
SECRET_KEY = re.compile(r"token|secret|password|verifier|salt", re.I)
NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\Z")
KEY = re.compile(r"^[a-z][a-z_]{0,31}\Z")
GENERATION = re.compile(r"^(0|[1-9][0-9]{0,17})\Z")
# Record values are printed by pool.sh status, and line-splitting readers
# treat U+2028, U+0085 and the like as line breaks.
NOT_PRINTABLE_ASCII = re.compile(r"[^\x20-\x7e\n]")
HEX32 = re.compile(r"^[0-9a-f]{32}\Z")
HEX40 = re.compile(r"^[0-9a-f]{40}\Z")
HEX64 = re.compile(r"^[0-9a-f]{64}\Z")
RECORD_KEYS = {"schema", "slot", "op_id", "identity_kind", "identity", "slot_generation", "state", "seq", "salt",
               "verifier", "gates", "quiescence_method", "preserved_head"}


class Refused(Exception):
    """The transition was not made. A refusal on validation changes nothing; a
    storage or hook failure may leave an unfinished transition, which never
    authorizes anything and only the same operation's exact retry finishes."""


class Busy(Refused):
    """Another transition holds the slot, or a crashed one left its lock."""


class Corrupt(Refused):
    """The stored record cannot be trusted; the owner must inspect it."""


def digest(obj):
    return hashlib.sha256(json.dumps(obj, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def _verifier(salt, token):
    return hmac.new(bytes.fromhex(salt), token.encode(), hashlib.sha256).hexdigest()


def _now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _no_secrets(obj, where):
    if isinstance(obj, dict):
        for k, v in obj.items():
            if SECRET_KEY.search(str(k)):
                raise Refused(f"{where} carries a secret-looking field {k!r}")
            _no_secrets(v, where)
    elif isinstance(obj, list):
        for v in obj:
            _no_secrets(v, where)


def _bound(evidence, where, **expect):
    if not isinstance(evidence, dict):
        raise Refused(f"{where} is missing")
    _no_secrets(evidence, where)
    for k, v in expect.items():
        if evidence.get(k) != v or type(evidence.get(k)) is not type(v):
            raise Refused(f"{where} is not bound to {k}={v!r}")


# ---------------------------------------------------------------- slot records

Record = namedtuple("Record", "kind state generation identity legacy token recovery_op holder")


def _without_tokens(text):
    return "\n".join(line for line in text.split("\n") if not line.startswith(("token=", "lease_token=")))


def image_digest(text):
    """Token-free digest of a slot record's bytes."""
    if not isinstance(text, str):
        raise Refused("slot record text is missing")
    return hashlib.sha256(_without_tokens(text).encode()).hexdigest()


def parse_record(text, slot, *, legacy=False):
    """Reads one slot record: v1 leased, v2 free/leased/quarantined, or, only
    when the owner passes legacy=True, a lease written before versioning.
    Malformed, ambiguous, contradictory and unsupported records are refused;
    the message never quotes a value from the record."""
    if not isinstance(text, str) or not text:
        raise Refused("slot record text is missing")
    if NOT_PRINTABLE_ASCII.search(text):
        raise Refused("slot record contains a character that is not printable ASCII")
    lines = text.split("\n")
    if lines[-1] == "":
        lines.pop()
    fields = {}
    for line in lines:
        k, sep, v = line.partition("=")
        if not sep or not KEY.match(k):
            raise Refused("malformed slot record line")
        if k in fields:
            raise Refused(f"slot record repeats field {k}")
        fields[k] = v
    version = fields.get("version")
    if version == "2" and "token" in fields:
        raise Refused("a version 2 slot record must not carry token=")
    if version is None:
        if not legacy:
            raise Refused("lease written before versioning; only an owner-initiated legacy adoption reads it")
        kind, state = "legacy", "leased"
        required = {"slot", "task", "token"}
    elif version == "1":
        kind, state = "v1", "leased"
        required = {"version", "slot", "task", "lease_id", "token"}
    elif version == "2":
        kind, state = "v2", fields.get("state")
        required = {"version", "slot", "generation", "state"} | {
            "free": set(), "leased": {"lease_id", "task", "lease_token"},
            "quarantined": {"recovery_op"}}.get(state, set())
        if state not in ("free", "leased", "quarantined"):
            raise Refused("unsupported version 2 slot record state")
    else:
        raise Refused("unsupported slot record version")
    allowed = required | (set(HOLDER) if state != "free" else set()) | (
        {"lease_id", "legacy_digest"} if state == "quarantined" else set())
    if set(fields) - allowed:
        raise Refused(f"slot record has unexpected fields {sorted(set(fields) - allowed)}")
    missing = sorted(k for k in required if not fields.get(k))
    if missing:
        raise Refused(f"slot record lacks {', '.join(missing)}")
    if fields["slot"] != slot:
        raise Refused("slot record names a different slot")
    generation = 0
    if kind == "v2":
        if not GENERATION.match(fields["generation"]):
            raise Refused("slot record generation is not a canonical number")
        generation = int(fields["generation"])
        if state != "free" and generation < 1:
            raise Refused("a leased or quarantined slot record needs a generation of at least 1")
    token = fields.get("token") if kind != "v2" else fields.get("lease_token")
    if state == "leased" and not HEX32.match(token or ""):
        raise Refused("slot record has no valid lease token")
    identity, is_legacy, recovery_op = None, kind == "legacy", None
    if kind == "legacy":
        identity = hashlib.sha256(_without_tokens(text).encode()).hexdigest()
    elif state == "leased":
        identity = fields["lease_id"]
        if not HEX32.match(identity):
            raise Refused("slot record has no valid lease_id")
    elif state == "quarantined":
        if ("lease_id" in fields) == ("legacy_digest" in fields):
            raise Refused("quarantined slot record needs exactly one of lease_id and legacy_digest")
        is_legacy = "legacy_digest" in fields
        identity = fields["legacy_digest"] if is_legacy else fields["lease_id"]
        if not (HEX64 if is_legacy else HEX32).match(identity):
            raise Refused("quarantined slot record has a malformed identity")
        recovery_op = fields["recovery_op"]
        if not NAME.match(recovery_op):
            raise Refused("quarantined slot record has a malformed recovery_op")
    return Record(kind, state, generation, identity, is_legacy, token, recovery_op,
                  {k: fields[k] for k in HOLDER if k in fields})


def quarantined_record(slot, op_id, generation, pre):
    """The v2 quarantined record that replaces the lease `pre` (a Record)."""
    key = "legacy_digest" if pre.legacy else "lease_id"
    lines = ["version=2", f"slot={slot}", f"generation={generation}", "state=quarantined",
             f"{key}={pre.identity}", f"recovery_op={op_id}"]
    lines += [f"{k}={pre.holder[k]}" for k in HOLDER if k in pre.holder]
    return "\n".join(lines) + "\n"


def _adoption_digest(kind, identity):
    return digest({"identity_kind": kind, "identity": identity})


def _marker(rec):
    return f"state={rec['state']}\nseq={rec['seq']}\nop_id={rec['op_id']}\n".encode()


class RecoveryStore:
    """Recovery records under `root`, which the caller names; there is no
    default and its parent must exist. Each operation lives in
    `root/<slot>/<op_id>/`.

    Without `held_lock` the store takes its own per-slot mkdir lock. With it,
    the caller already holds the pool's per-slot lock and `held_lock(slot)`
    names it: the store checks the lock exists and never takes or removes it.
    `fault(point)` is a test hook called at each crash point."""

    def __init__(self, root, fault=None, held_lock=None):
        self.root = Path(root)
        self.fault = fault or (lambda point: None)
        self.held_lock = held_lock

    # ------------------------------------------------------------ paths and storage

    def _op_dir(self, slot, op_id):
        if not SUPPORTED:
            raise Refused("durable recovery writes are not supported on this platform; every operation is refused")
        if not isinstance(slot, str) or not NAME.match(slot):
            raise Refused(f"invalid slot name {slot!r}")
        if not isinstance(op_id, str) or not NAME.match(op_id):
            raise Refused(f"invalid operation id {op_id!r}")
        return self.root / slot / op_id

    @staticmethod
    def _private(st, what):
        if st.st_uid != os.getuid() or st.st_mode & 0o077:
            raise Corrupt(f"{what} is not private to this user")

    def _check_dir(self, path):
        st = os.lstat(path)
        if not stat.S_ISDIR(st.st_mode):
            raise Corrupt(f"{path} is not a directory")
        self._private(st, path)

    def _private_dirs(self, slot, op_id):
        for p in (self.root, self.root / slot, self.root / slot / op_id):
            try:
                self._check_dir(p)
            except FileNotFoundError:
                raise Refused(f"{slot} has no recovery record for operation {op_id}") from None
            except OSError as e:
                raise Corrupt(f"{p} unreadable: {e}") from None

    def _ensure_dir(self, path):
        # The parent is synced every time, not only on creation: an earlier
        # call may have made the directory and failed to sync it.
        try:
            os.mkdir(path, 0o700)
        except FileExistsError:
            pass
        self._check_dir(path)
        self._fsync_dir(path.parent)

    @contextmanager
    def _lock(self, slot):
        if self.held_lock is not None:
            lock = Path(self.held_lock(slot))
            try:
                held = stat.S_ISDIR(os.lstat(lock).st_mode)
            except OSError:
                held = False
            if not held:
                raise Refused(f"{slot}: the caller does not hold its slot lock {lock}")
            yield
            return
        # A lock left by a crash is never broken here: the slot stays Busy
        # until the owner has checked that no transition is still running.
        lock = self.root / f".{slot}.lock"
        try:
            os.mkdir(lock, 0o700)
        except FileExistsError:
            raise Busy(f"{slot} recovery lock held: {lock}") from None
        except OSError as e:
            raise Refused(f"cannot take the {slot} recovery lock: {e}") from None
        try:
            yield
        finally:
            os.rmdir(lock)

    @staticmethod
    def _sync(fd):
        # fsync alone leaves data in the drive cache on macOS.
        if sys.platform == "darwin":
            import fcntl
            fcntl.fcntl(fd, fcntl.F_FULLFSYNC)
        else:
            os.fsync(fd)

    @staticmethod
    def _write_all(fd, data):
        while data:
            data = data[os.write(fd, data):]

    def _fsync_dir(self, path):
        fd = os.open(path, os.O_RDONLY)
        try:
            self._sync(fd)
        finally:
            os.close(fd)

    def _read_file(self, path):
        """The bytes of a private regular file, or None when it is absent."""
        try:
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        except FileNotFoundError:
            return None
        except OSError as e:
            raise Corrupt(f"{path.name} unreadable: {e}") from None
        try:
            st = os.fstat(fd)
            if not stat.S_ISREG(st.st_mode):
                raise Corrupt(f"{path.name} is not a regular file")
            self._private(st, path.name)
            chunks = []
            while chunk := os.read(fd, 1 << 16):
                chunks.append(chunk)
            return b"".join(chunks)
        except OSError as e:
            raise Corrupt(f"{path.name} unreadable: {e}") from None
        finally:
            os.close(fd)

    def _append(self, d, entry):
        path = d / "journal.jsonl"
        new = not os.path.lexists(path)
        fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        try:
            st = os.fstat(fd)
            if not stat.S_ISREG(st.st_mode):
                raise Corrupt("journal is not a regular file")
            self._private(st, "journal")
            self._write_all(fd, (json.dumps(entry, sort_keys=True) + "\n").encode())
            self._sync(fd)
        finally:
            os.close(fd)
        if new:
            self._fsync_dir(d)

    def _sync_journal(self, d):
        # A retry reuses an intent whose own sync may have failed.
        fd = os.open(d / "journal.jsonl", os.O_RDONLY | os.O_NOFOLLOW)
        try:
            self._sync(fd)
        finally:
            os.close(fd)
        self._fsync_dir(d)

    def _replace(self, d, name, data, point):
        """Durable atomic replace: private temp file, flush, rename, flush dir."""
        tmp = d / f".{name}.{secrets.token_hex(8)}.tmp"
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        try:
            try:
                self._write_all(fd, data)
                self._sync(fd)
            finally:
                os.close(fd)
            self.fault(point)
            os.replace(tmp, d / name)
        except BaseException:
            tmp.unlink(missing_ok=True)
            raise
        self._fsync_dir(d)

    def _journal(self, d, slot):
        raw = self._read_file(d / "journal.jsonl")
        if raw is None:
            raise Corrupt(f"{slot} recovery journal is missing")
        try:
            return [json.loads(line) for line in raw.decode().splitlines()]
        except (ValueError, RecursionError) as e:
            raise Corrupt(f"{slot} recovery journal unreadable: {e}") from None

    @staticmethod
    def _effect_ok(e):
        effect = e.get("effect")
        if e.get("to") != QUARANTINED:
            return "effect" not in e
        return (isinstance(effect, dict) and sorted(effect) == ["post", "pre"]
                and all(HEX64.match(str(effect[k])) for k in effect))

    @classmethod
    def _replay(cls, slot, op_id, journal):
        """(committed, pending): the completed transitions, which must run from
        adoption one legal step at a time, each completing the intent right
        before it; and a trailing intent with no completion, if any. A second
        intent before a completion is corrupt."""
        committed, pending = [], None
        for e in journal:
            if not isinstance(e, dict) or e.get("kind") not in ("intent", "complete"):
                raise Corrupt(f"{slot} journal has a malformed entry")
            head = committed[-1] if committed else None
            frm = head["to"] if head else None
            gen = e.get("slot_generation")
            gen_ok = type(gen) is int and (gen >= 0 if head is None else
                                           gen == head["slot_generation"] + (e.get("to") == QUARANTINED))
            if e.get("to") not in STEPS.get(frm, ()) or type(e.get("seq")) is not int \
                    or e.get("seq") != len(committed) or e.get("from") != frm \
                    or not gen_ok or e.get("op_id") != op_id or not isinstance(e.get("evidence"), str) \
                    or not cls._effect_ok(e):
                raise Corrupt(f"{slot} journal entry {e.get('seq')!r} is not the next legal transition")
            if e["kind"] == "intent":
                if pending is not None:
                    raise Corrupt(f"{slot} journal has a second intent before a completion")
                pending = e
            elif pending is None or any(pending.get(k) != e.get(k) for k in ENTRY_KEYS):
                raise Corrupt(f"{slot} journal completion {e['seq']} has no matching intent")
            else:
                committed.append(e)
                pending = None
        return committed, pending

    @staticmethod
    def _matches(rec, history):
        """True when the record is exactly the state the history ends in."""
        last = history[-1]
        if (rec["seq"], rec["state"], rec["slot_generation"], rec["op_id"]) != \
                (last["seq"], last["to"], last["slot_generation"], last["op_id"]):
            return False
        if history[0]["evidence"] != _adoption_digest(rec["identity_kind"], rec["identity"]):
            return False
        return all(rec["gates"].get(TRANSITION[e["to"]]) == e["evidence"] for e in history[1:])

    def _read(self, slot, op_id):
        """(record, pending). pending is a trailing intent with no completion,
        whether or not it replaced the state; while it exists the record is
        unfinished and only that transition's exact retry may proceed."""
        d = self._op_dir(slot, op_id)
        self._private_dirs(slot, op_id)
        raw = self._read_file(d / "state.json")
        if raw is None:
            raise Refused(f"{slot} has no recovery record for operation {op_id}")
        try:
            rec = json.loads(raw)
        except (ValueError, RecursionError) as e:
            raise Corrupt(f"{slot} recovery record unreadable: {e}") from None
        self._validate(slot, op_id, rec)
        committed, pending = self._replay(slot, op_id, self._journal(d, slot))
        if committed and self._matches(rec, committed):
            return rec, pending
        if pending and self._matches(rec, committed + [pending]):
            return rec, pending
        raise Corrupt(f"{slot} recovery record does not match its journal")

    @staticmethod
    def _validate(slot, op_id, rec):
        ok = (isinstance(rec, dict) and set(rec) <= RECORD_KEYS and rec.get("schema") == SCHEMA
              and rec.get("slot") == slot and rec.get("op_id") == op_id and rec.get("state") in STATES
              and rec.get("identity_kind") in IDENTITY_KINDS
              and bool((HEX64 if rec["identity_kind"] == "legacy" else HEX32).match(str(rec.get("identity"))))
              and type(rec.get("slot_generation")) is int and rec["slot_generation"] >= 0
              and type(rec.get("seq")) is int and rec["seq"] >= 0
              and bool(HEX32.match(str(rec.get("salt", "")))) and bool(HEX64.match(str(rec.get("verifier", ""))))
              and isinstance(rec.get("gates"), dict))
        if not ok:
            raise Corrupt(f"{slot} recovery record is malformed or of an unsupported schema")
        allowed = CANCEL_GATES if rec["state"] == CANCELLED else (GATES[rec["state"]],)
        if tuple(sorted(rec["gates"])) not in [tuple(sorted(g)) for g in allowed]:
            raise Corrupt(f"{slot} recovery record in {rec['state']} lacks or adds evidence gates")
        bound = (("quiescence_method", "quarantine", lambda v: v in QUIESCENCE_METHODS),
                 ("preserved_head", "preservation", lambda v: bool(HEX40.match(str(v)))))
        for field, gate, valid in bound:
            if (field in rec) != (gate in rec["gates"]) or (field in rec and not valid(rec[field])):
                raise Corrupt(f"{slot} recovery record has an inconsistent {field}")

    # ------------------------------------------------------------ commit protocol

    @staticmethod
    def _entry(frm, new, evidence_digest, effect=None):
        entry = {"seq": new["seq"], "op_id": new["op_id"], "from": frm, "to": new["state"],
                 "slot_generation": new["slot_generation"], "evidence": evidence_digest}
        if effect is not None:
            entry["effect"] = effect
        return entry

    def _run_hook(self, hook, pre, post):
        try:
            written = hook(pre, post)
        except Exception as e:
            raise Refused(f"slot record replace failed ({type(e).__name__}); the quarantine is unfinished "
                          f"until reconciled against the live record") from e
        if written != post:
            raise Refused("slot record replace did not report writing the quarantined record; the quarantine is "
                          "unfinished until reconciled against the live record")

    def _complete(self, d, new, entry, *, hook=None, pre=None, post=None, state_written=False, resumed=False):
        """Everything after the intent: the slot record hook, the state, the
        completion and, for a terminal state, its marker."""
        try:
            if hook is not None:
                self._run_hook(hook, pre, post)
                self.fault("after_record")
            if state_written:
                self._fsync_dir(d)
            else:
                self._replace(d, "state.json", json.dumps(new, sort_keys=True).encode(), "before_state_rename")
                self.fault("after_state")
            done = {**{k: entry[k] for k in ENTRY_KEYS if k in entry}, "kind": "complete", "utc": _now()}
            if resumed:
                done["resumed"] = True
            self._append(d, done)
            self.fault("after_complete")
            if new["state"] in TERMINAL_STATES:
                self._replace(d, "TERMINAL", _marker(new), "before_marker_rename")
        except OSError as e:
            raise Refused(f"{new['slot']} recovery storage failed: {e}") from e
        return new

    def _commit(self, d, rec, new, evidence_digest, *, effect=None, hook=None, pre=None, post=None):
        self._validate(new["slot"], new["op_id"], new)
        entry = self._entry(rec["state"] if rec else None, new, evidence_digest, effect)
        try:
            self.fault("before_intent")
            self._append(d, {**entry, "kind": "intent", "utc": _now()})
            self.fault("after_intent")
        except OSError as e:
            raise Refused(f"{new['slot']} recovery storage failed: {e}") from e
        return self._complete(d, new, entry, hook=hook, pre=pre, post=post)

    def _ensure_marker(self, d, rec):
        marker = self._read_file(d / "TERMINAL")
        if marker is None:
            try:
                self._replace(d, "TERMINAL", _marker(rec), "before_marker_rename")
            except OSError as e:
                raise Refused(f"{rec['slot']} recovery storage failed: {e}") from e
        elif marker != _marker(rec):
            raise Corrupt(f"{rec['slot']} terminal marker does not match its record")

    # ------------------------------------------------------------ live record checks

    def _lease_matches(self, rec, record):
        """Before quarantine: the live record is the adopted lease at the
        adopted generation."""
        if record is None:
            raise Refused(f"{rec['slot']} live slot record is absent; the adopted lease is gone")
        obs = parse_record(record, rec["slot"], legacy=rec["identity_kind"] == "legacy")
        if obs.state != "leased" or obs.kind != rec["identity_kind"] or obs.identity != rec["identity"] \
                or obs.generation != rec["slot_generation"] or not self._same_token(rec, obs):
            raise Refused(f"{rec['slot']} live slot record is not the adopted lease at generation "
                          f"{rec['slot_generation']}")
        return obs

    @staticmethod
    def _same_token(rec, obs):
        # A legacy identity leaves the token out, so a re-acquired lease with
        # the same other bytes differs only here.
        return hmac.compare_digest(_verifier(rec["salt"], obs.token), rec["verifier"])

    def _quarantine_matches(self, rec, record):
        """After quarantine: the live record is this operation's quarantined
        record; the original lease bytes are never compared again."""
        if record is None:
            raise Refused(f"{rec['slot']} live slot record is absent")
        obs = parse_record(record, rec["slot"])
        if obs.state != "quarantined" or obs.recovery_op != rec["op_id"] \
                or obs.legacy != (rec["identity_kind"] == "legacy") or obs.identity != rec["identity"] \
                or obs.generation != rec["slot_generation"]:
            raise Refused(f"{rec['slot']} live slot record is not the quarantined record of operation "
                          f"{rec['op_id']} at generation {rec['slot_generation']}")

    def _released_or_unchanged(self, rec, record):
        """Cancel: the live record shows the adopted lease unchanged, or proves
        it released. Anything else, including any quarantined record, refuses."""
        slot, kind, gen = rec["slot"], rec["identity_kind"], rec["slot_generation"]
        if record is None:
            if kind in ("v1", "legacy"):
                return
            raise Refused(f"{slot} live slot record is absent, which a version 2 slot never is")
        obs = parse_record(record, slot, legacy=kind == "legacy")
        if obs.state == "quarantined":
            raise Refused(f"{slot} live slot record is quarantined; cancel is not possible")
        if obs.state == "free":
            if obs.generation >= gen:
                return
            raise Refused(f"{slot} live slot record is free at an older generation")
        if obs.kind == kind and obs.identity == rec["identity"]:
            if obs.generation == gen and self._same_token(rec, obs):
                return
            raise Refused(f"{slot} live slot record carries the adopted identity with another generation or token")
        if obs.kind == "legacy":
            raise Refused(f"{slot} live slot record is a different legacy lease; release cannot be established")
        if kind == "legacy" or (kind == obs.kind == "v1") or obs.generation > gen:
            return
        raise Refused(f"{slot} live slot record is a different lease that does not prove a release")

    def _classify(self, slot, pending, record):
        if record is None or not isinstance(record, str):
            raise Corrupt(f"{slot} live slot record is absent during an unfinished quarantine; owner must inspect")
        seen = image_digest(record)
        if seen == pending["effect"]["pre"]:
            return "pre"
        if seen == pending["effect"]["post"]:
            return "post"
        raise Corrupt(f"{slot} live slot record matches neither side of the unfinished quarantine; "
                      f"owner must inspect")

    # ------------------------------------------------------------ bindings

    @staticmethod
    def _binding(rec, transition, repeat):
        """What every authorization and piece of evidence must name: the state
        it leaves, not the state a completed repeat already reached."""
        gen = rec["slot_generation"] - (1 if repeat and transition == "quarantine" else 0)
        return {"slot": rec["slot"], "op_id": rec["op_id"], "transition": transition,
                "identity_kind": rec["identity_kind"], "identity": rec["identity"], "slot_generation": gen,
                "seq": rec["seq"] - (1 if repeat else 0)}

    def _authorized(self, authorization, rec, transition, repeat, extra=()):
        _bound(authorization, f"{transition} authorization", **self._binding(rec, transition, repeat))
        if authorization.get("trust") != TRUST:
            raise Refused(f"{transition} authorization trust level is unavailable; only {TRUST} is accepted, "
                          f"and it is not proof of owner approval")
        missing = [f for f in AUTHORIZATION_FIELDS + extra if authorization.get(f) in (None, "")]
        if missing:
            raise Refused(f"{transition} authorization lacks {', '.join(missing)}")

    def _recovery_token(self, rec, token):
        if not isinstance(token, str) or not HEX32.match(token) \
                or not hmac.compare_digest(_verifier(rec["salt"], token), rec["verifier"]):
            raise Refused("recovery token does not match")

    # ------------------------------------------------------------ transitions

    def _step(self, slot, op_id, to, evidence, check, record, update=None, hook=None):
        """Moves the operation into `to` once `check(rec, repeat)` accepts the
        caller and the live record fits the phase. An exact repeat of the
        completed step changes nothing; with a trailing intent, only that
        transition's exact retry proceeds."""
        transition = TRANSITION[to]
        try:
            evidence_digest = digest(evidence)
        except (TypeError, ValueError, RecursionError):
            raise Refused(f"{transition} evidence is not plain data") from None
        d = self._op_dir(slot, op_id)
        with self._lock(slot):
            rec, pending = self._read(slot, op_id)
            if pending is not None:
                if (pending["to"], pending["evidence"]) != (to, evidence_digest):
                    raise Refused(f"{slot} operation {op_id} has an unfinished {TRANSITION.get(pending['to'], 'adoption')}; "
                                  f"only its exact retry may finish it")
                try:
                    self._sync_journal(d)
                except OSError as e:
                    raise Refused(f"{slot} recovery storage failed: {e}") from e
                if rec["seq"] == pending["seq"]:
                    check(rec, repeat=True)
                    if to == QUARANTINED:
                        self._quarantine_matches(rec, record)
                    return self._complete(d, rec, pending, state_written=True, resumed=True)
                check(rec, repeat=False)
                new = self._advance(rec, to, evidence_digest, update)
                if to != QUARANTINED:
                    self._observe(rec, record, to)
                    return self._complete(d, new, pending, resumed=True)
                if self._classify(slot, pending, record) == "post":
                    self._quarantine_matches(new, record)
                    return self._complete(d, new, pending, resumed=True)
                pre = self._lease_matches(rec, record)
                post = quarantined_record(slot, op_id, new["slot_generation"], pre)
                if image_digest(post) != pending["effect"]["post"]:
                    raise Corrupt(f"{slot} unfinished quarantine expected a different record; owner must inspect")
                return self._complete(d, new, pending, hook=self._hook(hook), pre=record, post=post, resumed=True)
            if rec["state"] == to and rec["gates"].get(transition) == evidence_digest:
                check(rec, repeat=True)
                if to == QUARANTINED:
                    self._quarantine_matches(rec, record)
                if to in TERMINAL_STATES:
                    self._ensure_marker(d, rec)
                return rec
            if to not in STEPS.get(rec["state"], ()):
                raise Refused(f"{slot} is {rec['state']}; {transition} is not a legal step from it")
            check(rec, repeat=False)
            new = self._advance(rec, to, evidence_digest, update)
            if to != QUARANTINED:
                self._observe(rec, record, to)
                return self._commit(d, rec, new, evidence_digest)
            run = self._hook(hook)
            pre = self._lease_matches(rec, record)
            post = quarantined_record(slot, op_id, new["slot_generation"], pre)
            effect = {"pre": image_digest(record), "post": image_digest(post)}
            return self._commit(d, rec, new, evidence_digest, effect=effect, hook=run, pre=record, post=post)

    @staticmethod
    def _hook(hook):
        if not callable(hook):
            raise Refused("quarantine needs a callable that replaces the live slot record")
        return hook

    @staticmethod
    def _advance(rec, to, evidence_digest, update):
        new = dict(rec, state=to, seq=rec["seq"] + 1, gates=dict(rec["gates"], **{TRANSITION[to]: evidence_digest}))
        if to == QUARANTINED:
            new["slot_generation"] += 1
        if update:
            update(new)
        return new

    def _observe(self, rec, record, to):
        if to == REQUESTED:
            self._lease_matches(rec, record)
        elif to == CANCELLED:
            self._released_or_unchanged(rec, record)
        else:
            self._quarantine_matches(rec, record)

    def _others_terminal(self, slot, op_id):
        try:
            names = sorted(os.listdir(self.root / slot))
        except OSError as e:
            raise Corrupt(f"{slot} recovery directory unreadable: {e}") from None
        for name in names:
            if name == op_id or name.startswith(".adopt-"):
                continue  # an adoption that was never published is not an operation
            if not NAME.match(name):
                raise Corrupt(f"{slot} recovery directory holds an unexpected entry")
            try:
                done = self.terminal(slot, name)
            except Corrupt as e:
                raise Corrupt(f"{slot} operation {name} cannot be trusted: {e}") from None
            except Refused as e:
                raise Refused(f"{slot} operation {name} is not finished: {e}") from None
            if done is None:
                raise Refused(f"{slot} operation {name} is not finished")

    def adopt(self, slot, op_id, record, *, legacy=False):
        """Records the live lease as ACTIVE for a new operation. Only a v1 or
        v2 leased record qualifies, or, when the owner passes legacy=True, a
        lease written before versioning; its digest proves only that the
        bytes other than the token line did not change. Every earlier
        operation of the slot must be terminal, and op_id must be new."""
        if type(legacy) is not bool:
            raise Refused("legacy must be True or False")
        lease = parse_record(record, slot, legacy=legacy)
        if lease.state != "leased":
            raise Refused(f"a version 2 {lease.state} record is never an adoption source")
        if legacy != (lease.kind == "legacy"):
            raise Refused("legacy adoption reads only a lease written before versioning")
        d = self._op_dir(slot, op_id)
        identity = _adoption_digest(lease.kind, lease.identity)
        try:
            self._ensure_dir(self.root)
        except OSError as e:
            raise Refused(f"cannot create the recovery store: {e}") from e
        with self._lock(slot):
            try:
                self._ensure_dir(self.root / slot)
            except OSError as e:
                raise Refused(f"cannot create the {slot} record directory: {e}") from e
            self._others_terminal(slot, op_id)
            if os.path.lexists(d):
                raise Refused(f"{slot} operation {op_id} already exists; a new recovery needs a new op_id")
            # The operation is built in a private directory and renamed into
            # place, so an interrupted adoption leaves no operation behind.
            tmp = self.root / slot / f".adopt-{op_id}.{secrets.token_hex(8)}"
            salt = secrets.token_hex(16)
            rec = {"schema": SCHEMA, "slot": slot, "op_id": op_id, "identity_kind": lease.kind,
                   "identity": lease.identity, "slot_generation": lease.generation, "state": ACTIVE, "seq": 0,
                   "salt": salt, "verifier": _verifier(salt, lease.token), "gates": {}}
            try:
                os.mkdir(tmp, 0o700)
                self._check_dir(tmp)
            except OSError as e:
                raise Refused(f"cannot create the {slot} record directory: {e}") from e
            self._commit(tmp, None, rec, identity)
            try:
                self.fault("before_publish")
                os.rename(tmp, d)
                self._fsync_dir(d.parent)
            except OSError as e:
                raise Refused(f"{slot} recovery storage failed: {e}") from e
            return rec

    def request(self, slot, *, op_id, authorization, record):
        """ACTIVE -> RECOVERY_REQUESTED, on an authorization of the same class
        as cancel: once gating exists, a request blocks the holder's own
        release. The live record must still be the adopted lease."""
        def check(rec, repeat):
            self._authorized(authorization, rec, "request", repeat)
        return self._step(slot, op_id, REQUESTED, authorization, check, record)

    def cancel(self, slot, *, op_id, authorization, record):
        """ACTIVE or RECOVERY_REQUESTED -> CANCELLED, terminal. Never writes
        the slot record and is impossible after quarantine. A cancelled operation
        cannot resume; a new recovery needs a new op_id and a fresh adoption."""
        def check(rec, repeat):
            self._authorized(authorization, rec, "cancel", repeat)
        return self._step(slot, op_id, CANCELLED, authorization, check, record)

    def quarantine(self, slot, *, op_id, authorization, recovery_token, record, replace_record):
        """RECOVERY_REQUESTED -> QUARANTINED. `record` is the live slot record,
        which must be the adopted lease. After a durable intent naming the
        pre- and post-image digests, `replace_record(pre, post)` must replace
        the live record with `post` only if it still equals `pre` (the caller
        holds the slot lock). The generation moves on and recovery_token,
        which the caller keeps privately, binds every later step."""
        if not isinstance(recovery_token, str) or not HEX32.match(recovery_token):
            raise Refused("recovery token must be 32 lowercase hex characters")

        def check(rec, repeat):
            self._authorized(authorization, rec, "quarantine", repeat, QUARANTINE_FIELDS)
            if authorization["quiescence_method"] not in QUIESCENCE_METHODS:
                raise Refused("authorization names an unknown quiescence method")
            if repeat:
                self._recovery_token(rec, recovery_token)
            elif hmac.compare_digest(_verifier(rec["salt"], recovery_token), rec["verifier"]):
                raise Refused("recovery token must not be the lease token")

        def update(new):
            new["quiescence_method"] = authorization["quiescence_method"]
            new["salt"] = secrets.token_hex(16)
            new["verifier"] = _verifier(new["salt"], recovery_token)
        return self._step(slot, op_id, QUARANTINED, authorization, check, record, update, replace_record)

    def verify_quiescence(self, slot, *, op_id, recovery_token, evidence, record):
        """QUARANTINED -> QUIESCENCE_VERIFIED. The evidence is the owner's
        account of how every possible old writer was stopped; this module
        records it and checks it is complete, it does not observe processes."""
        def check(rec, repeat):
            self._recovery_token(rec, recovery_token)
            _bound(evidence, "quiescence evidence", **self._binding(rec, "quiescence", repeat))
            if evidence.get("method") not in QUIESCENCE_METHODS:
                raise Refused("quiescence evidence names no accepted method")
            if evidence["method"] != rec["quiescence_method"]:
                raise Refused("quiescence evidence uses a different method than the owner authorized")
            if not evidence.get("actor") or not evidence.get("evidence_refs") \
                    or not isinstance(evidence["evidence_refs"], list):
                raise Refused("quiescence evidence lacks an actor or evidence references")
            accounted = evidence.get("accounted")
            gaps = [c for c in QUIESCENCE_ACCOUNTED if not (isinstance(accounted, dict) and accounted.get(c) is True)]
            if gaps:
                raise Refused(f"quiescence evidence does not account for {', '.join(gaps)}")
        return self._step(slot, op_id, QUIESCENT, evidence, check, record)

    def record_preservation(self, slot, *, op_id, recovery_token, evidence, record):
        """QUIESCENCE_VERIFIED -> PRESERVED, once every dirty, untracked or
        ignored file and every unpublished commit has a verified copy."""
        def check(rec, repeat):
            self._recovery_token(rec, recovery_token)
            _bound(evidence, "preservation evidence", **self._binding(rec, "preservation", repeat))
            if not HEX40.match(str(evidence.get("head", ""))):
                raise Refused("preservation evidence has no exact HEAD")
            if evidence.get("complete") is not True:
                raise Refused("preservation is not complete")
            inventory, preserved = evidence.get("inventory"), evidence.get("preserved")
            if not isinstance(inventory, dict) or not isinstance(preserved, dict) \
                    or any(not isinstance(inventory.get(k), list) for k in INVENTORY) \
                    or any(not isinstance(item, str) for k in INVENTORY for item in inventory[k]):
                raise Refused("preservation evidence has no full inventory")
            unsaved = [item for k in INVENTORY for item in inventory[k]
                       if not (isinstance(preserved.get(item), dict) and preserved[item].get("verified") is True
                               and preserved[item].get("to"))]
            if unsaved:
                raise Refused(f"not preserved and verified: {', '.join(map(str, unsaved[:5]))}")

        def update(new):
            new["preserved_head"] = evidence["head"]
        return self._step(slot, op_id, PRESERVED, evidence, check, record, update)

    def mark_ready(self, slot, *, op_id, recovery_token, final_check, record):
        """PRESERVED -> READY_FOR_REASSIGNMENT, on a final observation that the
        slot is clean, unleased and still at the preserved HEAD. READY is
        terminal for the operation and the slot record stays quarantined."""
        def check(rec, repeat):
            self._recovery_token(rec, recovery_token)
            _bound(final_check, "final check", **self._binding(rec, "ready", repeat))
            if final_check.get("clean") is not True or final_check.get("lease_absent") is not True:
                raise Refused("final check does not show a clean, unleased slot")
            if final_check.get("head") != rec["preserved_head"]:
                raise Refused("final check HEAD is not the preserved HEAD")
            if final_check.get("preservation") != rec["gates"].get("preservation"):
                raise Refused("final check is not bound to the recorded preservation")
        return self._step(slot, op_id, READY, final_check, check, record)

    # ------------------------------------------------------------ queries

    def state(self, slot, op_id):
        """(state, slot_generation, incomplete) for a readable record. Raises
        Refused or Corrupt otherwise."""
        rec, pending = self._read(slot, op_id)
        return rec["state"], rec["slot_generation"], pending is not None

    def reconcile(self, slot, op_id, record):
        """Classifies the live slot record against an unfinished quarantine:
        "none" when nothing is unfinished, "pre" when the replace did not
        happen (the same operation may retry), "post" when it did (the same
        operation may only finish). Anything else raises Corrupt."""
        _, pending = self._read(slot, op_id)
        if pending is None or "effect" not in pending:
            return "none"
        return self._classify(slot, pending, record)

    def terminal(self, slot, op_id):
        """CANCELLED or READY_FOR_REASSIGNMENT when the marker, the state and
        the journal agree; None while the operation is not terminal. A marker
        never overrides an inconsistent record: that raises Corrupt."""
        rec, pending = self._read(slot, op_id)
        marker = self._read_file(self._op_dir(slot, op_id) / "TERMINAL")
        if marker is None:
            return None
        if pending is not None or rec["state"] not in TERMINAL_STATES or marker != _marker(rec):
            raise Corrupt(f"{slot} terminal marker of operation {op_id} does not match its record")
        return rec["state"]

    def ready_for_reassignment(self, slot, op_id):
        """True only for a complete, consistent, marked READY_FOR_REASSIGNMENT."""
        try:
            return self.terminal(slot, op_id) == READY
        except Refused:
            return False

    def lease_action_allowed(self, slot, op_id, identity, slot_generation, token):
        """True only while the operation is ACTIVE and every credential
        matches. A request or quarantine refuses the old holder."""
        try:
            rec, pending = self._read(slot, op_id)
        except Refused:
            return False
        return (rec["state"] == ACTIVE and pending is None and rec["identity"] == identity
                and type(slot_generation) is int and rec["slot_generation"] == slot_generation and isinstance(token, str)
                and bool(HEX32.match(token)) and hmac.compare_digest(_verifier(rec["salt"], token), rec["verifier"]))

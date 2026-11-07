"""Orphan lease recovery state machine, isolated (CG-150 phase 2a).

Nothing in pool.sh, the coordinator or the campaign imports this module and it
has no command-line entry point, so it cannot quarantine, reclaim or reassign
a real slot. It records recovery decisions for one slot in a store directory
the caller names; wiring it to the pool's own lock and lease files is later
work. Policy and limits: docs/agent-pool.md, "Recovery state machine".

The only path to READY_FOR_REASSIGNMENT is

    ACTIVE -> RECOVERY_REQUESTED -> QUARANTINED -> QUIESCENCE_VERIFIED
           -> PRESERVED -> READY_FOR_REASSIGNMENT

and each step needs its own evidence. Every unreadable, inconsistent or
half-written record is refused, never read as available.
"""

import hashlib
import hmac
import json
import os
import re
import sys
import secrets
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path

ACTIVE = "ACTIVE"
REQUESTED = "RECOVERY_REQUESTED"
QUARANTINED = "QUARANTINED"
QUIESCENT = "QUIESCENCE_VERIFIED"
PRESERVED = "PRESERVED"
READY = "READY_FOR_REASSIGNMENT"
ORDER = (ACTIVE, REQUESTED, QUARANTINED, QUIESCENT, PRESERVED, READY)
# Each state names the evidence gates that must already be recorded in it.
GATES = {
    ACTIVE: (),
    REQUESTED: ("request",),
    QUARANTINED: ("request", "authorization"),
    QUIESCENT: ("request", "authorization", "quiescence"),
    PRESERVED: ("request", "authorization", "quiescence", "preservation"),
    READY: ("request", "authorization", "quiescence", "preservation", "final_check"),
}

# The step that leads into each state, and the evidence gate it records.
NEXT = {None: ACTIVE, ACTIVE: REQUESTED, REQUESTED: QUARANTINED, QUARANTINED: QUIESCENT, QUIESCENT: PRESERVED,
        PRESERVED: READY}
GATE_OF = {REQUESTED: "request", QUARANTINED: "authorization", QUIESCENT: "quiescence", PRESERVED: "preservation",
           READY: "final_check"}
# The fields a completion must share with its intent. A resumed completion
# adds "resumed" and its own time; both are ignored when they are compared.
ENTRY_KEYS = ("seq", "op_id", "from", "to", "generation", "evidence")

SCHEMA = 1
LEASE_VERSION = "1"
QUIESCENCE_METHODS = ("containment-teardown", "owner-quiescence", "reboot-runbook")
# Every class of writer the owner must have accounted for. A dead PID, an empty
# process group, no process cwd or a stale heartbeat accounts for none of them.
QUIESCENCE_ACCOUNTED = ("old_coordinators", "wrappers", "escaped_descendants", "remote_writers",
                        "automatic_restarts")
INVENTORY = ("dirty", "untracked", "ignored", "unpublished_commits")
AUTHORIZATION_FIELDS = ("slot", "lease_id", "generation", "op_id", "actor", "reason",
                        "assessment_digest", "quiescence_method", "preservation_disposition", "utc")
SECRET_KEY = re.compile(r"token|secret|password|verifier|salt", re.I)
NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
HEX32 = re.compile(r"^[0-9a-f]{32}$")
HEX40 = re.compile(r"^[0-9a-f]{40}$")


class Refused(Exception):
    """The transition was not made. A refusal on validation changes nothing; a
    storage failure may leave an unfinished transition, which never
    authorizes reassignment and only the same operation can finish."""


class Busy(Refused):
    """Another transition holds the slot, or a crashed one left its lock."""


class Corrupt(Refused):
    """The stored record cannot be trusted; the owner must inspect it."""


def digest(obj):
    return hashlib.sha256(json.dumps(obj, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def _verifier(salt, token):
    return hmac.new(bytes.fromhex(salt), token.encode(), hashlib.sha256).hexdigest()


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
        if evidence.get(k) != v:
            raise Refused(f"{where} is not bound to {k}={v!r}")


def parse_lease(text, slot):
    """The identity of a version 1 lease. Legacy, unsupported, malformed or
    ambiguous records are refused."""
    if not isinstance(text, str):
        raise Refused("lease text is missing")
    fields = {}
    for line in text.splitlines():
        if not line:
            continue
        k, sep, v = line.partition("=")
        if not sep or not k:
            raise Refused("malformed lease line")
        if k in fields:
            raise Refused(f"lease repeats field {k}")
        fields[k] = v
    if "version" not in fields:
        raise Refused("lease written before versioning; owner must inspect")
    if fields["version"] != LEASE_VERSION:
        raise Refused(f"lease schema version {fields['version']!r} unsupported")
    if fields.get("slot") != slot:
        raise Refused("lease names a different slot")
    if not HEX32.match(fields.get("lease_id", "")) or not HEX32.match(fields.get("token", "")):
        raise Refused("lease has no valid lease_id or token")
    if not fields.get("task"):
        raise Refused("lease has no task")
    return fields["lease_id"], fields["token"]


class RecoveryStore:
    """Per-slot recovery records under `root`, which the caller names; there
    is no default. `fault(point)` is a test hook called at each crash point."""

    def __init__(self, root, fault=None):
        self.root = Path(root)
        self.fault = fault or (lambda point: None)

    # ------------------------------------------------------------ storage

    def _dir(self, slot):
        if not isinstance(slot, str) or not NAME.match(slot):
            raise Refused(f"invalid slot name {slot!r}")
        return self.root / slot

    @contextmanager
    def _lock(self, slot):
        # A lock left by a crash is never broken here: the slot stays Busy
        # until the owner has checked that no transition is still running.
        self._dir(slot)
        lock = self.root / f".{slot}.lock"
        try:
            os.mkdir(lock)
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
        # Windows cannot open a directory, so every transition is refused there.
        fd = os.open(path, os.O_RDONLY)
        try:
            self._sync(fd)
        finally:
            os.close(fd)

    def _mkdir(self, path):
        if not path.is_dir():
            path.mkdir(mode=0o700)
            self._fsync_dir(path.parent)

    def _append(self, slot, entry):
        path = self._dir(slot) / "journal.jsonl"
        new = not path.exists()
        fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
        try:
            self._write_all(fd, (json.dumps(entry, sort_keys=True) + "\n").encode())
            self._sync(fd)
        finally:
            os.close(fd)
        if new:
            self._fsync_dir(path.parent)

    def _write_state(self, slot, record):
        d = self._dir(slot)
        tmp = d / f".state.{secrets.token_hex(8)}.tmp"
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            try:
                self._write_all(fd, json.dumps(record, sort_keys=True).encode())
                self._sync(fd)
            finally:
                os.close(fd)
            self.fault("before_state_rename")
            os.replace(tmp, d / "state.json")
        except BaseException:
            tmp.unlink(missing_ok=True)
            raise
        self._fsync_dir(d)

    def _journal(self, slot):
        try:
            lines = (self._dir(slot) / "journal.jsonl").read_text().splitlines()
            return [json.loads(line) for line in lines]
        except (OSError, ValueError) as e:
            raise Corrupt(f"{slot} recovery journal unreadable: {e}") from None

    @staticmethod
    def _replay(slot, journal):
        """(committed, pending): the completed transitions, which must run from
        adoption one legal step at a time, each completing its latest intent;
        and the latest intent after the last completion, if any. Intents that
        never completed are allowed: a crash or a refused write leaves them."""
        committed, pending = [], None
        for e in journal:
            if not isinstance(e, dict) or e.get("kind") not in ("intent", "complete"):
                raise Corrupt(f"{slot} journal has a malformed entry")
            head = committed[-1] if committed else None
            frm = head["to"] if head else None
            gen = 1 if head is None else head["generation"] + (e.get("to") == QUARANTINED)
            op_ok = (e.get("op_id") is None if e.get("to") == ACTIVE else
                     isinstance(e.get("op_id"), str) and NAME.match(e["op_id"]) if e.get("to") == REQUESTED else
                     head is not None and e.get("op_id") == head["op_id"])
            if e.get("seq") != len(committed) or e.get("from") != frm or e.get("to") != NEXT.get(frm) \
                    or e.get("generation") != gen or not op_ok or not isinstance(e.get("evidence"), str):
                raise Corrupt(f"{slot} journal entry {e.get('seq')!r} is not the next legal transition")
            if e["kind"] == "intent":
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
        if (rec["seq"], rec["state"], rec["generation"], rec["op_id"]) != \
                (last["seq"], last["to"], last["generation"], last["op_id"]):
            return False
        if history[0]["evidence"] != digest({"lease_id": rec["lease_id"]}):
            return False
        return all(rec["gates"].get(GATE_OF[e["to"]]) == e["evidence"] for e in history[1:])

    def _read(self, slot):
        """(record, pending). pending is the intent of a transition that
        replaced the state but whose completion was never written; such a
        record is never ready, and only that operation's retry finishes it."""
        try:
            raw = (self._dir(slot) / "state.json").read_text()
        except FileNotFoundError:
            raise Refused(f"{slot} has no recovery record") from None
        except OSError as e:
            raise Corrupt(f"{slot} recovery record unreadable: {e}") from None
        try:
            rec = json.loads(raw)
        except ValueError as e:
            raise Corrupt(f"{slot} recovery record unreadable: {e}") from None
        self._validate(slot, rec)
        committed, pending = self._replay(slot, self._journal(slot))
        if committed and self._matches(rec, committed):
            return rec, None
        if pending and self._matches(rec, committed + [pending]):
            return rec, pending
        raise Corrupt(f"{slot} recovery record does not match its journal")

    @staticmethod
    def _validate(slot, rec):
        ok = (isinstance(rec, dict) and rec.get("schema") == SCHEMA and rec.get("slot") == slot
              and rec.get("state") in GATES and HEX32.match(str(rec.get("lease_id", "")))
              and isinstance(rec.get("generation"), int) and rec["generation"] >= 1
              and isinstance(rec.get("seq"), int) and rec["seq"] >= 0
              and HEX32.match(str(rec.get("salt", ""))) and re.match(r"^[0-9a-f]{64}$", str(rec.get("verifier", "")))
              and isinstance(rec.get("gates"), dict)
              and (rec.get("op_id") is None if rec.get("state") == ACTIVE else NAME.match(str(rec.get("op_id")))))
        if not ok:
            raise Corrupt(f"{slot} recovery record is malformed or of an unsupported schema")
        if sorted(rec["gates"]) != sorted(GATES[rec["state"]]):
            raise Corrupt(f"{slot} recovery record in {rec['state']} lacks or adds evidence gates")
        bound = (("quiescence_method", "authorization", lambda v: v in QUIESCENCE_METHODS),
                 ("preserved_head", "preservation", lambda v: bool(HEX40.match(str(v)))))
        for field, gate, valid in bound:
            if (field in rec) != (gate in rec["gates"]) or (field in rec and not valid(rec[field])):
                raise Corrupt(f"{slot} recovery record has an inconsistent {field}")

    def _commit(self, slot, rec, new, evidence_digest):
        entry = {"seq": new["seq"], "op_id": new["op_id"], "from": rec["state"] if rec else None,
                 "to": new["state"], "generation": new["generation"], "evidence": evidence_digest,
                 "utc": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")}
        self._validate(slot, new)
        try:
            self.fault("before_intent")
            self._append(slot, {**entry, "kind": "intent"})
            self.fault("after_intent")
            self._write_state(slot, new)
            self.fault("after_state")
            self._append(slot, {**entry, "kind": "complete"})
        except OSError as e:
            raise Refused(f"{slot} recovery storage failed: {e}") from e
        return new

    def _finish(self, slot, pending):
        """Writes the completion a crash left out, for the operation's own retry.
        The state replacement is made durable first: its directory sync may be
        the step that failed."""
        try:
            self._fsync_dir(self._dir(slot))
            self._append(slot, {**{k: pending[k] for k in ENTRY_KEYS}, "kind": "complete", "resumed": True,
                                "utc": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")})
        except OSError as e:
            raise Refused(f"{slot} recovery storage failed: {e}") from e

    def _step(self, slot, op_id, frm, to, gate, evidence, check, update=None):
        """Moves `slot` from `frm` to `to` once `check(rec)` accepts the caller.
        Repeating a completed step with the same evidence returns it unchanged;
        anything else on a record already past `frm` is refused."""
        try:
            evidence_digest = digest(evidence)
        except (TypeError, ValueError):
            raise Refused(f"{gate} evidence is not plain data") from None
        with self._lock(slot):
            rec, pending = self._read(slot)
            if rec["state"] == to and rec["op_id"] == op_id and rec["gates"].get(gate) == evidence_digest:
                check(rec, repeat=True)
                if pending:
                    self._finish(slot, pending)
                return rec
            if pending:
                raise Refused(f"{slot} has an unfinished transition of operation {rec['op_id']}; "
                              f"only its own retry can finish it")
            if rec["state"] != frm:
                raise Refused(f"{slot} is {rec['state']}; {to} needs {frm}")
            check(rec, repeat=False)
            new = dict(rec, state=to, op_id=op_id, seq=rec["seq"] + 1,
                       gates=dict(rec["gates"], **{gate: evidence_digest}))
            if update:
                update(new)
            return self._commit(slot, rec, new, evidence_digest)

    def _recovery_token(self, rec, op_id, generation, token):
        if rec["op_id"] != op_id or rec["generation"] != generation:
            raise Refused("operation or generation does not match the recovery record")
        if not isinstance(token, str) or not hmac.compare_digest(_verifier(rec["salt"], token), rec["verifier"]):
            raise Refused("recovery token does not match")

    # ------------------------------------------------------------ transitions

    def adopt(self, slot, lease_text):
        """Records a version 1 lease as ACTIVE at generation 1."""
        lease_id, token = parse_lease(lease_text, slot)
        d = self._dir(slot)
        try:
            self.root.parent.mkdir(parents=True, exist_ok=True)
            self._mkdir(self.root)
        except OSError as e:
            raise Refused(f"cannot create the recovery store: {e}") from e
        identity = digest({"lease_id": lease_id})
        with self._lock(slot):
            if (d / "state.json").exists():
                rec, pending = self._read(slot)
                if pending and rec["seq"] == 0 and pending["evidence"] == identity \
                        and hmac.compare_digest(_verifier(rec["salt"], token), rec["verifier"]):
                    self._finish(slot, pending)
                    return rec
                raise Refused(f"{slot} already has a recovery record")
            if (d / "journal.jsonl").exists():
                committed, _ = self._replay(slot, self._journal(slot))
                if committed:
                    raise Corrupt(f"{slot} journal records transitions but its state is gone; owner must inspect")
                if any(e["evidence"] != identity for e in self._journal(slot)):
                    raise Refused(f"{slot} has an interrupted adoption of a different lease; owner must inspect")
            try:
                self._mkdir(d)
            except OSError as e:
                raise Refused(f"cannot create the {slot} record directory: {e}") from e
            salt = secrets.token_hex(16)
            rec = {"schema": SCHEMA, "slot": slot, "lease_id": lease_id, "generation": 1, "state": ACTIVE,
                   "op_id": None, "seq": 0, "salt": salt, "verifier": _verifier(salt, token), "gates": {}}
            return self._commit(slot, None, rec, identity)

    def request(self, slot, *, op_id, lease_id, generation):
        """ACTIVE -> RECOVERY_REQUESTED. Records that recovery was asked for
        and authorizes nothing. From here on lease_action_allowed refuses the
        lease's own actions, and this phase has no way back to ACTIVE."""
        request = {"slot": slot, "op_id": op_id, "lease_id": lease_id, "generation": generation}
        if not isinstance(op_id, str) or not NAME.match(op_id):
            raise Refused("invalid operation id")

        def check(rec, repeat):
            if rec["lease_id"] != lease_id or rec["generation"] != generation:
                raise Refused("request is not bound to the current lease and generation")
        return self._step(slot, op_id, ACTIVE, REQUESTED, "request", request, check)

    def quarantine(self, slot, *, op_id, authorization, recovery_token):
        """RECOVERY_REQUESTED -> QUARANTINED, on the owner's authorization for
        exactly this slot, lease, generation and operation. The generation
        moves on, so the old lease's token can no longer act; recovery_token,
        which the caller keeps privately, binds every later step."""
        if not isinstance(recovery_token, str) or not HEX32.match(recovery_token):
            raise Refused("recovery token must be 32 lowercase hex characters")

        def check(rec, repeat):
            gen = rec["generation"] - 1 if repeat else rec["generation"]
            _bound(authorization, "authorization", slot=slot, lease_id=rec["lease_id"], generation=gen,
                   op_id=op_id)
            missing = [f for f in AUTHORIZATION_FIELDS if authorization.get(f) in (None, "")]
            if missing:
                raise Refused(f"authorization lacks {', '.join(missing)}")
            if authorization["quiescence_method"] not in QUIESCENCE_METHODS:
                raise Refused(f"authorization names an unknown quiescence method")
            if repeat:
                self._recovery_token(rec, op_id, rec["generation"], recovery_token)
                if rec["quiescence_method"] != authorization["quiescence_method"]:
                    raise Refused("authorization names a different quiescence method")
            elif rec["op_id"] != op_id:
                raise Refused("authorization is for a different recovery operation")
            elif hmac.compare_digest(_verifier(rec["salt"], recovery_token), rec["verifier"]):
                raise Refused("recovery token must not be the lease token")

        def update(new):
            new["quiescence_method"] = authorization["quiescence_method"]
            new["generation"] += 1
            new["salt"] = secrets.token_hex(16)
            new["verifier"] = _verifier(new["salt"], recovery_token)
        return self._step(slot, op_id, REQUESTED, QUARANTINED, "authorization", authorization, check, update)

    def verify_quiescence(self, slot, *, op_id, generation, recovery_token, evidence):
        """QUARANTINED -> QUIESCENCE_VERIFIED. The evidence is the owner's
        account of how every possible old writer was stopped; this module
        records it and checks it is complete, it does not observe processes."""
        def check(rec, repeat):
            self._recovery_token(rec, op_id, generation, recovery_token)
            _bound(evidence, "quiescence evidence", slot=slot, op_id=op_id, generation=generation)
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
        return self._step(slot, op_id, QUARANTINED, QUIESCENT, "quiescence", evidence, check)

    def record_preservation(self, slot, *, op_id, generation, recovery_token, evidence):
        """QUIESCENCE_VERIFIED -> PRESERVED, once every dirty, untracked or
        ignored file and every unpublished commit has a verified copy."""
        def check(rec, repeat):
            self._recovery_token(rec, op_id, generation, recovery_token)
            _bound(evidence, "preservation evidence", slot=slot, op_id=op_id, generation=generation)
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
        return self._step(slot, op_id, QUIESCENT, PRESERVED, "preservation", evidence, check, update)

    def mark_ready(self, slot, *, op_id, generation, recovery_token, final_check):
        """PRESERVED -> READY_FOR_REASSIGNMENT, on a final observation that the
        slot is clean, unleased and still at the preserved HEAD."""
        def check(rec, repeat):
            self._recovery_token(rec, op_id, generation, recovery_token)
            _bound(final_check, "final check", slot=slot, op_id=op_id, generation=generation)
            if final_check.get("clean") is not True or final_check.get("lease_absent") is not True:
                raise Refused("final check does not show a clean, unleased slot")
            if final_check.get("head") != rec["preserved_head"]:
                raise Refused("final check HEAD is not the preserved HEAD")
            if final_check.get("preservation") != rec["gates"].get("preservation"):
                raise Refused("final check is not bound to the recorded preservation")
        return self._step(slot, op_id, PRESERVED, READY, "final_check", final_check, check)

    # ------------------------------------------------------------ queries

    def state(self, slot):
        """(state, generation, incomplete) for a readable record. Raises
        Refused or Corrupt otherwise."""
        rec, pending = self._read(slot)
        return rec["state"], rec["generation"], pending is not None

    def ready_for_reassignment(self, slot):
        """True only for a complete, consistent READY_FOR_REASSIGNMENT record."""
        try:
            rec, incomplete = self._read(slot)
        except Refused:
            return False
        return rec["state"] == READY and not incomplete

    def lease_action_allowed(self, slot, lease_id, generation, token):
        """True only while the lease is ACTIVE and every credential matches.
        Recovery moves the generation on, so a stale owner is refused."""
        try:
            rec, incomplete = self._read(slot)
        except Refused:
            return False
        return (rec["state"] == ACTIVE and not incomplete and rec["lease_id"] == lease_id
                and rec["generation"] == generation and isinstance(token, str)
                and hmac.compare_digest(_verifier(rec["salt"], token), rec["verifier"]))

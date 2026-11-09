#!/usr/bin/env python3
"""Recovery command line for synthetic pools only (CG-150 phase 2b-2c-2).
pool.sh runs it; policy and limits: docs/agent-pool.md, "Recovery command line".

    python3 -I -B recovery_cli.py check <pool> <slot>
    python3 -I -B recovery_cli.py inspect <pool> <slot> [--op <op_id>]
    python3 -I -B recovery_cli.py reconcile <pool> <slot> --op <op_id>
    python3 -I -B recovery_cli.py adopt <pool> <slot>
    python3 -I -B recovery_cli.py request|cancel|quarantine|quiescence|ready <pool> <slot> --op <op_id>
    python3 -I -B recovery_cli.py preservation <pool> <slot> --op <op_id> --evidence <file>

Every action refuses a pool that is not a marked, migrated synthetic pool, and
refuses the live pool and any path aliasing it. That interlock, and the
interactive confirmation every mutating action asks for on /dev/tty, prevent
accidents; they authenticate nobody. Any process of the owner's uid can forge
the marker, drive a pseudo-terminal or call recovery_state.py directly.

Run it only through `pool.sh recover`, which takes the slot lock: run
directly, it accepts any existing lock directory as held. The mutating
actions run only under the slot lock pool.sh holds. Exit 0 is
done, 1 refused with nothing written, 2 usage, 3 an interpreter older than
MIN_PYTHON, 4 a write whose step did not finish, or an unconfirmed outcome.
"""

import json
import os
import pwd
import re
import secrets
import stat
import sys

MIN_PYTHON = (3, 9)
if sys.version_info < MIN_PYTHON:
    sys.exit(3)

from pathlib import Path  # noqa: E402

sys.path.insert(0, str(Path(__file__).resolve().parent))
import pool_record as pr  # noqa: E402
import recovery_state as rs  # noqa: E402

SYNTHETIC = ".synthetic-pool"
SYNTHETIC_HEAD = "codegraph-synthetic-recovery-pool=1"
MUTATING = ("adopt", "request", "cancel", "quarantine", "quiescence", "preservation", "ready")
ACTIONS = MUTATING + ("reconcile", "inspect")
TARGET = {"request": rs.REQUESTED, "cancel": rs.CANCELLED, "quarantine": rs.QUARANTINED,
          "quiescence": rs.QUIESCENT, "preservation": rs.PRESERVED, "ready": rs.READY}
NEXT = {rs.ACTIVE: "request or cancel", rs.REQUESTED: "quarantine or cancel", rs.QUARANTINED: "quiescence",
        rs.QUIESCENT: "preservation", rs.PRESERVED: "ready", rs.CANCELLED: "none; the operation is terminal",
        rs.READY: "none; reassignment is not available in this phase"}
MAX_INPUT = 1024
MAX_EVIDENCE = 64 * 1024
NOTICE = ("This confirmation is an accident-prevention interlock, not authentication: trust level "
          f"{rs.TRUST}. Any process of this user can imitate a terminal.")


class Unfinished(rs.Refused):
    """A transition was started and not completed; only its exact retry finishes it."""


def _clean(text):
    return re.sub(r"[^\x20-\x7e]", " ", str(text))


# ---------------------------------------------------------------- interlock

def live_pool():
    """The live pool, as pool.sh names it by default. The home directory comes
    from the user database, so a redirected HOME does not move it."""
    return os.path.join(pwd.getpwuid(os.getuid()).pw_dir, "Projekti", "codegraph-evidence", "pool")


def _ids(path):
    """(dev, inode) of `path` and of every ancestor that exists."""
    out = set()
    while True:
        try:
            st = os.stat(path)
            out.add((st.st_dev, st.st_ino))
        except OSError:
            pass
        parent = os.path.dirname(path)
        if parent == path:
            return out
        path = parent


def _related(a, b):
    return a == b or a.startswith(b.rstrip("/") + "/") or b.startswith(a.rstrip("/") + "/")


def _not_live(pool):
    live = live_pool()
    for form in {live, os.path.realpath(live)}:
        if _related(pool, form):
            raise rs.Refused("the pool path is the live pool, inside it or one of its parents; nothing changed")
    try:
        st = os.stat(live)
        live_id = (st.st_dev, st.st_ino)
    except OSError:
        live_id = None
    # Inodes catch aliases realpath keeps, such as macOS firmlinks.
    if live_id is not None and live_id in _ids(pool):
        raise rs.Refused("the pool path aliases the live pool or a path inside it; nothing changed")
    try:
        st = os.stat(pool)
    except OSError:
        return
    if (st.st_dev, st.st_ino) in _ids(live):
        raise rs.Refused("the pool path aliases the live pool or one of its parents; nothing changed")


def _private_file(path, limit, what):
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except FileNotFoundError:
        raise rs.Refused(f"{what} is missing") from None
    except OSError:
        raise rs.Refused(f"{what} is not a readable regular file") from None
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or st.st_uid != os.getuid() or st.st_mode & 0o077 or st.st_nlink != 1:
            raise rs.Refused(f"{what} is not a private regular file of this user")
        data = os.read(fd, limit + 1)
        if len(data) > limit:
            raise rs.Refused(f"{what} is too large")
        return data.decode("ascii")
    except UnicodeDecodeError:
        raise rs.Refused(f"{what} is not ASCII text") from None
    finally:
        os.close(fd)


def synthetic_pool(pool, slot):
    """Refuses unless `pool` is a marked, migrated synthetic pool that is not
    the live one and `slot` is one of its slots. An accident guard: a process
    of this user can forge every part of it."""
    if not rs.SUPPORTED:
        raise rs.Refused("recovery is not supported on this platform; nothing changed")
    if not isinstance(pool, str) or not os.path.isabs(pool):
        raise rs.Refused("recover needs an explicit absolute pool path")
    if os.path.realpath(pool) != pool:
        raise rs.Refused("the pool path must be canonical: no symlink, no . or .. part, no trailing slash")
    if not isinstance(slot, str) or not rs.NAME.match(slot):
        raise rs.Refused("invalid slot name")
    _not_live(pool)
    try:
        st = os.lstat(pool)
    except OSError:
        raise rs.Refused("the pool directory does not exist or cannot be read") from None
    if not stat.S_ISDIR(st.st_mode) or st.st_uid != os.getuid():
        raise rs.Refused("the pool path is not a directory of this user")
    lines = _private_file(os.path.join(pool, SYNTHETIC), 4096, "the synthetic pool marker").split("\n")
    slots = lines[1][len("slots="):].split(" ") if len(lines) == 3 and lines[1].startswith("slots=") else []
    if lines[0] != SYNTHETIC_HEAD or lines[-1] != "" or not slots or len(set(slots)) != len(slots) \
            or not all(rs.NAME.match(s) for s in slots):
        raise rs.Refused("the synthetic pool marker is malformed")
    if slot not in slots:
        raise rs.Refused(f"{slot} is not a slot of this synthetic pool")
    if pr.marker_state(pool) != pr.MIGRATED:
        raise rs.Refused("the synthetic pool is not migrated; recovery runs only on version 2 slot records")
    for s in slots:
        try:
            st = os.lstat(os.path.join(pool, f"{s}.lease"))
            if not stat.S_ISREG(st.st_mode) or st.st_nlink != 1:
                raise OSError
        except OSError:
            raise rs.Refused(f"the synthetic pool lacks a regular, singly linked slot record for {s}") from None


# ---------------------------------------------------------------- terminal

class Terminal:
    """The controlling terminal, opened directly: never stdin or stdout."""

    def __init__(self):
        try:
            self.fd = os.open("/dev/tty", os.O_RDWR | os.O_NOCTTY)
        except OSError:
            raise rs.Refused("this action needs an interactive terminal; nothing changed") from None
        if not os.isatty(self.fd):
            os.close(self.fd)
            raise rs.Refused("this action needs an interactive terminal; nothing changed")

    def write(self, text):
        data = text.encode()
        while data:
            data = data[os.write(self.fd, data):]

    def ask(self, prompt, secret=False):
        import termios
        old = termios.tcgetattr(self.fd) if secret else None
        if secret:
            # Echo goes off before the prompt appears, and no typed input is discarded.
            new = termios.tcgetattr(self.fd)
            new[3] &= ~termios.ECHO
            termios.tcsetattr(self.fd, termios.TCSANOW, new)
        self.write(prompt)
        try:
            line = b""
            while not line.endswith(b"\n"):
                chunk = os.read(self.fd, 1)
                if not chunk:
                    raise rs.Refused("the terminal closed; nothing changed")
                line += chunk
                if len(line) > MAX_INPUT:
                    raise rs.Refused("terminal input is too long; nothing changed")
        finally:
            if secret:
                termios.tcsetattr(self.fd, termios.TCSANOW, old)
                self.write("\n")
        text = line.decode("ascii", "replace").strip()
        if re.search(r"[^\x20-\x7e]", text):
            raise rs.Refused("terminal input must be printable ASCII; nothing changed")
        return text


# ---------------------------------------------------------------- pool access

def _store(pool, fault=None):
    return rs.RecoveryStore(Path(pool) / "recovery", fault=fault, held_lock=lambda s: Path(pool) / f".{s}.lock")


def _record_path(pool, slot):
    return Path(pool) / f"{slot}.lease"


def _live(pool, slot):
    return pr.read_record(_record_path(pool, slot))


def _sound_history(pool, slot, op):
    """Refuses unless recovery/ is sound and every operation of the slot other
    than `op` is cancelled and still fits the live record."""
    verdict, entries = pr.gate_entries(pool, slot)
    if verdict is not None and verdict[0] != "none":
        raise rs.Refused(f"{slot} recovery state is {verdict[0]}: {verdict[3]}; nothing changed")
    names = [name for name, _ in entries]
    if op is not None and op not in names:
        raise rs.Refused(f"{slot} has no recovery operation {op}")
    for name, (cls, _, state, reason) in entries:
        if name != op and cls != "cancelled":
            raise rs.Refused(f"{slot} recovery entry {_clean(name)} is {cls}{' (' + state + ')' if state else ''}: "
                             f"{reason}; nothing changed")
    if op is not None:
        store = _store(pool)
        left = pr._listing(store, Path(pool) / "recovery" / slot / op) - pr.OP_FILES
        if left:
            raise rs.Refused(f"{slot} operation {op} holds a temporary file left by an interrupted write; "
                             "owner must inspect; nothing changed")


def _fingerprint(pool, slot):
    """Everything a step may write: the slot's recovery tree and its slot
    record. A tree that cannot be read compares unequal to anything."""
    root = Path(pool) / "recovery"
    out = [os.path.lexists(root), os.path.lexists(root / slot)]
    try:
        for d, dirs, files in os.walk(root / slot):
            dirs.sort()
            for n in sorted(dirs + files):
                path = os.path.join(d, n)
                st = os.lstat(path)
                body = None
                if stat.S_ISREG(st.st_mode):
                    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
                    try:
                        body = os.read(fd, st.st_size + 1)
                    finally:
                        os.close(fd)
                out.append((path, st.st_mode, body))
        out.append(_live(pool, slot))
    except (OSError, rs.Refused):
        return object()
    return out


def _actor():
    return pwd.getpwuid(os.getuid()).pw_name


def _show(term, pool, slot, op, transition, kind, identity, generation, seq, payload=None):
    term.write("\n" + NOTICE + "\n"
               f"pool: {pool}\nslot: {slot}\noperation: {op}\ntransition: {transition}\n"
               f"adopted identity: {kind} {identity}\nslot generation: {generation}\njournal sequence: {seq}\n")
    if payload is not None:
        term.write(f"{transition} record (canonical JSON):\n"
                   f"{json.dumps(payload, sort_keys=True, separators=(',', ':'))}\n"
                   f"digest: {rs.digest(payload)}\n")


def _confirm(term, slot, op, transition, identity, generation, seq, payload=None):
    checks = [("slot", slot), ("operation", op), ("transition", transition),
              ("first 12 characters of the adopted identity", identity[:12]),
              ("slot generation", str(generation)), ("journal sequence", str(seq))]
    if payload is not None:
        checks.append(("first 12 characters of the digest", rs.digest(payload)[:12]))
    for label, want in checks:
        if term.ask(f"Retype the {label}: ") != want:
            raise rs.Refused(f"the retyped {label} does not match; nothing changed")


# ---------------------------------------------------------------- actions

def adopt(term, pool, slot, store):
    record = _live(pool, slot)
    lease = rs.parse_record(record, slot)
    if lease.kind != "v2" or lease.state != "leased":
        raise rs.Refused(f"{slot} holds no version 2 lease; only a v2 leased record is adopted")
    _sound_history(pool, slot, None)
    op = f"rec-{rs._now().replace('-', '').replace(':', '')}-{secrets.token_hex(4)}"
    _show(term, pool, slot, op, "adopt", lease.kind, lease.identity, lease.generation, 0)
    _confirm(term, slot, op, "adopt", lease.identity, lease.generation, 0)
    rec = store.adopt(slot, op, record)
    return rec


def _phase(store, slot, op, target):
    """(rec, repeat, expected): the binding mode of `target` and, for an
    interrupted or marker-less step, the digest its retry must reproduce.
    None for rec when the operation is already in `target`."""
    rec, pending = store._read(slot, op)
    if rec["identity_kind"] != "v2":
        raise rs.Refused("only operations on version 2 leases are handled")
    transition = rs.TRANSITION[target]
    if pending is not None:
        if pending["to"] != target:
            raise rs.Refused(f"{slot} operation {op} has an unfinished {rs.TRANSITION[pending['to']]}; "
                             "only its exact retry may finish it")
        return rec, rec["seq"] == pending["seq"], pending["evidence"]
    if rec["state"] == target:
        if target in rs.TERMINAL_STATES and store.terminal(slot, op) is None:
            return rec, True, rec["gates"][transition]
        return None, False, None
    return rec, False, None


def _retyped(term, expected, fields):
    """The original values of an interrupted authorization, retyped."""
    if expected is None:
        return None
    term.write("An interrupted step can finish only with its exact original record. "
               f"Journal digest begins {expected[:12]}.\n")
    return {k: term.ask(f"Original {k}: ") for k in fields}


def _same(payload, expected):
    if expected is not None and rs.digest(payload) != expected:
        raise rs.Refused("this differs from the interrupted step's record (changed or regenerated); nothing changed")


def authorize(term, store, pool, slot, op, action):
    target = TARGET[action]
    rec, repeat, expected = _phase(store, slot, op, target)
    if rec is None:
        return None
    binding = store._binding(rec, action, repeat)
    extra = ("quiescence_method", "preservation_disposition", "assessment_digest") if action == "quarantine" else ()
    old = _retyped(term, expected, ("utc", "reason") + extra)
    if old is None:
        fields = {"reason": term.ask("Reason: ")}
        if action == "quarantine":
            fields["quiescence_method"] = term.ask(f"Quiescence method ({', '.join(rs.QUIESCENCE_METHODS)}): ")
            fields["preservation_disposition"] = term.ask("Preservation disposition: ")
            fields["assessment_digest"] = term.ask("Assessment digest (64 hex): ")
        fields["utc"] = rs._now()
    else:
        fields = old
    auth = {**binding, "trust": rs.TRUST, "actor": _actor(), **fields}
    _same(auth, expected)
    _show(term, pool, slot, op, action, rec["identity_kind"], rec["identity"], binding["slot_generation"],
          binding["seq"], auth)
    _confirm(term, slot, op, action, rec["identity"], binding["slot_generation"], binding["seq"], auth)
    record = _live(pool, slot)
    if action != "quarantine":
        return getattr(store, action)(slot, op_id=op, authorization=auth, record=record)
    if repeat:
        token = term.ask("Recovery token you retained (not echoed): ", secret=True)
    else:
        if expected is not None:
            term.write("The interrupted quarantine never recorded a token. Any token shown before is void; "
                       "a new one replaces it.\n")
        token = secrets.token_hex(16)
        term.write("\nRECOVERY TOKEN, shown once. Keep it privately; it is never stored, logged or shown again,\n"
                   "and every later step needs it. A lost token leaves the operation quarantined for good.\n"
                   f"{token}\n\n")
        if term.ask("Retype the recovery token to confirm you kept it (not echoed): ", secret=True) != token:
            raise rs.Refused("the recovery token was not confirmed; quarantine not started; nothing changed")
    rec = store.quarantine(slot, op_id=op, authorization=auth, recovery_token=token, record=record,
                           replace_record=_replace_hook(pool, slot, op))
    # The hook's answer is not proof: the record itself must now be the quarantined one.
    try:
        store._quarantine_matches(rec, _live(pool, slot))
    except rs.Refused as e:
        raise Unfinished(f"the quarantine completed but the live record does not show it ({e}); "
                         "owner must inspect") from None
    return rec


def _replace_hook(pool, slot, op):
    """Replaces the live record with the quarantined one only if it is still
    exactly `pre`, under the slot lock this process runs under."""
    path = _record_path(pool, slot)

    def replace(pre, post):
        pr._held(pool, slot)
        if pr.marker_state(pool) != pr.MIGRATED:
            raise rs.Refused("the pool is no longer migrated")
        if pr.read_record(path) != pre:
            raise rs.Refused(f"{slot} live record changed before the replace")
        old, new = rs.parse_record(pre, slot), rs.parse_record(post, slot)
        if (old.kind, old.state, new.kind, new.state) != ("v2", "leased", "v2", "quarantined") \
                or new.recovery_op != op or new.identity != old.identity or new.legacy \
                or new.generation != old.generation + 1 or "\ntoken=" in "\n" + post:
            raise rs.Refused(f"{slot} quarantined record does not follow from the adopted lease")
        pr.durable_replace(path, post)
        return pr.read_record(path)
    return replace


def _token(term):
    return term.ask("Recovery token (not echoed): ", secret=True)


def evidence_step(term, store, pool, slot, op, action, evidence_file):
    target = TARGET[action]
    rec, repeat, expected = _phase(store, slot, op, target)
    if rec is None:
        return None
    binding = store._binding(rec, action, repeat)
    if expected is not None:
        term.write("An interrupted step can finish only with its exact original record. "
                   f"Journal digest begins {expected[:12]}.\n")
    if action == "quiescence":
        refs = [r.strip() for r in term.ask("Evidence references (comma separated): ").split(",") if r.strip()]
        accounted = {}
        for c in rs.QUIESCENCE_ACCOUNTED:
            accounted[c] = term.ask(f"Every {c.replace('_', ' ')} writer is accounted for (type yes): ") == "yes"
        if not refs or not all(accounted.values()):
            raise rs.Refused("quiescence needs evidence references and every writer class accounted for; "
                             "nothing changed")
        payload = {**binding, "method": rec["quiescence_method"], "actor": _actor(), "evidence_refs": refs,
                   "accounted": accounted}
    elif action == "preservation":
        if not evidence_file or not os.path.isabs(evidence_file):
            raise rs.Refused("preservation needs --evidence with an absolute path")
        try:
            data = json.loads(_private_file(evidence_file, MAX_EVIDENCE, "the preservation evidence file"))
        except ValueError:
            raise rs.Refused("the preservation evidence file is not JSON") from None
        if not isinstance(data, dict) or set(data) != {"head", "inventory", "preserved"}:
            raise rs.Refused("the preservation evidence file needs exactly head, inventory and preserved")
        payload = {**binding, **data, "complete": True}
    else:
        head = term.ask("HEAD observed in the slot (40 hex): ")
        clean = term.ask("The slot is clean, with no dirty, untracked or ignored file (type yes): ") == "yes"
        # A migrated slot always has a record; unleased means it is this operation's quarantined record.
        store._quarantine_matches(rec, _live(pool, slot))
        payload = {**binding, "clean": clean, "lease_absent": True, "head": head,
                   "preservation": rec["gates"].get("preservation")}
    _same(payload, expected)
    _show(term, pool, slot, op, action, rec["identity_kind"], rec["identity"], binding["slot_generation"],
          binding["seq"], payload)
    _confirm(term, slot, op, action, rec["identity"], binding["slot_generation"], binding["seq"], payload)
    token = _token(term)
    record = _live(pool, slot)
    if action == "quiescence":
        return store.verify_quiescence(slot, op_id=op, recovery_token=token, evidence=payload, record=record)
    if action == "preservation":
        return store.record_preservation(slot, op_id=op, recovery_token=token, evidence=payload, record=record)
    return store.mark_ready(slot, op_id=op, recovery_token=token, final_check=payload, record=record)


# ---------------------------------------------------------------- read-only

def _relation(store, rec, record):
    if record is None:
        return "absent"
    try:
        if rec["state"] in rs.AFTER_QUARANTINE:
            store._quarantine_matches(rec, record)
            return "replaced"
        if rec["state"] == rs.CANCELLED:
            store._released_or_unchanged(rec, record)
            return "unchanged" if rs.parse_record(record, rec["slot"]).state == "leased" else "released"
        store._lease_matches(rec, record)
        return "unchanged"
    except rs.Refused:
        return "incompatible"


def _observed(slot, record):
    if record is None:
        return "absent"
    try:
        r = rs.parse_record(record, slot)
    except rs.Refused as e:
        return f"invalid ({_clean(e)})"
    return " ".join(x for x in (r.kind, r.state, f"generation={r.generation}",
                                f"identity={r.identity}" if r.identity else "",
                                f"recovery_op={r.recovery_op}" if r.recovery_op else "") if x)


def describe(pool, slot, op):
    store = rs.RecoveryStore(Path(pool) / "recovery")
    try:
        record = _live(pool, slot)
    except rs.Refused as e:
        record, err = None, _clean(e)
    else:
        err = ""
    out = [f"slot={slot}", f"operation={op}"]
    try:
        rec, pending = store._read(slot, op)
    except rs.Refused as e:
        return out + [f"error={_clean(e)}", "next=none; owner must inspect"]
    try:
        marker = store.terminal(slot, op)
        marker = "consistent" if marker else ("missing" if rec["state"] in rs.TERMINAL_STATES else "none")
    except rs.Refused:
        marker = "inconsistent"
    gen = rec["slot_generation"]
    expected = (f"v2 quarantined generation={gen} identity={rec['identity']} recovery_op={op}"
                if rec["state"] in rs.AFTER_QUARANTINE else
                f"{rec['identity_kind']} leased generation={gen} identity={rec['identity']}")
    relation = _relation(store, rec, record)
    out += [f"state={rec['state']}", f"seq={rec['seq']}", f"slot_generation={gen}",
            f"identity_kind={rec['identity_kind']}", f"identity={rec['identity']}",
            f"pending={'none' if pending is None else rs.TRANSITION[pending['to']] + ' seq=' + str(pending['seq'])}",
            f"terminal_marker={marker}", f"expected_record={expected}",
            f"observed_record={_observed(slot, record) if not err else 'unreadable (' + err + ')'}"]
    if pending is not None and "effect" in pending:
        try:
            side = store.reconcile(slot, op, record)
        except rs.Refused:
            side = "corrupt"
        relation = {"pre": "unchanged", "post": "replaced"}.get(side, "incompatible")
        out.append(f"reconcile={side}")
    out.append(f"record={relation}")
    if marker == "inconsistent":
        nxt = "none; the terminal marker does not match; owner must inspect"
    elif relation in ("incompatible", "absent") and rec["state"] != rs.CANCELLED:
        nxt = "none; the live record does not fit the operation; owner must inspect"
    elif pending is not None:
        nxt = (f"exact retry of {rs.TRANSITION[pending['to']]} with the identical record "
               f"(digest {pending['evidence'][:12]})")
    elif marker == "missing":
        nxt = f"repeat {rs.TRANSITION[rec['state']]} with the identical record to write the terminal marker"
    else:
        nxt = NEXT[rec["state"]]
    return out + [f"next={nxt}"]


def inspect(pool, slot, op=None):
    verdict, entries = pr.gate_entries(pool, slot)
    gate = pr.recovery_gate(pool, slot)
    out = [f"pool={pool}", f"slot={slot}", f"gate={gate[0]}", f"gate_reason={_clean(gate[3])}"]
    if verdict is not None:
        return out
    for name, (cls, _, _, reason) in entries:
        if op is not None and name != op:
            continue
        out.append("")
        if not rs.NAME.match(name):
            out += [f"entry={_clean(name)}", f"class={cls}", f"reason={_clean(reason)}"]
            continue
        out += describe(pool, slot, name) + [f"class={cls}"]
    if op is not None and op not in [n for n, _ in entries]:
        out += ["", f"operation={op}", "error=no such operation"]
    return out


def reconcile(pool, slot, op):
    store = rs.RecoveryStore(Path(pool) / "recovery")
    try:
        side = store.reconcile(slot, op, _live(pool, slot))
    except rs.Corrupt as e:
        side, why = "corrupt", _clean(e)
    else:
        why = {"none": "no unfinished quarantine",
               "pre": "the record replace did not happen; the exact retry may run it",
               "post": "the record replace happened; the exact retry may only finish"}[side]
    return [f"slot={slot}", f"operation={op}", f"reconcile={side}", f"meaning={why}"]


# ---------------------------------------------------------------- command line

USAGE = ("usage: recovery_cli.py check|inspect|reconcile|adopt|request|cancel|quarantine|quiescence|preservation|ready "
         "<pool> <slot> [--op <op_id>] [--evidence <file>] (see the module doc)")


def _args(argv):
    if len(argv) < 4 or argv[1] not in ACTIONS + ("check",):
        return None
    action, pool, slot, rest = argv[1], argv[2], argv[3], argv[4:]
    opts = {}
    while rest:
        if rest[0] not in ("--op", "--evidence") or len(rest) < 2 or rest[0] in opts:
            return None
        opts[rest[0]] = rest[1]
        rest = rest[2:]
    needs_op = action not in ("check", "adopt", "inspect")
    if (needs_op and "--op" not in opts) or (action in ("check", "adopt") and "--op" in opts) \
            or ("--evidence" in opts and action != "preservation") or (action == "preservation" and "--evidence" not in opts):
        return None
    if "--op" in opts and not rs.NAME.match(opts["--op"]):
        return None
    return action, pool, slot, opts.get("--op"), opts.get("--evidence")


def run(action, pool, slot, op=None, evidence=None, term=None, fault=None):
    """The output lines of one action. Tests pass a scripted `term` and a store
    `fault`; the command line never does."""
    synthetic_pool(pool, slot)
    if action == "check":
        return [f"synthetic-pool={pool}"]
    if action == "inspect":
        return inspect(pool, slot, op)
    if action == "reconcile":
        return reconcile(pool, slot, op)
    pr._held(pool, slot)
    term = term or Terminal()
    store = _store(pool, fault)
    before = _fingerprint(pool, slot)
    try:
        if action == "adopt":
            rec = adopt(term, pool, slot, store)
            op = rec["op_id"]
        else:
            _sound_history(pool, slot, op)
            if action in ("request", "cancel", "quarantine"):
                rec = authorize(term, store, pool, slot, op, action)
            else:
                rec = evidence_step(term, store, pool, slot, op, action, evidence)
    except rs.Refused as e:
        # Exit 1 promises nothing was written; a failure after any write is unfinished.
        if isinstance(e, Unfinished) or _fingerprint(pool, slot) == before:
            raise
        raise Unfinished(f"{e}; recovery state or the slot record changed before the failure: run recover inspect, "
                         "then finish or retry the step exactly") from e
    if rec is None:
        st = store._read(slot, op)[0]
        return [f"recover {action}: slot={slot} operation={op} already {st['state']}; nothing changed"]
    return [f"recover {action}: slot={slot} operation={op} state={rec['state']} seq={rec['seq']} "
            f"slot_generation={rec['slot_generation']} trust={rs.TRUST}"]


def main(argv):
    parsed = _args(argv)
    if parsed is None:
        print(USAGE, file=sys.stderr)
        return 2
    action = parsed[0]
    try:
        out = run(*parsed)
    except Unfinished as e:
        print(f"recover {action}: {_clean(e)}", file=sys.stderr)
        return 4
    except pr.Unconfirmed as e:
        print(f"recover {action}: {_clean(e)}", file=sys.stderr)
        return 4
    except rs.Refused as e:
        print(f"recover {action}: {_clean(e)}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print(f"recover {action}: interrupted; run recover inspect", file=sys.stderr)
        return 4
    except OSError as e:
        print(f"recover {action}: storage failure ({_clean(e.strerror or type(e).__name__)}); run recover inspect",
              file=sys.stderr)
        return 4
    except Exception as e:  # noqa: BLE001 - fail closed without a traceback
        print(f"recover {action}: internal error ({type(e).__name__}); outcome unconfirmed; run recover inspect",
              file=sys.stderr)
        return 4
    try:
        print("\n".join(out), flush=True)
    except OSError:
        return 4
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

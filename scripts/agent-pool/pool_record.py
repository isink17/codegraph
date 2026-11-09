#!/usr/bin/env python3
"""Strict reader and durable writer for agent pool slot records (CG-150
phases 2b-2a and 2b-2b). Interface and policy: docs/agent-pool.md, "Version 2
slot records".

    python3 -I -B pool_record.py inspect <slot> <path>
    python3 -I -B pool_record.py owns <slot> <path>                     (token on stdin)
    python3 -I -B pool_record.py acquire <slot> <path> <generation> <field>=<value>...
    python3 -I -B pool_record.py release <slot> <path> <generation> <lease_id>  (token on stdin)
    python3 -I -B pool_record.py gate <leases> <slot>
    python3 -I -B pool_record.py migrate [--dry-run] <leases> <slot>...
    python3 -I -B pool_record.py unmigrate <leases> <slot>...

`inspect` prints key=value lines whose first line is `record=` and exits 0
once the record is classified (an invalid record is a classification). `gate`
prints the six lines slot=, recovery=, gate=, recovery_operation=,
recovery_state= and reason= and exits 0 once the recovery state of a slot is
classified; it never changes anything and has no way to create or advance a
recovery operation. Every
other command changes records only under the slot locks pool.sh holds, and
only in a pool whose schema marker allows it. Exit 0 is success, 1 a refusal
or failure (reason on stderr), 2 a usage error, 3 an interpreter older than
MIN_PYTHON, 4 a write that was published but whose directory flush failed.
Tokens never appear in arguments, messages or file names.
"""

import hmac
import os
import re
import secrets
import stat
import sys

MIN_PYTHON = (3, 9)
if sys.version_info < MIN_PYTHON:
    sys.exit(3)

from pathlib import Path  # noqa: E402

sys.path.insert(0, str(Path(__file__).resolve().parent))
import recovery_state as rs  # noqa: E402

MAX_BYTES = 64 * 1024
MARKER = ".schema"
MIGRATED = "2"
UNMIGRATING = "unmigrating"


class Report(rs.Refused):
    """A refusal that changed nothing, with one report line per slot."""

    def __init__(self, lines):
        super().__init__("\n".join(lines))
        self.lines = lines


class Unconfirmed(rs.Refused):
    """The new record or marker was published, but flushing its directory
    failed: it may not survive a power loss."""


def read_record(path):
    """The text of the record at `path`, or None when nothing is there. A
    symlink, a non-regular file, an oversized or non-UTF-8 record is refused."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except FileNotFoundError:
        return None
    except OSError:
        raise rs.Refused("slot record is not a readable regular file") from None
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise rs.Refused("slot record is not a regular file")
        data = b""
        while len(data) <= MAX_BYTES:
            chunk = os.read(fd, MAX_BYTES + 1 - len(data))
            if not chunk:
                break
            data += chunk
        if len(data) > MAX_BYTES:
            raise rs.Refused("slot record is larger than any valid record")
        return data.decode("utf-8")
    except UnicodeDecodeError:
        raise rs.Refused("slot record is not UTF-8 text") from None
    finally:
        os.close(fd)


def inspect(slot, path):
    try:
        text = read_record(path)
        if text is None:
            return ["record=absent"]
        r = rs.parse_record(text, slot, legacy=True)
    except rs.Refused as e:
        return ["record=invalid", "reason=" + re.sub(r"[\x00-\x1f\x7f]", " ", str(e))]
    out = [f"record={r.kind}", f"state={r.state}", f"generation={r.generation}"]
    if r.identity is not None:
        out.append(f"{'legacy_digest' if r.legacy else 'lease_id'}={r.identity}")
    if r.recovery_op is not None:
        out.append(f"recovery_op={r.recovery_op}")
    out += [f"public.{k}={r.holder[k]}" for k in rs.HOLDER if k in r.holder]
    return out


# ---------------------------------------------------------------- durable writes

def free_record(slot, generation):
    return f"version=2\nslot={slot}\ngeneration={generation}\nstate=free\n"


def _durable_write(path, data, *, create=False):
    """Publishes `data` at `path`: private temporary file, flushed, then
    renamed over `path` (or, with `create`, linked to it, which fails if
    `path` exists), then the directory flushed. A failure before publication
    leaves `path` unchanged and raises it; one after raises Unconfirmed."""
    if not rs.SUPPORTED:
        raise rs.Refused("durable record writes are not supported on this platform")
    path = Path(path)
    tmp = path.with_name(f".{path.name}.{secrets.token_hex(8)}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        try:
            rs.RecoveryStore._write_all(fd, data)
            rs.RecoveryStore._sync(fd)
        finally:
            os.close(fd)
        if create:
            os.link(tmp, path)
        else:
            os.replace(tmp, path)
    except FileExistsError:
        tmp.unlink(missing_ok=True)
        raise rs.Refused(f"{path.name} appeared while it was being created; nothing written") from None
    except BaseException:
        tmp.unlink(missing_ok=True)
        raise
    try:
        if create:
            tmp.unlink()
        _sync_dir(path.parent)
    except OSError as e:
        raise Unconfirmed(f"{path.name} was written but flushing its directory failed ({e.strerror}); "
                          "owner must inspect") from None


def _sync_dir(path):
    fd = os.open(path, os.O_RDONLY)
    try:
        rs.RecoveryStore._sync(fd)
    finally:
        os.close(fd)


def _durable_unlink(path):
    os.unlink(path)
    try:
        _sync_dir(Path(path).parent)
    except OSError as e:
        raise Unconfirmed(f"{Path(path).name} was removed but flushing its directory failed ({e.strerror}); "
                          "owner must inspect") from None


def durable_replace(path, text, *, create=False):
    """Replaces (or with `create`, creates) the slot record at `path` with the
    version 2 record `text`. The caller must hold the slot lock."""
    path = Path(path)
    if not path.name.endswith(".lease"):
        raise rs.Refused("slot record path must end in .lease")
    if not isinstance(text, str) or not text.endswith("\n"):
        raise rs.Refused("slot record must end with a newline")
    if len(text.encode()) > MAX_BYTES:
        raise rs.Refused("slot record would be larger than any valid record; nothing written")
    if rs.parse_record(text, path.name[:-len(".lease")]).kind != "v2":
        raise rs.Refused("only version 2 records are written")
    try:
        if not stat.S_ISREG(os.lstat(path).st_mode):
            raise rs.Refused("slot record path is not a regular file")
    except FileNotFoundError:
        pass
    _durable_write(path, text.encode(), create=create)


# ---------------------------------------------------------------- pool state

def marker_state(leases):
    """absent, MIGRATED, UNMIGRATING or unknown, read as pool.sh reads it."""
    p = Path(leases) / MARKER
    try:
        st = os.lstat(p)
    except FileNotFoundError:
        return "absent"
    if not stat.S_ISREG(st.st_mode):
        return "unknown"
    try:
        text = read_record(p)
    except rs.Refused:
        return "unknown"
    text = (text or "").rstrip("\n")
    return text if text in (MIGRATED, UNMIGRATING) else "unknown"


def recovery_seen(leases, slot):
    """True when the slot may have recovery state, as pool.sh decides it."""
    r = Path(leases) / "recovery"
    if not os.path.lexists(r):
        return False
    if not (os.path.isdir(r) and os.access(r, os.X_OK)):
        return True
    return os.path.lexists(r / slot)


# Recovery gate classes, mildest first; a slot takes the worst class of its
# operations. Only the first three let an ordinary pool command proceed.
GATE_CLASSES = ("none", "cancelled", "active", "blocked", "incomplete", "corrupt", "unknown")
GATE_OPEN = GATE_CLASSES[:3]
OP_FILES = {"journal.jsonl", "state.json", "TERMINAL"}
# What an interrupted durable replace inside an operation directory leaves.
OP_LEFTOVER = re.compile(r"^\.(state\.json|TERMINAL)\.[0-9a-f]{16}\.tmp\Z")


def _listing(store, path):
    """The entry names of a private recovery directory. One that cannot be
    both listed and searched is unreadable, never empty."""
    store._check_dir(path)
    if not os.access(path, os.R_OK | os.X_OK):
        raise OSError(f"{path.name} cannot be listed and searched")
    return set(os.listdir(path))


def _gate_operation(store, slot, op, record):
    """(class, op_id, state, reason) of one entry of recovery/<slot>/."""
    if op.startswith(".adopt-"):
        return "incomplete", "", "", "an interrupted adoption left an unpublished operation; owner must inspect"
    if not rs.NAME.match(op):
        return "corrupt", "", "", "the slot recovery directory holds an unexpected entry"
    entries = _listing(store, store.root / slot / op)
    if any(not OP_LEFTOVER.match(n) for n in entries - OP_FILES):
        return "corrupt", op, "", "an operation directory holds an unexpected entry"
    if entries - OP_FILES:
        return "incomplete", op, "", "an interrupted write left a temporary file; owner must inspect"
    if "state.json" not in entries:
        return "corrupt", op, "", "an operation has no state record"
    rec, pending = store._read(slot, op)
    done = store.terminal(slot, op)
    state = rec["state"]
    if pending is not None:
        return "incomplete", op, state, "an unfinished transition remains; only its exact retry may finish it"
    if rec["identity_kind"] != "v2":
        return "corrupt", op, state, "the operation adopted a lease that is not version 2"
    if state in rs.TERMINAL_STATES and done != state:
        return "incomplete", op, state, "the terminal marker was not written"
    try:
        if state == rs.ACTIVE:
            store._lease_matches(rec, record)
            return "active", op, state, "the adopted lease is unchanged; only its holder may act"
        if state == rs.CANCELLED:
            store._released_or_unchanged(rec, record)
            return "cancelled", op, state, "cancelled; the slot record decides"
    except rs.Corrupt:
        raise
    except rs.Refused:
        return "blocked", op, state, "the slot record does not fit the operation; owner must inspect"
    if state == rs.READY:
        return "blocked", op, state, "ready for reassignment, which needs a separate authorization"
    return "blocked", op, state, "recovery in progress"


def gate_entries(leases, slot):
    """(verdict, entries) for the recovery state of `slot`: verdict is the
    (class, op_id, state, reason) of the whole slot when the recovery
    directory itself decides it, else None; entries pairs each entry of
    recovery/<slot>/ with its (class, op_id, state, reason). Read-only."""
    leases = Path(leases)
    root = leases / "recovery"
    try:
        try:
            os.lstat(root)
        except FileNotFoundError:
            return ("none", "", "", ""), []
        if not rs.SUPPORTED:
            return ("unknown", "", "", "recovery state cannot be evaluated on this platform"), []
        store = rs.RecoveryStore(root)
        names = _listing(store, root)
        for name in names:
            s = name[1:-len(".lock")] if name.startswith(".") and name.endswith(".lock") else name
            if not rs.NAME.match(s) or not os.path.lexists(leases / f"{s}.lease"):
                return ("corrupt", "", "", "the recovery directory holds an unexpected entry"), []
        # Presence comes from the listing: a lookup in a directory that cannot
        # be searched reports absence.
        if f".{slot}.lock" in names:
            return ("incomplete", "", "", "a recovery transition holds or left its own lock; owner must inspect"), []
        if slot not in names:
            return ("none", "", "", ""), []
        ops = _listing(store, root / slot)
        record = read_record(leases / f"{slot}.lease")
        return None, [(op, _gate_operation(store, slot, op, record)) for op in sorted(ops)]
    except rs.Corrupt as e:
        return ("corrupt", "", "", re.sub(r"[^\x20-\x7e]", " ", str(e))), []
    except Exception:  # noqa: BLE001 - an unreadable state is never no recovery
        return ("unknown", "", "", "recovery state could not be read; owner must inspect"), []


def recovery_gate(leases, slot):
    """(class, op_id, state, reason) of the recovery state of `slot` in a
    migrated pool, from GATE_CLASSES. It only reads: nothing under recovery/
    is created, repaired or removed. Every operation must be valid and its
    journal, state and marker must agree; anything unreadable or unexpected
    is never read as no recovery. A same-uid process can still forge a
    consistent operation or delete one: this proves consistency, not owner
    approval."""
    verdict, entries = gate_entries(leases, slot)
    if verdict is not None:
        return verdict
    seen = [v for _, v in entries]
    if not seen:
        return "none", "", "", ""
    if sum(1 for v in seen if v[2] and v[2] not in rs.TERMINAL_STATES) > 1:
        return "corrupt", "", "", "more than one operation is unfinished"
    # Ties go to the operation furthest along, never to a cancelled one.
    rank = {"": 0, rs.CANCELLED: 1, **{st: 2 + i for i, st in enumerate(rs.ORDER)}}
    return max(seen, key=lambda v: (GATE_CLASSES.index(v[0]), rank[v[2]]))


def _slot_path(slot, path):
    if not isinstance(slot, str) or not rs.NAME.match(slot):
        raise rs.Refused("invalid slot name")
    path = Path(path)
    if path.name != f"{slot}.lease":
        raise rs.Refused("slot record path does not name the slot")
    return path


def _held(leases, slot):
    lock = Path(leases) / f".{slot}.lock"
    try:
        held = stat.S_ISDIR(os.lstat(lock).st_mode)
    except OSError:
        held = False
    if not held:
        raise rs.Refused(f"{slot}: the caller does not hold the slot lock")


def _v2_pool(slot, path):
    """Checks, under the slot lock, that slot `slot` may be changed as a
    version 2 slot and returns its current record (text, Record)."""
    path = _slot_path(slot, path)
    leases = path.parent
    _held(leases, slot)
    if marker_state(leases) != MIGRATED:
        raise rs.Refused("the pool is not migrated to version 2 records; nothing written")
    gate = recovery_gate(leases, slot)
    if gate[0] not in GATE_OPEN:
        raise rs.Refused(f"{slot} recovery state is {gate[0]}; nothing written")
    text = read_record(path)
    if text is None:
        raise rs.Refused(f"{slot} slot record is missing in a migrated pool; owner must intervene")
    r = rs.parse_record(text, slot, legacy=True)
    if r.kind != "v2":
        raise rs.Refused(f"{slot} holds a version 1 or legacy lease in a migrated pool; owner must intervene")
    return text, r


def _token(token):
    if not isinstance(token, str) or not rs.HEX32.match(token):
        raise rs.Refused("token does not match the slot lease")
    return token


def owns(slot, path, token):
    """The live leased record of `slot` when `token` is its lease token."""
    _, r = _v2_pool(slot, path)
    if r.state != "leased":
        raise rs.Refused(f"{slot} has no active lease")
    if not hmac.compare_digest(r.token, _token(token)):
        raise rs.Refused(f"token does not match the {slot} lease")
    return r


def acquire(slot, path, generation, holder):
    """Leases the free record of `slot` at `generation`; returns the new
    lease token once the leased record is durable."""
    _, r = _v2_pool(slot, path)
    if r.state != "free":
        raise rs.Refused(f"{slot} is not free")
    if r.generation != generation:
        raise rs.Refused(f"{slot} slot record generation changed; nothing written")
    if set(holder) - set(rs.HOLDER) or not holder.get("task"):
        raise rs.Refused("acquire needs a task and only holder fields")
    token = secrets.token_hex(16)
    lines = ["version=2", f"slot={slot}", f"generation={generation + 1}", "state=leased",
             f"lease_id={secrets.token_hex(16)}"]
    lines += [f"{k}={holder.get(k, '')}" for k in rs.HOLDER if k != "utc"] + [f"utc={rs._now()}"]
    durable_replace(path, "\n".join(lines + [f"lease_token={token}"]) + "\n")
    return token


def release(slot, path, generation, lease_id, token):
    """Frees the lease of `slot` named by `generation`, `lease_id` and its
    token, keeping the generation."""
    r = owns(slot, path, token)
    if r.generation != generation or r.identity != lease_id:
        raise rs.Refused(f"the {slot} lease changed; nothing written")
    durable_replace(path, free_record(slot, r.generation))


def _classify(leases, slot):
    """(kind, text) of the record of `slot`: absent, initial (the canonical
    free record of generation 0), lease (version 1 or legacy), or a refusal
    reason for anything else."""
    path = Path(leases) / f"{slot}.lease"
    try:
        text = read_record(path)
        if text is None:
            return "absent", None
        if text == free_record(slot, 0):
            return "initial", text
        r = rs.parse_record(text, slot, legacy=True)
    except rs.Refused:
        return "an unreadable or invalid slot record", None
    if r.kind in ("v1", "legacy"):
        return "lease", text
    if r.state == "free":
        return "a free record whose generation or content changed since migration", None
    return f"a {r.state} version 2 record", None


def migrate(leases, slots, dry_run=False):
    """Writes the free record of generation 0 for every slot, then the
    marker. Refuses as a whole, writing nothing, unless every slot has no
    record or already that record. Returns the report lines."""
    leases = Path(leases)
    marker = marker_state(leases)
    if marker != "absent":
        raise rs.Refused("the pool already carries a schema marker; nothing written")
    lines, write, refused = [], [], False
    for slot in slots:
        _slot_path(slot, leases / f"{slot}.lease")
        if not dry_run:
            _held(leases, slot)
        kind, _ = _classify(leases, slot)
        if recovery_seen(leases, slot):
            lines.append(f"{slot}: refused: recovery state present")
            refused = True
        elif kind == "absent":
            lines.append(f"{slot}: {'would write' if dry_run else 'writing'} a free record of generation 0")
            write.append(slot)
        elif kind == "initial":
            lines.append(f"{slot}: already holds the free record of generation 0")
        elif kind == "lease":
            lines.append(f"{slot}: refused: holds a version 1 or legacy lease, which migration never rewrites")
            refused = True
        else:
            lines.append(f"{slot}: refused: holds {kind}")
            refused = True
    if refused:
        raise Report(lines if dry_run else lines + ["migration refused; nothing written"])
    if dry_run:
        return lines
    for slot in write:
        durable_replace(leases / f"{slot}.lease", free_record(slot, 0), create=True)
    for slot in slots:
        if read_record(leases / f"{slot}.lease") != free_record(slot, 0):
            raise rs.Refused(f"{slot} record did not verify after writing; marker not written; owner must inspect")
    _durable_write(leases / MARKER, f"{MIGRATED}\n".encode(), create=True)
    if marker_state(leases) != MIGRATED:
        raise rs.Refused("the schema marker did not verify after writing; owner must inspect")
    return lines + ["migrated: marker written"]


def unmigrate(leases, slots):
    """Removes the free records of generation 0 and then the marker, after
    checking every slot. Returns the report lines."""
    leases = Path(leases)
    marker = marker_state(leases)
    if marker == "unknown":
        raise rs.Refused("the schema marker is not one this tool wrote; nothing changed; owner must inspect")
    lines, remove, refused = [], [], False
    for slot in slots:
        _slot_path(slot, leases / f"{slot}.lease")
        _held(leases, slot)
        kind, _ = _classify(leases, slot)
        if recovery_seen(leases, slot):
            lines.append(f"{slot}: refused: recovery state present")
            refused = True
        elif kind == "initial":
            lines.append(f"{slot}: removing the free record of generation 0")
            remove.append(slot)
        elif kind == "absent" and marker != MIGRATED:
            lines.append(f"{slot}: no record")
        elif kind == "absent":
            lines.append(f"{slot}: refused: slot record missing in a migrated pool; its generation is unknown")
            refused = True
        elif kind == "lease" and marker != MIGRATED:
            lines.append(f"{slot}: keeps its version 1 or legacy lease")
        elif kind == "lease":
            lines.append(f"{slot}: refused: holds a version 1 or legacy lease in a migrated pool")
            refused = True
        else:
            lines.append(f"{slot}: refused: holds {kind}")
            refused = True
    if refused:
        raise Report(lines + ["unmigrate refused; nothing changed"])
    if marker == MIGRATED:
        p = leases / MARKER
        _durable_write(p, f"{UNMIGRATING}\n".encode())
    for slot in remove:
        p = leases / f"{slot}.lease"
        if read_record(p) != free_record(slot, 0):
            raise rs.Refused(f"{slot} record changed during unmigrate; pool left unavailable; owner must inspect")
        _durable_unlink(p)
    if marker != "absent":
        _durable_unlink(leases / MARKER)
    return lines + ["unmigrated: no marker remains"]


# ---------------------------------------------------------------- command line

def _stdin_token():
    return sys.stdin.readline().rstrip("\n")


def _generation(text):
    if not rs.GENERATION.match(text):
        raise rs.Refused("invalid generation")
    return int(text)


def _holder(args):
    holder = {}
    for a in args:
        k, sep, v = a.partition("=")
        if not sep or k in holder or "\n" in v:
            raise rs.Refused("holder fields must be distinct single-line field=value arguments")
        holder[k] = v
    return holder


USAGE = "usage: pool_record.py inspect|gate|owns|acquire|release|migrate|unmigrate ... (see the module doc)"


def main(argv):
    cmd, args = (argv[1], argv[2:]) if len(argv) > 1 else ("", [])
    dry = cmd == "migrate" and args[:1] == ["--dry-run"]
    if dry:
        args = args[1:]
    arity = {"inspect": 2, "gate": 2, "owns": 2, "release": 4}
    if (cmd in arity and len(args) != arity[cmd]) or (cmd == "acquire" and len(args) < 4) or \
            (cmd in ("migrate", "unmigrate") and len(args) < 2) or \
            cmd not in ("inspect", "gate", "owns", "acquire", "release", "migrate", "unmigrate"):
        print(USAGE, file=sys.stderr)
        return 2
    try:
        if cmd == "inspect":
            out = inspect(*args)
        elif cmd == "gate":
            if not rs.NAME.match(args[1]):
                raise rs.Refused("invalid slot name")
            cls, op, state, reason = recovery_gate(args[0], args[1])
            out = [f"slot={args[1]}", f"recovery={cls}", f"gate={'open' if cls in GATE_OPEN else 'blocked'}",
                   f"recovery_operation={op}", f"recovery_state={state}", f"reason={reason}"]
        elif cmd == "owns":
            r = owns(args[0], args[1], _stdin_token())
            out = [f"generation={r.generation}", f"lease_id={r.identity}"]
        elif cmd == "acquire":
            out = [f"token={acquire(args[0], args[1], _generation(args[2]), _holder(args[3:]))}"]
        elif cmd == "release":
            release(args[0], args[1], _generation(args[2]), args[3], _stdin_token())
            out = []
        elif cmd == "migrate":
            out = migrate(args[0], args[1:], dry_run=dry)
        else:
            out = unmigrate(args[0], args[1:])
    except Report as e:
        print("\n".join(e.lines))
        return 1
    except Unconfirmed as e:
        print(f"pool_record {cmd}: {e}", file=sys.stderr)
        return 4
    except rs.Refused as e:
        print(f"pool_record {cmd}: {e}", file=sys.stderr)
        return 1
    except OSError as e:
        print(f"pool_record {cmd}: storage failure ({e.strerror or type(e).__name__}); stopped at the failing "
              "step, whose record is unchanged; check status", file=sys.stderr)
        return 1
    except Exception as e:  # noqa: BLE001 - fail closed without a traceback
        # The failing step is unknown, so a write may have been published.
        print(f"pool_record {cmd}: internal error ({type(e).__name__}); state unconfirmed", file=sys.stderr)
        return 4
    try:
        if out:
            print("\n".join(out), flush=True)
    except OSError:
        return 4
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

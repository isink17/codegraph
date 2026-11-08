#!/usr/bin/env python3
"""Strict reader for agent pool slot records (CG-150 phase 2b-2a).

pool.sh hands every record that may be version 2, or is ambiguous about its
version, to `inspect` instead of reading it in bash. The rules are those of
recovery_state.parse_record. Interface and policy: docs/agent-pool.md,
"Version 2 slot records".

    python3 -I -B pool_record.py inspect <slot> <path>

prints key=value lines whose first line is `record=`, and exits 0 once the
record is classified (an invalid record is a classification). Exit 2 is a
usage error, exit 3 an interpreter older than MIN_PYTHON; anything else is a
failure the caller must treat as unknown. Tokens are never printed.

`durable_replace` is the writer the enforcement phase will use. No command
here calls it, and pool.sh does not reach it.
"""

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


def durable_replace(path, text):
    """Replaces the slot record at `path` with the version 2 record `text`:
    private temporary file, flushed, renamed over the record, directory
    flushed. The caller must hold the slot lock. Not used in this phase."""
    if not rs.SUPPORTED:
        raise rs.Refused("durable record writes are not supported on this platform")
    path = Path(path)
    if not path.name.endswith(".lease"):
        raise rs.Refused("slot record path must end in .lease")
    if not isinstance(text, str) or not text.endswith("\n"):
        raise rs.Refused("slot record must end with a newline")
    if rs.parse_record(text, path.name[:-len(".lease")]).kind != "v2":
        raise rs.Refused("only version 2 records are written")
    try:
        if not stat.S_ISREG(os.lstat(path).st_mode):
            raise rs.Refused("slot record path is not a regular file")
    except FileNotFoundError:
        pass
    tmp = path.with_name(f".{path.name}.{secrets.token_hex(8)}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        try:
            data = text.encode()
            while data:
                data = data[os.write(fd, data):]
            rs.RecoveryStore._sync(fd)
        finally:
            os.close(fd)
        os.replace(tmp, path)
    except BaseException:
        tmp.unlink(missing_ok=True)
        raise
    fd = os.open(path.parent, os.O_RDONLY)
    try:
        rs.RecoveryStore._sync(fd)
    finally:
        os.close(fd)


def main(argv):
    if len(argv) != 4 or argv[1] != "inspect":
        print("usage: pool_record.py inspect <slot> <path>", file=sys.stderr)
        return 2
    print("\n".join(inspect(argv[2], argv[3])))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

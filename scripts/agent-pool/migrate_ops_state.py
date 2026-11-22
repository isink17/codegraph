#!/usr/bin/env python3
"""One-time owner migration of the agent-workflow state from
~/Projekti/codegraph-evidence/{pool,coordinator,plane-sync} to
~/Projekti/codegraph-ops/, leaving a symlink at each old path so copies of
pool.sh and the coordinator older than the move share the same store and
locks (docs/agent-pool.md, Migration).

Run it with every pool.sh, coordinator and campaign stopped. It checks all
three paths before changing anything, never overwrites, merges or creates
state, and is safe to rerun.

    python3 scripts/agent-pool/migrate_ops_state.py [--check]
"""

import fcntl
import os
import stat
import sys
from pathlib import Path

NAMES = ("pool", "coordinator", "plane-sync")


class Stop(Exception):
    pass


def own_dir(p):
    """A real directory (not a symlink) of this user."""
    try:
        st = os.lstat(p)
    except FileNotFoundError:
        return False
    return stat.S_ISDIR(st.st_mode) and st.st_uid == os.getuid()


def linked(src, dst):
    return (os.path.islink(src) and os.lstat(src).st_uid == os.getuid() and own_dir(dst)
            and os.path.realpath(src) == os.path.realpath(dst))


def busy(src):
    """A pool slot lock or a running coordinator in an old directory."""
    locks = sorted(n for n in os.listdir(src) if n.startswith(".") and n.endswith(".lock"))
    if locks:
        return f"{src} holds {locks[0]}; a pool.sh may be running"
    lock = os.path.join(src, "coordinator.lock")
    if os.path.isfile(lock) and not os.path.islink(lock):
        fd = os.open(lock, os.O_RDONLY)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return f"a coordinator holds {lock}"
        finally:
            os.close(fd)
    return None


def plan(src, dst):
    if os.path.islink(src):
        if linked(src, dst):
            return "done"
        raise Stop(f"{src} is a symlink, but not this user's link to the directory {dst}")
    if os.path.lexists(dst) and not own_dir(dst):
        raise Stop(f"{dst} is not a directory of this user")
    if not os.path.lexists(src):
        return "link"
    if not own_dir(src):
        raise Stop(f"{src} is not a directory of this user")
    why = busy(src)
    if why:
        raise Stop(why)
    if not os.listdir(src):
        return "link"  # left empty by an older pool.sh's mkdir -p
    if os.path.lexists(dst) and os.listdir(dst):
        raise Stop(f"{src} and {dst} both hold state; the owner reconciles them")
    return "move"


def apply(action, src, dst):
    if action == "move":
        os.rename(src, dst)  # replaces an empty dst atomically
    else:
        if os.path.lexists(src):
            os.rmdir(src)
        if not os.path.lexists(dst):
            os.mkdir(dst, 0o700)
    os.chmod(dst, 0o700)
    tmp = f"{src}.{os.getpid()}.migrating"
    os.symlink(dst, tmp)
    try:
        # Unlike ln -s, rename fails instead of linking inside a directory an
        # older pool.sh or coordinator recreated meanwhile.
        os.rename(tmp, src)
    except OSError:
        os.unlink(tmp)
        raise Stop(f"{src} reappeared during the migration; an older pool.sh or coordinator is running")


def main(argv):
    check = argv == ["--check"]
    if argv and not check:
        print(__doc__.strip().splitlines()[-1].strip(), file=sys.stderr)
        return 2
    old, ops = Path.home() / "Projekti/codegraph-evidence", Path.home() / "Projekti/codegraph-ops"
    try:
        for root in (old, ops):
            if os.path.lexists(root) and not own_dir(root):
                raise Stop(f"{root} is not a directory of this user")
        steps = [(plan(str(old / n), str(ops / n)), str(old / n), str(ops / n)) for n in NAMES]
        for action, src, dst in steps:
            print(f"{action}: {src} -> {dst}")
        if check or all(a == "done" for a, _, _ in steps):
            return 0
        for root in (old, ops):
            if not os.path.lexists(root):
                os.mkdir(root, 0o700)
        os.chmod(ops, 0o700)
        for action, src, dst in steps:
            if action != "done":
                apply(action, src, dst)
    except (Stop, OSError) as e:
        print(f"stopped: {e}; nothing further changed", file=sys.stderr)
        return 1
    print("migrated")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

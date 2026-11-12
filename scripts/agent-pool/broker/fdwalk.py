"""Read-only, descriptor-relative filesystem access for the Git observer.

Every path is opened one component at a time relative to an open directory
descriptor, with O_NOFOLLOW, so no symbolic link is ever followed. FIFOs,
sockets and devices are never opened. Every read is bounded. Any failure or
inconsistency raises Unknown, which the observer reports as UNKNOWN.

Limitation: equal (dev, ino, nlink, size, mtime, ctime) before and after a read
show that no change was seen. They do not prove that a file someone else can
write stayed immutable for the whole scan (ctime granularity, clock changes,
root)."""

import collections
import dataclasses
import errno
import os
import stat

_DIR_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NOCTTY
_FILE_FLAGS = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC | os.O_NOCTTY
SUPPORTED = (os.open in os.supports_dir_fd and os.stat in os.supports_dir_fd
             and os.readlink in os.supports_dir_fd and os.listdir in os.supports_fd)


class Unknown(Exception):
    """The observation cannot be completed; code is a short fixed string."""

    def __init__(self, code, path=b""):
        super().__init__(code)
        self.code, self.path = code, path


@dataclasses.dataclass(frozen=True)
class Limits:
    max_entries: int = 100_000
    max_depth: int = 64
    max_file: int = 64 << 20
    max_total: int = 1 << 30
    max_meta: int = 1 << 20
    max_index: int = 64 << 20
    max_object: int = 16 << 20
    max_trees: int = 50_000


Meta = collections.namedtuple("Meta", "kind perm dev ino nlink size mtime_ns ctime_ns")


def meta(st):
    m = st.st_mode
    kind = ("dir" if stat.S_ISDIR(m) else "file" if stat.S_ISREG(m) else "link" if stat.S_ISLNK(m)
            else "special")
    return Meta(kind, stat.S_IMODE(m), st.st_dev, st.st_ino, st.st_nlink, st.st_size, st.st_mtime_ns,
                st.st_ctime_ns)


def valid_name(name):
    return type(name) is bytes and name not in (b"", b".", b"..") and b"/" not in name and b"\0" not in name


def _os_error(e, path):
    if isinstance(e, PermissionError):
        return Unknown("permission-denied", path)
    if e.errno in (errno.ENOENT, errno.ELOOP, errno.ENOTDIR, errno.ENXIO):
        return Unknown("changed-during-observation", path)
    return Unknown("filesystem-error", path)


def lstat_at(dirfd, name, path):
    """Metadata of name inside dirfd without following it; None when absent."""
    if not valid_name(name):
        raise Unknown("invalid-name", path)
    try:
        return meta(os.stat(name, dir_fd=dirfd, follow_symlinks=False))
    except FileNotFoundError:
        return None
    except OSError as e:
        raise _os_error(e, path) from None


def open_dir_at(dirfd, name, expect, path):
    try:
        fd = os.open(name, _DIR_FLAGS, dir_fd=dirfd)
    except OSError as e:
        raise _os_error(e, path) from None
    try:
        got = meta(os.fstat(fd))
    except OSError as e:
        os.close(fd)
        raise _os_error(e, path) from None
    if got.kind != "dir" or (got.dev, got.ino) != (expect.dev, expect.ino):
        os.close(fd)
        raise Unknown("changed-during-observation", path)
    return fd


def open_root(path):
    """Opens an absolute, normalized directory path one component at a time
    from "/", refusing a symbolic link anywhere in it. Returns (fd, stat)."""
    if (type(path) is not str or not path.startswith("/") or path == "/" or os.path.normpath(path) != path
            or "\0" in path):
        raise Unknown("invalid-root")
    fd = os.open("/", _DIR_FLAGS)
    try:
        for comp in os.fsencode(path).split(b"/")[1:]:
            m = lstat_at(fd, comp, comp)
            if m is None:
                raise Unknown("root-missing", comp)
            if m.kind != "dir":
                raise Unknown("root-not-a-plain-directory", comp)
            nfd = open_dir_at(fd, comp, m, comp)
            os.close(fd)
            fd = nfd
        st = os.fstat(fd)
    except BaseException:
        os.close(fd)
        raise
    return fd, st


def read_at(dirfd, name, expect, limit, path, dev):
    """Reads a regular, single-link file on device dev whose metadata must stay
    expect from before the open until after the last byte."""
    if expect.kind != "file":
        raise Unknown("unexpected-file-type", path)
    if expect.nlink != 1:
        raise Unknown("hardlink", path)
    if expect.dev != dev:
        raise Unknown("cross-device", path)
    if expect.size > limit:
        raise Unknown("limit-exceeded", path)
    try:
        fd = os.open(name, _FILE_FLAGS, dir_fd=dirfd)
    except OSError as e:
        raise _os_error(e, path) from None
    try:
        if meta(os.fstat(fd)) != expect:
            raise Unknown("changed-during-observation", path)
        chunks, n = [], 0
        while n <= expect.size:
            b = os.read(fd, min(1 << 20, expect.size + 1 - n))
            if not b:
                break
            chunks.append(b)
            n += len(b)
        if n != expect.size or meta(os.fstat(fd)) != expect:
            raise Unknown("changed-during-observation", path)
        return b"".join(chunks)
    except OSError as e:
        raise _os_error(e, path) from None
    finally:
        os.close(fd)


def readlink_at(dirfd, name, expect, limit, path):
    if expect.kind != "link":
        raise Unknown("unexpected-file-type", path)
    if expect.size > limit:
        raise Unknown("limit-exceeded", path)
    try:
        target = os.readlink(name, dir_fd=dirfd)
    except OSError as e:
        raise _os_error(e, path) from None
    if len(target) != expect.size or lstat_at(dirfd, name, path) != expect:
        raise Unknown("changed-during-observation", path)
    return target


def list_dir(fd, path, limit):
    try:
        names = os.listdir(fd)
    except OSError as e:
        raise _os_error(e, path) from None
    if len(names) > limit:
        raise Unknown("limit-exceeded", path)
    out = sorted(os.fsencode(n) for n in names)
    for n in out:
        if not valid_name(n):
            raise Unknown("invalid-name", path)
    return out


def stat_path(root, rel, dev):
    """Metadata of a relative path under root, never following a link; None
    when any component is absent."""
    comps = rel.split(b"/")
    fd = root
    try:
        for i, comp in enumerate(comps):
            here = b"/".join(comps[:i + 1])
            m = lstat_at(fd, comp, here)
            if m is None:
                return None
            if m.dev != dev:
                raise Unknown("cross-device", here)
            if i == len(comps) - 1:
                return m, fd
            if m.kind != "dir":
                raise Unknown("unexpected-file-type", here)
            nfd = open_dir_at(fd, comp, m, here)
            if fd != root:
                os.close(fd)
            fd = nfd
    except BaseException:
        if fd != root:
            os.close(fd)
        raise


def read_path(root, rel, limit, dev):
    """Bytes of the regular file at rel under root, or None when absent."""
    found = stat_path(root, rel, dev)
    if found is None:
        return None
    m, fd = found
    try:
        return read_at(fd, rel.rsplit(b"/", 1)[-1], m, limit, rel, dev)
    finally:
        if fd != root:
            os.close(fd)


def exists(root, rel, dev):
    found = stat_path(root, rel, dev)
    if found is None:
        return None
    m, fd = found
    if fd != root:
        os.close(fd)
    return m


def dir_is_empty(root, rel, dev, limits):
    found = stat_path(root, rel, dev)
    if found is None:
        return True
    m, fd = found
    try:
        if m.kind != "dir":
            return False
        cfd = open_dir_at(fd, rel.rsplit(b"/", 1)[-1], m, rel)
        try:
            return not list_dir(cfd, rel, limits.max_entries)
        finally:
            os.close(cfd)
    finally:
        if fd != root:
            os.close(fd)


def walk(root, dev, limits, skip_top=(), visit=None):
    """Metadata of every entry below root, keyed by slash-joined relative bytes
    path. Directories are entered only through open_dir_at; special files and
    other devices raise Unknown. visit(dirfd, name, rel, meta) runs on each
    non-directory entry while its directory is open."""
    entries = {}

    def rec(dfd, prefix, depth):
        if depth > limits.max_depth:
            raise Unknown("limit-exceeded", prefix)
        for name in list_dir(dfd, prefix, limits.max_entries):
            if not prefix and name in skip_top:
                continue
            rel = prefix + name
            m = lstat_at(dfd, name, rel)
            if m is None:
                raise Unknown("changed-during-observation", rel)
            if m.dev != dev:
                raise Unknown("cross-device", rel)
            if m.kind == "special":
                raise Unknown("special-file", rel)
            entries[rel] = m
            if len(entries) > limits.max_entries:
                raise Unknown("limit-exceeded", rel)
            if m.kind == "dir":
                cfd = open_dir_at(dfd, name, m, rel)
                try:
                    rec(cfd, rel + b"/", depth + 1)
                finally:
                    os.close(cfd)
            elif visit is not None:
                visit(dfd, name, rel, m)

    rec(root, b"", 0)
    return entries

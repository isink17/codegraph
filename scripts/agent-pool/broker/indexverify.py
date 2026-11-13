"""Index and worktree verification, and the observation result.

observe(ctx) compares a worktree against its index and the index against the
HEAD tree. It reads only the three roots the caller authorized and never runs
Git. CLEAN means only that every supported read-only check passed for the
snapshot observed. It is not proof of quiescence, of immutability, or of the
absence of concurrent writers, and it carries no provenance: nothing privileged
may treat it as authorization. Untracked and ignored entries, including empty
directories, are NOT_CLEAN. Unsupported or inconsistent state is UNKNOWN."""

import bisect
import collections
import contextlib
import dataclasses
import enum
import hashlib
import os

from . import fdwalk, gitraw, sanitize
from .fdwalk import Unknown

MAX_REASONS = 16
_PATH_SHOWN = 160


class Status(str, enum.Enum):
    CLEAN = "CLEAN"
    NOT_CLEAN = "NOT_CLEAN"
    UNKNOWN = "UNKNOWN"
    IDENTITY_CHANGED = "IDENTITY_CHANGED"


_RANK = {Status.CLEAN: 0, Status.NOT_CLEAN: 1, Status.UNKNOWN: 2, Status.IDENTITY_CHANGED: 3}


class IdentityChanged(Exception):
    def __init__(self, code, path=b""):
        super().__init__(code)
        self.code, self.path = code, path


# One root's identity: device, inode and birth time in ns (None where the
# platform does not report it).
RootId = collections.namedtuple("RootId", "dev ino birth_ns")
Binding = collections.namedtuple("Binding", "worktree admin common links_sha256")


@dataclasses.dataclass(frozen=True)
class Context:
    """Roots the caller authorizes. For an ordinary repository admin is
    worktree + "/.git" and common equals admin. For a linked worktree the
    worktree's .git file must name admin exactly, and admin's commondir must
    resolve to common; neither may widen what is read."""
    worktree: str
    admin: str
    common: str
    expected: Binding = None
    require_birthtime: bool = True
    limits: fdwalk.Limits = fdwalk.Limits()


class Observation:
    __slots__ = ("status", "reasons", "digests", "binding")

    def __init__(self, status, reasons=(), digests=None, binding=None):
        object.__setattr__(self, "status", Status(status))
        object.__setattr__(self, "reasons", tuple(reasons)[:MAX_REASONS])
        object.__setattr__(self, "digests", tuple(sorted((digests or {}).items())))
        object.__setattr__(self, "binding", binding)

    def __setattr__(self, *_):
        raise AttributeError("observations are immutable")

    def __bool__(self):
        raise TypeError("an observation has no truth value; compare status with Status.CLEAN")

    @property
    def clean(self):
        return self.status is Status.CLEAN

    def __repr__(self):
        return f"Observation({self.status.value}, {self.reasons!r})"


def _reason(code, path=b""):
    return code, sanitize.esc(path)[:_PATH_SHOWN]


def combine(observations):
    """One result for several observations of the same attempt: the worst
    status wins, and CLEAN results that disagree in any digest are UNKNOWN. A
    caller must combine every observation it made; a retry never upgrades an
    earlier result."""
    obs = list(observations)
    if not obs or not all(isinstance(o, Observation) for o in obs):
        raise TypeError("combine needs observations")
    worst = max(obs, key=lambda o: _RANK[o.status])
    if worst.status is not Status.CLEAN:
        reasons = [r for o in obs if o.status is worst.status for r in o.reasons]
        return Observation(worst.status, reasons, dict(worst.digests), worst.binding)
    if any(o.digests != obs[0].digests or o.binding != obs[0].binding for o in obs):
        return Observation(Status.UNKNOWN, [_reason("observations-disagree")])
    return obs[0]


def has_children(names, path):
    """Whether any of the sorted paths lies below path; O(log n) per call."""
    under = path + b"/"
    i = bisect.bisect_left(names, under)
    return i < len(names) and names[i].startswith(under)


def blob_oid(data):
    return hashlib.sha1(b"blob %d\0" % len(data) + data).hexdigest().encode()


def parse_index(data, limits):
    """{path: (mode, oid)} from an index of version 2 or 3. Every flag,
    extension and stage outside the supported subset raises Unknown."""
    if data is None:
        return {}
    if len(data) < 32 or hashlib.sha1(data[:-20]).digest() != data[-20:]:
        raise Unknown("malformed-index")
    if data[:4] != b"DIRC":
        raise Unknown("malformed-index")
    version, count = int.from_bytes(data[4:8], "big"), int.from_bytes(data[8:12], "big")
    if version not in (2, 3):
        raise Unknown("unsupported-index-feature", b"version %d" % version)
    if count > limits.max_entries:
        raise Unknown("limit-exceeded", b"index")
    end, pos, out, prev = len(data) - 20, 12, {}, None
    for _ in range(count):
        if pos + 62 > end:
            raise Unknown("malformed-index")
        mode = int.from_bytes(data[pos + 24:pos + 28], "big")
        oid = data[pos + 40:pos + 60].hex().encode()
        flags = int.from_bytes(data[pos + 60:pos + 62], "big")
        name_at = pos + 62
        if flags & 0x4000:
            if version < 3 or name_at + 2 > end:
                raise Unknown("malformed-index")
            if int.from_bytes(data[name_at:name_at + 2], "big"):
                raise Unknown("unsupported-index-feature", b"skip-worktree or intent-to-add")
            name_at += 2
        if flags & 0x8000:
            raise Unknown("unsupported-index-feature", b"assume-unchanged")
        if flags & 0x3000:
            raise Unknown("unresolved-conflict")
        nul = data.find(b"\0", name_at, end)
        if nul < 0:
            raise Unknown("malformed-index")
        name, length = data[name_at:nul], flags & 0xFFF
        if not name:
            raise Unknown("unsupported-index-feature", b"split index")
        if len(name) > limits.max_path or name.count(b"/") > limits.max_depth:
            raise Unknown("limit-exceeded", b"index path")
        entry_end = pos + ((name_at - pos + len(name) + 8) & ~7)
        if (length != min(len(name), 0xFFF) or entry_end > end or data[nul:entry_end].strip(b"\0")
                or not gitraw.valid_path(name)):
            raise Unknown("malformed-index", name)
        if mode == gitraw.MODE_GITLINK:
            raise Unknown("submodule", name)
        if mode not in (gitraw.MODE_FILE, gitraw.MODE_EXEC, gitraw.MODE_LINK):
            raise Unknown("malformed-index", name)
        if prev is not None and name <= prev:
            raise Unknown("malformed-index", name)
        out[name], prev, pos = (mode, oid), name, entry_end
    while pos < end:
        if pos + 8 > end:
            raise Unknown("malformed-index")
        sig, size = data[pos:pos + 4], int.from_bytes(data[pos + 4:pos + 8], "big")
        if pos + 8 + size > end:
            raise Unknown("malformed-index")
        if sig not in (b"TREE", b"REUC", b"EOIE", b"IEOT"):
            raise Unknown("unsupported-index-feature", sig)
        pos += 8 + size
    names = list(out)
    for name in names:
        if has_children(names, name):
            raise Unknown("malformed-index", name)
    return out


def _root_id(st):
    birth = getattr(st, "st_birthtime_ns", None)
    if birth is None and getattr(st, "st_birthtime", None) is not None:
        birth = int(st.st_birthtime * 1_000_000_000)
    return RootId(st.st_dev, st.st_ino, birth)


def _check_context(ctx):
    for p in (ctx.worktree, ctx.admin, ctx.common):
        if type(p) is not str or not p.startswith("/") or p == "/" or os.path.normpath(p) != p or "\0" in p:
            raise Unknown("invalid-context")
    ordinary = ctx.admin == ctx.worktree + "/.git"
    if ordinary != (ctx.common == ctx.admin) or (ctx.expected is not None and not isinstance(ctx.expected, Binding)):
        raise Unknown("invalid-context")
    if not ordinary and (ctx.admin + "/").startswith(ctx.worktree + "/"):
        raise Unknown("invalid-context")
    return ordinary


def _links(ctx, ordinary, wt, wt_dev, admin, admin_st):
    """Checks that the repository's own pointers name exactly the authorized
    roots, without following them. Returns the pointer bytes."""
    m = fdwalk.lstat_at(wt, b".git", b".git")
    if ordinary:
        if m is None or m.kind != "dir" or (m.dev, m.ino) != (admin_st.st_dev, admin_st.st_ino):
            raise IdentityChanged("git-link-mismatch", b".git")
        if fdwalk.exists(admin, b"commondir", admin_st.st_dev) is not None:
            raise IdentityChanged("git-link-mismatch", b"commondir")
        return b""
    if m is None or m.kind != "file":
        raise IdentityChanged("git-link-mismatch", b".git")
    link = fdwalk.read_at(wt, b".git", m, 4096, b".git", wt_dev)
    if link != b"gitdir: " + os.fsencode(ctx.admin) + b"\n":
        raise IdentityChanged("git-link-mismatch", b".git")
    common = fdwalk.read_path(admin, b"commondir", 4096, admin_st.st_dev)
    if common is None or common[-1:] != b"\n" or b"\0" in common:
        raise IdentityChanged("git-link-mismatch", b"commondir")
    target = os.fsdecode(common[:-1])
    resolved = target if target.startswith("/") else os.path.normpath(os.path.join(ctx.admin, target))
    if resolved != ctx.common:
        raise IdentityChanged("git-link-mismatch", b"commondir")
    back = fdwalk.read_path(admin, b"gitdir", 4096, admin_st.st_dev)
    if back != os.fsencode(ctx.worktree) + b"/.git\n":
        raise IdentityChanged("git-link-mismatch", b"gitdir")
    return link + b"\0" + common + b"\0" + back


def _bind(ctx, ordinary, fds, stats):
    wt, admin, _ = fds
    links = _links(ctx, ordinary, wt, stats[0].st_dev, admin, stats[1])
    ids = [_root_id(st) for st in stats]
    return Binding(*ids, hashlib.sha256(links).hexdigest())


class _Worktree:
    """Pass one: hashes every tracked entry while its directory is open."""

    def __init__(self, index, dev, limits):
        self.index, self.dev, self.limits = index, dev, limits
        self.found, self.total = {}, 0

    def visit(self, dirfd, name, rel, m):
        want = self.index.get(rel)
        if want is None:
            return
        mode, oid = want
        if m.kind == "link":
            if mode != gitraw.MODE_LINK:
                self.found[rel] = "type-changed"
                return
            data = fdwalk.readlink_at(dirfd, name, m, 4096, rel)
        else:
            if mode == gitraw.MODE_LINK:
                self.found[rel] = "type-changed"
                return
            if bool(m.perm & 0o100) != (mode == gitraw.MODE_EXEC):
                self.found[rel] = "mode-changed"
                return
            self.total += m.size
            if self.total > self.limits.max_total:
                raise Unknown("limit-exceeded", rel)
            data = fdwalk.read_at(dirfd, name, m, self.limits.max_file, rel, self.dev)
        self.found[rel] = (blob_oid(data) == oid) or "modified"


def _compare(index, tree, entries, found):
    reasons = []
    if index != tree:
        for p in sorted(set(index) ^ set(tree) | {p for p in index.keys() & tree.keys() if index[p] != tree[p]}):
            reasons.append(_reason("staged", p))
    for p in sorted(index):
        got = found.get(p)
        if got is True:
            continue
        reasons.append(_reason(got if got else "type-changed" if p in entries else "deleted", p))
    names = sorted(index)
    for p, m in entries.items():
        if p not in index and not (m.kind == "dir" and has_children(names, p)):
            reasons.append(_reason("untracked", p))
    return reasons


def _manifest(entries, found, index):
    h = hashlib.sha256()
    for p in sorted(entries):
        m = entries[p]
        tracked = found.get(p) is True
        h.update(p + b"\0" + m.kind.encode() + b"\0" + (index[p][1] if tracked else b"-") + b"\n")
    return h.hexdigest()


def _attributes(paths):
    return next((p for p in paths if p.rsplit(b"/", 1)[-1].lower() == b".gitattributes"), None)


def _observe(ctx):
    if not fdwalk.SUPPORTED:
        raise Unknown("platform-unsupported")
    ordinary = _check_context(ctx)
    limits = ctx.limits
    with contextlib.ExitStack() as stack:
        fds, stats = [], []
        for p in (ctx.worktree, ctx.admin, ctx.common):
            fd, st = fdwalk.open_root(p)
            stack.callback(os.close, fd)
            fds.append(fd)
            stats.append(st)
        binding = _bind(ctx, ordinary, fds, stats)
        if ctx.expected is not None and binding != ctx.expected:
            raise IdentityChanged("binding-mismatch")
        if ctx.require_birthtime and any(r.birth_ns is None for r in binding[:3]):
            raise Unknown("birthtime-unavailable")
        wt, admin, common = fds
        dev = stats[0].st_dev
        repo = gitraw.Repo(admin, stats[1].st_dev, common, stats[2].st_dev, limits)
        inputs, ref, commit = repo.snapshot()
        index = parse_index(inputs[4], limits)
        tree_oid = repo.commit_tree(commit) if commit is not None else None
        tree = repo.flatten(tree_oid) if tree_oid is not None else {}
        if _attributes(index) is not None or _attributes(tree) is not None:
            raise Unknown("attributes-present", _attributes(index) or _attributes(tree))
        walker = _Worktree(index, dev, limits)
        entries = fdwalk.walk(wt, dev, limits, (b".git",), walker.visit)
        if _attributes(entries) is not None:
            raise Unknown("attributes-present", _attributes(entries))
        reasons = _compare(index, tree, entries, walker.found)

        if fdwalk.walk(wt, dev, limits, (b".git",)) != entries or repo.snapshot()[0] != inputs:
            raise Unknown("changed-during-observation")
        for p, st in zip((ctx.worktree, ctx.admin, ctx.common), stats):
            fd, again = fdwalk.open_root(p)
            os.close(fd)
            if (again.st_dev, again.st_ino) != (st.st_dev, st.st_ino):
                raise IdentityChanged("root-replaced", os.fsencode(p))
        if _bind(ctx, ordinary, fds, stats) != binding:
            raise Unknown("changed-during-observation")
        digests = {
            "binding": hashlib.sha256(repr(tuple(binding)).encode()).hexdigest(),
            "inputs": hashlib.sha256(b"\0".join(hashlib.sha256(x or b"").digest() for x in inputs)).hexdigest(),
            "index": hashlib.sha256(inputs[4] or b"").hexdigest(),
            "worktree": _manifest(entries, walker.found, index),
            "head_ref": (ref or b"").decode(),
            "commit": (commit or b"").decode(),
            "tree": (tree_oid or b"").decode(),
        }
        return Observation(Status.NOT_CLEAN if reasons else Status.CLEAN, reasons, digests, binding)


def observe(ctx):
    try:
        return _observe(ctx)
    except IdentityChanged as e:
        return Observation(Status.IDENTITY_CHANGED, [_reason(e.code, e.path)])
    except Unknown as e:
        return Observation(Status.UNKNOWN, [_reason(e.code, e.path)])
    except OSError:
        return Observation(Status.UNKNOWN, [_reason("filesystem-error")])

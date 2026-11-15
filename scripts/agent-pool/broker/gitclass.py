"""PR-3b Git feature classification over captured raw bytes (PR-3b contract
R4 §5, R4.1 §3). Pure: the input is the byte content of the entries a
manifest captured, already re-hashed by the caller; nothing runs Git, opens a
file or follows a repository-selected path.

The five questions stay separate: Q1 raw capture is the caller's; this module
answers Q2 (features) per (repository, check) with every finding reported, and
the caller derives Q3 (semantic verification), Q4 (verified preservation) and
Q5 (recovery eligibility) from those findings only. A stored summary is never
read. The parsing subset follows the PR-2 observer (`gitraw`), which core
modules may not import; anything outside it is a finding, never a success."""

import hashlib
import re
import zlib

HEX40 = re.compile(rb"[0-9a-f]{40}\Z")
HEX64 = re.compile(rb"[0-9a-f]{64}\Z")
_REF = re.compile(rb"refs/[A-Za-z0-9._/-]{1,240}\Z")
_HEAD_REF = re.compile(rb"refs/heads/[A-Za-z0-9._/-]{1,200}\Z")
_SECTION = re.compile(rb'\[([A-Za-z0-9.-]+)(?: "([^"\\\n]*)")?\]\Z')
_KEY_VALUE = re.compile(rb"([A-Za-z][A-Za-z0-9-]*)[ \t]*(?:=[ \t]*(.*))?\Z")
MAX_REF = 4096
MAX_OBJECTS = 100000
MAX_OBJECT_BYTES = 16 << 20
_HFS_IGNORED = re.compile("[‌-‏‪-‮⁪-⁯﻿]")

# Q4 INCOMPLETE: the bytes do not form a repository PR-3b can vouch for.
DEFECTS = frozenset({"malformed_config", "missing_head", "malformed_head", "malformed_ref", "malformed_packed",
                     "oversize_ref", "unsupported_object_format", "missing_object", "corrupt_object", "unknown"})
# Q4 INCOMPLETE(git_external): objects or administration live outside the captured tree.
EXTERNAL = frozenset({"alternates", "gitlink_file", "commondir"})
# Q3 INCOMPLETE and Q5 NOT_ELIGIBLE: preserved as bytes, never claimed recoverable through Git.
UNSUPPORTED = frozenset({"reftable", "sha256", "extensions", "shallow", "grafts", "replace_refs", "promisor",
                         "submodule_gitdir", "git_head", "unborn_head"})
# Q3 INCOMPLETE only (still eligible: restoration never uses Git). `packed`: a
# reachable object is not loose but a consistent pack index lists it; its
# packed content is never inflated or hashed, so it is not verified.
UNVERIFIED = frozenset({"packed"})
# Recorded, benign.
BENIGN = frozenset({"dangling_symref", "residue", "symref", "packed_refs", "nested"})


class Repo:
    __slots__ = ("root", "features", "head", "head_oid", "tips")

    def __init__(self, root):
        self.root, self.features, self.head, self.head_oid, self.tips = root, set(), None, None, set()


def is_gitdir_name(c):
    """R2 K3: a `.git` name compared case-insensitively, also with the code
    points HFS+ ignores (Git's `is_hfs_dotgit`), so `.GIT` and `.Git` count."""
    if c.lower() == b".git":
        return True
    try:
        return _HFS_IGNORED.sub("", c.decode("utf-8")).lower() == ".git"
    except UnicodeDecodeError:
        return False


def git_index(parts):
    """Index of the first administrative-directory component, or None."""
    return next((i for i, c in enumerate(parts) if is_gitdir_name(c)), None)


def is_git_path(path):
    return git_index(path.split(b"/")) is not None


def repositories(files):
    """{root: Repo} for a captured tree. `files` maps a tree-relative path
    (bytes) to its bytes. A repository is the prefix before the first `.git`
    component; a `.git` regular file is a gitlink (linked worktree or submodule
    checkout) whose objects are elsewhere. A root whose administrative
    directory is spelled more than one way is ambiguous: `unknown`."""
    split, names = {}, {}
    for path, data in files.items():
        parts = path.split(b"/")
        i = git_index(parts)
        if i is None:
            continue
        root = b"/".join(parts[:i])
        split.setdefault(root, {})[b"/".join(parts[i + 1:])] = data
        names.setdefault(root, set()).add(parts[i])
    repos = {}
    for root, fs in split.items():
        repos[root] = classify(root, fs)
        if len(names[root]) > 1:
            repos[root].features.add("unknown")
    return repos


def _config(r, data):
    section = None
    for raw in data.split(b"\n"):
        line = raw.strip()
        if not line or line[:1] in (b"#", b";"):
            continue
        m = _SECTION.match(line)
        if m:
            section = (m.group(1).lower(), m.group(2))
            continue
        kv = _KEY_VALUE.match(line)
        if section is None or kv is None:
            r.features.add("malformed_config")
            continue
        key, value = kv.group(1).lower(), (kv.group(2) or b"true").strip().lower()
        if section[0] == b"extensions":
            r.features.add({b"objectformat": "sha256" if value == b"sha256" else "extensions",
                            b"refstorage": "reftable" if value == b"reftable" else "extensions"}.get(key,
                                                                                               "extensions"))
        elif section[0] == b"core" and key == b"repositoryformatversion" and value not in (b"0", b"1"):
            r.features.add("malformed_config")
        elif section[0] == b"core" and key in (b"worktree", b"bare") and not (key == b"bare" and value == b"false"):
            r.features.add("commondir")
        elif section[0] == b"remote" and key in (b"promisor", b"partialclonefilter"):
            r.features.add("promisor")


def _oid_file(r, data, what):
    """A loose ref body: exactly one 40-hex id and a newline, or a symref."""
    if len(data) > MAX_REF:
        r.features.add("oversize_ref")
        return None, None
    body = data[:-1] if data.endswith(b"\n") else None
    if body is None:
        r.features.add(what)
        return None, None
    if HEX40.match(body):
        return body.decode(), None
    if HEX64.match(body):
        r.features.add("unsupported_object_format")
        return None, None
    if body.startswith(b"ref: ") and _REF.match(body[5:]):
        return None, body[5:]
    r.features.add(what)
    return None, None


def _packed(r, data, refs):
    lines = data.split(b"\n")
    if lines and lines[-1] == b"":
        lines.pop()
    else:
        r.features.add("malformed_packed")
    last = None
    for line in lines:
        if line.startswith(b"# pack-refs with:"):
            continue
        if line.startswith(b"^") and HEX40.match(line[1:]) and last is not None:
            continue
        oid, _, name = line.partition(b" ")
        if HEX40.match(oid) and _REF.match(name):
            refs.setdefault(name, ("packed", oid.decode()))
            last = name
        elif HEX64.match(oid):
            r.features.add("unsupported_object_format")
        else:
            r.features.add("malformed_packed")


def _read_object(files, oid):
    """Returns (type, body) for an intact loose object, "missing" or "corrupt"."""
    data = files.get(b"objects/" + oid[:2].encode() + b"/" + oid[2:].encode())
    if data is None:
        return "missing"
    try:
        z = zlib.decompressobj()
        raw = z.decompress(data, MAX_OBJECT_BYTES + 1)
        if len(raw) > MAX_OBJECT_BYTES or z.unconsumed_tail or z.unused_data or not z.eof:
            return "corrupt"
    except zlib.error:
        return "corrupt"
    if hashlib.sha1(raw).hexdigest() != oid:  # noqa: S324 - Git object ids are SHA-1 (integrity only)
        return "corrupt"
    head, sep, body = raw.partition(b"\0")
    typ, _, size = head.partition(b" ")
    if not sep or typ not in (b"commit", b"tree", b"blob", b"tag") or not size.isdigit() or int(size) != len(body):
        return "corrupt"
    return typ, body


def _links(typ, body):
    """Object ids an intact commit, tag or tree refers to (gitlinks excluded)."""
    if typ in (b"commit", b"tag"):
        out = []
        for line in body.split(b"\n"):
            if not line:
                break
            key, _, val = line.partition(b" ")
            if key in ((b"tree", b"parent") if typ == b"commit" else (b"object",)):
                if not HEX40.match(val):
                    return None
                out.append(val.decode())
        return out
    if typ == b"tree":
        out, i = [], 0
        while i < len(body):
            sp, nul = body.find(b" ", i), body.find(b"\0", i)
            if sp < 0 or nul < sp or nul + 21 > len(body):
                return None
            if body[i:sp] != b"160000":
                out.append(body[nul + 1:nul + 21].hex())
            i = nul + 21
        return out
    return []


def _idx_names(idx, pack):
    """The object ids a v2 pack index lists, or None unless the index and its
    sibling pack are structurally consistent: header, monotonic fan-out that
    matches the strictly increasing names, bounded count, exact size, offsets
    inside the pack and distinct, index checksum, pack header count and pack
    checksum equal to the one the index records. Presence only (R2 K7): no
    packed object is inflated or hashed."""
    if pack is None or len(idx) < 1072 or idx[:8] != b"\xfftOc\x00\x00\x00\x02":
        return None
    fan = [int.from_bytes(idx[8 + 4 * i:12 + 4 * i], "big") for i in range(256)]
    n = fan[255]
    if n > MAX_OBJECTS or any(a > b for a, b in zip(fan, fan[1:])):
        return None
    offsets_at, large_at = 1032 + 24 * n, 1032 + 28 * n
    offs = [int.from_bytes(idx[offsets_at + 4 * k:offsets_at + 4 * k + 4], "big") for k in range(n)]
    large = sum(1 for o in offs if o & 0x80000000)
    if len(idx) != large_at + 8 * large + 40 or hashlib.sha1(idx[:-20]).digest() != idx[-20:]:  # noqa: S324
        return None
    if len(pack) < 32 or pack[:4] != b"PACK" or int.from_bytes(pack[4:8], "big") not in (2, 3) \
            or int.from_bytes(pack[8:12], "big") != n or idx[-40:-20] != pack[-20:] \
            or hashlib.sha1(pack[:-20]).digest() != pack[-20:]:  # noqa: S324 - Git checksums are SHA-1
        return None
    names, seen, prev = set(), set(), b""
    for k in range(n):
        name = idx[1032 + 20 * k:1052 + 20 * k]
        b, o = name[0], offs[k]
        if name <= prev or not (fan[b - 1] if b else 0) <= k < fan[b]:
            return None
        if o & 0x80000000:
            j = o & 0x7FFFFFFF
            o = int.from_bytes(idx[large_at + 8 * j:large_at + 8 * j + 8], "big") if j < large else 0
            if o < 0x80000000:
                return None
        if not 12 <= o < len(pack) - 20 or o in seen:
            return None
        seen.add(o)
        names.add(name.hex())
        prev = name
    return names


def _pack_index(r, files):
    """Ids listed by every pack index of the repository. A malformed index,
    or one without its matching pack, is `unknown`; a pack without an index
    lists nothing."""
    listed = set()
    for p in sorted(files):
        if p.startswith(b"objects/pack/") and p.endswith(b".idx") and b"/" not in p[len(b"objects/pack/"):]:
            got = _idx_names(files[p], files.get(p[:-4] + b".pack"))
            if got is None:
                r.features.add("unknown")
            else:
                listed |= got
    return listed


def _closure(r, files, tips):
    packed = _pack_index(r, files)
    seen, todo = set(), sorted(tips)
    while todo:
        oid = todo.pop()
        if oid in seen:
            continue
        if len(seen) >= MAX_OBJECTS:
            r.features.add("unknown")
            return
        seen.add(oid)
        got = _read_object(files, oid)
        if got == "missing":
            r.features.add("packed" if oid in packed else "missing_object")
            continue
        if got == "corrupt":
            r.features.add("corrupt_object")
            continue
        links = _links(*got)
        if links is None:
            r.features.add("corrupt_object")
            continue
        todo.extend(x for x in links if x not in seen)


def classify(root, files):
    """One repository's findings. `files` maps the gitdir-relative path to bytes."""
    r = Repo(root)
    if b"" in files:
        r.features.add("gitlink_file")
    if b"config" in files:
        _config(r, files[b"config"])
    for name, feat in ((b"shallow", "shallow"), (b"objects/info/alternates", "alternates"),
                       (b"objects/info/http-alternates", "alternates"), (b"info/grafts", "grafts"),
                       (b"commondir", "commondir"), (b"reftable", "reftable")):
        if name in files or any(p.startswith(name + b"/") for p in files):
            r.features.add(feat)
    if any(p.startswith(b"modules/") for p in files):
        r.features.add("submodule_gitdir")
    if any(p.startswith(b"refs/replace/") for p in files):
        r.features.add("replace_refs")
    if any(p.endswith(b".lock") or b"/tmp_obj_" in b"/" + p for p in files):
        r.features.add("residue")
    refs = {}
    for p in sorted(files):
        if p.startswith(b"refs/") and not p.endswith(b".lock"):
            oid, sym = _oid_file(r, files[p], "malformed_ref")
            if oid is not None or sym is not None:
                refs[p] = ("loose", oid) if oid is not None else ("sym", sym)
    if b"packed-refs" in files:
        r.features.add("packed_refs")
        _packed(r, files[b"packed-refs"], refs)
    for name, (kind, val) in refs.items():
        if kind == "sym":
            r.features.add("symref")
            if val not in refs:
                r.features.add("dangling_symref")
    if b"HEAD" not in files:
        r.features.add("missing_head")
    else:
        oid, sym = _oid_file(r, files[b"HEAD"], "malformed_head")
        if oid is not None:
            r.head, r.head_oid = {"kind": "detached"}, oid
        elif sym is not None:
            if not _HEAD_REF.match(sym):
                r.features.add("git_head")
            target = refs.get(sym)
            if target is None:
                r.features.add("unborn_head")
            elif target[0] == "sym":
                r.features.add("git_head")
            else:
                r.head_oid = target[1]
            r.head = {"kind": "symbolic"}
    r.tips = {val for kind, val in refs.values() if kind != "sym"} | ({r.head_oid} if r.head_oid else set())
    _closure(r, files, r.tips)
    return r


def questions(repos, q1):
    """(q2 features, q3, q4, q5, reasons, head) for the captured tree. `repos`
    maps a repository root to its Repo; the slot's own repository is the root
    `b""`. Unknown never succeeds; one finding never hides another."""
    feats = set()
    for root, r in repos.items():
        feats |= r.features | ({"nested"} if root else set())
    top = repos.get(b"")
    reasons = sorted(feats - BENIGN)
    q3 = "VERIFIED" if top is not None and not feats & (DEFECTS | EXTERNAL | UNSUPPORTED | UNVERIFIED) \
        else "INCOMPLETE"
    q4 = "COMPLETE" if q1 == "COMPLETE" and top is not None and not feats & (DEFECTS | EXTERNAL) else "INCOMPLETE"
    head = top.head_oid if top is not None else None
    q5 = "ELIGIBLE" if q4 == "COMPLETE" and head is not None and not feats & UNSUPPORTED else "NOT_ELIGIBLE"
    return sorted(feats), q3, q4, q5, reasons, head


def unpublished_commit_items(repo):
    """N-8 over-approximation: every ref tip of the repository is listed as an
    unpublished commit item id; every object is preserved as raw bytes anyway."""
    return sorted("i" + hashlib.sha256(b"commit:" + oid.encode()).hexdigest()[:63] for oid in repo.tips)

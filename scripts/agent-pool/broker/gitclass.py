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

# Q4 INCOMPLETE: the bytes do not form a repository PR-3b can vouch for.
DEFECTS = frozenset({"malformed_config", "missing_head", "malformed_head", "malformed_ref", "malformed_packed",
                     "oversize_ref", "unsupported_object_format", "missing_object", "corrupt_object", "unknown"})
# Q4 INCOMPLETE(git_external): objects or administration live outside the captured tree.
EXTERNAL = frozenset({"alternates", "gitlink_file", "commondir"})
# Q3 INCOMPLETE and Q5 NOT_ELIGIBLE: preserved as bytes, never claimed recoverable through Git.
UNSUPPORTED = frozenset({"reftable", "sha256", "extensions", "shallow", "grafts", "replace_refs", "promisor",
                         "submodule_gitdir", "git_head", "unborn_head"})
# Q3 INCOMPLETE only (still eligible: restoration never uses Git).
UNVERIFIED = frozenset({"packed"})
# Recorded, benign.
BENIGN = frozenset({"dangling_symref", "residue", "symref", "packed_refs", "nested"})


class Repo:
    __slots__ = ("root", "features", "head", "head_oid", "tips")

    def __init__(self, root):
        self.root, self.features, self.head, self.head_oid, self.tips = root, set(), None, None, set()


def split_repositories(files):
    """`files` maps a tree-relative path (bytes) to its bytes. A repository is
    the prefix before the first `.git` component; a `.git` regular file is a
    gitlink (linked worktree or submodule checkout) whose objects are elsewhere."""
    repos = {}
    for path, data in files.items():
        parts = path.split(b"/")
        if b".git" not in parts:
            continue
        i = parts.index(b".git")
        root = b"/".join(parts[:i])
        rel = b"/".join(parts[i + 1:])
        repos.setdefault(root, {})[rel] = data
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


def _closure(r, files, tips):
    packs = any(p.startswith(b"objects/pack/") and p.endswith((b".pack", b".idx")) for p in files)
    if packs:
        r.features.add("packed")
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
            r.features.add("packed" if packs else "missing_object")
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

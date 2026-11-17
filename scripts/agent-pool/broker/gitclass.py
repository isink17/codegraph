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
import unicodedata
import zlib

HEX40 = re.compile(rb"[0-9a-f]{40}\Z")
HEX64 = re.compile(rb"[0-9a-f]{64}\Z")
# A ref name: printable ASCII (the PR-2 observer's packed-ref subset) that
# also passes Git's check-ref-format rules, which `_REF_BAD` rejects.
_REF = re.compile(rb"refs/[!-~]{1,240}\Z")
_REF_BAD = re.compile(rb"[~^:?*\[\\]|\.\.|@\{|//|/\.|\.lock(?:/|\Z)|[/.]\Z")
_HEAD_REF = re.compile(rb"refs/heads/[A-Za-z0-9._/-]{1,200}\Z")
_SECTION = re.compile(rb'\[([A-Za-z0-9.-]+)(?: "([^"\\\n]*)")?\]\Z')
_KEY_VALUE = re.compile(rb"([A-Za-z][A-Za-z0-9-]*)[ \t]*(?:=[ \t]*(.*))?\Z")
_PACKED_HEADER = b"# pack-refs with: "
MAX_REF = 4096
MAX_OBJECTS = 100000
MAX_OBJECT_BYTES = 16 << 20
_TYPES = (b"commit", b"tree", b"blob", b"tag")
# Tree entry mode: the type its object must have; a gitlink is not followed.
_MODES = {b"100644": b"blob", b"100755": b"blob", b"120000": b"blob", b"40000": b"tree", b"160000": None}
# Git's fsck_ident: `name <email> seconds +hhmm`, name nonempty, no zero padding.
_IDENT = re.compile(rb"[^<>\n]+ <[^<>\n]*> (0|[1-9][0-9]{0,19}) [+-][0-9]{4}\Z")
# Git's date_overflows: the seconds must fit a signed 64-bit time_t.
_TIME_MAX = (1 << 63) - 1
# Object header size: canonical decimal, as Git writes it and the PR-2 observer reads it.
_SIZE = re.compile(rb"(?:0|[1-9][0-9]*)\Z")
# The only headers Git continues on following lines (leading space).
_MULTILINE = frozenset({b"gpgsig", b"gpgsig-sha256", b"mergetag"})
_HFS_IGNORED = re.compile("[\u200c-\u200f\u202a-\u202e\u206a-\u206f\ufeff]")

# Q4 INCOMPLETE: the bytes do not form a repository PR-3b can vouch for.
DEFECTS = frozenset({"malformed_config", "missing_head", "malformed_head", "malformed_ref", "malformed_packed",
                     "oversize_ref", "unsupported_object_format", "missing_object", "corrupt_object", "unknown"})
# Q4 INCOMPLETE(git_external): objects or administration live outside the captured tree.
EXTERNAL = frozenset({"alternates", "gitlink_file", "commondir", "config_include"})
# Q3 INCOMPLETE and Q5 NOT_ELIGIBLE: preserved as bytes, never claimed recoverable through Git.
# `commit_graph`: Git reads parents and generations from a commit-graph
# instead of the verified commits, and recovery has no step that drops or
# rebuilds it, so the Git view after a restore is not the verified one.
UNSUPPORTED = frozenset({"reftable", "sha256", "extensions", "shallow", "grafts", "replace_refs", "promisor",
                         "submodule_gitdir", "git_head", "unborn_head", "commit_graph"})
# Q3 INCOMPLETE and Q5 NOT_ELIGIBLE, Q4 unchanged (owner amendment R4.2 to
# R2 K7 / R4 §5.2 / P2). `packed`: a consistent pack index lists a reachable
# object, even one with a verified loose copy, because Git reads a pack before
# a loose object; packed content is never inflated or hashed, so nothing proves
# what the pack holds. `multi_pack_index`: Git looks objects up through it
# before any pack index and it is never parsed here.
UNVERIFIED = frozenset({"packed", "multi_pack_index"})
# Recorded, benign.
BENIGN = frozenset({"dangling_symref", "residue", "symref", "packed_refs", "nested", "gitlink"})


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


def repositories(files, captured=None):
    """{root: Repo} for a captured tree. `files` maps a tree-relative path
    (bytes) to its bytes. A repository is the prefix before the first `.git`
    component; a `.git` regular file is a gitlink (linked worktree or submodule
    checkout) whose objects are elsewhere. A root whose administrative
    directory is spelled more than one way is ambiguous: `unknown`. So is a
    root with a path that shares a file or directory with another captured
    path (`captured`: every manifest path, default `files`), either as the
    same path below the administrative directory or on a case- and
    normalization-insensitive volume: the bytes read here need not be the
    bytes that end up there. A path that is not UTF-8 has no name on such a
    volume at all."""
    shared = _shared(files if captured is None else captured)
    split, names, ambiguous = {}, {}, set()
    for path, data in files.items():
        parts = path.split(b"/")
        i = git_index(parts)
        if i is None:
            continue
        root = b"/".join(parts[:i])
        fs = split.setdefault(root, {})
        sub = b"/".join(parts[i + 1:])
        if sub in fs:
            ambiguous.add(root)
        fs[sub] = data
        names.setdefault(root, set()).add(parts[i])
        f = _fold(path).split("/")
        if any("/".join(f[:k]) in shared for k in range(1, len(f) + 1)) or not _utf8(path):
            ambiguous.add(root)
    repos = {}
    for root, fs in split.items():
        repos[root] = classify(root, fs)
        if len(names[root]) > 1 or root in ambiguous:
            repos[root].features.add("unknown")
    return repos


def _fold(path):
    """`path` as APFS or HFS+ compares names (case-insensitive,
    normalization-insensitive), one component at a time; folding too much
    only refuses. A component that is not UTF-8 (APFS refuses those) stays
    apart from every UTF-8 one."""
    out = []
    for c in path.split(b"/"):
        try:
            s = c.decode("utf-8")
        except UnicodeDecodeError:
            out.append("\0" + c.decode("latin-1").lower())
            continue
        out.append(unicodedata.normalize("NFD", _HFS_IGNORED.sub("", s)).casefold())
    return "/".join(out)


def _utf8(path):
    try:
        path.decode("utf-8")
    except UnicodeDecodeError:
        return False
    return True


def _shared(paths):
    """Folded names two captured paths share on APFS or HFS+: one file named
    twice, a file that is also another path's directory, or one directory
    spelled two ways."""
    files, dirs, twice = set(), {}, set()
    for p in paths:
        f = _fold(p)
        (twice if f in files else files).add(f)
        raw, parts = p.split(b"/"), f.split("/")
        for k in range(1, len(parts)):
            dirs.setdefault("/".join(parts[:k]), set()).add(b"/".join(raw[:k]))
    return twice | (files & dirs.keys()) | {d for d, spellings in dirs.items() if len(spellings) > 1}


def _prefixes(paths):
    """Every path in `paths` and every directory above one."""
    out = set()
    for p in paths:
        parts = p.split(b"/")
        out.update(b"/".join(parts[:k]) for k in range(1, len(parts) + 1))
    return out


def _config(r, data):
    section = None
    for raw in data.split(b"\n"):
        line = raw.strip()
        if not line or line[:1] in (b"#", b";"):
            continue
        m = _SECTION.match(line)
        if m:
            section = (m.group(1).lower(), m.group(2))
            if section[0] in (b"include", b"includeif"):
                r.features.add("config_include")
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
    if body.startswith(b"ref: ") and _valid_ref(body[5:]):
        return None, body[5:]
    r.features.add(what)
    return None, None


def _valid_ref(name):
    return _REF.match(name) is not None and _REF_BAD.search(name) is None


def _packed(r, data, refs):
    """Parses `packed-refs` as Git reads it and returns ({name: peel id or
    None}, header traits). Anything Git would read differently is
    `malformed_packed`: a repeated name (Git may resolve either line), a
    header that is not the first line, a `^` line not directly after a ref,
    and names out of order under a `sorted` header (Git then binary-searches
    and can miss a ref or expose a packed value a loose ref shadows)."""
    lines = data.split(b"\n")
    if lines and lines[-1] == b"":
        lines.pop()
    else:
        r.features.add("malformed_packed")
    traits = set()
    if lines and lines[0].startswith(_PACKED_HEADER):
        traits = set(lines.pop(0)[len(_PACKED_HEADER):].split(b" "))
    peels, last, prev = {}, None, None
    for line in lines:
        if line.startswith(b"^"):
            if last is None or not HEX40.match(line[1:]):
                r.features.add("malformed_packed")
            else:
                peels[last] = line[1:].decode()
            last = None
            continue
        last = None
        oid, _, name = line.partition(b" ")
        if HEX40.match(oid) and _valid_ref(name):
            if name in peels or (b"sorted" in traits and prev is not None and name < prev):
                r.features.add("malformed_packed")
            peels[name] = None
            refs.setdefault(name, ("packed", oid.decode()))
            last = prev = name
            if name.startswith(b"refs/replace/"):
                r.features.add("replace_refs")
        elif HEX64.match(oid):
            r.features.add("unsupported_object_format")
        else:
            r.features.add("malformed_packed")
    return peels, traits


def _peeled(files, oid):
    """What Git's peel of `oid` gives: the final target of a tag chain, None
    when `oid` is not a tag, False when a link is not an intact loose tag."""
    got = _read_object(files, oid)
    if not isinstance(got, tuple):
        return False
    if got[0] != b"tag":
        return None
    for _ in range(MAX_OBJECTS):
        links = _tag_links(got[1])
        if links is None:
            return False
        oid, typ = links[0]
        if typ != b"tag":
            return oid
        got = _read_object(files, oid)
        if not isinstance(got, tuple) or got[0] != b"tag":
            return False
    return False


def _check_peels(r, files, refs, peels, traits):
    """Git trusts a `^` line as the ref's peeled value, and under a
    `fully-peeled` (or, for `refs/tags/`, `peeled`) header Git 2.38 also
    trusts its absence. Either must match the verified objects. A line that
    a loose ref shadows is never read. A broken tag chain is reported by
    `_closure` already."""
    for name, peel in peels.items():
        kind, oid = refs[name]
        if kind != "packed":
            continue
        want = _peeled(files, oid)
        knows = b"fully-peeled" in traits or (b"peeled" in traits and name.startswith(b"refs/tags/"))
        if want is not False and peel != want and (peel is not None or knows):
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
    nul = raw.find(b"\0", 0, 32)
    typ, sep, size = raw[:max(nul, 0)].partition(b" ")
    if nul < 0 or not sep or typ not in _TYPES or not _SIZE.match(size) or int(size) != len(raw) - nul - 1:
        return "corrupt"
    return typ, raw[nul + 1:]


def _headers(body):
    """[(key, value)] of a commit or tag header, up to the blank line or the
    end of an empty message; a continuation line (leading space) belongs to
    the header before it and is allowed only after `gpgsig`,
    `gpgsig-sha256` or `mergetag`. None unless every header line is
    `key value` and the header is newline-terminated without NUL."""
    end = body.find(b"\n\n")
    head = body[:end + 1] if end >= 0 else body
    if not head.endswith(b"\n") or b"\0" in head:
        return None
    out = []
    for line in head[:-1].split(b"\n"):
        key, sp, val = line.partition(b" ")
        if not key and out and out[-1][0] in _MULTILINE:
            continue
        if not key or not sp:
            return None
        out.append((key, val))
    return out


def _ident(value):
    m = _IDENT.match(value)
    return m is not None and int(m.group(1)) <= _TIME_MAX


def _commit_links(body):
    """Git's fsck commit order: one `tree`, zero or more `parent`, one
    `author`, one `committer`, then optional headers that never repeat a
    mandatory key."""
    h = _headers(body)
    if h is None:
        return None
    keys = [k for k, _ in h]
    n = keys.count(b"parent")
    if keys[:n + 3] != [b"tree"] + [b"parent"] * n + [b"author", b"committer"] \
            or set(keys[n + 3:]) & {b"tree", b"parent", b"author", b"committer"} \
            or not all(HEX40.match(v) for _, v in h[:n + 1]) or not all(_ident(v) for _, v in h[n + 1:n + 3]):
        return None
    return [(h[0][1].decode(), b"tree")] + [(v.decode(), b"commit") for _, v in h[1:n + 1]]


def _tag_links(body):
    """`object`, `type`, `tag`, an optional `tagger` (old tags lack it), then
    optional headers; the target must have the declared type."""
    h = _headers(body)
    if h is None:
        return None
    keys = [k for k, _ in h]
    m = 4 if keys[3:4] == [b"tagger"] else 3
    if keys[:3] != [b"object", b"type", b"tag"] or set(keys[m:]) & {b"object", b"type", b"tag", b"tagger"} \
            or not HEX40.match(h[0][1]) or h[1][1] not in _TYPES or not h[2][1] \
            or (m == 4 and not _ident(h[3][1])):
        return None
    return [(h[0][1].decode(), h[1][1])]


def _tree_links(r, body):
    """The subset of the PR-2 observer (`gitraw.parse_tree`): known modes,
    valid distinct names in Git's tree order, whole 20-byte ids. A gitlink is
    a submodule commit outside this object store: recorded, not followed."""
    out, i, prev, names = [], 0, None, set()
    while i < len(body):
        sp = body.find(b" ", i, i + 7)
        nul = body.find(b"\0", sp + 1) if sp >= 0 else -1
        if sp < 0 or nul < 0 or nul + 21 > len(body):
            return None
        mode, name = body[i:sp], body[sp + 1:nul]
        if mode not in _MODES or not 0 < len(name) <= 255 or name in (b".", b"..") or b"/" in name \
                or is_gitdir_name(name) or name in names:
            return None
        key = name + b"/" if mode == b"40000" else name
        if prev is not None and key <= prev:
            return None
        if _MODES[mode] is None:
            r.features.add("gitlink")
        else:
            out.append((body[nul + 1:nul + 21].hex(), _MODES[mode]))
        names.add(name)
        prev, i = key, nul + 21
    return out


def _links(r, typ, body):
    """[(id, required type)] an intact object refers to, or None when its
    structure is outside the supported subset."""
    if typ == b"commit":
        return _commit_links(body)
    if typ == b"tag":
        return _tag_links(body)
    if typ == b"tree":
        return _tree_links(r, body)
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


def _closure(r, files, tips, commits):
    """Walks every object reachable from `tips`; `commits` must be commits
    (HEAD and branches, as Git's fsck requires). A link to an object of
    another type than its referrer requires is `corrupt_object`. A listed
    object is `packed` even when its loose copy verifies (owner amendment
    R4.2, NB-12); the loose copy is still checked and walked."""
    packed = _pack_index(r, files)
    kinds, todo = {}, [(oid, b"commit" if oid in commits else None) for oid in sorted(tips)]
    while todo:
        oid, want = todo.pop()
        if oid not in kinds:
            if len(kinds) >= MAX_OBJECTS:
                r.features.add("unknown")
                return
            got = _read_object(files, oid)
            kinds[oid] = None
            if oid in packed:
                r.features.add("packed")
            if got == "missing":
                r.features.add("packed" if oid in packed else "missing_object")
            elif got == "corrupt":
                r.features.add("corrupt_object")
            else:
                links = _links(r, *got)
                if links is None:
                    r.features.add("corrupt_object")
                else:
                    kinds[oid] = got[0]
                    todo.extend(links)
        if want is not None and kinds[oid] is not None and kinds[oid] != want:
            r.features.add("corrupt_object")


# Every administrative path below the gitdir that `classify` reads by name.
# Git on APFS opens them case-insensitively, so another spelling is the same
# file to Git and an unseen one here.
_ADMIN = frozenset({b"HEAD", b"config", b"shallow", b"packed-refs", b"commondir", b"reftable", b"modules",
                    b"info", b"info/grafts", b"objects", b"objects/info", b"objects/info/alternates",
                    b"objects/info/http-alternates", b"objects/info/commit-graph",
                    b"objects/info/commit-graphs", b"objects/pack", b"objects/pack/multi-pack-index",
                    b"objects/pack/multi-pack-index.d", b"refs", b"refs/heads",
                    b"refs/replace"})
_ADMIN_FOLDED = frozenset(_fold(a) for a in _ADMIN)
# The `_ADMIN` names Git opens as directories. A manifest entry is untyped, so
# one at such a name may be a symbolic link that restore recreates, and Git
# then reads packs, alternates, grafts or refs outside the slot (NB-13).
# Other spellings are already `unknown` through `_ADMIN_FOLDED`.
_ADMIN_DIRS = frozenset({b"info", b"modules", b"objects", b"objects/info", b"objects/info/commit-graphs",
                         b"objects/pack", b"objects/pack/multi-pack-index.d", b"refs", b"refs/heads",
                         b"refs/replace", b"reftable"})


def classify(root, files):
    """One repository's findings. `files` maps the gitdir-relative path to bytes."""
    r = Repo(root)
    if b"" in files:
        r.features.add("gitlink_file")
    if any(_fold(q) in _ADMIN_FOLDED and q not in _ADMIN for q in _prefixes(files)) \
            or not _ADMIN_DIRS.isdisjoint(files):
        r.features.add("unknown")
    if b"config" in files:
        _config(r, files[b"config"])
    for name, feat in ((b"shallow", "shallow"), (b"objects/info/alternates", "alternates"),
                       (b"objects/info/http-alternates", "alternates"), (b"info/grafts", "grafts"),
                       (b"commondir", "commondir"), (b"reftable", "reftable"),
                       (b"objects/info/commit-graph", "commit_graph"),
                       (b"objects/info/commit-graphs", "commit_graph"),
                       (b"objects/pack/multi-pack-index", "multi_pack_index"),
                       (b"objects/pack/multi-pack-index.d", "multi_pack_index")):
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
    peels, traits = {}, set()
    if b"packed-refs" in files:
        r.features.add("packed_refs")
        peels, traits = _packed(r, files[b"packed-refs"], refs)
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
    branches = {val for name, (kind, val) in refs.items() if kind != "sym" and name.startswith(b"refs/heads/")}
    _closure(r, files, r.tips, branches | ({r.head_oid} if r.head_oid else set()))
    _check_peels(r, files, refs, peels, traits)
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
    q5 = "ELIGIBLE" if q4 == "COMPLETE" and head is not None and not feats & (UNSUPPORTED | UNVERIFIED) \
        else "NOT_ELIGIBLE"
    return sorted(feats), q3, q4, q5, reasons, head


def unpublished_commit_items(repo):
    """N-8 over-approximation: every ref tip of the repository is listed as an
    unpublished commit item id; every object is preserved as raw bytes anyway."""
    return sorted("i" + hashlib.sha256(b"commit:" + oid.encode()).hexdigest()[:63] for oid in repo.tips)

"""Read-only reader for the Git data one observation needs: strict-subset
config, HEAD, loose and packed refs, and loose commit and tree objects. It never
runs Git and never follows repository-selected paths: every read is relative to
an administrative or common directory the caller authorized.

Anything outside the supported subset raises Unknown: packed-only objects,
SHA-256 repositories, alternates, grafts, replace refs, shallow and
partial-clone state, extensions and any config the subset does not list.

Object ids are SHA-1 and are checked only for integrity. They are
observational values, not collision-resistant identities."""

import hashlib
import re
import zlib

from . import fdwalk
from .fdwalk import Unknown

HEX40 = re.compile(rb"[0-9a-f]{40}\Z")
HEX64 = re.compile(rb"[0-9a-f]{64}\Z")
_HEAD_REF = re.compile(rb"refs/heads/[A-Za-z0-9._/-]{1,200}\Z")
_SECTION = re.compile(rb'\[([A-Za-z0-9.-]+)(?: "([^"\\\n]*)")?\]\Z')
_KEY_VALUE = re.compile(rb"([A-Za-z][A-Za-z0-9-]*)[ \t]*(?:=[ \t]*(.*))?\Z")
_BOOL = {b"true", b"false"}
_CORE = {b"repositoryformatversion": {b"0", b"1"}, b"filemode": _BOOL, b"bare": {b"false"},
         b"logallrefupdates": _BOOL, b"ignorecase": _BOOL, b"precomposeunicode": _BOOL, b"symlinks": _BOOL}
# Sections whose keys do not change what status reports.
_INERT = {b"remote", b"branch", b"user"}
_PARTIAL_CLONE_KEYS = {b"promisor", b"partialclonefilter"}
MODE_FILE, MODE_EXEC, MODE_LINK, MODE_DIR, MODE_GITLINK = 0o100644, 0o100755, 0o120000, 0o40000, 0o160000
_TREE_MODES = {b"100644": MODE_FILE, b"100755": MODE_EXEC, b"120000": MODE_LINK, b"40000": MODE_DIR,
               b"160000": MODE_GITLINK}
# Paths under the common directory whose presence changes object or history
# resolution in ways this reader does not implement.
COMMON_FEATURES = (b"shallow", b"info/grafts", b"objects/info/alternates", b"objects/info/http-alternates",
                   b"info/attributes", b"info/sparse-checkout")
ADMIN_FEATURES = (b"index.lock", b"config.worktree", b"info/sparse-checkout")


def valid_component(name):
    return fdwalk.valid_name(name) and name.lower() != b".git"


def valid_path(path):
    return type(path) is bytes and all(valid_component(c) for c in path.split(b"/"))


def parse_config(data):
    """Accepts only the listed core keys, extensions.objectformat = sha1 and
    the inert sections. Quoting, continuations, includes and inline comments
    are refused."""
    section = None
    for line in data.split(b"\n"):
        s = line.strip(b" \t")
        if not s or s[:1] in (b"#", b";"):
            continue
        if s[:1] == b"[":
            m = _SECTION.match(s)
            if m is None:
                raise Unknown("unsupported-config")
            section, sub = m[1].lower(), m[2]
            if section not in {b"core", b"extensions"} | _INERT or (
                    section in (b"core", b"extensions") and sub is not None):
                raise Unknown("unsupported-config", section)
            continue
        m = _KEY_VALUE.match(s)
        if section is None or m is None or any(c in s for c in b'"\\#;'):
            raise Unknown("unsupported-config")
        key, value = m[1].lower(), m[2]
        if section == b"core":
            if value not in _CORE.get(key, ()):
                raise Unknown("unsupported-config", b"core." + key)
        elif section == b"extensions":
            if key != b"objectformat":
                raise Unknown("unsupported-config", b"extensions." + key)
            if value != b"sha1":
                raise Unknown("unsupported-object-format")
        elif section == b"remote" and key in _PARTIAL_CLONE_KEYS:
            raise Unknown("unsupported-repository-feature", b"remote." + key)


def _oid_line(data, path):
    if len(data) == 41 and data[40:] == b"\n" and HEX40.match(data[:40]):
        return data[:40]
    if len(data) == 65 and data[64:] == b"\n" and HEX64.match(data[:64]):
        raise Unknown("unsupported-object-format", path)
    raise Unknown("malformed-ref", path)


def valid_head_ref(name):
    if not _HEAD_REF.match(name) or b".." in name or b"@{" in name:
        return False
    return all(c and c[:1] != b"." and c[-1:] != b"." and not c.endswith(b".lock") for c in name.split(b"/"))


def parse_head(data):
    """(refname, None) for a branch, (None, oid) when detached."""
    if data[:5] == b"ref: ":
        name = data[5:]
        if name[-1:] != b"\n" or not valid_head_ref(name[:-1]):
            raise Unknown("unsupported-head", b"HEAD")
        return name[:-1], None
    return None, _oid_line(data, b"HEAD")


def parse_packed_refs(data):
    out, peelable = {}, False
    if not data:
        return out
    if data[-1:] != b"\n":
        raise Unknown("malformed-ref", b"packed-refs")
    for i, line in enumerate(data[:-1].split(b"\n")):
        if i == 0 and line.startswith(b"# pack-refs with:"):
            continue
        if line[:1] == b"^":
            if not peelable or not HEX40.match(line[1:]):
                raise Unknown("malformed-ref", b"packed-refs")
            peelable = False
            continue
        oid, sep, ref = line.partition(b" ")
        if not sep or not HEX40.match(oid) or not ref.startswith(b"refs/") or ref in out \
                or not all(0x21 <= c <= 0x7E for c in ref):
            raise Unknown("malformed-ref", b"packed-refs")
        if ref.startswith(b"refs/replace/"):
            raise Unknown("unsupported-repository-feature", b"refs/replace")
        out[ref], peelable = oid, True
    return out


def parse_object(oid, raw, limit):
    """(type, body) of a loose object; its zlib stream is decompressed to at
    most limit bytes and its SHA-1 must equal oid (integrity only)."""
    d = zlib.decompressobj()
    bound = limit + 64
    try:
        out = d.decompress(raw, bound)
    except zlib.error:
        raise Unknown("malformed-object", oid) from None
    if len(out) >= bound:
        raise Unknown("limit-exceeded", oid)
    if not d.eof or d.unconsumed_tail or d.unused_data:
        raise Unknown("malformed-object", oid)
    nul = out.find(b"\0", 0, 32)
    typ, sep, size = out[:max(nul, 0)].partition(b" ")
    if nul < 0 or not sep or typ not in (b"commit", b"tree", b"blob", b"tag") or not size.isdigit() \
            or (size[:1] == b"0" and size != b"0") or int(size) != len(out) - nul - 1:
        raise Unknown("malformed-object", oid)
    if hashlib.sha1(out).hexdigest().encode() != oid:
        raise Unknown("malformed-object", oid)
    return typ, out[nul + 1:]


def parse_commit_tree(oid, typ, body):
    if typ != b"commit" or body[:5] != b"tree " or body[45:46] != b"\n" or not HEX40.match(body[5:45]):
        raise Unknown("malformed-object", oid)
    return body[5:45]


def parse_tree(oid, typ, body):
    """[(name, mode, oid)] in Git's tree order, with every name checked."""
    if typ != b"tree":
        raise Unknown("malformed-object", oid)
    out, pos, prev, names = [], 0, None, set()
    while pos < len(body):
        sp = body.find(b" ", pos, pos + 7)
        nul = body.find(b"\0", sp + 1) if sp >= 0 else -1
        if sp < 0 or nul < 0 or nul + 21 > len(body):
            raise Unknown("malformed-object", oid)
        mode, name = _TREE_MODES.get(body[pos:sp]), body[sp + 1:nul]
        if mode is None or not valid_component(name) or name in names:
            raise Unknown("malformed-object", oid)
        if mode == MODE_GITLINK:
            raise Unknown("submodule", name)
        key = name + b"/" if mode == MODE_DIR else name
        if prev is not None and key <= prev:
            raise Unknown("malformed-object", oid)
        out.append((name, mode, body[nul + 1:nul + 21].hex().encode()))
        names.add(name)
        prev, pos = key, nul + 21
    return out


class Repo:
    """Reads through descriptors of the authorized administrative and common
    directories only."""

    def __init__(self, admin_fd, admin_dev, common_fd, common_dev, limits):
        self.admin, self.admin_dev = admin_fd, admin_dev
        self.common, self.common_dev = common_fd, common_dev
        self.limits = limits

    def read_admin(self, rel, limit=None):
        return fdwalk.read_path(self.admin, rel, limit or self.limits.max_meta, self.admin_dev)

    def read_common(self, rel, limit=None):
        return fdwalk.read_path(self.common, rel, limit or self.limits.max_meta, self.common_dev)

    def check_features(self):
        for rel in COMMON_FEATURES:
            if fdwalk.exists(self.common, rel, self.common_dev) is not None:
                raise Unknown("unsupported-repository-feature", rel)
        for rel in ADMIN_FEATURES:
            if fdwalk.exists(self.admin, rel, self.admin_dev) is not None:
                raise Unknown("unsupported-repository-feature", rel)
        if not fdwalk.dir_is_empty(self.common, b"refs/replace", self.common_dev, self.limits):
            raise Unknown("unsupported-repository-feature", b"refs/replace")

    def snapshot(self):
        """Every repository input to one observation, read once. Returns
        (inputs, head ref, commit oid or None)."""
        config = self.read_common(b"config")
        if config is None:
            raise Unknown("unsupported-repository-feature", b"config")
        parse_config(config)
        self.check_features()
        head = self.read_admin(b"HEAD")
        if head is None:
            raise Unknown("unsupported-head", b"HEAD")
        ref, commit = parse_head(head)
        packed = self.read_common(b"packed-refs")
        refs = parse_packed_refs(packed or b"")
        loose = None
        if ref is not None:
            loose = self.read_common(ref)
            commit = _oid_line(loose, ref) if loose is not None else refs.get(ref)
        index = self.read_admin(b"index", self.limits.max_index)
        inputs = (config, head, packed, loose, index)
        return inputs, ref, commit

    def _has_packs(self):
        return not fdwalk.dir_is_empty(self.common, b"objects/pack", self.common_dev, self.limits)

    def read_object(self, oid):
        raw = self.read_common(b"objects/" + oid[:2] + b"/" + oid[2:], self.limits.max_object)
        if raw is None:
            raise Unknown("object-in-unsupported-pack" if self._has_packs() else "object-missing", oid)
        return parse_object(oid, raw, self.limits.max_object)

    def commit_tree(self, oid):
        return parse_commit_tree(oid, *self.read_object(oid))

    def flatten(self, tree_oid):
        """{path: (mode, oid)} for every non-tree entry reachable from tree_oid."""
        out, count = {}, [0]

        def rec(oid, prefix, depth):
            count[0] += 1
            if depth > self.limits.max_depth or count[0] > self.limits.max_trees:
                raise Unknown("limit-exceeded", oid)
            for name, mode, child in parse_tree(oid, *self.read_object(oid)):
                if mode == MODE_DIR:
                    rec(child, prefix + name + b"/", depth + 1)
                else:
                    out[prefix + name] = (mode, child)
                    if len(out) > self.limits.max_entries:
                        raise Unknown("limit-exceeded", oid)

        rec(tree_oid, b"", 0)
        return out

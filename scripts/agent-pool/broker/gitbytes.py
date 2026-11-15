"""Pure Git byte primitives shared by the read-only observer (`gitraw`) and
PR-3b classification (`gitclass`). Each caller keeps its own error policy:
the observer stops at its first finding, classification records every
finding and continues."""

import hashlib
import re
import zlib

HEX40 = re.compile(rb"[0-9a-f]{40}\Z")
HEX64 = re.compile(rb"[0-9a-f]{64}\Z")
OBJECT_TYPES = (b"commit", b"tree", b"blob", b"tag")
_HFS_IGNORED = re.compile("[\u200c-\u200f\u202a-\u202e\u206a-\u206f\ufeff]")


class Malformed(ValueError):
    """A loose object outside the subset; `reason` is `malformed-object` or
    `limit-exceeded`."""

    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


def is_gitdir_name(c):
    """R2 K3: a `.git` name compared case-insensitively, also with the code
    points HFS+ ignores (Git's `is_hfs_dotgit`), so `.GIT` and `.Git` count."""
    if c.lower() == b".git":
        return True
    try:
        return _HFS_IGNORED.sub("", c.decode("utf-8")).lower() == ".git"
    except UnicodeDecodeError:
        return False


def loose_object(oid, raw, limit):
    """(type, body) of the loose object `oid` (hex str): its zlib stream is
    complete and inflates to under `limit` + 64 bytes, its header is
    `type size\\0` with a canonical decimal size equal to the body length, and
    the SHA-1 of the whole is `oid` (integrity only). Raises Malformed."""
    d = zlib.decompressobj()
    bound = limit + 64
    try:
        out = d.decompress(raw, bound)
    except zlib.error:
        raise Malformed("malformed-object") from None
    if len(out) >= bound:
        raise Malformed("limit-exceeded")
    if not d.eof or d.unconsumed_tail or d.unused_data:
        raise Malformed("malformed-object")
    nul = out.find(b"\0", 0, 32)
    typ, sep, size = out[:max(nul, 0)].partition(b" ")
    if nul < 0 or not sep or typ not in OBJECT_TYPES or not size.isdigit() \
            or (size[:1] == b"0" and size != b"0") or int(size) != len(out) - nul - 1:
        raise Malformed("malformed-object")
    if hashlib.sha1(out).hexdigest() != oid:  # noqa: S324 - Git object ids are SHA-1 (integrity only)
        raise Malformed("malformed-object")
    return typ, out[nul + 1:]

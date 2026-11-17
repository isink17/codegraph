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
HFS_IGNORED = re.compile("[\u200c-\u200f\u202a-\u202e\u206a-\u206f\ufeff]")
_SIZE = re.compile(rb"(?:0|[1-9][0-9]*)\Z")


class Malformed(ValueError):
    """A loose object outside the subset; `reason` is `malformed-object` or
    `limit-exceeded`."""

    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


def is_gitdir_name(c):
    """R2 K3 detection: a `.git` name compared case-insensitively, also with
    the code points HFS+ ignores (Git's `is_hfs_dotgit`), so `.GIT` and
    `.g<U+200C>it` count. Detection only: `gitclass.repositories` accepts
    only the exact `.git` as a repository."""
    if c.lower() == b".git":
        return True
    try:
        return HFS_IGNORED.sub("", c.decode("utf-8")).lower() == ".git"
    except UnicodeDecodeError:
        return False


def loose_object(oid, raw, bound):
    """(type, body) of the loose object `oid` (40 hex bytes): its zlib stream
    is complete and inflates to fewer than `bound` bytes (each caller keeps
    its own bound), its header is `type size\\0` within 32 bytes with a
    canonical decimal size equal to the body length, and the SHA-1 of the
    whole is `oid` (integrity only). Raises Malformed."""
    d = zlib.decompressobj()
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
    if nul < 0 or not sep or typ not in OBJECT_TYPES or not _SIZE.match(size) or int(size) != len(out) - nul - 1:
        raise Malformed("malformed-object")
    if hashlib.sha1(out).hexdigest().encode() != oid:  # noqa: S324 - Git object ids are SHA-1 (integrity only)
        raise Malformed("malformed-object")
    return typ, out[nul + 1:]

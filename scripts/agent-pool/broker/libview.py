"""E-view (r4 §8.5, r3 §3.3): Tier C keeps a leased record without its token,
with lease_salt and lease_verifier in its place; the library sees view(L), the
same record with lease_token=<custody token>. No library change (OD-1)."""

import hashlib
import hmac
from collections import namedtuple

import recovery_state as rs

from .refusal import Code, Refused

FREE = "FREE"
LEASED = "LEASED"
SUSPECT_FREE = "SUSPECT_FREE"
SUSPECT_LEASED = "SUSPECT_LEASED"
QUARANTINED = "QUARANTINED"
UNKNOWN = "UNKNOWN"

L = namedtuple("L", "kind g lid op digest")
_PLACEHOLDER = "0" * 32
_HIDDEN = ("lease_salt=", "lease_verifier=", "suspect=")


def verifier(salt, token):
    return hmac.new(bytes.fromhex(salt), token.encode(), hashlib.sha256).hexdigest()


def _fields(text):
    lines = text.split("\n")
    if lines[-1] != "":
        raise Refused(Code.STATE_UNKNOWN, "Tier C record")
    out = {}
    for line in lines[:-1]:
        k, sep, v = line.partition("=")
        if not sep or k in out:
            raise Refused(Code.STATE_UNKNOWN, "Tier C record")
        out[k] = v
    return lines[:-1], out


def _structural_view(text, token):
    lines, f = _fields(text)
    if ("lease_salt" in f) != ("lease_verifier" in f):
        raise Refused(Code.STATE_UNKNOWN, "Tier C record")
    out = []
    for line in lines:
        if line.startswith("lease_salt="):
            out.append(f"lease_token={token}")
        elif not line.startswith(_HIDDEN):
            out.append(line)
    return "\n".join(out) + "\n", f


def component(text, slot):
    """L from Tier C bytes (BROKER_RECORD; decisive only through the anchor)."""
    if text is None:
        return L(UNKNOWN, None, None, None, None)
    digest = hashlib.sha256(text.encode("ascii", "replace")).hexdigest()
    try:
        view_text, f = _structural_view(text, _PLACEHOLDER)
        suspect = f.get("suspect")
        if suspect not in (None, "1") or (f.get("state") == "quarantined" and suspect):
            raise Refused(Code.STATE_UNKNOWN)
        if f.get("state") == "leased" and not (rs.HEX32.match(f.get("lease_salt", ""))
                                              and rs.HEX64.match(f.get("lease_verifier", ""))):
            raise Refused(Code.STATE_UNKNOWN)
        if f.get("state") != "leased" and "lease_salt" in f:
            raise Refused(Code.STATE_UNKNOWN)
        rec = rs.parse_record(view_text, slot)
        if rec.kind != "v2":
            raise Refused(Code.STATE_UNKNOWN)
    except (Refused, rs.Refused):
        return L(UNKNOWN, None, None, None, digest)
    if rec.state == "free":
        return L(SUSPECT_FREE if suspect else FREE, rec.generation, None, None, digest)
    if rec.state == "leased":
        return L(SUSPECT_LEASED if suspect else LEASED, rec.generation, rec.identity, None, digest)
    return L(QUARANTINED, rec.generation, rec.identity, rec.recovery_op, digest)


def view(text, slot, token):
    """view(L): verifies HMAC(lease_salt, token) == lease_verifier first."""
    if not isinstance(token, str) or not rs.HEX32.match(token):
        raise Refused(Code.CUSTODY_MISMATCH)
    v, f = _structural_view(text, token)
    if f.get("state") != "leased" or not hmac.compare_digest(verifier(f["lease_salt"], token),
                                                               f["lease_verifier"]):
        raise Refused(Code.CUSTODY_MISMATCH)
    rs.parse_record(v, slot)
    return v


def tierc_from_view(view_text, salt, suspect=False):
    """The Tier C bytes for a leased library record (acquire, migration)."""
    out = []
    for line in view_text.split("\n")[:-1]:
        if line.startswith("lease_token="):
            token = line.partition("=")[2]
            out += [f"lease_salt={salt}", f"lease_verifier={verifier(salt, token)}"]
        else:
            out.append(line)
    if suspect:
        out.append("suspect=1")
    return "\n".join(out) + "\n"


def token_free_image(text):
    """sha256 of the Tier C bytes minus salt, verifier and suspect lines; equals
    recovery_state.image_digest(view(L))."""
    return hashlib.sha256("\n".join(line for line in text.split("\n")
                                    if not line.startswith(_HIDDEN)).encode()).hexdigest()


def with_suspect(text, on):
    lines = [line for line in text.split("\n")[:-1] if line != "suspect=1"]
    return "\n".join(lines + (["suspect=1"] if on else [])) + "\n"

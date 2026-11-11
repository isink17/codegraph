"""Request parser (r3 §7.1) and strict canonical document parser parse_doc
(r4 §11 C8). Nothing is normalized: a value that does not match refuses."""

import json
import re

from . import canonical, opclass
from .refusal import Code, Refused

MAX_REQUEST = 4096
MAX_DOC = 65536
PROTOCOL_VERSION = 1

NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\Z")
TEXT = re.compile(r"^[\x20-\x7e]{1,200}\Z")
QUIESCENCE_METHODS = ("containment-teardown", "owner-quiescence", "reboot-runbook")
QUIESCENCE_ACCOUNTED = ("old_coordinators", "wrappers", "escaped_descendants", "remote_writers",
                        "automatic_restarts")
PRESERVATION_DISPOSITIONS = ("preserve-all",)
ATTEST_KEYS = ("observed", "note", "ticket", "drift", "host", "process", "cause", "contact")

# Never accepted from a request (r3 §7.1 rule 6; R5-1 §R4.3 adds repair_plan).
NOT_ALLOWED = frozenset((
    "trust", "actor", "actor_uid", "session", "nonce", "nonce_digest", "issued_at", "not_after", "class",
    "repair_plan", "token", "lease_token", "recovery_token", "path", "executable", "environment", "env",
    "config", "cwd", "store", "root", "confirmed_at", "entry",
))


def _enum(values):
    return lambda v: type(v) is str and v in values


def _name(v):
    return type(v) is str and bool(NAME.match(v))


def _text(v):
    return type(v) is str and bool(TEXT.match(v))


def _bool(v):
    return type(v) is bool


def _attest(v):
    return (type(v) is dict and 0 < len(v) <= 8 and all(k in ATTEST_KEYS for k in v)
            and all(_text(x) for x in v.values()))


_BASE = {"slot": _name, "reason": _text}
_OP = dict(_BASE, op_id=_name)
_ACCOUNTED = {f"accounted_{c}": _bool for c in QUIESCENCE_ACCOUNTED}

# verb -> (required fields, optional fields); "v" and "verb" are implicit.
FIELDS = {
    "status": ({}, {"slot": _name}),
    "inspect": ({"slot": _name}, {}),
    "reconcile-readonly": ({"slot": _name, "op_id": _name}, {}),
    "stop": ({"reason": _text}, {}),
    "resume": ({"reason": _text}, {}),
    "mark-suspect": (_BASE, {"attest": _attest}),
    "clear-suspect": (_BASE, {"attest": _attest}),
    "acquire": (dict(_BASE, task=_text), {}),
    "release": (_BASE, {}),
    "adopt": (_OP, {}),
    "request": (_OP, {}),
    "cancel": (_OP, {}),
    "freeze-start": (_OP, {}),
    "freeze-continue": (_OP, {}),
    "thaw": (_OP, {}),
    "quarantine": (dict(_OP, quiescence_method=_enum(QUIESCENCE_METHODS),
                        preservation_disposition=_enum(PRESERVATION_DISPOSITIONS)), {}),
    "quiescence": (dict(_OP, method=_enum(QUIESCENCE_METHODS), **_ACCOUNTED), {}),
    "snapshot": (_OP, {}),
    "verify-preservation": (_OP, {}),
    "preservation": (_OP, {}),
    "ready": (_OP, {}),
    "resume-step": (_BASE, {"op_id": _name}),
    "custody-destroy": (_OP, {}),
}


class _Duplicate(Exception):
    pass


class _Number(Exception):
    pass


def _pairs(pairs):
    out = {}
    for k, v in pairs:
        if k in out:
            raise _Duplicate()
        out[k] = v
    return out


def _no_float(_):
    raise _Number()


def _int(s):
    if len(s.lstrip("-")) > 19:
        raise _Number()
    return int(s)


def _decoder():
    return json.JSONDecoder(object_pairs_hook=_pairs, parse_constant=_no_float, parse_float=_no_float,
                            parse_int=_int)


def _decode(text):
    try:
        return _decoder().raw_decode(text)
    except _Duplicate:
        raise Refused(Code.DUPLICATE_KEY) from None
    except _Number:
        raise Refused(Code.FIELD_INVALID, "number outside the protocol") from None
    except Exception:
        raise Refused(Code.ENCODING, "not one JSON value") from None


def parse_request(raw, entry):
    """Returns (op, fields) for one request. `raw` is bytes."""
    if type(raw) is not bytes:
        raise Refused(Code.ENCODING)
    if len(raw) > MAX_REQUEST:
        raise Refused(Code.OVERSIZE)
    if b"\x00" in raw or b"\\" in raw or raw.startswith(b"\xef\xbb\xbf"):
        raise Refused(Code.ENCODING)
    try:
        text = raw.decode("utf-8", "strict")
    except UnicodeDecodeError:
        raise Refused(Code.ENCODING) from None
    obj, end = _decode(text)
    if text[end:] not in ("", "\n"):
        raise Refused(Code.ENCODING, "trailing data")
    if type(obj) is not dict:
        raise Refused(Code.FIELD_INVALID, "not an object")
    if obj.get("v") != PROTOCOL_VERSION or type(obj.get("v")) is not int:
        raise Refused(Code.FIELD_INVALID, "protocol version")
    verb = obj.get("verb")
    if type(verb) is not str:
        raise Refused(Code.FIELD_INVALID, "verb")
    op = opclass.lookup(entry, verb)
    required, optional = FIELDS[op.verb]
    fields = {k: v for k, v in obj.items() if k not in ("v", "verb")}
    if any(k in NOT_ALLOWED for k in fields):
        raise Refused(Code.FIELD_NOT_ALLOWED)
    if any(k not in required and k not in optional for k in fields):
        raise Refused(Code.FIELD_UNKNOWN)
    for k, check in list(required.items()) + list(optional.items()):
        if k not in fields:
            if k in required:
                raise Refused(Code.FIELD_INVALID, f"missing {k}")
            continue
        if not check(fields[k]):
            raise Refused(Code.FIELD_INVALID, k)
    return op, fields


def parse_doc(raw):
    """Strict parser for stored and displayed documents: the bytes must be
    exactly CANON of an object in D, so CANON(parse_doc(b)) == b."""
    if type(raw) is not bytes:
        raise Refused(Code.ENCODING)
    if len(raw) > MAX_DOC:
        raise Refused(Code.OVERSIZE)
    if any(not 0x20 <= c <= 0x7E for c in raw):
        raise Refused(Code.ENCODING)
    obj, end = _decode(raw.decode("ascii"))
    if end != len(raw) or type(obj) is not dict:
        raise Refused(Code.FIELD_INVALID, "not one object")
    try:
        if canonical.canon(obj) != raw:
            raise Refused(Code.FIELD_INVALID, "not canonical")
    except canonical.DomainError:
        raise Refused(Code.FIELD_INVALID, "outside D") from None
    return obj

"""Domain D and CANON (r3 §7.2, r4 §4.2). sha256(CANON(x)) equals
recovery_state.digest(x) for every x in D; no Unicode normalization exists."""

import hashlib
import json
import re

KEY = re.compile(r"^[a-z][a-z0-9_]{0,63}\Z")
SECRET_KEY = re.compile(r"token|secret|password|verifier|salt", re.I)
HEX32 = re.compile(r"^[0-9a-f]{32}\Z")
HEX64 = re.compile(r"^[0-9a-f]{64}\Z")
MAX_INT = 2 ** 53 - 1
MAX_DEPTH = 16


class DomainError(ValueError):
    pass


def check(obj, depth=0):
    """Raises DomainError unless obj is in D, nested at most MAX_DEPTH deep."""
    if depth > MAX_DEPTH:
        raise DomainError("nesting too deep")
    t = type(obj)
    if t is dict:
        for k, v in obj.items():
            if type(k) is not str or not KEY.match(k):
                raise DomainError("key outside D")
            check(v, depth + 1)
    elif t is list:
        for v in obj:
            check(v, depth + 1)
    elif t is int:
        if not -MAX_INT <= obj <= MAX_INT:
            raise DomainError("integer outside D")
    elif t is bool:
        pass
    elif t is str:
        if any(not 0x20 <= ord(c) <= 0x7E for c in obj):
            raise DomainError("string outside printable ASCII")
    else:
        raise DomainError("type outside D")


def canon(obj):
    check(obj)
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True,
                      allow_nan=False).encode("ascii")


def digest(obj):
    return hashlib.sha256(canon(obj)).hexdigest()


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def secret_free(obj):
    """True when no key at any depth matches SECRET_KEY (the library's _no_secrets)."""
    if type(obj) is dict:
        return all(not SECRET_KEY.search(k) and secret_free(v) for k, v in obj.items())
    if type(obj) is list:
        return all(secret_free(v) for v in obj)
    return True


def serialize(obj):
    """Deterministic bytes for store snapshots (not a document; no domain check)."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode("ascii")

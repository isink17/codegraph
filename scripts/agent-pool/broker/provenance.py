"""Provenance types and rules (r3 §5, r4 §7). Decisive provenance has no public
constructor here: producers in PR-2..PR-4 own it (LB3), and an AST test keeps
every other module away from _produce."""

import enum

from . import canonical
from .refusal import Code, Refused


class Provenance(str, enum.Enum):
    AGENT_REPORT = "AGENT_REPORT"
    REMOTE_GIT = "REMOTE_GIT"
    ISOLATED_VALIDATION = "ISOLATED_VALIDATION"
    BROKER_RECORD = "BROKER_RECORD"
    PROTECTED_RECORD = "PROTECTED_RECORD"
    ROOT_OBSERVED = "ROOT_OBSERVED"
    ROOT_REVERIFIED = "ROOT_REVERIFIED"
    OWNER_DECISION = "OWNER_DECISION"
    TRUSTED_OBSERVER = "TRUSTED_OBSERVER"


DECISIVE = frozenset((Provenance.PROTECTED_RECORD, Provenance.ROOT_OBSERVED, Provenance.ROOT_REVERIFIED,
                      Provenance.OWNER_DECISION))
# Weakest first. TRUSTED_OBSERVER has no producer and decides nothing in PR-1.
_RANK = {Provenance.AGENT_REPORT: 0, Provenance.REMOTE_GIT: 0, Provenance.ISOLATED_VALIDATION: 1,
         Provenance.BROKER_RECORD: 1, Provenance.TRUSTED_OBSERVER: 1, Provenance.PROTECTED_RECORD: 2,
         Provenance.ROOT_OBSERVED: 2, Provenance.ROOT_REVERIFIED: 2, Provenance.OWNER_DECISION: 2}
OWNER_DECISION_FIELDS = frozenset(("reason", "quiescence_method", "preservation_disposition", "method",
                                   "accounted", "confirmation"))
_SEAL = object()


class Component:
    __slots__ = ("name", "provenance", "role", "sha256")

    def __init__(self, name, provenance, role, sha256, _seal=None):
        if provenance in DECISIVE and _seal is not _SEAL:
            raise TypeError("decisive provenance is set only by its producer")
        if role not in ("decisive", "supporting") or (role == "decisive" and provenance not in DECISIVE):
            raise Refused(Code.PROVENANCE_INSUFFICIENT)
        if type(name) is not str or not canonical.KEY.match(name) or not canonical.HEX64.match(sha256 or ""):
            raise Refused(Code.FIELD_INVALID, "evidence component")
        object.__setattr__(self, "name", name)
        object.__setattr__(self, "provenance", Provenance(provenance))
        object.__setattr__(self, "role", role)
        object.__setattr__(self, "sha256", sha256)

    def __setattr__(self, *_):
        raise AttributeError("components are immutable")

    def as_dict(self):
        return {"name": self.name, "provenance": self.provenance.value, "role": self.role, "sha256": self.sha256}


def supporting(name, provenance, sha256):
    """The only public constructor: a non-decisive component."""
    if provenance in DECISIVE:
        raise TypeError("decisive provenance is set only by its producer")
    return Component(name, provenance, "supporting", sha256)


def _produce(name, provenance, role, sha256):
    """Producer-only constructor (PR-2..PR-4 producers; _testsupport in PR-1)."""
    return Component(name, provenance, role, sha256, _seal=_SEAL)


def weakest(provenances):
    provs = [Provenance(p) for p in provenances]
    if not provs:
        raise ValueError("no inputs")
    return min(provs, key=lambda p: (_RANK[p], p not in DECISIVE))


def derived(name, sha256, inputs):
    """A value computed from several components keeps the weakest input
    provenance; transformation never upgrades it."""
    prov = weakest([c.provenance for c in inputs])
    if prov in DECISIVE:
        return Component(name, prov, "decisive", sha256, _seal=_SEAL)
    return supporting(name, prov, sha256)


def reverified(name, recomputed_sha256, copied_by_root, expected):
    """ROOT_REVERIFIED only when root recomputed the digest from its own copy and
    the expected digest is decisive and equal (r4 §7.1). Otherwise the value is
    not upgraded: it stays supporting with the copy's provenance."""
    if (copied_by_root and expected.provenance in DECISIVE and expected.role == "decisive"
            and canonical.HEX64.match(recomputed_sha256 or "") and recomputed_sha256 == expected.sha256):
        return Component(name, Provenance.ROOT_REVERIFIED, "decisive", recomputed_sha256, _seal=_SEAL)
    return supporting(name, Provenance.ISOLATED_VALIDATION, recomputed_sha256)


def evidence(components):
    """The typed evidence object (r3 §5.3); duplicate names refuse."""
    names = [c.name for c in components]
    if len(set(names)) != len(names):
        raise Refused(Code.FIELD_INVALID, "duplicate evidence component")
    return {"v": 1, "components": [c.as_dict() for c in sorted(components, key=lambda c: c.name)]}


def require_decisive(components, names):
    """Every named component must be present, decisive and of decisive provenance."""
    by_name = {c.name: c for c in components}
    for n in names:
        c = by_name.get(n)
        if c is None or c.role != "decisive" or c.provenance not in DECISIVE:
            raise Refused(Code.PROVENANCE_INSUFFICIENT, n)


def owner_decision_fields(doc):
    """Only these fields of a burned document are OWNER_DECISION (r4 §7.3)."""
    return {k: doc[k] for k in OWNER_DECISION_FIELDS if k in doc}

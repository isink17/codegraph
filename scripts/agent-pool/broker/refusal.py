"""Closed refusal enum and outcome classes (r3 §8, r4 §9, A1 §A5)."""

import enum


class Code(str, enum.Enum):
    UNKNOWN_OPERATION = "UNKNOWN_OPERATION"
    DELEGATION_DISABLED = "DELEGATION_DISABLED"
    OWNER_REQUIRED = "OWNER_REQUIRED"
    FIELD_UNKNOWN = "FIELD_UNKNOWN"
    FIELD_NOT_ALLOWED = "FIELD_NOT_ALLOWED"
    FIELD_INVALID = "FIELD_INVALID"
    OVERSIZE = "OVERSIZE"
    ENCODING = "ENCODING"
    DUPLICATE_KEY = "DUPLICATE_KEY"
    STOPPED = "STOPPED"
    GATE_CLOSED = "GATE_CLOSED"
    GATE_ADMIN = "GATE_ADMIN"
    STATE_UNKNOWN = "STATE_UNKNOWN"
    PENDING_INTENT = "PENDING_INTENT"
    ORIGINAL_AUTHZ_UNAVAILABLE = "ORIGINAL_AUTHZ_UNAVAILABLE"
    REPLAY = "REPLAY"
    EXPIRED = "EXPIRED"
    LEDGER_UNAVAILABLE = "LEDGER_UNAVAILABLE"
    LEDGER_FULL = "LEDGER_FULL"
    CUSTODY_CONFLICT = "CUSTODY_CONFLICT"
    CUSTODY_CORRUPT = "CUSTODY_CORRUPT"
    CUSTODY_MISMATCH = "CUSTODY_MISMATCH"
    CUSTODY_ABSENT = "CUSTODY_ABSENT"
    PROVENANCE_INSUFFICIENT = "PROVENANCE_INSUFFICIENT"
    NOT_HOLDER = "NOT_HOLDER"
    NOT_CLEAN = "NOT_CLEAN"
    NOT_PUSHED = "NOT_PUSHED"
    IDENTITY_CHANGED = "IDENTITY_CHANGED"
    THAW_AFTER_QUARANTINE = "THAW_AFTER_QUARANTINE"
    QUIESCENCE_NOT_ESTABLISHED = "QUIESCENCE_NOT_ESTABLISHED"
    ONE_WAY_NOT_ACCEPTED = "ONE_WAY_NOT_ACCEPTED"
    CONFIRMATION_FAILED = "CONFIRMATION_FAILED"
    SESSION_INVALID = "SESSION_INVALID"
    BUSY = "BUSY"
    RLIMIT = "RLIMIT"
    MIRROR_UNAVAILABLE = "MIRROR_UNAVAILABLE"
    TRUST_INSUFFICIENT = "TRUST_INSUFFICIENT"
    ANCHOR_MISMATCH = "ANCHOR_MISMATCH"
    VERB_NOT_ALLOWED_IN_STATE = "VERB_NOT_ALLOWED_IN_STATE"
    CONTEXT_CHANGED = "CONTEXT_CHANGED"


class Refused(Exception):
    """A refusal. Raised only before the first write (phases V, P, C, R), so
    every store is byte-identical afterwards. `detail` holds only broker
    constants and escaped values."""

    def __init__(self, code, detail=""):
        if not isinstance(code, Code):
            raise TypeError("refusal code must be a Code")
        super().__init__(f"{code.value}: {detail}" if detail else code.value)
        self.code = code
        self.detail = detail


class Outcome(str, enum.Enum):
    COMMITTED = "COMMITTED"
    ALREADY_COMPLETE = "ALREADY_COMPLETE"
    REFUSED = "REFUSED"
    UNFINISHED = "UNFINISHED"


class Result:
    """The outcome of one owner invocation. `durable_ref` (UNFINISHED only)
    lists the completed W items with their digests, never a token."""

    __slots__ = ("outcome", "code", "durable_ref", "detail")

    def __init__(self, outcome, code=None, durable_ref=(), detail=""):
        if (outcome is Outcome.REFUSED) != (code is not None):
            raise ValueError("a code is given exactly for a refusal")
        if durable_ref and outcome is not Outcome.UNFINISHED:
            raise ValueError("only an unfinished outcome carries durable references")
        self.outcome, self.code, self.durable_ref, self.detail = outcome, code, tuple(durable_ref), detail

    def __repr__(self):
        return f"Result({self.outcome.value}, {self.code.value if self.code else None}, {list(self.durable_ref)})"

"""Principals, trust derivation, authorization documents and the confirmation
and revalidation steps (r4 §4, §5.3; A1 §A1.6, §A2.1; R5-1 §R4.3).

trust is always derived, never input. OwnerCapability has no public
constructor: the session verifier (PR-4) creates it, and in PR-1 only
_testsupport/capability.py does. SyntheticOwner is the capability-less
context of a synthetic store (the accident guard of recovery_cli), never
valid on the protected store."""

import hmac
import time

from . import canonical, protocol, sanitize
from .authz_txn import _issue
from .refusal import Code, Refused

TRUST_OWNER = "owner-authenticated"
TRUST_SYNTHETIC = "same-uid-attestation"
TRUST_VALUES = (TRUST_OWNER, TRUST_SYNTHETIC)
SYNTHETIC = "synthetic"
PROTECTED = "protected"
MAX_WINDOW = 600

LIBRARY_DOCS = ("request", "cancel", "quarantine", "quiescence", "preservation", "ready")
BINDING_KEYS = ("slot", "op_id", "transition", "identity_kind", "identity", "slot_generation", "seq")
HEADER_KEYS = ("v", "doc", "entry", "verb", "class", "trust", "actor", "actor_uid", "session", "manifest_digest",
               "config_digest", "anchor_digest", "tierp_state_digest", "evidence", "reason", "utc", "issued_at",
               "not_after", "nonce_digest", "repair_plan")
SESSION_KEYS = ("tty_dev", "tty_ino", "session_leader_pid", "login_time")
RESUME_MODES = ("library_pending", "complete_terminal", "anchor_pending", "f_intent")
_SEAL = object()


class OwnerCapability:
    __slots__ = ("session", "_valid")

    def __init__(self, *_a, **_k):
        raise TypeError("an OwnerCapability is created only by the session verifier")

    def session_valid(self):
        return self._valid()


def _new_capability(seal, session, valid=lambda: True):
    if seal is not _SEAL:
        raise TypeError("an OwnerCapability is created only by the session verifier")
    cap = object.__new__(OwnerCapability)
    cap.session, cap._valid = dict(session), valid
    return cap


class SyntheticOwner:
    """The owner of a synthetic store: an accident guard, not authentication."""

    __slots__ = ("session", "_valid")

    def __init__(self, session=None, valid=lambda: True):
        self.session = dict(session or {k: 0 for k in SESSION_KEYS})
        self._valid = valid

    def session_valid(self):
        return self._valid()


class Ctx:
    __slots__ = ("principal", "store", "actor", "actor_uid", "manifest_digest", "config_digest")

    def __init__(self, principal, store, actor="owner", actor_uid=0, manifest_digest="0" * 64,
                 config_digest="0" * 64):
        if store not in (SYNTHETIC, PROTECTED):
            raise ValueError("store kind")
        self.principal, self.store = principal, store
        self.actor, self.actor_uid = actor, actor_uid
        self.manifest_digest, self.config_digest = manifest_digest, config_digest


def is_owner(ctx):
    """The principal check of opclass: an OwnerCapability, or the owner of a synthetic store."""
    return isinstance(ctx.principal, OwnerCapability) or (
        isinstance(ctx.principal, SyntheticOwner) and ctx.store == SYNTHETIC)


def may_retrieve(principal, store_kind):
    """custody.retrieve: a capability for the protected store, the synthetic
    owner only for a synthetic store."""
    if store_kind == PROTECTED:
        return isinstance(principal, OwnerCapability)
    return isinstance(principal, (OwnerCapability, SyntheticOwner))


def trust_of(ctx):
    """r4 §4.1. Any other combination refuses before a document is built."""
    if isinstance(ctx.principal, OwnerCapability) and ctx.store == PROTECTED:
        return TRUST_OWNER
    if isinstance(ctx.principal, SyntheticOwner) and ctx.store == SYNTHETIC:
        return TRUST_SYNTHETIC
    raise Refused(Code.TRUST_INSUFFICIENT)


def utc(seconds):
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(seconds))


def binding_digest(doc):
    """The ledger's deterministic binding key: verb, slot, op_id, generation."""
    return canonical.digest({k: doc[k] for k in ("verb", "slot", "op_id", "slot_generation") if k in doc})


def header(ctx, op, *, reason, evidence, anchor_digest, tierp_state_digest, nonce_digest, issued_at, window,
           repair_plan, doc):
    if not 0 < window <= MAX_WINDOW:
        raise ValueError("confirmation window")
    return {"v": 1, "doc": doc, "entry": op.entry, "verb": op.verb, "class": op.cls, "trust": trust_of(ctx),
            "actor": ctx.actor, "actor_uid": ctx.actor_uid,
            "session": {k: ctx.principal.session[k] for k in SESSION_KEYS},
            "manifest_digest": ctx.manifest_digest, "config_digest": ctx.config_digest,
            "anchor_digest": anchor_digest, "tierp_state_digest": tierp_state_digest, "evidence": evidence,
            "reason": reason, "utc": utc(issued_at), "issued_at": issued_at, "not_after": issued_at + window,
            "nonce_digest": nonce_digest, "repair_plan": list(repair_plan)}


def finish(doc):
    """Canonical bytes of a complete document; refuses anything outside D or
    carrying a secret-looking key."""
    try:
        data = canonical.canon(doc)
    except canonical.DomainError:
        raise Refused(Code.FIELD_INVALID, "document outside D") from None
    if not canonical.secret_free(doc):
        raise Refused(Code.FIELD_INVALID, "secret-looking key")
    if protocol.parse_doc(data) != doc:
        raise Refused(Code.FIELD_INVALID, "document does not round-trip")
    validate(doc)
    return data


def validate(doc):
    """Schema check of a built or stored document (r4 §4.3)."""
    if any(k not in doc for k in HEADER_KEYS) or doc.get("v") != 1 or doc.get("trust") not in TRUST_VALUES:
        raise Refused(Code.FIELD_INVALID, "document header")
    if doc["doc"] in LIBRARY_DOCS:
        if any(k not in doc for k in BINDING_KEYS) or doc["transition"] != doc["doc"]:
            raise Refused(Code.FIELD_INVALID, "library binding")
        if any(type(doc[k]) is not int for k in ("slot_generation", "seq")):
            raise Refused(Code.FIELD_INVALID, "library binding")
    if doc["doc"] == "resume-step" and (doc.get("mode") not in RESUME_MODES or "target_digest" not in doc):
        raise Refused(Code.FIELD_INVALID, "resume document")
    for item in doc["repair_plan"]:
        if sorted(item) != ["action", "body_sha256", "class", "evidence_pending", "len", "log", "offset", "sha256"]:
            raise Refused(Code.FIELD_INVALID, "repair plan")


def confirm(doc_bytes, nonce, confirmer, principal):
    """Phase C: display from the bytes, read the typed nonce, check the session."""
    text, digest = sanitize.render(doc_bytes)
    typed = confirmer(text, f"NONCE {nonce}")
    if type(typed) is not str or not hmac.compare_digest(typed.encode("ascii", "replace"),
                                                          nonce.encode("ascii")):
        raise Refused(Code.CONFIRMATION_FAILED)
    if not principal.session_valid():
        raise Refused(Code.SESSION_INVALID)
    return digest


def revalidate(doc, doc_digest, ctx_digest, ctx_digest_now, confirmed_at, burned, principal):
    """Phase R: the last refusal point. Returns the single-use CommitTxn."""
    if ctx_digest_now != ctx_digest:
        raise Refused(Code.CONTEXT_CHANGED)
    if not doc["issued_at"] <= confirmed_at <= doc["not_after"]:
        raise Refused(Code.EXPIRED)
    if doc_digest in burned or doc["nonce_digest"] in burned:
        raise Refused(Code.REPLAY)
    if not principal.session_valid():
        raise Refused(Code.SESSION_INVALID)
    return _issue(doc_digest, ctx_digest)

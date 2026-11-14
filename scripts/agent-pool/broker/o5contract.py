"""The PR-3a / PR-3b boundary of the O5 containment protocol (PR-3a contract R2
§7 as amended by R3, R4 and R4.1): constants, identities, the sealed boundary
types, the wiring and the shared cell vocabulary. Seals are an accident guard,
never authority (I14): every object is recomputed and compared in the phase
that uses it."""

from collections import namedtuple

from . import authz, canonical

SYNTHETIC = "SYNTHETIC"
PROTECTED = "PROTECTED"
FRESH = 600

CUTOVER_SUBJECT = "o5-cutover"
CUTOVER_VERBS = ("model-b-cutover", "model-b-rebaseline", "model-b-revoke")
CUTOVER_KIND = {"model-b-cutover": "cutover", "model-b-rebaseline": "rebaseline", "model-b-revoke": "revoked"}
MAX_CUTOVER_SEQ = 1024
REVOKE_SEQ = 1023
MODEL_A_POOL = ("codegraph-worker-a", "codegraph-worker-b", "codegraph-review")
OWNER_PRINCIPAL = {"name": "ivan", "uid": 501}
ZERO64 = "0" * 64

S_CUTOVER = "codegraph.o5.cutover/v1"
S_HEAD = "codegraph.o5.cutover-head/v1"
S_ENUM = "codegraph.o5.slot-enumeration/v1"
S_BARRIER = "codegraph.o5.barrier-record/v1"
S_EVIDENCE = "codegraph.o5.evidence/v1"

O5_VERBS = ("freeze-start", "freeze-continue", "thaw", "quarantine", "quiescence", "snapshot",
            "verify-preservation", "preservation", "ready")
F_VERBS = ("freeze-start", "freeze-continue", "thaw", "snapshot", "verify-preservation")
O5_ROWS = ("P7", "P7a", "P8", "P9", "P10", "P10a", "P11", "P12", "P13", "P14", "P15", "P16", "P17", "P18")

# Evidence kinds (R2 §3.6, R3 §3.3, R4 §4.1). TMB and WX kinds are pool-scoped.
TMB_KINDS = ("agent_identity_root", "principals", "no_agent_as_owner", "root_channels")
WX_KINDS = ("uid_process_scan", "holder_scan", "agent_channels", "enforcement_observation")
QUIES_KINDS = ("uid_process_scan_1", "uid_process_scan_2", "holder_scan_1", "holder_scan_2", "session_scan",
               "restart_sources", "tree_vector_1", "tree_vector_2")
H_EST_KINDS = ("enforcement_record", "holder_scan_1", "holder_scan_2", "uid_process_scan", "agent_channels",
               "setuid_writer_scan", "mounts_external", "claimed_set_observation", "writer_capabilities",
               "artifact_isolation", "source_identity", "ep_manifest", "journal_head", "library_record")
H_VAL_KINDS = ("superseded", "claimed_set_observation", "artifact_isolation", "holder_scan_protected",
               "protected_mounts", "source_identity", "ep_manifest", "journal_head", "library_record")

# §7.9 decision cells: (verb, row, f or resume mode) -> (cell, basis, required input kinds).
DECISION_CELLS = {
    "D-THAW": ("thaw", "thaw", ("ep_manifest", "journal_head", "original_blobs")),
    "D-THAW-R": ("resume-step", "thaw", ("ep_manifest", "journal_head", "original_blobs", "f_intent")),
    "D-QUAR": ("quarantine", "barrier", ("ep_manifest", "journal_head")),
    "D-QUIES": ("quiescence", "barrier", ("journal_head",)),
    "D-SNAP": ("snapshot", "barrier_or_handoff", ("ep_manifest", "journal_head", "source_identity")),
    "D-SNAP-R": ("resume-step", "barrier_or_handoff", ("ep_manifest", "journal_head", "source_identity",
                                                      "f_intent")),
    "D-VERIFY": ("verify-preservation", "barrier_or_handoff", ("ep_manifest", "fs_manifest", "journal_head")),
    "D-VERIFY-R": ("resume-step", "barrier_or_handoff", ("ep_manifest", "fs_manifest", "journal_head", "f_intent")),
    "D-PRES": ("preservation", "handoff", ("ep_manifest", "fs_manifest_verified", "journal_head")),
    "D-PRES-R": ("resume-step", "handoff", ("fs_manifest_verified", "original_doc", "library_pending")),
    "D-READY": ("ready", "handoff", ("fs_manifest_verified", "final_observation", "library_record", "tierc")),
    "D-READY-R": ("resume-step", "handoff", ("fs_manifest_verified", "final_observation", "original_doc",
                                            "library_pending", "tierc")),
    "D-READY-T": ("resume-step", "handoff", ("fs_manifest_verified", "final_observation", "library_record",
                                            "ready_doc", "tierc")),
}
VERDICT_CELLS = ("H-EST", "H-VAL")
DISCHARGE_CELLS = {"D3": "DIS-D3", "D4": "DIS-D4", "D3H": "DIS-D3H", "D4H": "DIS-D4H"}
READY_CELLS = {"ready": "D-READY", "library_pending": "D-READY-R", "complete_terminal": "D-READY-T"}


def h_val_kinds(f):
    """R4 §4.2 H-VAL: fs manifests from SNAPSHOTTED / VERIFIED onward."""
    extra = {"SNAPSHOTTED": ("fs_manifest",), "VERIFY_INTENT": ("fs_manifest",), "VERIFY_FAILED": ("fs_manifest",),
             "VERIFIED": ("fs_manifest", "fs_manifest_verified")}.get(f, ())
    return H_VAL_KINDS + extra


def discharge_kinds(kind, f):
    base = h_val_kinds(f) + ("holder_record",)
    return base + (("ready_doc", "preservation_doc") if kind in ("D4", "D4H") else ())


def mode(world):
    """R2 §3.2: SYNTHETIC iff the trust is the synthetic attestation."""
    return SYNTHETIC if authz.trust_of(world.ctx) == authz.TRUST_SYNTHETIC else PROTECTED


def cycle_id(slot, g_pre, op_id, freeze_start_auth):
    return canonical.digest({"domain": "codegraph.o5.cycle/v2", "slot": slot, "g_pre": g_pre, "op_id": op_id,
                             "freeze_start_auth": freeze_start_auth})


def barrier_id(auth):
    return canonical.digest({"domain": "codegraph.o5.barrier/v1", "auth": auth})


# ------------------------------------------------------------------ sealed boundary types

_CSEAL = object()
_PSEAL = object()
_UNBOUND = ("phase", "checked_at")


class _Sealed:
    __slots__ = ("_f",)
    FIELDS = ()
    SEAL = None

    def __init__(self, seal, **fields):
        if seal is not self._seal():
            raise TypeError(f"{type(self).__name__} is created only by its producer")
        if set(fields) != set(self.FIELDS):
            raise TypeError(f"{type(self).__name__} fields")
        object.__setattr__(self, "_f", dict(fields))

    @classmethod
    def _seal(cls):
        return _CSEAL if cls.SEAL == "C" else _PSEAL

    def __setattr__(self, k, v):
        raise AttributeError("sealed")

    def __getattr__(self, k):
        f = object.__getattribute__(self, "_f")
        if k in f:
            return f[k]
        raise AttributeError(k)

    def bound(self):
        return {k: v for k, v in self._f.items() if k not in _UNBOUND and v is not None}

    @property
    def digest(self):
        return canonical.digest(self.bound())


class PreBasis(_Sealed):
    __slots__ = ()
    SEAL = "C"
    FIELDS = ("mode", "verb", "row", "f", "slot", "op_id", "g_pre", "gen_now", "cycle_id", "freeze_start_auth",
              "cycle_cutover_head", "current_cutover_head", "holder_chain", "release_target", "cutover_head",
              "barrier_id", "barrier_head", "barrier_state", "scope", "root_id", "enumeration", "writer_uid",
              "stop", "custody_label", "selection", "phase", "checked_at")


class ContainmentContext(_Sealed):
    __slots__ = ()
    SEAL = "C"
    FIELDS = ("pre", "basis", "superseded", "verdict")

    def bound(self):
        return {"pre": self.pre.digest, "basis": self.basis, "superseded": self.superseded or "",
                "verdict": self.verdict.digest if self.verdict is not None else ""}


class HandoffVerdict(_Sealed):
    __slots__ = ()
    SEAL = "P"
    FIELDS = ("cell", "phase", "pre", "slot", "op_id", "cycle_id", "g_pre", "inputs", "passed", "reasons")


class PreservationDecision(_Sealed):
    __slots__ = ()
    SEAL = "P"
    FIELDS = ("cell", "ctx", "inputs", "result", "code", "detail", "doc_fields")


class Discharge(_Sealed):
    __slots__ = ()
    SEAL = "P"
    FIELDS = ("cell", "pre", "slot", "cycle_id", "g_pre", "holder", "superseded", "kind", "inputs", "verdict")


# ------------------------------------------------------------------ wiring and views

class Wiring:
    """`world.o5`: the PR-3a stores and the optional PR-3b provider (R2 §7.3).
    `floor` is the (seq, record digest) the PR-4 producer supplies, or None."""

    def __init__(self, stores, preservation=None, floor=None):
        self.stores, self.preservation, self.floor = stores, preservation, floor


View = namedtuple("View", "slot op_id row f selection objects facts")
FResult = namedtuple("FResult", "value reason bindings")

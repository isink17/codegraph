"""Adapter over an unmodified recovery_state.RecoveryStore (r4 §12). Reads go
through the library's own readers (_read, _read_file), which is the sanctioned
raw-byte path for the journal tail rule (A1 §A4.12, review N-12); every
mutating call needs the live CommitTxn. The adapter performs no file I/O itself;
listing operations is a caller-supplied function (PR-4: held descriptors)."""

from collections import namedtuple

import recovery_state as rs

from .refusal import Code, Refused
from .stores import Gated

LibView = namedtuple("LibView", "rec pending marker journal state")
MUTATING = ("adopt", "request", "cancel", "quarantine", "verify_quiescence", "record_preservation", "mark_ready")


class Library(Gated):
    name = "library"

    def __init__(self, store, list_ops, faults=None):
        super().__init__(faults)
        self.store, self.list_ops = store, list_ops

    def operations(self, slot):
        return sorted(n for n in self.list_ops(slot) if not n.startswith(".adopt-"))

    def read(self, slot, op_id):
        """LibView, or None when the operation does not exist. Corrupt, an
        unreadable record and a journal whose last byte is not LF refuse
        STATE_UNKNOWN."""
        if op_id not in self.operations(slot):
            return None
        d = self.store._op_dir(slot, op_id)
        try:
            state = self.store._read_file(d / "state.json")
            journal = self.store._read_file(d / "journal.jsonl")
            marker = self.store._read_file(d / "TERMINAL")
            if state is None or journal is None or not journal.endswith(b"\n"):
                raise Refused(Code.STATE_UNKNOWN, "library record")
            rec, pending = self.store._read(slot, op_id)
        except rs.Busy:
            raise Refused(Code.BUSY) from None
        except rs.Refused:
            raise Refused(Code.STATE_UNKNOWN, "library record") from None
        return LibView(rec, pending, marker, journal, state)

    def terminal(self, slot, op_id):
        try:
            return self.store.terminal(slot, op_id)
        except rs.Refused:
            raise Refused(Code.STATE_UNKNOWN, "library terminal") from None

    def call(self, txn, method, *args, **kwargs):
        if method not in MUTATING:
            raise ValueError("not a library transition")
        self._begin(txn, method)
        result = getattr(self.store, method)(*args, **kwargs)
        self._end(txn, method)
        return result

    def snapshot_slot(self, slot):
        out = {}
        for op_id in self.operations(slot):
            d = self.store._op_dir(slot, op_id)
            for name in ("state.json", "journal.jsonl", "TERMINAL"):
                try:
                    raw = self.store._read_file(d / name)
                except rs.Refused:
                    raw = b"<unreadable>"
                out[f"{op_id}/{name}"] = None if raw is None else raw.hex()
        return out

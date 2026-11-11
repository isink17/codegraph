"""Synthetic worlds for the broker tests. Every store is in memory except the
unmodified recovery library, which writes only under a fresh private temporary
directory. Nothing here reads or writes the live pool, a worker slot, the
primary checkout or any path outside that temporary directory."""

import hashlib
import itertools
import json
import os
import shutil
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))

import recovery_state as rs  # noqa: E402

from broker import authz, canonical, custody, invocation, ledger, library, libview  # noqa: E402
from broker._testsupport import capability  # noqa: E402
from broker.stores import FaultPlan  # noqa: E402

SLOT = "codegraph-worker-x"
HEAD = "a" * 40
T0 = 1762819200
_LIVE = (Path.home() / "Projekti" / "codegraph-evidence").resolve()


def scratch_dir():
    """A fresh private directory under the system temporary directory, never
    under the CodeGraph evidence tree (where the live pool lives)."""
    d = Path(tempfile.mkdtemp(prefix="o5-broker-")).resolve()
    if d == _LIVE or _LIVE in d.parents:
        shutil.rmtree(d)
        raise RuntimeError("temporary directory resolves into the CodeGraph evidence tree")
    os.chmod(d, 0o700)
    return d


def counter_hex(prefix):
    c = itertools.count(1)
    return lambda: hashlib.sha256(f"{prefix}-{next(c)}".encode()).hexdigest()[:32]


class Clock:
    def __init__(self, t=T0):
        self.t = t

    def __call__(self):
        self.t += 1
        return self.t


class Observer:
    """Stands in for the PR-2/PR-3 producers."""

    def components(self, verb, slot, op_id):
        h = hashlib.sha256(f"{verb}|{slot}|{op_id}".encode()).hexdigest()
        role = "supporting" if verb == "quiescence" else "decisive"
        return [capability.observed("root_observation", h, role=role)]

    def assessment(self, slot, op_id):
        return hashlib.sha256(f"assessment|{slot}|{op_id}".encode()).hexdigest()

    def preservation(self, slot, op_id):
        return {"head": HEAD, "complete": True,
                "inventory": {"dirty": ["e1"], "untracked": [], "ignored": [], "unpublished_commits": []},
                "preserved": {"e1": {"verified": True, "to": "tar:" + "f" * 64}},
                "inventory_manifest_digest": "e" * 64}

    def final_check(self, slot, op_id):
        return {"clean": True, "lease_absent": True, "head": HEAD}


FREEZE_RESULT = {"freeze-start": "MOVED", "freeze-continue": "REVOKED", "thaw": "NONE",
                 "snapshot": "SNAPSHOTTED", "verify-preservation": "VERIFIED"}


class Env:
    """One synthetic world plus its temporary library store."""

    def __init__(self, store_kind=authz.SYNTHETIC, principal=None, faults=None, cap=ledger.CAP, one_way=True):
        self.tmp = scratch_dir()
        self.faults = faults if faults is not None else FaultPlan()
        root = self.tmp / "store"
        store = rs.RecoveryStore(root, fault=lambda p: self.faults.hit(f"library:{p}"))

        def list_ops(slot):
            try:
                return [n for n in os.listdir(root / slot)]
            except FileNotFoundError:
                return []

        self.freeze_results = dict(FREEZE_RESULT)
        self.backups = {}
        principal = principal if principal is not None else authz.SyntheticOwner()
        self.ctx = authz.Ctx(principal, store_kind)
        self.world = invocation.World(
            library.Library(store, list_ops), self.ctx, clock=Clock(), observer=Observer(),
            freeze=lambda verb, slot, op: self.freeze_results[verb], backups=self._backup,
            ids=counter_hex("id"), tokens=counter_hex("token"), nonces=counter_hex("nonce"), faults=self.faults,
            cap=cap, one_way=one_way)
        self.declines = False

    def _backup(self, marker_digest):
        return self.backups.get(marker_digest, {"sha256": "b" * 64, "marker_digest": marker_digest})

    def close(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    # -------------------------------------------------------------- fixtures (raw seeding, no protocol)

    def _burn_seed(self, slot, tierc_digest):
        seed = {"v": 1, "doc": "migrate", "slot": slot, "tierc_digest": tierc_digest}
        data = canonical.canon(seed)
        d = canonical.sha256(data)
        self.world.authz.docs[d] = data
        res = self.world.ledger.classify()
        prev = res["entries"][-1]["line_digest"] if res["entries"] else ledger.ZERO64
        e = ledger.make_entry(ledger.LEDGER, len(res["entries"]), prev, {
            "auth_digest": d, "auth_kind": "initiate", "binding_digest": canonical.digest({"slot": slot}),
            "confirmed_at": T0, "issued_at": T0, "nonce_digest": canonical.sha256(d.encode())})
        self.world.ledger.data += canonical.canon(e) + b"\n"
        log = self.world.anchor_log(slot, create=True)
        a = ledger.make_entry(ledger.ANCHOR, 0, ledger.ZERO64, {"kind": "stable", "pre": tierc_digest,
                                                                 "post": tierc_digest, "verb": "migrate",
                                                                 "auth_digest": d})
        log.data = canonical.canon(a) + b"\n"

    def seed_leased(self, slot=SLOT, gen=1, suspect=False):
        lid, token, salt = self.world.ids(), self.world.tokens(), self.world.ids()
        view = (f"version=2\nslot={slot}\ngeneration={gen}\nstate=leased\nlease_id={lid}\ntask=t1\n"
                f"utc=2026-11-11T00:00:00Z\nlease_token={token}\n")
        text = libview.tierc_from_view(view, salt, suspect)
        self.world.tierc.records[slot] = text
        key = custody.lease_key(slot, lid, gen)
        self.world.custody._store(key, {"receipt": custody.receipt(key, token), "token": token,
                                        "state": custody.PERSISTED, "bound": [], "terminal_ref": {}})
        self._burn_seed(slot, canonical.sha256(text.encode()))
        self.lease_token = token
        return lid, token

    def seed_free(self, slot=SLOT, gen=0, suspect=False):
        text = libview.with_suspect(f"version=2\nslot={slot}\ngeneration={gen}\nstate=free\n", suspect)
        self.world.tierc.records[slot] = text
        self._burn_seed(slot, canonical.sha256(text.encode()))

    # -------------------------------------------------------------- driving the protocol

    def confirmer(self, text, nonce_line):
        self.last_render = text
        return "0" * 32 if self.declines else nonce_line.split(" ", 1)[1]

    def run(self, verb, **fields):
        raw = json.dumps(dict({"v": 1, "verb": verb}, **fields)).encode()
        return invocation.invoke(self.world, raw, self.confirmer)

    def snapshot(self):
        return self.world.snapshot()

    def op_state(self, op_id, slot=SLOT):
        return self.world.library.store.state(slot, op_id)


def reason():
    return "orphaned lease"


def to_quarantine(env, op="op1", slot=SLOT):
    """adopt -> request -> freeze-start -> freeze-continue (F = REVOKED)."""
    for verb, extra in (("adopt", {}), ("request", {}), ("freeze-start", {}), ("freeze-continue", {})):
        r = env.run(verb, slot=slot, op_id=op, reason=reason(), **extra)
        if r.outcome.value != "COMMITTED":
            raise AssertionError(f"{verb}: {r}")


QUARANTINE = {"quiescence_method": "owner-quiescence", "preservation_disposition": "preserve-all"}
QUIESCENCE = dict({"method": "owner-quiescence"}, **{f"accounted_{c}": True for c in rs.QUIESCENCE_ACCOUNTED})


def to_ready(env, op="op1", slot=SLOT):
    to_quarantine(env, op, slot)
    steps = (("quarantine", QUARANTINE), ("quiescence", QUIESCENCE), ("snapshot", {}), ("verify-preservation", {}),
             ("preservation", {}), ("ready", {}))
    for verb, extra in steps:
        r = env.run(verb, slot=slot, op_id=op, reason=reason(), **extra)
        if r.outcome.value != "COMMITTED":
            raise AssertionError(f"{verb}: {r}")

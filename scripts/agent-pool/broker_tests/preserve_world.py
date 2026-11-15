"""Test-only worlds for the PR-3b provider. Everything is in memory: Git
repositories are built as bytes (no Git, no filesystem), and the synthetic
PR-4 effect writes its journal, manifests and blobs straight into the
test-owned record stores (`RecordStore.items`). That is test setup, not a
write API: production `RecordStore.put` still requires the CommitTxn, and the
real effect writer is PR-4 (P-B1 option (b))."""

import hashlib
import json
import zlib

import harness
import recovery_state as rs

from broker import barrier, canonical, freeze, invocation, journal, o5contract as oc, preserve, protocol
from broker._testsupport import o5
from broker.refusal import Code, Refused

SLOT, OTHER = harness.O5_SLOTS
SETUID = canonical.sha256(b"/usr/local/bin/agent-helper")
MOUNTS = canonical.sha256(b"mount-table-baseline")
EST_DETAIL = {"enforcement": o5.ENFORCEMENT, "neutralized": list(o5.NEUTRALIZED), "neutralized_setuid": [SETUID],
              "mounts_baseline": MOUNTS}
J = journal.J_SCHEMA


class Crash(RuntimeError):
    pass


# ------------------------------------------------------------------ Git bytes

def gobj(typ, body):
    raw = typ + b" " + str(len(body)).encode() + b"\0" + body
    return hashlib.sha1(raw).hexdigest(), zlib.compress(raw)  # noqa: S324 - Git object ids


PACK_TYPES = {b"commit": 1, b"tree": 2, b"blob": 3, b"tag": 4}


def reseal(data):
    """Recomputes the trailing SHA-1 of a pack or pack index."""
    return data[:-20] + hashlib.sha1(data[:-20]).digest()  # noqa: S324 - Git checksums


def pack_pair(raws):
    """(pack, idx): a version 2 pack of undeltified objects and its version 2
    index, laid out as Git writes them. `raws` maps an object id to its loose
    form `type size\\0body`."""
    body, entries = b"", []
    for oid in sorted(raws):
        head, _, data = raws[oid].partition(b"\0")
        n = len(data)
        hdr = bytearray([PACK_TYPES[head.split(b" ")[0]] << 4 | n & 15 | (0x80 if n > 15 else 0)])
        n >>= 4
        while n:
            hdr.append(n & 0x7F | (0x80 if n > 0x7F else 0))
            n >>= 7
        entry = bytes(hdr) + zlib.compress(data)
        entries.append((oid, 12 + len(body), zlib.crc32(entry)))
        body += entry
    pack = reseal(b"PACK" + (2).to_bytes(4, "big") + len(raws).to_bytes(4, "big") + body + bytes(20))
    fan = [sum(1 for o, _, _ in entries if int(o[:2], 16) <= i) for i in range(256)]
    idx = b"\xfftOc" + (2).to_bytes(4, "big") + b"".join(c.to_bytes(4, "big") for c in fan)
    idx += b"".join(bytes.fromhex(o) for o, _, _ in entries) + b"".join(c.to_bytes(4, "big") for _, _, c in entries)
    idx += b"".join(off.to_bytes(4, "big") for _, off, _ in entries) + pack[-20:]
    return pack, reseal(idx + bytes(20))


def git_repo(worktree=None, variant=(), prefix=b""):
    """{tree path: bytes} for a worktree and its loose-object `.git`."""
    worktree = worktree if worktree is not None else {b"README": b"hello\n", b"main.go": b"package main\n"}
    objs, tree = {}, b""
    for name in sorted(worktree):
        oid, z = gobj(b"blob", worktree[name])
        objs[oid] = z
        tree += b"100644 " + name + b"\0" + bytes.fromhex(oid)
    toid, z = gobj(b"tree", tree)
    objs[toid] = z
    coid, z = gobj(b"commit", b"tree " + toid.encode() + b"\nauthor a <a> 0 +0000\ncommitter a <a> 0 +0000\n\nm\n")
    objs[coid] = z
    g = {b"HEAD": b"ref: refs/heads/main\n", b"refs/heads/main": coid.encode() + b"\n",
         b"config": b"[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n"}
    for oid, z in objs.items():
        g[b"objects/" + oid[:2].encode() + b"/" + oid[2:].encode()] = z
    v = set(variant)
    if "packed_refs" in v or "packed_only" in v:
        del g[b"refs/heads/main"]
        g[b"packed-refs"] = b"# pack-refs with: peeled fully-peeled sorted \n" + coid.encode() + b" refs/heads/main\n"
    if "packed_only" in v:
        for oid in objs:
            del g[b"objects/" + oid[:2].encode() + b"/" + oid[2:].encode()]
        pack, idx = pack_pair({oid: zlib.decompress(z) for oid, z in objs.items()})
        stem = b"objects/pack/pack-" + pack[-20:].hex().encode()
        g[stem + b".pack"], g[stem + b".idx"] = pack, idx
    if "shallow" in v:
        g[b"shallow"] = coid.encode() + b"\n"
    if "alternates" in v:
        g[b"objects/info/alternates"] = b"/elsewhere/objects\n"
    if "unborn" in v:
        g[b"HEAD"] = b"ref: refs/heads/none\n"
    if "detached" in v:
        g[b"HEAD"] = coid.encode() + b"\n"
    if "head_tag" in v:
        g[b"HEAD"] = b"ref: refs/tags/v1\n"
        g[b"refs/tags/v1"] = coid.encode() + b"\n"
    if "symref" in v:
        g[b"refs/heads/alias"] = b"ref: refs/heads/main\n"
    if "dangling_symref" in v:
        g[b"refs/heads/gone"] = b"ref: refs/heads/missing\n"
    if "lock" in v:
        g[b"refs/heads/main.lock"] = b"partial"
    if "malformed_ref" in v:
        g[b"refs/heads/bad"] = b"zzzz\n"
    if "malformed_packed" in v:
        g[b"packed-refs"] = b"not a packed line\n"
    if "sha256" in v:
        g[b"config"] += b"[extensions]\n\tobjectformat = sha256\n"
    if "missing_object" in v:
        del g[b"objects/" + toid[:2].encode() + b"/" + toid[2:].encode()]
    if "corrupt_object" in v:
        k = b"objects/" + coid[:2].encode() + b"/" + coid[2:].encode()
        g[k] = g[k][:-2] + b"xx"
    out = {prefix + p: d for p, d in worktree.items()}
    out.update({prefix + b".git/" + p: d for p, d in g.items()})
    return out, coid


# ------------------------------------------------------------------ synthetic PR-4 effects (test-owned stores)

class Effects:
    """World.freeze(verb, slot, op_id, ctx, cycle_id). The first write of a
    cycle is its seq-0 journal record (R4.1 §3.3); every object carries the
    §3.1 header and lives under the cycle's keys; irreversible steps are keyed
    by (cycle_id, step) from the cycle's own journal (§6.3)."""

    def __init__(self, env):
        self.env = env
        self.trees = {}
        self.k = {"escaped": [], "fs_mutate": False, "crash": set(), "crash_seq0": 0, "crash_snapshot": 0,
                  "crash_thaw": 0, "result": "COMPLETE", "subst": {}, "hdr": None, "per_chunk": 4096,
                  "per_index": 512, "drop_original": False, "corrupt_revoked": False, "fork_revoked": False,
                  "bs_drop": False}
        self.state, self.calls, self.irrev, self.latest = {}, [], [], {}

    @property
    def stores(self):
        return self.env.world.o5.stores

    def tree(self, slot):
        if slot not in self.trees:
            self.trees[slot] = git_repo()[0]
        return self.trees[slot]

    def _put(self, store, key, data):
        items = self.stores[store].items
        if key in items and items[key] != data:
            raise RuntimeError(f"effect: {key} exists with other content")
        items[key] = data

    def header(self, s):
        h = {"slot": s["slot"], "op_id": s["op_id"], "g_pre": s["g_pre"], "cycle_id": s["cid"],
             "start_ctx": s["start_ctx"], "cutover_head": s["cutover_head"], "barrier_id": s["barrier_id"],
             "root_id": s["root_id"], "mode": s["mode"]}
        return self.k["hdr"](h) if self.k["hdr"] else h

    def obj(self, s, o):
        data = canonical.canon(o)
        d = canonical.sha256(data)
        self._put("artifacts", f"artifact/{s['cid']}/{d}", data)
        return d

    def blob(self, data):
        d = canonical.sha256(data)
        self._put("artifacts", f"blob/{d}", data)
        return d

    def journal(self, s, kind, **extra):
        seq = s["seq"] + 1
        rec = dict({"schema": J, "header": self.header(s), "seq": seq, "prev": s["head"] if seq else oc.ZERO64,
                    "kind": kind}, **extra)
        if seq:
            rec["early"] = s["early"]
        self._put("journals", f"journal/{s['cid']}/{seq:08d}", canonical.canon(rec))
        s["seq"], s["head"] = seq, canonical.digest(rec)
        s["records"].append(rec)
        return s["head"]

    def originals(self, s):
        return {a: self.blob(f"original|{a}|{s['slot']}".encode()) for a in journal.ATTRS}

    def manifest(self, s, phase, files):
        entries = sorted([p.hex(), self.blob(d)] for p, d in files.items())
        chunks, per = [], self.k["per_chunk"]
        for i in range(0, len(entries), per):
            chunks.append(self.obj(s, {"header": self.header(s), "kind": "entries", "n": len(chunks),
                                       "entries": entries[i:i + per]}))
        index, pi = [], self.k["per_index"]
        for i in range(0, len(chunks), pi):
            index.append(self.obj(s, {"header": self.header(s), "kind": "entry_index", "n": len(index),
                                      "chunks": chunks[i:i + pi]}))
        m = {"schema": preserve.M_SCHEMA, "header": self.header(s), "phase": phase, "entry_index": index,
             "result": self.k["result"], "reasons": [] if self.k["result"] == "COMPLETE" else ["walk_gap"],
             "questions": {"raw": "COMPLETE", "semantics": "VERIFIED", "preservation": "COMPLETE",
                           "recovery": "ELIGIBLE"}}
        blobs = {d for _, d in entries}
        if phase == "early":
            m["originals"] = s["originals"]
            blobs |= set(s["originals"].values())
            if self.k["bs_drop"]:
                blobs.discard(min(blobs))
        md = self.obj(s, m)
        bs = self.obj(s, {"header": self.header(s), "set": sorted(blobs)})
        return self.obj(s, {"header": self.header(s), "cycle_id": s["cid"], "manifest": md, "blobs": bs}), md, bs

    def _start_ctx(self, ctx):
        if ctx.pre.cycle_id is None:
            return ctx.digest
        doc = protocol.parse_doc(self.env.world.authz.docs[ctx.pre.freeze_start_auth])
        return doc["o5"]["ctx"]

    def __call__(self, verb, slot, op, ctx, cid):
        self.calls.append((verb, cid))
        pre = ctx.pre
        s = self.state.get(cid)
        if s is None:
            if verb != "freeze-start":
                raise RuntimeError("effect: unknown cycle")
            s = self.state[cid] = {"cid": cid, "slot": slot, "op_id": op, "g_pre": pre.g_pre,
                                   "cutover_head": pre.cutover_head, "barrier_id": pre.barrier_id,
                                   "root_id": pre.root_id, "mode": pre.mode, "start_ctx": self._start_ctx(ctx),
                                   "seq": -1, "head": None, "early": "", "records": [], "done": set()}
            self.journal(s, "start")
            if self.k["crash_seq0"]:
                self.k["crash_seq0"] -= 1
                raise Crash("crash after the seq-0 journal record")
        self.latest[slot] = cid
        res = getattr(self, "_" + verb.replace("-", "_"))(s, slot)
        if verb in self.k["crash"]:
            self.k["crash"].discard(verb)
            raise Crash(f"crash after the {verb} effect")
        sub = self.k["subst"].get(verb)
        return sub(res) if sub else res

    def _freeze_start(self, s, slot):
        if "move" not in s["done"]:
            s["originals"] = self.originals(s)
            if self.k["drop_original"]:
                s["originals"] = dict(s["originals"], acl=canonical.sha256(b"never-captured"))
            s["early"], s["ep_m"], s["ep_bs"] = self.manifest(s, "early", self.tree(slot))
            self.irrev.append((s["cid"], "move"))
            self.journal(s, "move.done", linked=[])
            s["done"].add("move")
        return oc.FResult("MOVED", "", {"early": s["early"], "journal": s["head"]})

    def _freeze_continue(self, s, slot):
        if "revoke" not in s["done"]:
            for a in journal.ATTRS:
                self.irrev.append((s["cid"], "revoke:" + a))
                self.journal(s, "revoke.done", attr=a, original=s["originals"][a])
            if self.k["fork_revoked"]:
                s["head"] = canonical.digest(s["records"][1])
            self.journal(s, "revoked", escaped=list(self.k["escaped"]))
            if self.k["corrupt_revoked"]:
                key = f"journal/{s['cid']}/{s['seq']:08d}"
                self.stores["journals"].items[key] = self.stores["journals"].items[key][:-1]
            s["done"].add("revoke")
        esc = s["records"][-1].get("escaped")
        return oc.FResult("REVOKED", "", {"early": s["early"], "journal": s["head"],
                                          "escaped": canonical.digest({"escaped": esc if type(esc) is list else []})})

    def _thaw(self, s, slot):
        done = {r.get("attr") for r in s["records"] if r["kind"] == "thaw.done"}
        steps = [r["attr"] for r in s["records"] if r["kind"] == "revoke.done"] + ["move"]
        for step in steps:
            if step in done:
                continue
            if self.k["crash_thaw"]:
                self.k["crash_thaw"] -= 1
                raise Crash("crash during thaw")
            self.irrev.append((s["cid"], "thaw:" + step))
            self.journal(s, "thaw.done", attr=step)
            if self.k.get("crash_thaw_after"):
                self.k["crash_thaw_after"] -= 1
                if not self.k["crash_thaw_after"]:
                    raise Crash("crash during thaw, after a journaled step")
        if s["records"][-1]["kind"] != "end":
            self.journal(s, "end")
        return oc.FResult("NONE", "", {"journal": s["head"]})

    def _snapshot(self, s, slot):
        if "snapshot" not in s["done"]:
            if self.k["crash_snapshot"]:
                self.k["crash_snapshot"] -= 1
                self.blob(f"partial|{slot}".encode())
                raise Crash("crash mid-snapshot")
            files = dict(self.tree(slot))
            if self.k["fs_mutate"]:
                files[b"README"] = b"changed after the move\n"
            s["fs"], s["fs_m"], s["fs_bs"] = self.manifest(s, "final", files)
            s["reconcile"] = self.obj(s, {"header": self.header(s), "reconcile": []})
            s["done"].add("snapshot")
        return oc.FResult("SNAPSHOTTED", "", {"early": s["early"], "final": s["fs"], "reconcile": s["reconcile"],
                                              "journal": s["head"]})

    def _verify_preservation(self, s, slot):
        if "verify" not in s["done"]:
            s["verify"] = self.obj(s, {"header": self.header(s), "cycle_id": s["cid"], "manifest": s["fs_m"],
                                       "blobs": s["fs_bs"], "reverified": True, "sealed_at": self.env.world.clock()})
            s["done"].add("verify")
        return oc.FResult("VERIFIED", "", {"early": s["early"], "final": s["fs"], "reconcile": s["reconcile"],
                                           "verify": s["verify"], "journal": s["head"]})


# ------------------------------------------------------------------ the world

def set_established(env, bid, detail):
    """Fixture for the PR-4 `barrier-establish` producer: the typed
    neutralization lists (N-R41-9) and `mounts_baseline` (N-13)."""
    st = env.world.o5.stores["barriers"]
    key = f"barrier/{bid}/{2:010d}"
    rec = protocol.parse_doc(st.items[key])
    assert rec["kind"] == "established"
    st.items[key] = canonical.canon(dict(rec, detail=detail))


def ev(env, kind, slot, data, at, role="decisive", scope="slot", barrier_id=""):
    try:
        s = o5.subject(env, slot)
    except Exception:  # noqa: BLE001 - an unreadable world: the gate refuses
        return None
    if type(s["gen"]) is not int:
        return None
    obj = {"schema": oc.S_EVIDENCE, "kind": kind, "observed_at": at,
           "subject": {"scope": scope, "slot": slot if scope == "slot" else "",
                       "slot_generation": s["gen"] if scope == "slot" else 0,
                       "op_id": s["op_id"] if scope == "slot" else "", "cycle": s["cycle"] if scope == "slot" else "",
                       "barrier_id": barrier_id},
           "producer": o5.evidence.PRODUCER, "provenance": "ROOT_OBSERVED", "role": role,
           "mode": o5.fixture_mode(env), "data": data}
    return o5.evidence.ingest(o5.ingest_capability(), env.world.o5.stores["evidence"], obj)


class World:
    def __init__(self, provider=True, est_detail=None, store_kind=None, second=True):
        kw = {"store_kind": store_kind} if store_kind is not None else {}
        self.env = harness.Env(auto_handoff=False, preservation=False, **kw)
        self.env.seed_leased()
        if second:
            self.env.seed_leased(slot=OTHER)
        set_established(self.env, self.env.barrier_id, EST_DETAIL if est_detail is None else est_detail)
        self.fx = Effects(self.env)
        self.env.world.freeze = self.fx
        self.res = preserve.WorldResolver(self.env.world)
        self.prov = preserve.Preservation(self.res)
        self.env.world.o5.preservation = self.prov if provider else None
        self.ov, self.skip = {}, set()

    def close(self):
        self.env.close()

    @property
    def w(self):
        return self.env.world

    def cycles(self, slot=SLOT):
        return freeze.cycles(self.w, slot, freeze.burned_docs(self.w, self.w.ledger.entries()))

    def cid(self, slot=SLOT):
        return self.fx.latest[slot]

    def st(self, slot=SLOT):
        return self.fx.state[self.cid(slot)]

    def classify(self, slot=SLOT):
        w = self.w
        entries = w.ledger.entries()
        docs = freeze.burned_docs(w, entries)
        cyc = freeze.cycles(w, slot, docs)
        return [freeze.classify_burn(w, slot, docs, entries, a, cyc)
                for a in freeze.freeze_start_burns(w, docs, entries, slot)]

    def row(self, slot=SLOT):
        from broker import lifecycle
        w = self.w
        facts = invocation._read_slot(w, slot, w.ledger.entries())
        c = lifecycle.Components(invocation._g(w), facts.l, facts.a, facts.f, facts.r, facts.k, facts.op)
        return lifecycle.row(c), facts.f

    def _bound(self, slot):
        mine = [c for c in self.cycles(slot)]
        b = mine[-1].bindings if mine else {}
        return [b[k] for k in ("early", "journal", "final", "verify") if k in b]

    def final_data(self, slot=SLOT, **kw):
        es = sorted([p.hex(), canonical.sha256(d)] for p, d in self.fx.tree(slot).items())
        data = {"root_id": o5.h(f"root|{slot}"), "projection": canonical.digest({"entries": es}),
                "head_resolved": self.head(slot)}
        data.update(kw)
        return data

    def head(self, slot=SLOT):
        g = {p[len(b".git/"):]: d for p, d in self.fx.tree(slot).items() if p.startswith(b".git/")}
        from broker import gitclass
        return gitclass.classify(b"", g).head_oid

    def observe(self, slot=SLOT, at=None, extra=None):
        w = self.w
        at = at if at is not None else w.clock()
        bound = self._bound(slot)
        ov = {"uid_process_scan": {"uids": [o5.AGENT_UID], "processes": []},
              "setuid_writer_scan": {"files": [[SETUID, 0o4755, True]]},
              "mounts_external": {"mounts": MOUNTS, "baseline": MOUNTS},
              "claimed_set_observation": {"parent": [0, 0o700, 0], "ancestors_root_owned": True,
                                          "entries": [["61", 0, 0o400, 0, 1]], "linked": [0o700, []]},
              "writer_capabilities": {"grants": []},
              "artifact_isolation": {"objects": [[d, 0, 0o400, 0, 1, True] for d in bound]},
              "holder_scan_protected": {"holders": [], "uids_excluded": [0]},
              "protected_mounts": {"below": []}}
        ov.update(self.ov)
        ov.update(extra or {})
        skip = {"holder_scan_1", "holder_scan_2", "final_observation"} | self.skip
        o5.observe(self.env, slot, at=at, overrides=ov, skip=tuple(skip))
        hs = ov.get("holders", {"holders": [], "uids_excluded": [0]})
        if "holder_scan_1" not in self.skip:
            ev(self.env, "holder_scan_1", slot, ov.get("holder_scan_1", hs), at - 3, role=ov.get("holder_role",
                                                                                                "decisive"))
        if "holder_scan_2" not in self.skip:
            ev(self.env, "holder_scan_2", slot, ov.get("holder_scan_2", hs), at)
        if "final_observation" in ov or (slot in self.fx.latest and "fs" in self.st(slot)
                                         and "final_observation" not in self.skip):
            ev(self.env, "final_observation", slot, ov.get("final_observation") or self.final_data(slot), at)

    def run(self, verb, slot=SLOT, op="op1", extra=None, confirm_hook=None, **kw):
        kw = dict({"quarantine": harness.QUARANTINE, "quiescence": harness.QUIESCENCE}.get(verb, {}), **kw)
        fields = dict(kw, slot=slot, reason=harness.reason())
        if op:
            fields["op_id"] = op
        self.observe(slot, extra=extra)
        raw = json.dumps(dict({"v": 1, "verb": verb}, **fields)).encode()
        conf = self.env.confirmer
        if confirm_hook is not None:
            def conf(text, line):
                confirm_hook()
                return self.env.confirmer(text, line)
        return invocation.invoke(self.w, raw, conf)

    def ok(self, *verbs, slot=SLOT):
        for v in verbs:
            r = self.run(v, slot=slot)
            if r.outcome.value != "COMMITTED":
                raise AssertionError(f"{v}: {r.outcome.value} {r.code} {r.detail}")

    def handoff(self, slot=SLOT, between=None, extra=None):
        """PR-4 containment-handoff stand-in through the real H-EST interface,
        V and R each re-selecting (merged `synthetic_handoff` plus the PR-1
        context comparison)."""
        self.observe(slot, extra=extra)
        env, w = self.env, self.w
        entries = w.ledger.entries()
        docs = freeze.burned_docs(w, entries)
        facts = invocation._read_slot(w, slot, entries)
        cyc = [c for c in freeze.cycles(w, slot, docs) if c.op_id == facts.op_id][-1]
        doc = o5._owner_doc(env, "containment-handoff", slot=slot, op_id=facts.op_id, slot_generation=cyc.g_pre,
                            cycle_id=cyc.cycle_id, barrier_id=cyc.barrier_id)
        pres, verdicts = [], []
        for phase in ("V", "R"):
            if phase == "R" and between is not None:
                between()
            pre, sel = freeze.basis_for(w, slot, "handoff", doc=doc)
            pres.append(pre.digest)
            prov = w.o5.preservation
            verdicts.append(prov.handoff_establish(pre, sel.public(), None) if prov is not None else None)
            last = sel
        c = o5.chain(env, cyc.barrier_id)
        barrier.handoff_allowed(oc.mode(w), c, barrier.ESTABLISHED, "P13", slot, cyc, w.gates.stopped(), verdicts,
                                pres, lambda v: freeze.guard(v, "H-EST", v.pre, last, oc.H_EST_KINDS))
        if pres[0] != pres[1]:
            raise Refused(Code.CONTEXT_CHANGED)
        self.last_verdicts = verdicts
        auth = o5.burn(env, doc)
        return o5.append_record(env, cyc.barrier_id, "superseded", auth=auth, detail={
            "slot": slot, "cycle_id": cyc.cycle_id, "g_pre": cyc.g_pre, "root_id": c.root_ids[slot],
            "verdicts": [v.digest for v in verdicts]})

    def try_handoff(self, slot=SLOT, between=None, extra=None):
        try:
            self.handoff(slot, between, extra)
            return "established"
        except Refused as e:
            return (e.code.value, e.detail)

    def release(self, bid=None, **kw):
        snap = self.env.snapshot()
        try:
            out = freeze.release_check(self.w, bid or self.env.barrier_id, **kw)
        except Refused as e:
            out = (e.code.value, e.detail)
        assert self.env.snapshot() == snap
        return out

    def to_p13(self, slot=SLOT):
        self.ok("adopt", "request", "freeze-start", "freeze-continue", "quarantine", "quiescence", slot=slot)

    def to_verified(self, slot=SLOT):
        self.to_p13(slot)
        self.handoff(slot)
        self.ok("snapshot", "verify-preservation", slot=slot)

    def to_ready(self, slot=SLOT):
        self.to_verified(slot)
        self.ok("preservation", "ready", slot=slot)

    def last_doc(self, verb):
        for e in reversed(self.w.ledger.entries()):
            d = self.res.burned_doc(e["auth_digest"])
            if d and d.get("verb") == verb:
                return d
        return None

    def record(self, slot=SLOT, op="op1"):
        return self.w.library.read(slot, op)


def outcome(r):
    return (r.outcome.value,) + ((r.code.value, r.detail) if r.code is not None else ())


def rel(out, slot=SLOT):
    return dict(out).get(slot) if isinstance(out, list) else out


def store_bytes(w):
    return {n: dict(st.items) for n, st in w.w.o5.stores.items() if hasattr(st, "items")}


READY_STATE = rs.READY

"""Test-only semantic comparator (A1 §A3.1, review N5-14). Outside the broker
package and its MANIFEST. It compares an uninterrupted reference run with a
run interrupted at a fault point and completed through the protocol remedy."""

import hmac
import json
import re

import recovery_state as rs

from broker import canonical, custody, ledger

UTC = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\Z")
DETERMINISTIC = ("schema", "slot", "op_id", "identity_kind", "identity", "slot_generation", "state", "seq",
                 "quiescence_method", "preserved_head")
# Fields a replacement document may change (A1 §A3.2 plus N5-14 repair_plan, and
# the two state-binding digests, which necessarily differ after any extra burn).
REPLACEABLE = ("nonce_digest", "issued_at", "not_after", "utc", "session", "repair_plan", "tierp_state_digest",
               "anchor_digest")


class RunView:
    """Everything the comparator reads from one finished run."""

    def __init__(self, env, slot, op_id):
        w = env.world
        d = w.library.store._op_dir(slot, op_id)
        self.state = json.loads(w.library.store._read_file(d / "state.json"))
        self.journal = [json.loads(x) for x in w.library.store._read_file(d / "journal.jsonl").decode().splitlines()]
        self.marker = w.library.store._read_file(d / "TERMINAL")
        self.lease_token = env.lease_token
        self.key = custody.recovery_key(slot, op_id, self.state["slot_generation"] - (
            1 if self.state["state"] in rs.AFTER_QUARANTINE else 0))
        st = w.custody.lookup(self.key)
        self.custody = None if st.record is None else {"state": st.record["state"], "bound": st.record["bound"],
                                                       "check_digest": st.record["receipt"]["check_digest"],
                                                       "token": st.record["token"]}
        self.burns = [{k: e[k] for k in ("auth_digest", "auth_kind", "binding_digest", "nonce_digest")}
                      for e in w.ledger.entries()]
        self.anchors = [(e["kind"], e["pre"], e["post"], e["verb"]) for e in w.anchor_log(slot).entries()]
        self.docs = {dg: json.loads(w.authz.get(dg)) for dg in w.authz.docs}


def _fail(why):
    raise AssertionError(why)


def _verifier_ok(v, run):
    st = run.state
    if not (rs.HEX32.match(str(st.get("salt"))) and rs.HEX64.match(str(st.get("verifier")))):
        _fail("salt or verifier malformed")
    lease = hmac.compare_digest(rs._verifier(st["salt"], run.lease_token), st["verifier"])
    if st["state"] in (rs.ACTIVE, rs.REQUESTED, rs.CANCELLED):
        if not lease:
            _fail("verifier does not match the lease token")
        return
    if run.custody is None:
        _fail("custody missing")
    token = run.custody["token"]
    if lease or not hmac.compare_digest(rs._verifier(st["salt"], token), st["verifier"]):
        _fail("verifier does not match the custody token")
    if run.custody["check_digest"] != custody.check_digest(run.key, token):
        _fail("custody check_digest does not commit to the token")


def _doc_equivalent(ref, act, ref_digest, act_digest):
    if ref_digest == act_digest:
        return
    a, b = ref.docs.get(ref_digest), act.docs.get(act_digest)
    if a is None or b is None:
        _fail("gate or evidence is not a stored document")
    strip = lambda d: {k: v for k, v in d.items() if k not in REPLACEABLE}  # noqa: E731
    if a.get("doc") == "ready" and (a.get("preservation"), b.get("preservation")) == (
            ref.state["gates"].get("preservation"), act.state["gates"].get("preservation")):
        a, b = dict(a, preservation=None), dict(b, preservation=None)  # follows the preservation gate (A1 §A3.2)
    if strip(a) != strip(b):
        _fail("replacement document differs outside the replaceable fields")


def _bound(run):
    q = [e for e in run.journal if e["to"] == rs.QUARANTINED and e["kind"] == "complete"]
    if not q or run.custody["state"] == custody.PERSISTED:
        return []
    return [canonical.digest({k: q[-1][k] for k in rs.ENTRY_KEYS if k in q[-1]}), run.state["gates"]["quarantine"]]


def semantic_equal(ref, act, *, resumed_seqs, extra_burns, extra_anchor):
    """Raises AssertionError on the first difference. resumed_seqs: the seqs of
    completions that must carry resumed: true; extra_burns: the number of extra
    ledger burns; extra_anchor: extra (kind, verb) anchor entries."""
    if set(ref.state) != set(act.state):
        _fail("state key set differs")
    for k in DETERMINISTIC:
        if ref.state.get(k) != act.state.get(k):
            _fail(f"state field {k} differs")
    if set(ref.state["gates"]) != set(act.state["gates"]):
        _fail("gate keys differ")
    for t in ref.state["gates"]:
        _doc_equivalent(ref, act, ref.state["gates"][t], act.state["gates"][t])
    _verifier_ok(ref, ref)
    _verifier_ok(act, act)
    if len(ref.journal) != len(act.journal):
        _fail("journal length differs")
    for a, b in zip(ref.journal, act.journal):
        allowed = set(rs.ENTRY_KEYS) | {"kind", "utc", "resumed"}
        if set(b) - allowed or not UTC.match(str(b.get("utc"))):
            _fail("journal entry has unexpected keys or time")
        if a["kind"] != b["kind"]:
            _fail("journal kind differs")
        for k in rs.ENTRY_KEYS:
            if k == "evidence" and a.get("from") is not None:
                _doc_equivalent(ref, act, a.get(k), b.get(k))
            elif a.get(k) != b.get(k):
                _fail(f"journal field {k} differs")
        want = b["kind"] == "complete" and b["seq"] in resumed_seqs
        if ("resumed" in b) != want or ("resumed" in b and b["resumed"] is not True):
            _fail("resumed flag differs from the model")
    if ref.marker != act.marker:
        _fail("marker differs")
    if (ref.custody is None) != (act.custody is None):
        _fail("custody presence differs")
    if ref.custody is not None:
        for k in ("state", "check_digest"):
            if ref.custody[k] != act.custody[k]:
                _fail(f"custody {k} differs")
        if act.custody["bound"] != _bound(act):
            _fail("custody bound is not the quarantine intent and gate")
    if len(act.burns) != len(ref.burns) + extra_burns:
        _fail("ledger burns differ from the model")
    if len({b["nonce_digest"] for b in act.burns}) != len(act.burns) or \
            len({b["auth_digest"] for b in act.burns}) != len(act.burns):
        _fail("a nonce or authorization was burned twice")
    burned = {b["auth_digest"] for b in act.burns}
    for t, d in act.state["gates"].items():
        if d not in burned:
            _fail(f"gate {t} is not a burned document")
    for e in act.journal:
        if e.get("from") is not None and e["evidence"] not in burned:
            _fail("journal evidence is not a burned document")
    kinds = lambda xs: [(k, v) for k, _, _, v in xs]  # noqa: E731
    act_k = kinds(act.anchors)
    for item in extra_anchor:
        if item in act_k:
            act_k.remove(item)
    if act_k != kinds(ref.anchors):
        _fail("anchor sequence differs from the model")
    return True


def assert_chain(env):
    res = env.world.ledger.classify()
    if res["cls"] != ledger.CLEAN:
        _fail("ledger not clean")
    return canonical.sha256(env.world.ledger.data)

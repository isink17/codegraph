"""Model B cutover history (PR-3a contract R2 §4, R3 §4, R4.1 §3.4/§3.7): the
pure reconciliation of ledger burns, authorization documents, records, head
entries, witnesses and the off-host floor into one head. INVALID beats
PENDING and the first INVALID decides. The in-memory fakes give no rollback
resistance; the floor (PR-4) is the only rollback anchor."""

from collections import namedtuple

from . import authz, canonical, o5contract as oc, protocol
from .canonical import is_hex64
from .refusal import Code, Refused

Head = namedtuple("Head", "state reason seq record digest enumeration history pending_auth")
ABSENT, ACTIVE, REVOKED, PENDING, INVALID = "ABSENT", "ACTIVE", "REVOKED", "PENDING", "INVALID"
RECORD_KEYS = ("schema", "seq", "prev", "kind", "effective", "owner_auth", "agent", "owner_principal",
               "slot_enumeration", "baselines", "tests", "preconditions", "supersedes", "utc", "mode")
AGENT_KEYS = ("writer_uids", "account", "primary_gid", "scope")
BASELINE_KEYS = ("root", "owner", "principals", "channels")
ADMIN_GROUPS = ("admin", "wheel", "staff", "_developer")
MAX_RECORD = 65536

_KEYS = {}


def cutover_keys():
    """R2 §4.3 / R3 §4.3: binding_digest -> (verb, seq) over the published domain."""
    if not _KEYS:
        for v in oc.CUTOVER_VERBS:
            for s in range(oc.MAX_CUTOVER_SEQ):
                _KEYS[authz.binding_digest({"verb": v, "slot": oc.CUTOVER_SUBJECT, "slot_generation": s})] = (v, s)
    return _KEYS


def _invalid(reason):
    return Head(INVALID, reason, None, None, None, None, frozenset(), None)


def _parse(data):
    if data is None or len(data) > MAX_RECORD:
        return None
    try:
        return protocol.parse_doc(data)
    except Refused:
        return None


def _record_ok(r, i):
    if sorted(r) != sorted(RECORD_KEYS) or r["schema"] != oc.S_CUTOVER or r["seq"] != i:
        return False
    a, b = r["agent"], r["baselines"]
    return (r["kind"] in ("cutover", "rebaseline", "revoked") and r["effective"] in ("MODEL_B", "MODEL_A")
            and is_hex64(r["owner_auth"]) and is_hex64(r["prev"]) and type(a) is dict
            and sorted(a) == sorted(AGENT_KEYS)
            and type(b) is dict and sorted(b) == sorted(BASELINE_KEYS) and all(is_hex64(x) for x in b.values())
            and is_hex64(r["slot_enumeration"]) and is_hex64(r["tests"]) and type(r["preconditions"]) is list
            and (r["supersedes"] == "" or is_hex64(r["supersedes"])) and type(r["utc"]) is int)


def _identity_ok(r, prev_b):
    uids = r["agent"]["writer_uids"]
    if r["owner_principal"] != oc.OWNER_PRINCIPAL or type(uids) is not list or not uids \
            or any(type(u) is not int for u in uids) or uids != sorted(set(uids)) or {0, 501} & set(uids):
        return False
    if prev_b is not None and r["kind"] != "cutover":
        return (r["agent"]["account"], r["agent"]["writer_uids"]) == (prev_b["agent"]["account"],
                                                                      prev_b["agent"]["writer_uids"])
    return True


def _transition_ok(r, prev, prev_digest):
    if prev is None:
        return (r["kind"], r["effective"], r["supersedes"]) == ("cutover", "MODEL_B", "")
    if r["supersedes"] != prev_digest:
        return False
    if prev["effective"] == "MODEL_A":
        return (r["kind"], r["effective"]) == ("cutover", "MODEL_B")
    if r["kind"] == "rebaseline":
        return r["effective"] == "MODEL_B" and r["agent"] == prev["agent"]
    return (r["kind"], r["effective"]) == ("revoked", "MODEL_A")


def enumeration_of(store, digest, record, mode):
    """R2 §4.4. Returns (enumeration, reason) with reason None when valid."""
    data = store.get(digest)
    obj = _parse(data)
    if obj is None or canonical.sha256(data) != digest or sorted(obj) != ["mode", "schema", "slots"] \
            or obj["schema"] != oc.S_ENUM or obj["mode"] != mode or type(obj["slots"]) is not list:
        return None, "enumeration"
    names = []
    for s in obj["slots"]:
        if type(s) is not dict or sorted(s) != ["provisioned_root", "slot", "writer_uid"] \
                or not protocol.NAME.match(str(s["slot"])) or s["slot"] in oc.MODEL_A_POOL \
                or s["slot"] == oc.CUTOVER_SUBJECT or type(s["writer_uid"]) is not int:
            return None, "enumeration"
        if s["writer_uid"] not in record["agent"]["writer_uids"]:
            return None, "identity"
        names.append(s["slot"])
    if names != sorted(set(names)):
        return None, "enumeration"
    return obj, None


def _contiguous(store, prefix):
    out, i = [], 0
    while store.get(f"{prefix}/{i:010d}") is not None:
        out.append(store.get(f"{prefix}/{i:010d}"))
        i += 1
    if len(store.keys()) != i:
        return None
    return out


def head(entries, get_doc, stores, witnesses, floor, mode):
    """R2 §4.5 steps 1–9 (pure). `entries` come from a ledger classify() accepted."""
    keys = cutover_keys()
    trust = authz.TRUST_SYNTHETIC if mode == oc.SYNTHETIC else authz.TRUST_OWNER
    burns, seen = [], set()
    for e in entries:
        if e["auth_kind"] != "initiate" or e["binding_digest"] not in keys:
            continue
        v, s = keys[e["binding_digest"]]
        if s != len(burns) or e["auth_digest"] in seen:
            return _invalid("fork")
        seen.add(e["auth_digest"])
        burns.append((e["auth_digest"], v, s))
    docs = []
    for auth, v, s in burns:
        doc = get_doc(auth)
        if doc is None:
            return _invalid("auth_missing")
        if canonical.digest(doc) != auth or doc.get("verb") != v or doc.get("slot") != oc.CUTOVER_SUBJECT \
                or doc.get("slot_generation") != s or "op_id" in doc or doc.get("actor_uid") != 501 \
                or doc.get("trust") != trust or not is_hex64(doc.get("cutover_record")) \
                or not is_hex64(doc.get("cutover_prev")):
            return _invalid("auth_binding")
        docs.append(doc)
    raw = _contiguous(stores["cutovers"], "cutover")
    heads_raw = _contiguous(stores["cutover_heads"], "head")
    if raw is None or heads_raw is None:
        return _invalid("gap")
    records, prev, prev_d, prev_b = [], None, oc.ZERO64, None
    for i, data in enumerate(raw):
        r = _parse(data)
        if r is None:
            return _invalid("noncanonical")
        if not _record_ok(r, i):
            return _invalid("transition")
        if r["prev"] != prev_d:
            return _invalid("fork")
        if r["mode"] != mode:
            return _invalid("mode")
        if not _transition_ok(r, prev, prev_d):
            return _invalid("transition")
        if not _identity_ok(r, prev_b):
            return _invalid("identity")
        records.append(r)
        prev, prev_d = r, canonical.sha256(data)
        if r["effective"] == "MODEL_B":
            prev_b = r
    n, m, k = len(records), len(heads_raw), len(burns)
    digests = [canonical.sha256(d) for d in raw]
    for i, r in enumerate(records):
        if i >= k:
            return _invalid("auth_binding")
        doc = docs[i]
        body = {x: y for x, y in r.items() if x != "owner_auth"}
        if r["owner_auth"] != burns[i][0] or doc["cutover_record"] != canonical.digest(body) \
                or doc["cutover_prev"] != r["prev"] or oc.CUTOVER_KIND[burns[i][1]] != r["kind"]:
            return _invalid("auth_binding")
    for i in range(min(m, n)):
        h = _parse(heads_raw[i])
        if h != {"schema": oc.S_HEAD, "seq": i, "record": digests[i], "owner_auth": records[i]["owner_auth"],
                 "mode": mode}:
            return _invalid("fork")
    if any(w not in digests for w in witnesses):
        return _invalid("rollback")
    if mode == oc.PROTECTED and floor is None and k > 0:
        return _invalid("floor_missing")
    if floor is not None and not (0 <= floor[0] < n and digests[floor[0]] == floor[1]):
        return _invalid("rollback")
    if m > n or n < k - 1 or m < n - 1:
        return _invalid("rollback")
    pending = burns[-1][0] if burns else None
    if n == k - 1:
        return Head(PENDING, "record", None, None, None, None, frozenset(digests), pending) if m == n \
            else _invalid("rollback")
    if m == n - 1:
        return Head(PENDING, "head", None, None, None, None, frozenset(digests), pending)
    if floor is not None and floor[0] == n - 2:
        return Head(PENDING, "floor", None, None, None, None, frozenset(digests), pending)
    if floor is not None and floor[0] < n - 2:
        return _invalid("rollback")
    if n == 0:
        return Head(ABSENT, None, None, None, None, None, frozenset(), None)
    rec = records[-1]
    if rec["effective"] == "MODEL_A":
        return Head(REVOKED, None, n - 1, rec, digests[-1], None, frozenset(digests), None)
    enum, why = enumeration_of(stores["enumerations"], rec["slot_enumeration"], rec, mode)
    if why:
        return _invalid(why)
    return Head(ACTIVE, None, n - 1, rec, digests[-1], enum, frozenset(digests), None)


def require_closed(cycle_classes):
    """R4.1 §3.7: open (including ready_pending) refuses cycle_open; a
    STATE_UNKNOWN class refuses with that class; only closed kinds pass."""
    for c in cycle_classes:
        if c.startswith("STATE_UNKNOWN"):
            raise Refused(Code.STATE_UNKNOWN, c)
        if c not in ("none_no_move", "thaw_complete", "ready"):
            raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "cycle_open")


def cutover_allowed(mode, kind, h, chain_states, cycle_classes, seq):
    """R2 §4.8 with R3 §4.3/§4.8 and R4.1 §3.4 (pure; the PR-4 V and R)."""
    oc.require_synthetic(mode)
    if h.state in (PENDING, INVALID):
        raise Refused(Code.STATE_UNKNOWN, f"cutover_invalid({h.state.lower()}_{h.reason})")
    if kind == "cutover":
        if h.state not in (ABSENT, REVOKED) or seq >= oc.REVOKE_SEQ - 1:
            raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "cutover")
        return
    if h.state != ACTIVE:
        raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(model_a)")
    if kind == "rebaseline" and seq >= oc.REVOKE_SEQ - 1 or kind == "revoked" and seq > oc.REVOKE_SEQ:
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "sequence")
    if any(s != "RELEASED" for s in chain_states):
        raise Refused(Code.VERB_NOT_ALLOWED_IN_STATE, "barrier_in_use")
    require_closed(cycle_classes)


def model_b_gate(record, picked):
    """G7a (R3 §3.3): fresh decisive ROOT_OBSERVED components of root-controlled
    state only, compared with the cutover record's baselines."""
    a, b = record["agent"], record["baselines"]
    for kind in oc.TMB_KINDS:
        data = picked[kind][1]["data"]
        ok = data.get("result") != "contradicts" and {
            "agent_identity_root": lambda: data.get("writer_uid") in a["writer_uids"]
            and data.get("primary_gid") == a["primary_gid"] and type(data.get("groups")) is list
            and not set(data["groups"]) & set(ADMIN_GROUPS) and not any(
                str(g).startswith("com.apple.access_") for g in data["groups"]),
            "principals": lambda: data.get("digest") == b["principals"],
            "no_agent_as_owner": lambda: data.get("result") == "absent",
            "root_channels": lambda: data.get("digest") == b["channels"],
        }[kind]()
        if not ok:
            raise Refused(Code.QUIESCENCE_NOT_ESTABLISHED, "barrier_unavailable(baseline_drift)")

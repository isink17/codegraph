"""Tier P anchor for Tier C (r4 §7.4): the A component of the product machine,
derived from one slot's anchor log. The Tier C consistency rule (r4 §6.1) is in
lifecycle.decide."""

from collections import namedtuple

from .ledger import ANCHOR, UNCLASSIFIABLE, classify

STABLE = "STABLE"
PENDING = "PENDING"
ABSENT = "ABSENT"
CORRUPT = "CORRUPT"

A = namedtuple("A", "kind digest pre post verb auth_digest")


def component(log_bytes, evidence=None):
    """A from the committed entries. A torn tail confers nothing (R5-1)."""
    if not log_bytes:
        return A(ABSENT, None, None, None, None, None)
    res = classify(log_bytes, ANCHOR, evidence)
    if res["cls"] == UNCLASSIFIABLE or not res["entries"]:
        return A(CORRUPT if res["cls"] == UNCLASSIFIABLE else ABSENT, None, None, None, None, None)
    return fold(res["entries"])


def fold(entries):
    """Replays the anchor entries; any out-of-order entry is CORRUPT."""
    cur = None
    for e in entries:
        k = e["kind"]
        if cur is None:
            if k != "stable" or e["verb"] != "migrate":
                return A(CORRUPT, None, None, None, None, None)
            cur = A(STABLE, e["post"], None, None, None, e["auth_digest"])
        elif k == "pending" and cur.kind == STABLE and e["pre"] == cur.digest and e["verb"] != "migrate":
            cur = A(PENDING, None, e["pre"], e["post"], e["verb"], e["auth_digest"])
        elif k in ("complete", "abort") and cur.kind == PENDING and (e["pre"], e["post"], e["verb"]) == (
                cur.pre, cur.post, cur.verb):
            cur = A(STABLE, e["post"] if k == "complete" else e["pre"], None, None, None, e["auth_digest"])
        else:
            return A(CORRUPT, None, None, None, None, None)
    return cur


def entry_fields(kind, pre, post, verb, auth_digest):
    return {"kind": kind, "pre": pre, "post": post, "verb": verb, "auth_digest": auth_digest}

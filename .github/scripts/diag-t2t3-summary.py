"""Summarize a diag-t2-t3 go test -json log: outcomes, failure kinds, DIAG t2 lines."""
import collections
import json
import re
import sys

outcomes = collections.Counter()
kinds = collections.Counter()
diag = []
cur = []
with open(sys.argv[1], encoding="utf-8") as f:
    for line in f:
        try:
            ev = json.loads(line)
        except ValueError:
            continue
        test, action, out = ev.get("Test"), ev.get("Action"), ev.get("Output", "")
        if not test or "/" in test:
            continue
        if action == "output":
            cur.append(out)
            if "DIAG t2" in out:
                diag.append(out.strip()[:300])
        elif action in ("pass", "skip"):
            outcomes[action] += 1
            cur = []
        elif action == "fail":
            outcomes["fail"] += 1
            msg = next((l.strip() for l in cur if re.search(r"_test\.go:\d+:", l) and "DIAG" not in l), "no message")
            kinds[re.sub(r'D:\\\S+|"[^"]*"', "<path>", msg)[:160]] += 1
            cur = []
print(f"## {sys.argv[1]}")
print(f"- outcomes: {dict(outcomes)}")
for k, v in kinds.most_common():
    print(f"  - {v}x {k}")
print(f"- DIAG t2 lines: {len(diag)}")
for d in diag[:30]:
    print(f"  - {d}")

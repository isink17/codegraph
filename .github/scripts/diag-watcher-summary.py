"""Summarize the watcher diagnosis: test outcomes and DIAG store-close lines."""
import collections
import json
import re
import sys

outcomes = collections.Counter()
failed = collections.Counter()
release = collections.Counter()
nonzero = []
cleanup_errors = []
with open(sys.argv[1], encoding="utf-8") as f:
    for line in f:
        try:
            ev = json.loads(line)
        except ValueError:
            continue
        test, action, out = ev.get("Test"), ev.get("Action"), ev.get("Output", "")
        if test and "/" not in test and action in ("pass", "fail", "skip"):
            outcomes[action] += 1
            if action == "fail":
                failed[test] += 1
        if "TempDir RemoveAll cleanup" in out:
            cleanup_errors.append(f"{test}: {out.strip()}")
        m = re.search(r"DIAG close test=(\S+) .*release_ms=(-?\d+)", out)
        if m:
            ms = int(m.group(2))
            release["0" if ms == 0 else ">0" if ms > 0 else "never(5s)"] += 1
            if ms != 0:
                nonzero.append(out.strip())
print(f"## {sys.argv[1]}")
print(f"- top-level tests: {dict(outcomes)}")
print(f"- failed tests: {dict(failed) or 'none'}")
print(f"- TempDir cleanup errors: {len(cleanup_errors)}")
for e in cleanup_errors[:20]:
    print(f"  - {e}")
print(f"- graph.sqlite release after Store.Close: {dict(release)}")
for n in nonzero[:20]:
    print(f"  - {n}")

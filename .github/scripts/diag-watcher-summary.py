"""Summarize the watcher diagnosis: test outcomes and DIAG store-close lines."""
import collections
import json
import re
import sys

outcomes = collections.Counter()
failed = collections.Counter()
release = collections.Counter()
sqlite_rows = collections.Counter()
details = collections.Counter()
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
        m2 = re.search(r"DIAG sqlite test=\S+ opened=\d+ open=(\d+) zombies=(\d+) locked=(\w+) fds_after=(-?\d+)", out)
        if m2:
            sqlite_rows[(f"open={m2.group(1)}", f"zombies={m2.group(2)}", f"locked={m2.group(3)}", f"fds_after={m2.group(4)}")] += 1
        m3 = re.search(r"DIAG sqlite-detail test=\S+ conn \d+ (ZOMBIE|NEVER CLOSED|CANCEL-LEAK)(.*)", out)
        if m3:
            details[(m3.group(1) + re.sub(r"conn \d+|D:\\\S+|/\S+/", "", m3.group(2)))[:400]] += 1
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
print("- store close vs native connections (open / zombies / locked):")
for k, v in sorted(sqlite_rows.items()):
    print(f"  - {' '.join(k)}: {v}")
print("- distinct leak details:")
for k, v in details.most_common(15):
    print(f"  - {v}x {k}")

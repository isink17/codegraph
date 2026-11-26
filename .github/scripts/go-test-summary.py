"""Summarize `go test -json` output: counts and every failed and skipped test.

Usage: python go-test-summary.py go-test.json
Prints to stdout and, when set, appends to $GITHUB_STEP_SUMMARY.
"""
import json
import os
import sys

results, output, pkg_fail = {}, {}, []
with open(sys.argv[1], encoding="utf-8", errors="replace") as f:
    for line in f:
        try:
            e = json.loads(line)
        except ValueError:
            continue
        test, action = e.get("Test"), e.get("Action")
        if not test:
            if action == "fail":
                pkg_fail.append(e["Package"])
            continue
        key = e["Package"] + " " + test
        if action == "output":
            output.setdefault(key, []).append(e.get("Output", ""))
        elif action in ("pass", "fail", "skip"):
            results[key] = action


def reason(key):
    lines = [l.strip() for l in output.get(key, []) if l.strip() and not l.strip().startswith(("=== ", "--- "))]
    return (" | ".join(lines))[:300]


counts = {a: sum(1 for v in results.values() if v == a) for a in ("pass", "fail", "skip")}
out = ["## go test results", "", "passed %(pass)d, failed %(fail)d, skipped %(skip)d (tests and subtests)" % counts, ""]
for action in ("fail", "skip"):
    keys = sorted(k for k, v in results.items() if v == action)
    out.append("### %s (%d)" % ("Failed" if action == "fail" else "Skipped", len(keys)))
    out += ["- `%s` %s" % (k, reason(k)) for k in keys]
    out.append("")
out.append("### Failed packages (%d)" % len(pkg_fail))
out += ["- `%s`" % p for p in sorted(set(pkg_fail))]
text = "\n".join(out) + "\n"
sys.stdout.write(text)
if os.environ.get("GITHUB_STEP_SUMMARY"):
    with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as f:
        f.write(text)

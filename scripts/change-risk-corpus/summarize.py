#!/usr/bin/env python3
"""Print Markdown tables from run.py's results.json and the per-build outputs.

usage: summarize.py WORK_DIR
"""
import json
import sys

# What each constructed label claims about the change, independently of
# CodeGraph: "break" means a surviving dependent or behavior is broken,
# "same" means the program means the same after the change.
TRUTH = {"compile_break": "break", "behavior_break": "break", "rebind_compiles": "break", "compiles": None}
SAME = {"uuid-move", "uuid-move-header-differs", "uuid-rename-everywhere", "errors-move", "codegraph-move"}


def risk(work, case, build):
    with open(f"{work}/results/{case}-{build}1.json") as f:
        r = json.load(f)["diff"]["risk"]
    rules = ",".join(f"{c['rule']}={c['count']}" for c in r["components"] if c["count"]) or "-"
    return r["assessment"], r["score"], rules


def main():
    work = sys.argv[1]
    with open(f"{work}/results/results.json") as f:
        rows = json.load(f)
    print("| case | kind | label | oracle base/head (build,vet,test) | label holds | CGO assessment (score; rules) | CGO_ENABLED=0 assessment (score; rules) | pairs / refused | deterministic | parity | checkout unchanged | traceable |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|")
    for r in rows:
        if "error" in r:
            print(f"| {r['id']} | {r['kind']} | {r['label']} | | | diff failed {r['rc']} | | | | | {r['checkout_unchanged']} | |")
            continue
        o = r.get("oracle")
        fmt = lambda x: ",".join("-" if x[k] is None else str(x[k]) for k in ("build", "vet", "test"))
        oracle = f"{fmt(o['base'])} / {fmt(o['head'])}" if o else "-"
        c, n = risk(work, r["id"], "cgo"), risk(work, r["id"], "nocgo")
        refused = ",".join(r["refused"]) or "-"
        parity = "same" if r["cgo_identical"] else ("same modulo signature text" if r["cgo_parity"] else "differs: " + ",".join(r["cgo_parity_fields"]))
        det = "yes" if all(r["deterministic"].values()) else str(r["deterministic"])
        empty = " (empty)" if r["kind"] == "self" and r["empty"] else ""
        print(f"| {r['id']} | {r['kind']} | {r['label']} | {oracle} | {r.get('label_holds', '-')} | {c[0]}{empty} ({c[1]}; {c[2]}) | {n[0]} ({n[1]}; {n[2]}) | {r['pairs']} / {r['refused_count']} {refused} | {det} | {parity} | {r['checkout_unchanged']} | {'yes' if not r['traceability'] else r['traceability']} |")
    print()
    for build in ("cgo", "nocgo"):
        cells = {}
        for r in rows:
            if r["kind"] != "controlled" or "error" in r or not r.get("label_holds"):
                continue
            truth = TRUTH[r["label"]] or ("same" if r["id"] in SAME else None)
            if truth is None:
                continue
            flagged = risk(work, r["id"], build)[0] == "risk_evidence"
            cells.setdefault((truth, flagged), []).append(r["id"])
        print(f"{build}: break flagged {cells.get(('break', True), [])}; break not flagged {cells.get(('break', False), [])}; "
              f"same flagged {cells.get(('same', True), [])}; same not flagged {cells.get(('same', False), [])}")


if __name__ == "__main__":
    main()

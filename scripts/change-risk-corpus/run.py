#!/usr/bin/env python3
"""Run `codegraph diff` over the pinned corpus in manifest.tsv and record, per
case: the risk assessment and its evidence, declaration pairs and refusals,
byte determinism, CGO / CGO_ENABLED=0 parity, an unmodified source checkout,
and a Go toolchain oracle on both revisions.

usage: run.py --work EMPTY_DIR --cgo BIN --nocgo BIN [--case ID ...]
"""
import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))

# A fixed identity and date make every controlled commit SHA reproducible.
COMMIT_ENV = {
    "GIT_AUTHOR_NAME": "corpus", "GIT_AUTHOR_EMAIL": "corpus@example.invalid",
    "GIT_COMMITTER_NAME": "corpus", "GIT_COMMITTER_EMAIL": "corpus@example.invalid",
    "GIT_AUTHOR_DATE": "2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE": "2026-01-01T00:00:00Z",
    "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1",
}


def replace(path, old, new, count=1):
    with open(path) as f:
        s = f.read()
    if s.count(old) != count:
        raise SystemExit(f"edit anchor in {path}: found {s.count(old)}, want {count}: {old!r}")
    with open(path, "w") as f:
        f.write(s.replace(old, new))


def cut(path, text):
    replace(path, text, "")
    return text


XTOB = """// xtob converts hex characters x1 and x2 into a byte.
func xtob(x1, x2 byte) (byte, bool) {
	b1 := xvalues[x1]
	b2 := xvalues[x2]
	return (b1 << 4) | b2, b1 != 255 && b2 != 255
}
"""
ENCODE_HEX = """func encodeHex(dst []byte, uuid UUID) {
	hex.Encode(dst, uuid[:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], uuid[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], uuid[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], uuid[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:], uuid[10:])
}
"""


def edit_uuid_move(d):
    # util.go and hash.go share their header comment and import only standard
    # packages whose names xtob never spells.
    cut(f"{d}/util.go", XTOB + "\n")
    with open(f"{d}/hash.go", "a") as f:
        f.write("\n" + XTOB)


def edit_uuid_move_header_differs(d):
    # uuid.go's copyright line names another year than util.go's.
    cut(f"{d}/util.go", XTOB + "\n")
    with open(f"{d}/uuid.go", "a") as f:
        f.write("\n" + XTOB)


def edit_uuid_remove_used(d):
    cut(f"{d}/uuid.go", ENCODE_HEX + "\n")


def edit_uuid_rename_definition(d):
    replace(f"{d}/util.go", "func xtob(", "func hexToByte(")


def edit_uuid_rename_everywhere(d):
    replace(f"{d}/util.go", "xtob", "hexToByte", 2)
    replace(f"{d}/uuid.go", "xtob(", "hexToByte(", 6)


def edit_uuid_add_parameter(d):
    replace(f"{d}/uuid.go", "func encodeHex(dst []byte, uuid UUID) {", "func encodeHex(dst []byte, uuid UUID, upper bool) {")


def edit_uuid_body_change(d):
    replace(f"{d}/util.go", "return (b1 << 4) | b2,", "return (b2 << 4) | b1,")


FUNCNAME = """// funcname removes the path prefix component of a function's name reported by func.Name().
func funcname(name string) string {
	i := strings.LastIndex(name, "/")
	name = name[i+1:]
	i = strings.Index(name, ".")
	return name[i+1:]
}
"""


def edit_errors_remove_used(d):
    cut(f"{d}/stack.go", """func callers() *stack {
	const depth = 32
	var pcs [depth]uintptr
	n := runtime.Callers(3, pcs[:])
	var st stack = pcs[0:n]
	return &st
}
""")


def edit_errors_move(d):
    cut(f"{d}/stack.go", "\n" + FUNCNAME)
    replace(f"{d}/stack.go", '\t"strings"\n', "")
    with open(f"{d}/funcname.go", "w") as f:
        f.write('package errors\n\nimport "strings"\n\n' + FUNCNAME)


EDITS = {
    "uuid-move": edit_uuid_move,
    "uuid-move-header-differs": edit_uuid_move_header_differs,
    "uuid-remove-used": edit_uuid_remove_used,
    "uuid-rename-definition": edit_uuid_rename_definition,
    "uuid-rename-everywhere": edit_uuid_rename_everywhere,
    "uuid-add-parameter": edit_uuid_add_parameter,
    "uuid-body-change": edit_uuid_body_change,
    "errors-remove-used": edit_errors_remove_used,
    "errors-move": edit_errors_move,
}


def run(cmd, cwd=None, env=None, check=True):
    p = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True)
    if check and p.returncode != 0:
        raise SystemExit(f"{' '.join(cmd)}: rc={p.returncode}\n{p.stderr.decode(errors='replace')}")
    return p


def git(repo, *args, env=None):
    return run(["git", "-C", repo, *args], env=env).stdout.decode().strip()


def read_manifest():
    repos, cases = {}, []
    with open(os.path.join(HERE, "manifest.tsv")) as f:
        for line in f:
            line = line.rstrip("\n")
            if not line or line.startswith("#"):
                continue
            row = line.split("\t")
            if row[0] == "repo":
                repos[row[1]] = {"url": row[2], "pin": row[3], "oracle_module": None if row[4] == "-" else row[4]}
            elif row[0] == "case":
                cases.append(dict(zip(["id", "repo", "kind", "base", "head", "label"], row[1:])))
    return repos, cases


def checkout_state(repo):
    """Everything a diff could change in the source checkout."""
    return {
        "head": git(repo, "rev-parse", "HEAD"),
        "symbolic": run(["git", "-C", repo, "symbolic-ref", "-q", "HEAD"], check=False).stdout.decode().strip(),
        "status": git(repo, "status", "--porcelain=v2", "--untracked-files=all", "--ignored"),
        "index": hashlib.sha256(run(["git", "-C", repo, "ls-files", "-s"]).stdout).hexdigest(),
        "refs": hashlib.sha256(run(["git", "-C", repo, "for-each-ref"]).stdout).hexdigest(),
        "stash": git(repo, "stash", "list"),
    }


def files_newer(repo, since):
    out = []
    for root, dirs, files in os.walk(repo):
        if root == repo and ".git" in dirs:
            dirs.remove(".git")
        for name in files + dirs:
            p = os.path.join(root, name)
            if os.lstat(p).st_mtime_ns > since:
                out.append(os.path.relpath(p, repo))
    return sorted(out)


def toolchain(work, repos, name, rev, cache):
    """go build, go vet and go test of one revision, exported outside the checkout."""
    key = (name, rev)
    if key in cache:
        return cache[key]
    commit = git(f"{work}/repos/{name}", "rev-parse", rev + "^{commit}")
    dest = f"{work}/oracle/{name}-{commit[:12]}"
    if os.path.exists(dest):
        shutil.rmtree(dest)
    os.makedirs(dest)
    archive = run(["git", "-C", f"{work}/repos/{name}", "archive", commit]).stdout
    subprocess.run(["tar", "-x", "-C", dest], input=archive, check=True)
    module = repos[name]["oracle_module"]
    if module and not os.path.exists(f"{dest}/go.mod"):
        with open(f"{dest}/go.mod", "w") as f:
            f.write(f"module {module}\n\ngo 1.13\n")
    env = dict(os.environ, GOFLAGS="-mod=mod", GOWORK="off")
    out = {"commit": commit}
    for step, cmd in [("build", ["go", "build", "./..."]), ("vet", ["go", "vet", "./..."]), ("test", ["go", "test", "-count=1", "./..."])]:
        if step == "test" and name == "codegraph":
            out[step] = None  # the full suite is far longer than the oracle needs
            continue
        p = run(cmd, cwd=dest, env=env, check=False)
        out[step] = p.returncode
        with open(f"{work}/results/oracle-{name}-{commit[:12]}-{step}.log", "wb") as f:
            f.write(p.stdout + p.stderr)
    cache[key] = out
    return out


def label_holds(label, b, h):
    ok = lambda r: r["build"] == 0 and r["vet"] == 0
    return {
        "compile_break": ok(b) and not ok(h),
        "behavior_break": ok(b) and ok(h) and b["test"] == 0 and h["test"] not in (0, None),
        "compiles": ok(b) and ok(h),
        "rebind_compiles": ok(b) and ok(h),
        "-": True,
    }[label]


def controlled_commit(work, case):
    name, base, how = case["repo"], case["base"], case["head"]
    edit = f"{work}/edit/{case['id']}"
    run(["git", "clone", "--quiet", "--no-checkout", f"{work}/repos/{name}", edit])
    git(edit, "checkout", "--quiet", "--detach", base)
    kind, _, arg = how.partition(":")
    if kind == "edit":
        EDITS[arg](edit)
    else:
        git(edit, "apply", os.path.join(HERE, "patches", arg))
    env = dict(os.environ, **COMMIT_ENV)
    git(edit, "add", "-A", env=env)
    git(edit, "commit", "--quiet", "--no-verify", "-m", case["id"], env=env)
    sha = git(edit, "rev-parse", "HEAD")
    git(f"{work}/repos/{name}", "fetch", "--quiet", edit, sha)
    return sha


def identity(s):
    """A declaration identity without its signature text, which the two Go
    parsers render differently."""
    v = json.loads(s)
    return (v[0], v[2], v[3], v[5])


def projection(doc, exact):
    """What CGO and CGO_ENABLED=0 builds must agree on."""
    d = doc["diff"]
    r = d["risk"]
    if exact:
        rows = lambda c: sorted(json.dumps(e, sort_keys=True) for e in c["evidence"])
    else:
        rows = lambda c: sorted((identity(e["declaration"]), identity(e["dependent"]), e["change"]) for e in c["evidence"])
    return {
        "summary": d["summary"],
        "assessment": r["assessment"], "score": r["score"], "level": r["level"],
        "components": [(c["rule"], c["count"], c["points"], rows(c)) for c in r["components"]],
        "uncertainty": sorted(json.dumps(u, sort_keys=True) for u in r["uncertainty"]) if exact else sorted(u["reason"] for u in r["uncertainty"]),
        "pairs": sorted((p["from_path"], p["to_path"], p["before"]["qualified_name"]) for p in d["declaration_pairs"]["pairs"]),
        "refused": sorted((x["from_path"], x["to_path"], x["reason"], (x.get("declaration") or {}).get("qualified_name", "")) for x in d["declaration_pairs"]["refused"]),
    }


def traceability(doc):
    """Every point of the score comes from a listed evidence row."""
    r = doc["diff"]["risk"]
    problems = []
    total = 0
    for c in r["components"]:
        if c["count"] != len(c["evidence"]):
            problems.append(f"{c['rule']}: count {c['count']} != {len(c['evidence'])} evidence rows")
        if c["points"] != min(c["count"] * c["points_each"], c["max_points"]):
            problems.append(f"{c['rule']}: points {c['points']} not min(count*{c['points_each']}, {c['max_points']})")
        for e in c["evidence"]:
            if not e.get("declaration") or not e.get("dependent") or not e.get("sites"):
                problems.append(f"{c['rule']}: evidence row without declaration, dependent or site")
        total += c["points"]
    if r["score"] is not None:
        if r["score"] != min(total, 100):
            problems.append(f"score {r['score']} != min(sum of points {total}, 100)")
        want = "risk_evidence" if r["score"] > 0 else ("limited_evidence" if r["uncertainty"] else "no_risk_evidence")
        if r["assessment"] != want:
            problems.append(f"assessment {r['assessment']}, rules say {want}")
    elif r["assessment"] != "insufficient_evidence":
        problems.append("null score without insufficient_evidence")
    return problems


def is_empty(doc):
    d = doc["diff"]
    return all(v == 0 for v in d["summary"].values()) and d["total"] == 0 and not d["declaration_pairs"]["pairs"] and not d["declaration_pairs"]["refused"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--work", required=True)
    ap.add_argument("--cgo", required=True)
    ap.add_argument("--nocgo", required=True)
    ap.add_argument("--case", action="append")
    a = ap.parse_args()
    work = os.path.abspath(a.work)
    if os.path.exists(work) and os.listdir(work):
        raise SystemExit(f"work directory must be empty: {work}")
    for sub in ("repos", "edit", "oracle", "results", "home"):
        os.makedirs(f"{work}/{sub}", exist_ok=True)
    env = dict(os.environ, CODEGRAPH_HOME=f"{work}/home", CODEGRAPH_NO_UPDATE_CHECK="1")
    repos, cases = read_manifest()
    if a.case:
        cases = [c for c in cases if c["id"] in a.case]
    for name in sorted({c["repo"] for c in cases}):
        dest = f"{work}/repos/{name}"
        run(["git", "clone", "--quiet", "--no-checkout", repos[name]["url"], dest])
        git(dest, "checkout", "--quiet", "--detach", repos[name]["pin"])
        if git(dest, "rev-parse", "HEAD") != repos[name]["pin"]:
            raise SystemExit(f"{name}: pin not checked out")
    cache, rows = {}, []
    for case in cases:
        repo = f"{work}/repos/{case['repo']}"
        head = controlled_commit(work, case) if case["kind"] == "controlled" else case["head"]
        before = checkout_state(repo)
        since = time.time_ns()
        outputs, rcs, seconds = {}, {}, {}
        for build, binary in (("cgo", a.cgo), ("nocgo", a.nocgo)):
            for i in (1, 2):
                t0 = time.monotonic()
                p = run([binary, "diff", case["base"], head, "--repo-root", repo], env=env, check=False)
                seconds[f"{build}{i}"] = round(time.monotonic() - t0, 2)
                rcs[f"{build}{i}"] = p.returncode
                outputs[f"{build}{i}"] = p.stdout
                with open(f"{work}/results/{case['id']}-{build}{i}.json", "wb") as f:
                    f.write(p.stdout)
                with open(f"{work}/results/{case['id']}-{build}{i}.err", "wb") as f:
                    f.write(p.stderr)
        after = checkout_state(repo)
        touched = files_newer(repo, since)
        row = {"id": case["id"], "repo": case["repo"], "kind": case["kind"], "label": case["label"],
               "base": git(repo, "rev-parse", case["base"] + "^{commit}"), "head": git(repo, "rev-parse", head + "^{commit}"),
               "rc": rcs, "seconds": seconds,
               "deterministic": {b: outputs[f"{b}1"] == outputs[f"{b}2"] for b in ("cgo", "nocgo")},
               "checkout_unchanged": before == after and not touched, "checkout_touched": touched}
        if any(rcs.values()):
            row["error"] = "diff failed"
            rows.append(row)
            continue
        docs = {k: json.loads(v) for k, v in outputs.items()}
        doc = docs["cgo1"]
        r = doc["diff"]["risk"]
        row.update({
            "comparison": doc["comparison"]["status"],
            "assessment": r["assessment"], "score": r["score"], "level": r["level"],
            "components": {c["rule"]: c["count"] for c in r["components"] if c["count"]},
            "uncertainty": sorted({u["reason"] for u in r["uncertainty"]}),
            "pairs": len(doc["diff"]["declaration_pairs"]["pairs"]),
            "refused": sorted({x["reason"] for x in doc["diff"]["declaration_pairs"]["refused"]}),
            "refused_count": len(doc["diff"]["declaration_pairs"]["refused"]),
            "empty": is_empty(doc),
            "cgo_parity": projection(docs["cgo1"], False) == projection(docs["nocgo1"], False),
            "cgo_identical": projection(docs["cgo1"], True) == projection(docs["nocgo1"], True),
            "traceability": sorted({p for d in docs.values() for p in traceability(d)}),
        })
        if not row["cgo_parity"]:
            pc, pn = projection(docs["cgo1"], False), projection(docs["nocgo1"], False)
            row["cgo_parity_fields"] = sorted(k for k in pc if pc[k] != pn[k])
        if case["kind"] != "self":
            b = toolchain(work, repos, case["repo"], case["base"], cache)
            h = toolchain(work, repos, case["repo"], head, cache)
            row["oracle"] = {"base": b, "head": h}
            row["label_holds"] = label_holds(case["label"], b, h)
        rows.append(row)
        print(json.dumps(row, sort_keys=True), flush=True)
    with open(f"{work}/results/results.json", "w") as f:
        json.dump(rows, f, indent=1, sort_keys=True)
        f.write("\n")


if __name__ == "__main__":
    main()

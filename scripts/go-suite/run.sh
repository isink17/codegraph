#!/usr/bin/env bash
# Runs the full Go test suite once, bounded per package, and reports each
# package as it finishes. Policy and timeout rationale: docs/go-suite.md.
#
# Usage: scripts/go-suite/run.sh [package pattern...]   (default ./...)
#        scripts/go-suite/run.sh summarize EVENTS.jsonl [GO_TEST_EXIT]
#            (summary of a saved run)
# Env: OUT (output dir, default a new temp dir), TIMEOUT (per test binary,
#      default 20m). Writes OUT/events.jsonl and OUT/summary.json.
# Exit: 0 only when go test succeeded and every package passed or had no tests.
set -uo pipefail

# One record per package: pass, no_tests, fail, timeout, build_failed or
# incomplete (started but never finished, e.g. the run was killed). The
# timeout panic is attributed to the running test, and that test gets no
# fail event of its own. A line that is not JSON (a run killed mid-write) is
# counted as malformed and fails the run.
SUMMARY_JQ='
	(map(select(.Action == "malformed")) | length) as $malformed |
	reduce .[] as $e ({};
		if $e.Action == "start" then .[$e.Package] //= {package: $e.Package, result: "incomplete", tests: {pass: 0, fail: 0, skip: 0}}
		elif $e.Package == null then .
		elif $e.Action == "output" and $e.Test != null and ($e.Output | startswith("panic: test timed out after")) then .[$e.Package].timed_out = true
		elif $e.Test != null and ($e.Action | IN("pass", "fail", "skip")) then .[$e.Package].tests[$e.Action] += 1
		elif $e.Test == null and ($e.Action | IN("pass", "fail", "skip")) then
			.[$e.Package].elapsed = $e.Elapsed |
			.[$e.Package].result = (
				if $e.Action == "pass" then "pass"
				elif $e.Action == "skip" then "no_tests"
				elif $e.FailedBuild then "build_failed"
				elif .[$e.Package].timed_out then "timeout"
				else "fail" end)
		else . end)
	| [.[] | del(.timed_out)] | sort_by(.package)
	| {packages: ., malformed_lines: $malformed, counts: (group_by(.result) | map({key: .[0].result, value: length}) | from_entries),
	   all_passed: (length > 0 and $malformed == 0 and all(.[]; .result | IN("pass", "no_tests")))}'

# summarize EVENTS GO_TEST_EXIT: the run passed only when go test also exited 0.
summarize() {
	jq -R -c 'fromjson? // {Action: "malformed"}' "$1" | jq -s "$SUMMARY_JQ" |
		jq --argjson rc "$2" '{go_test_exit: $rc} + . | .all_passed = (.all_passed and $rc == 0)'
}

if [ "${1:-}" = summarize ]; then
	[ $# -eq 2 ] || [ $# -eq 3 ] || { echo "usage: $0 summarize EVENTS.jsonl [GO_TEST_EXIT]" >&2; exit 2; }
	summarize "$2" "${3:-0}"
	exit
fi

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${OUT:-$(mktemp -d "${TMPDIR:-/tmp}/go-suite.XXXXXX")} || exit 1
TIMEOUT=${TIMEOUT:-20m}
mkdir -p "$OUT" || exit 1
[ $# -gt 0 ] || set -- ./...
echo "go-suite: timeout=$TIMEOUT out=$OUT packages=$*"
start=$SECONDS
(cd "$ROOT" && go test -json -count=1 -timeout "$TIMEOUT" "$@") 2>"$OUT/stderr.log" | tee "$OUT/events.jsonl" |
	jq --unbuffered -r 'select(.Test == null and (.Action | IN("pass", "fail", "skip"))) |
		"\(.Action | ascii_upcase)\t\(.Package)\t\(.Elapsed)s" + (if .FailedBuild then "\t[build failed]" else "" end)'
rc=${PIPESTATUS[0]}
summarize "$OUT/events.jsonl" "$rc" | jq --argjson wall $((SECONDS - start)) --arg timeout "$TIMEOUT" \
	'{wall_seconds: $wall, timeout_per_package: $timeout} + .' >"$OUT/summary.json"
jq -r '"go-suite: exit=\(.go_test_exit) wall=\(.wall_seconds)s \(.counts | to_entries | map("\(.key)=\(.value)") | join(" "))"' "$OUT/summary.json"
jq -r '.packages[] | select(.result | IN("pass", "no_tests") | not) | "go-suite: \(.result): \(.package)"' "$OUT/summary.json"
jq -e .all_passed "$OUT/summary.json" >/dev/null

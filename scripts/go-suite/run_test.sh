#!/usr/bin/env bash
# Tests for the go-suite summary. Run: bash scripts/go-suite/run_test.sh
# testdata/events.jsonl is a real `go test -json -timeout 1s` stream over five
# packages: pass with a skipped test, timeout, failing test, build failure,
# and no test files (stack traces and timestamps trimmed).
set -uo pipefail

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/go-suite-test.XXXXXX") || exit 1
trap 'rm -rf "$work"' EXIT
failures=0

expect() {
	local name=$1 want=$2 got=$3
	if [ "$want" != "$got" ]; then
		echo "FAIL $name: want '$want', got '$got'"
		failures=$((failures + 1))
	fi
}
summary() { bash "$here/run.sh" summarize "$@"; }

events=$here/testdata/events.jsonl
expect results "m/a=pass m/b=timeout m/c=fail m/d=build_failed m/e=no_tests" \
	"$(summary "$events" | jq -r '[.packages[] | "\(.package)=\(.result)"] | join(" ")')"
expect counted-tests "1 0 1" "$(summary "$events" | jq -r '.packages[0].tests | "\(.pass) \(.fail) \(.skip)"')"
expect failing-run-not-passed false "$(summary "$events" | jq .all_passed)"

grep -E '"Package":"m/[ae]"' "$events" >"$work/passing.jsonl"
expect passing-run true "$(summary "$work/passing.jsonl" | jq .all_passed)"

# A package that started but never finished (the run was killed) is not a pass.
{ cat "$work/passing.jsonl"; echo '{"Action":"start","Package":"m/x"}'; } >"$work/killed.jsonl"
expect killed-package incomplete "$(summary "$work/killed.jsonl" | jq -r '.packages[] | select(.package == "m/x") | .result')"
expect killed-run-not-passed false "$(summary "$work/killed.jsonl" | jq .all_passed)"

# go test failing outside any package event still fails the run.
expect nonzero-exit-not-passed false "$(summary "$work/passing.jsonl" 1 | jq .all_passed)"

# A run killed mid-write ends with half a line: still summarized, never a pass.
{ cat "$work/killed.jsonl"; printf '{"Action":"pass","Pack'; } >"$work/truncated.jsonl"
expect truncated-run "m/x=incomplete 1 false" \
	"$(summary "$work/truncated.jsonl" | jq -r '"\(.packages[] | select(.package == "m/x") | "\(.package)=\(.result)") \(.malformed_lines) \(.all_passed)"')"

: >"$work/empty.jsonl"
expect empty-run-not-passed false "$(summary "$work/empty.jsonl" | jq .all_passed)"

if [ "$failures" -ne 0 ]; then
	echo "$failures failure(s)"
	exit 1
fi
echo "go-suite: all tests passed"

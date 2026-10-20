#!/usr/bin/env bash
# Runs every internal/store test under -race in independently completing
# chunks. Under -race the pure-Go SQLite driver is itself instrumented, so one
# whole-package run takes longer than a practical go test timeout.
#
# Usage: scripts/store-race/run.sh [chunk...]   (no args: every chunk)
# Env: OUT (log dir, default ./store-race-out), CHUNK (tests per chunk, 40),
#      TIMEOUT (per chunk, 30m).
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=${OUT:-$PWD/store-race-out}
CHUNK=${CHUNK:-40}
TIMEOUT=${TIMEOUT:-30m}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
BIN=$OUT/store.race.test

(cd "$ROOT" && go test -race -c -o "$BIN" ./internal/store) || exit 1
cd "$ROOT/internal/store" || exit 1
"$BIN" -test.list '.*' | grep -E '^(Test|Fuzz|Example)' >"$OUT/tests.txt" || exit 1
split -l "$CHUNK" -a 3 -d "$OUT/tests.txt" "$OUT/chunk."
chunks=("$OUT"/chunk.[0-9][0-9][0-9])
total=$(wc -l <"$OUT/tests.txt")
echo "tests=$total chunks=${#chunks[@]} chunk=$CHUNK timeout=$TIMEOUT" | tee "$OUT/plan.txt"

if [[ $# -gt 0 ]]; then
	selected=()
	for n in "$@"; do selected+=("$(printf '%s/chunk.%03d' "$OUT" "$n")"); done
else
	selected=("${chunks[@]}")
fi

status=0
for c in "${selected[@]}"; do
	name=$(basename "$c")
	want=$(wc -l <"$c")
	pattern="^($(paste -sd'|' "$c"))\$"
	start=$(date +%s)
	"$BIN" -test.run "$pattern" -test.count=1 -test.timeout "$TIMEOUT" -test.v >"$OUT/$name.log" 2>&1
	rc=$?
	wall=$(($(date +%s) - start))
	pass=$(grep -c '^--- PASS: ' "$OUT/$name.log")
	fail=$(grep -c '^--- FAIL: ' "$OUT/$name.log")
	skip=$(grep -c '^--- SKIP: ' "$OUT/$name.log")
	races=$(grep -c '^WARNING: DATA RACE' "$OUT/$name.log")
	ok=yes
	[[ $rc -ne 0 || $((pass + fail + skip)) -ne $want ]] && ok=no
	[[ $ok == no ]] && status=1
	printf '%s\ttests=%s\tpass=%s\tfail=%s\tskip=%s\traces=%s\twall=%ss\trc=%s\tcomplete=%s\n' \
		"$name" "$want" "$pass" "$fail" "$skip" "$races" "$wall" "$rc" "$ok" | tee -a "$OUT/summary.tsv"
done
exit $status

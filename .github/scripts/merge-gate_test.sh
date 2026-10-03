#!/usr/bin/env bash
# Tests for merge-gate.sh against a stub gh. Run:
#   bash .github/scripts/merge-gate_test.sh
set -uo pipefail

script="$(cd "$(dirname "$0")" && pwd)/merge-gate.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0

sha=1234567890abcdef1234567890abcdef12345678
other=abcdefabcdefabcdefabcdefabcdefabcdefabcd

# The stub answers from environment variables and logs merge calls.
mkdir "$work/bin"
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
case "$1 $2" in
"pr view")
	if [[ "$*" == *mergeCommit* ]]; then echo "MERGED fedcba"; else echo "$STUB_PR"; fi ;;
"pr merge") echo "$*" >>"$STUB_LOG" ;;
"api repos/{owner}/{repo}/compare/"*) echo "$STUB_BEHIND" ;;
"api repos/{owner}/{repo}/commits/"*) printf '%b' "$STUB_CHECKS" ;;
*) echo "unexpected gh $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$work/bin/gh"

review_ok() {
	printf 'Independent review\nPR: #42\nReviewed-Head: %s\nVerdict: APPROVE\nBlocking: 0\n' "$sha"
}
green='ci\tcompleted\tsuccess\nquality\tcompleted\tskipped\nchanges\tcompleted\tsuccess\n'

# case NAME WANT(pass|block) [ARGS...] — environment overrides apply.
case_() {
	local name=$1 want=$2
	shift 2
	local got
	if PATH="$work/bin:$PATH" STUB_LOG="$work/merge.log" \
		STUB_PR="${PR_JSON:-OPEN false v2.0 $sha MERGEABLE}" \
		STUB_BEHIND="${BEHIND:-0}" STUB_CHECKS="${CHECKS-$green}" \
		bash "$script" "$@" >/dev/null 2>&1; then got=pass; else got=block; fi
	if [ "$got" != "$want" ]; then
		echo "FAIL $name: want $want, got $got"
		failures=$((failures + 1))
	fi
}

review_ok >"$work/review.md"
r="$work/review.md"

case_ verify-ok pass 42 "$sha" "$r"
case_ short-sha block 42 1234567 "$r"
case_ bad-pr block x "$sha" "$r"
case_ bad-mode block 42 "$sha" "$r" --force
case_ missing-review block 42 "$sha" "$work/none.md"
PR_JSON="OPEN false v2.0 $other MERGEABLE" case_ head-moved block 42 "$sha" "$r"
PR_JSON="OPEN true v2.0 $sha MERGEABLE" case_ draft block 42 "$sha" "$r"
PR_JSON="OPEN false master $sha MERGEABLE" case_ master-base block 42 "$sha" "$r"
PR_JSON="MERGED false v2.0 $sha UNKNOWN" case_ not-open block 42 "$sha" "$r"
PR_JSON="OPEN false v2.0 $sha CONFLICTING" case_ conflict block 42 "$sha" "$r"
BEHIND=3 case_ behind-base block 42 "$sha" "$r"
CHECKS='ci\tin_progress\t\n' case_ pending-check block 42 "$sha" "$r"
CHECKS='ci\tcompleted\tsuccess\nrace\tcompleted\tfailure\n' case_ failed-check block 42 "$sha" "$r"
CHECKS='ci\tcompleted\tsuccess\nrace\tcompleted\tcancelled\n' case_ cancelled-check block 42 "$sha" "$r"
CHECKS='quality\tcompleted\tsuccess\n' case_ no-ci-check block 42 "$sha" "$r"
CHECKS='ci\tcompleted\tskipped\n' case_ ci-skipped block 42 "$sha" "$r"
CHECKS='' case_ no-checks block 42 "$sha" "$r"

review_ok | sed "s/$sha/$other/" >"$work/r1.md"
case_ review-other-head block 42 "$sha" "$work/r1.md"
review_ok | sed 's/PR: #42/PR: #41/' >"$work/r2.md"
case_ review-other-pr block 42 "$sha" "$work/r2.md"
review_ok | sed 's/APPROVE/BLOCK/' >"$work/r3.md"
case_ review-block block 42 "$sha" "$work/r3.md"
review_ok | sed 's/Blocking: 0/Blocking: 2/' >"$work/r4.md"
case_ review-blocking-findings block 42 "$sha" "$work/r4.md"
{ review_ok; echo 'Verdict: BLOCK'; } >"$work/r5.md"
case_ review-approve-then-block block 42 "$sha" "$work/r5.md"
grep -v '^Verdict' "$r" >"$work/r6.md"
case_ review-started-only block 42 "$sha" "$work/r6.md"
# A prefix of the SHA in the review is not the head.
review_ok | sed "s/$sha/${sha:0:12}/" >"$work/r7.md"
case_ review-short-head block 42 "$sha" "$work/r7.md"

: >"$work/merge.log"
case_ merge-blocked-no-call block 42 "$sha" "$work/r3.md" --merge
[ ! -s "$work/merge.log" ] || { echo "FAIL merge-blocked-no-call: gh pr merge was called"; failures=$((failures + 1)); }
case_ merge-ok pass 42 "$sha" "$r" --merge
grep -qx "pr merge 42 --squash --match-head-commit $sha" "$work/merge.log" ||
	{ echo "FAIL merge-ok: merge call was: $(cat "$work/merge.log")"; failures=$((failures + 1)); }

if [ "$failures" -ne 0 ]; then
	echo "$failures failure(s)"
	exit 1
fi
echo "merge-gate: all tests passed"

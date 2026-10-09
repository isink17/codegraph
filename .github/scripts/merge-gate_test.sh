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
	if [[ "$*" == *mergeCommit* ]]; then echo "${STUB_AFTER:-MERGED fedcba}"; else echo "$STUB_PR"; fi ;;
"pr merge") echo "$*" >>"$STUB_LOG" ;;
"api repos/{owner}/{repo}/compare/"*)
	# The compare must be against the base the gate was asked for.
	[[ "$2" == "repos/{owner}/{repo}/compare/${STUB_WANT_BASE:-v2.0}...$STUB_SHA" ]] || { echo "unexpected compare $2" >&2; exit 1; }
	echo "$STUB_BEHIND" ;;
"api --paginate") printf '%b' "$STUB_CHECKS" ;;
"api repos/{owner}/{repo}/commits/"*) echo "$STUB_STATUS" ;;
*) echo "unexpected gh $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$work/bin/gh"

review_ok() {
	printf 'PR: #42\nReviewed-Head: %s\nVerdict: APPROVE\nBlocking: 0\n\nFindings.\n' "$sha"
}
green='ci\tcompleted\tsuccess\tgithub-actions\nquality\tcompleted\tskipped\tgithub-actions\nchanges\tcompleted\tsuccess\tgithub-actions\n'

# case NAME WANT(pass|block) [ARGS...] — environment overrides apply.
case_() {
	local name=$1 want=$2
	shift 2
	local got
	if PATH="$work/bin:$PATH" STUB_LOG="$work/merge.log" \
		STUB_PR="${PR_JSON:-OPEN false v2.0 $sha MERGEABLE}" \
		STUB_BEHIND="${BEHIND:-0}" STUB_CHECKS="${CHECKS-$green}" \
		STUB_STATUS="${STATUS:-0 pending}" STUB_AFTER="${AFTER:-}" \
		STUB_WANT_BASE="${WANT_BASE:-v2.0}" STUB_SHA="$sha" \
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
CHECKS='ci\tin_progress\t\tgithub-actions\n' case_ pending-check block 42 "$sha" "$r"
CHECKS='ci\tcompleted\tsuccess\tgithub-actions\nrace\tcompleted\tfailure\tgithub-actions\n' case_ failed-check block 42 "$sha" "$r"
CHECKS='ci\tcompleted\tsuccess\tgithub-actions\nrace\tcompleted\tcancelled\tgithub-actions\n' case_ cancelled-check block 42 "$sha" "$r"
CHECKS='quality\tcompleted\tsuccess\tgithub-actions\n' case_ no-ci-check block 42 "$sha" "$r"
CHECKS='ci\tcompleted\tskipped\tgithub-actions\n' case_ ci-skipped block 42 "$sha" "$r"
CHECKS='' case_ no-checks block 42 "$sha" "$r"
CHECKS='ci\tcompleted\tsuccess\tsome-other-app\n' case_ ci-from-other-app block 42 "$sha" "$r"
STATUS='2 failure' case_ legacy-status-failure block 42 "$sha" "$r"
STATUS='1 pending' case_ legacy-status-pending block 42 "$sha" "$r"
STATUS='3 success' case_ legacy-status-success pass 42 "$sha" "$r"

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
# Quoted header lines further down never count.
printf 'PR: #42\nReviewed-Head: %s\nVerdict: APPROVE\nBlocking: 2\n\n> Blocking: 0\nBlocking: 0\n' "$sha" >"$work/r8.md"
case_ review-spoofed-blocking block 42 "$sha" "$work/r8.md"
printf 'Notes\nPR: #42\nReviewed-Head: %s\nVerdict: APPROVE\nBlocking: 0\n' "$sha" >"$work/r9.md"
case_ review-header-not-first block 42 "$sha" "$work/r9.md"
printf 'PR: #42\nReviewed-Head: %s\nVerdict: APPROVE\nBlocking: 0\nReviewed-Head: %s\n' "$other" "$sha" >"$work/r10.md"
case_ review-old-head-new-sha-later block 42 "$sha" "$work/r10.md"
# A prefix of the SHA in the review is not the head.
review_ok | sed "s/$sha/${sha:0:12}/" >"$work/r7.md"
case_ review-short-head block 42 "$sha" "$work/r7.md"

: >"$work/merge.log"
case_ merge-blocked-no-call block 42 "$sha" "$work/r3.md" --merge
[ ! -s "$work/merge.log" ] || { echo "FAIL merge-blocked-no-call: gh pr merge was called"; failures=$((failures + 1)); }
case_ merge-ok pass 42 "$sha" "$r" --merge
grep -qx "pr merge 42 --squash --match-head-commit $sha" "$work/merge.log" ||
	{ echo "FAIL merge-ok: merge call was: $(cat "$work/merge.log")"; failures=$((failures + 1)); }

# Base selection: v2.0 by default, the listed integration branch only when
# asked for explicitly, and never master or an unlisted branch.
ib=integration/v2.0-wave-20261008
case_ default-base-is-v2 pass 42 "$sha" "$r"
PR_JSON="OPEN false $ib $sha MERGEABLE" case_ integration-pr-default-base block 42 "$sha" "$r"
PR_JSON="OPEN false $ib $sha MERGEABLE" WANT_BASE=$ib case_ integration-ok pass --base "$ib" 42 "$sha" "$r"
WANT_BASE=$ib case_ v2-pr-claims-integration block --base "$ib" 42 "$sha" "$r"
PR_JSON="OPEN false master $sha MERGEABLE" WANT_BASE=master case_ base-master-refused block --base master 42 "$sha" "$r"
PR_JSON="OPEN false integration/v2.0-wave-20990101 $sha MERGEABLE" WANT_BASE=integration/v2.0-wave-20990101 \
	case_ base-unlisted-integration block --base integration/v2.0-wave-20990101 42 "$sha" "$r"
PR_JSON="OPEN false integration/v2.0 $sha MERGEABLE" WANT_BASE=integration/v2.0 case_ base-prefix-refused block --base integration/v2.0 42 "$sha" "$r"
case_ base-missing-value block --base
ib3=integration/v2.0-wave-20261009
PR_JSON="OPEN false $ib3 $sha MERGEABLE" WANT_BASE=$ib3 case_ wave3-integration-ok pass --base "$ib3" 42 "$sha" "$r"
PR_JSON="OPEN false $ib3 $sha MERGEABLE" case_ wave3-pr-default-base block 42 "$sha" "$r"
PR_JSON="OPEN false $ib3 $sha MERGEABLE" WANT_BASE=$ib case_ wave3-pr-claims-wave2 block --base "$ib" 42 "$sha" "$r"
PR_JSON="OPEN false integration/v2.0-wave-2026100 $sha MERGEABLE" WANT_BASE=integration/v2.0-wave-2026100 case_ wave3-prefix-refused block --base integration/v2.0-wave-2026100 42 "$sha" "$r"
PR_JSON="OPEN false $ib $other MERGEABLE" WANT_BASE=$ib case_ integration-stale-head block --base "$ib" 42 "$sha" "$r"
PR_JSON="OPEN false $ib $sha MERGEABLE" WANT_BASE=$ib BEHIND=2 case_ integration-behind block --base "$ib" 42 "$sha" "$r"
PR_JSON="OPEN false $ib $sha MERGEABLE" WANT_BASE=$ib case_ integration-missing-review block --base "$ib" 42 "$sha" "$work/none.md"
PR_JSON="OPEN false $ib $sha MERGEABLE" WANT_BASE=$ib case_ integration-review-other-head block --base "$ib" 42 "$sha" "$work/r1.md"
PR_JSON="OPEN false $ib $sha MERGEABLE" WANT_BASE=$ib CHECKS='' case_ integration-no-ci block --base "$ib" 42 "$sha" "$r"
PR_JSON="OPEN false $ib $sha MERGEABLE" WANT_BASE=$ib CHECKS='ci\tcompleted\tfailure\tgithub-actions\n' \
	case_ integration-ci-failed block --base "$ib" 42 "$sha" "$r"
PR_JSON="OPEN false $ib $sha MERGEABLE" WANT_BASE=$ib case_ integration-short-sha block --base "$ib" 42 1234567 "$r"
: >"$work/merge.log"
PR_JSON="OPEN false $ib $sha MERGEABLE" WANT_BASE=$ib case_ integration-merge pass --base "$ib" 42 "$sha" "$r" --merge
grep -qx "pr merge 42 --squash --match-head-commit $sha" "$work/merge.log" ||
	{ echo "FAIL integration-merge: merge call was: $(cat "$work/merge.log")"; failures=$((failures + 1)); }

# The merge is read back: a PR still open afterwards fails the gate.
AFTER='OPEN ' case_ merge-not-read-back block 42 "$sha" "$r" --merge

if [ "$failures" -ne 0 ]; then
	echo "$failures failure(s)"
	exit 1
fi
echo "merge-gate: all tests passed"

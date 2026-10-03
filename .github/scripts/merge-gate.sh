#!/usr/bin/env bash
# Coordinator merge gate for pull requests into v2.0.
#
#   merge-gate.sh PR HEAD_SHA REVIEW_FILE [--merge]
#
# Verifies, and with --merge squash-merges, only when all of these hold:
#   - the PR is open, not a draft, and targets v2.0;
#   - its head is exactly HEAD_SHA (full 40-character SHA);
#   - REVIEW_FILE is a completed independent review of that exact head:
#     lines "PR: #<PR>", "Reviewed-Head: <HEAD_SHA>", "Verdict: APPROVE"
#     and "Blocking: 0", and no "Verdict: BLOCK";
#   - every check run on HEAD_SHA has completed as success, skipped or
#     neutral, and the aggregate "ci" check succeeded;
#   - the head contains the current v2.0 tip (CI ran against today's base)
#     and GitHub reports no conflict.
# The merge passes --match-head-commit, so GitHub refuses it if the head
# moved after verification; the result is read back afterwards.
set -euo pipefail

base=v2.0

fail() {
	echo "merge-gate: BLOCKED: $*" >&2
	exit 1
}

[ $# -ge 3 ] || { echo "usage: $0 PR HEAD_SHA REVIEW_FILE [--merge]" >&2; exit 2; }
pr=$1 sha=$2 review=$3 mode=${4:-verify}
case "$mode" in verify | --merge) ;; *) fail "unknown mode $mode" ;; esac
[[ "$pr" =~ ^[0-9]+$ ]] || fail "PR must be a number: $pr"
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || fail "HEAD_SHA must be a full 40-character SHA: $sha"

# Review: must name this PR and this exact head, approve, and list no
# blocking findings.
[ -f "$review" ] || fail "review file not found: $review"
grep -qx "PR: #$pr" "$review" || fail "review does not name PR #$pr"
grep -qx "Reviewed-Head: $sha" "$review" || fail "review does not name head $sha"
grep -qx "Verdict: APPROVE" "$review" || fail "review verdict is not APPROVE"
! grep -q "^Verdict: BLOCK" "$review" || fail "review contains a BLOCK verdict"
grep -qx "Blocking: 0" "$review" || fail "review does not report zero blocking findings"

# Pull request state.
state=$(gh pr view "$pr" --json state,isDraft,baseRefName,headRefOid,mergeable \
	--jq '[.state, (.isDraft|tostring), .baseRefName, .headRefOid, .mergeable] | join(" ")')
read -r pr_state draft pr_base head mergeable <<<"$state"
[ "$pr_state" = OPEN ] || fail "PR is $pr_state"
[ "$draft" = false ] || fail "PR is a draft"
[ "$pr_base" = "$base" ] || fail "PR targets $pr_base, not $base"
[ "$head" = "$sha" ] || fail "PR head is $head, reviewed head is $sha"
[ "$mergeable" = MERGEABLE ] || fail "GitHub reports mergeable=$mergeable"

# Base: the head must contain the current base tip, so the checks ran
# against the code that will be merged into.
behind=$(gh api "repos/{owner}/{repo}/compare/$base...$sha" --jq .behind_by)
[ "$behind" = 0 ] || fail "head is $behind commit(s) behind $base; update the branch, rerun CI and re-review the new head"

# Checks on the exact head.
checks=$(gh api "repos/{owner}/{repo}/commits/$sha/check-runs?per_page=100" \
	--jq '.check_runs[] | "\(.name)\t\(.status)\t\(.conclusion)"')
[ -n "$checks" ] || fail "no check runs on $sha"
ci_ok=false
while IFS=$'\t' read -r name status conclusion; do
	[ "$status" = completed ] || fail "check '$name' is $status"
	case "$conclusion" in
	success | skipped | neutral) ;;
	*) fail "check '$name' concluded $conclusion" ;;
	esac
	if [ "$name" = ci ] && [ "$conclusion" = success ]; then ci_ok=true; fi
done <<<"$checks"
[ "$ci_ok" = true ] || fail "aggregate 'ci' check has not succeeded on $sha"

echo "merge-gate: PASS: PR #$pr head $sha (reviewed, checks green, up to date with $base, mergeable)"
[ "$mode" = --merge ] || exit 0

gh pr merge "$pr" --squash --match-head-commit "$sha"
merged=$(gh pr view "$pr" --json state,mergeCommit --jq '[.state, .mergeCommit.oid] | join(" ")')
read -r merged_state merge_commit <<<"$merged"
[ "$merged_state" = MERGED ] || fail "after merge the PR is $merged_state"
echo "merge-gate: MERGED: PR #$pr as $merge_commit"

#!/usr/bin/env bash
# Coordinator merge gate for pull requests into v2.0 or an authorized
# integration (staging) branch.
#
#   merge-gate.sh [--base BASE] PR HEAD_SHA REVIEW_FILE [--merge]
#
# BASE defaults to v2.0. Any other base must be listed exactly in
# allowed_bases below; master and arbitrary branches are refused.
#
# Verifies, and with --merge squash-merges, only when all of these hold:
#   - the PR is open, not a draft, and targets BASE;
#   - its head is exactly HEAD_SHA (full 40-character SHA);
#   - REVIEW_FILE is a completed independent review of that exact head:
#     its first four lines are exactly "PR: #<PR>",
#     "Reviewed-Head: <HEAD_SHA>", "Verdict: APPROVE" and "Blocking: 0";
#   - every check run on HEAD_SHA has completed as success, skipped or
#     neutral, the aggregate "ci" check from GitHub Actions succeeded, and
#     no legacy commit status is failing or pending;
#   - the head contains the current BASE tip (CI ran against today's base)
#     and GitHub reports no conflict.
# The merge passes --match-head-commit, so GitHub refuses it if the head
# moved after verification; the result is read back afterwards.
set -euo pipefail

# Integration branches are staging targets for one wave each. Add a branch
# here only in the change that creates it, and remove it once the wave has
# merged and nothing targets it; the list is reviewed like code.
allowed_bases=(v2.0 integration/v2.0-wave-20261009 integration/v2.0-wave-20261010)

fail() {
	echo "merge-gate: BLOCKED: $*" >&2
	exit 1
}

base=v2.0
if [ "${1:-}" = --base ]; then
	[ $# -ge 2 ] || fail "--base needs a branch name"
	base=$2
	shift 2
fi
base_ok=false
for b in "${allowed_bases[@]}"; do
	if [ "$base" = "$b" ]; then base_ok=true; fi
done
[ "$base_ok" = true ] || fail "base '$base' is not an authorized merge target"

[ $# -ge 3 ] || { echo "usage: $0 [--base BASE] PR HEAD_SHA REVIEW_FILE [--merge]" >&2; exit 2; }
pr=$1 sha=$2 review=$3 mode=${4:-verify}
case "$mode" in verify | --merge) ;; *) fail "unknown mode $mode" ;; esac
[[ "$pr" =~ ^[0-9]+$ ]] || fail "PR must be a number: $pr"
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || fail "HEAD_SHA must be a full 40-character SHA: $sha"

# Review: the first four lines are the verdict header, exactly. Quoted
# text further down the report can never satisfy or override it.
[ -f "$review" ] || fail "review file not found: $review"
want_header=$(printf 'PR: #%s\nReviewed-Head: %s\nVerdict: APPROVE\nBlocking: 0' "$pr" "$sha")
[ "$(head -n 4 "$review")" = "$want_header" ] ||
	fail "review header is not 'PR: #$pr / Reviewed-Head: $sha / Verdict: APPROVE / Blocking: 0'"
# Any blocking verdict or count anywhere in the report also stops the merge.
! grep -qE '^(Verdict: BLOCK|Blocking: [1-9])' "$review" || fail "review reports a block or blocking findings"

# Pull request state.
state=$(gh pr view "$pr" --json state,isDraft,baseRefName,headRefOid,mergeable \
	--jq '[.state, (.isDraft|tostring), .baseRefName, .headRefOid, .mergeable] | join(" ")')
read -r pr_state draft pr_base head mergeable <<<"$state"
[ "$pr_state" = OPEN ] || fail "PR is $pr_state"
[ "$draft" = false ] || fail "PR is a draft"
[ "$pr_base" = "$base" ] || fail "PR targets $pr_base, not $base"
[ "$head" = "$sha" ] || fail "PR head is $head, reviewed head is $sha"
[ "$mergeable" = MERGEABLE ] || fail "GitHub reports mergeable=$mergeable (UNKNOWN right after a push: rerun shortly)"

# Base: the head must contain the current base tip, so the checks ran
# against the code that will be merged into.
behind=$(gh api "repos/{owner}/{repo}/compare/$base...$sha" --jq .behind_by)
[ "$behind" = 0 ] || fail "head is $behind commit(s) behind $base; update the branch, rerun CI and re-review the new head"

# Checks on the exact head, every page.
checks=$(gh api --paginate "repos/{owner}/{repo}/commits/$sha/check-runs?per_page=100" \
	--jq '.check_runs[] | "\(.name)\t\(.status)\t\(.conclusion)\t\(.app.slug)"')
[ -n "$checks" ] || fail "no check runs on $sha"
ci_ok=false
while IFS=$'\t' read -r name status conclusion app; do
	[ "$status" = completed ] || fail "check '$name' is $status"
	case "$conclusion" in
	success | skipped | neutral) ;;
	*) fail "check '$name' concluded $conclusion" ;;
	esac
	if [ "$name" = ci ] && [ "$app" = github-actions ] && [ "$conclusion" = success ]; then ci_ok=true; fi
done <<<"$checks"
[ "$ci_ok" = true ] || fail "aggregate 'ci' check from GitHub Actions has not succeeded on $sha"

# Legacy commit statuses, if any were ever posted, must be green too.
statuses=$(gh api "repos/{owner}/{repo}/commits/$sha/status" --jq '"\(.total_count) \(.state)"')
case "$statuses" in
"0 "* | *" success") ;;
*) fail "commit status on $sha is $statuses" ;;
esac

echo "merge-gate: PASS: PR #$pr head $sha (reviewed, checks green, up to date with $base, mergeable)"
[ "$mode" = --merge ] || exit 0

gh pr merge "$pr" --squash --match-head-commit "$sha"
merged=$(gh pr view "$pr" --json state,mergeCommit --jq '[.state, .mergeCommit.oid] | join(" ")')
read -r merged_state merge_commit <<<"$merged"
[ "$merged_state" = MERGED ] || fail "after merge the PR is $merged_state"
echo "merge-gate: MERGED: PR #$pr as $merge_commit"

#!/usr/bin/env bash
# Tests for pool.sh against a throwaway pool; never touches the live pool.
set -euo pipefail

POOL=$(cd "$(dirname "$0")" && pwd)/pool.sh
T=$(mktemp -d "${TMPDIR:-/tmp}/pool-test.XXXXXX") || exit 1
T=$(cd "$T" && pwd -P) || exit 1
trap 'rm -rf "$T"' EXIT
export CG_POOL_ROOT=$T/slots CG_POOL_LEASES=$T/leases CG_POOL_CWD_FILE=$T/cwds CG_POOL_REPO=$T/seed
: >"$T/cwds"
fails=0

fail() { echo "FAIL: $*"; fails=$((fails + 1)); }
expect() { # expect <exit code> <command...>
	local want=$1 got=0
	shift
	"$@" >"$T/out" 2>&1 || got=$?
	[[ $got == "$want" ]] || fail "line ${BASH_LINENO[0]}: $* exited $got, want $want: $(cat "$T/out")"
}
state() { "$POOL" status "$1" | sed -n 1p | sed 's/^[^:]*: //'; }
token() { sed -n 's/^token=//p' "$CG_POOL_LEASES/$1.lease"; }
g() { git -c user.name=t -c user.email=t@t -c init.defaultBranch=main "$@"; }
# Commits a new file <name>.txt in slot $1.
change() {
	echo "$2" >"$CG_POOL_ROOT/$1/$2.txt"
	g -C "$CG_POOL_ROOT/$1" add "$2.txt"
	g -C "$CG_POOL_ROOT/$1" commit -q -m "$2"
}

g init -q --bare "$T/origin.git"
g clone -q "$T/origin.git" "$T/seed" 2>/dev/null
g -C "$T/seed" commit -q --allow-empty -m base
g -C "$T/seed" push -q origin HEAD:v2.0
mkdir -p "$CG_POOL_ROOT"
for s in codegraph-worker-a codegraph-worker-b codegraph-review; do
	g -C "$T/seed" worktree add -q --detach "$CG_POOL_ROOT/$s" origin/v2.0
done
A=codegraph-worker-a B=codegraph-worker-b R=codegraph-review

# Two simultaneous attempts on one worker: exactly one wins.
for i in 1 2 3 4 5 6 7 8; do
	("$POOL" acquire --slot $A --task t$i --agent ag$i >"$T/race$i" 2>&1 && echo ok >>"$T/race$i") &
done
wait
wins=$(grep -l '^ok$' "$T"/race? | wc -l | tr -d ' ')
[[ $wins == 1 ]] || fail "same-slot race: $wins winners"
[[ $(state $A) == busy ]] || fail "leased slot state $(state $A)"
expect 1 "$POOL" acquire --slot $A --task again --agent x

# Clean but leased worker stays busy; wrong token and double release refused.
TA=$(token $A)
expect 1 "$POOL" release --slot $A --token wrong
[[ $(token $A) == "$TA" ]] || fail "wrong-token release changed the lease"
expect 0 "$POOL" release --slot $A --token "$TA"
expect 1 "$POOL" release --slot $A --token "$TA"
[[ $(state $A) == free ]] || fail "released slot state $(state $A)"

# Separate workers are leased concurrently.
"$POOL" acquire --slot $A --task a --agent x >/dev/null &
"$POOL" acquire --slot $B --task b --agent y >/dev/null &
wait
[[ -e $CG_POOL_LEASES/$A.lease && -e $CG_POOL_LEASES/$B.lease ]] || fail "separate workers not both leased"

# Both workers busy.
expect 3 "$POOL" acquire --slot auto --task c --agent z
grep -q BLOCKED_POOL_BUSY "$T/out" || fail "no BLOCKED_POOL_BUSY"

# Review slot is never leased for implementation.
expect 1 "$POOL" acquire --slot $R --task impl --agent x
expect 0 "$POOL" acquire --slot $R --task review --agent x --review
RR_TOKEN=$(token $R)
RHEAD=$(git -C "$T/seed" rev-parse origin/v2.0)
g -C "$T/seed" switch -q --detach origin/v2.0
g -C "$T/seed" commit -q --allow-empty -m review-head
RHEAD=$(git -C "$T/seed" rev-parse HEAD)
g -C "$T/seed" push -q origin HEAD:refs/heads/review-head

# Review checkout requires the live lease token and refuses every unsafe state.
expect 2 "$POOL" review-checkout --slot $R --branch review-head --head "$RHEAD"
expect 1 "$POOL" review-checkout --slot $R --token missing --branch review-head --head "$RHEAD"
expect 1 "$POOL" review-checkout --slot $R --token "$RR_TOKEN" --branch review-head --head "$(printf '%040d' 0)"
expect 0 "$POOL" review-checkout --slot $R --token "$RR_TOKEN" --branch review-head --head "$RHEAD"
[[ $(git -C "$CG_POOL_ROOT/$R" rev-parse HEAD) == "$RHEAD" ]] || fail "review checkout landed at wrong head"
expect 0 "$POOL" release --slot $R --token "$RR_TOKEN"
RBEFORE=$(git -C "$CG_POOL_ROOT/$R" rev-parse HEAD)
expect 1 "$POOL" review-checkout --slot $R --token "$RR_TOKEN" --branch review-head --head "$RHEAD"
[[ $(git -C "$CG_POOL_ROOT/$R" rev-parse HEAD) == "$RBEFORE" ]] || fail "checkout without lease changed review slot"

# Malformed lease never authorizes checkout; a real lease does not bypass safety checks.
printf 'slot=%s\n' "$R" >"$CG_POOL_LEASES/$R.lease"
expect 1 "$POOL" review-checkout --slot $R --token "$RR_TOKEN" --branch review-head --head "$RHEAD"
rm "$CG_POOL_LEASES/$R.lease"
expect 0 "$POOL" acquire --slot $R --task review-safe --agent x --review
RR_TOKEN=$(token $R)
echo "$CG_POOL_ROOT/$R/sub" >"$T/cwds"
expect 1 "$POOL" review-checkout --slot $R --token "$RR_TOKEN" --branch review-head --head "$RHEAD"
: >"$T/cwds"
echo dirty >"$CG_POOL_ROOT/$R/dirty.txt"
expect 1 "$POOL" review-checkout --slot $R --token "$RR_TOKEN" --branch review-head --head "$RHEAD"
rm "$CG_POOL_ROOT/$R/dirty.txt"
mkdir "$T/fail-checkout"
{
	printf '%s\n' '#!/bin/sh
for arg in "$@"; do [ "$arg" = checkout ] && { echo injected checkout failure >&2; exit 128; }; done'
	printf 'exec "%s" "$@"\n' "$(command -v git)"
} >"$T/fail-checkout/git"
chmod +x "$T/fail-checkout/git"
RBEFORE=$(git -C "$CG_POOL_ROOT/$R" rev-parse HEAD)
expect 1 env PATH="$T/fail-checkout:$PATH" "$POOL" review-checkout --slot $R --token "$RR_TOKEN" --branch review-head --head "$RHEAD"
[[ $(git -C "$CG_POOL_ROOT/$R" rev-parse HEAD) == "$RBEFORE" ]] || fail "failed checkout changed review slot"
expect 0 "$POOL" review-checkout --slot $R --token "$RR_TOKEN" --branch review-head --head "$RHEAD"
expect 1 "$POOL" review-checkout --slot $R --token wrong --branch review-head --head "$RHEAD"
expect 0 "$POOL" release --slot $R --token "$RR_TOKEN"

# Release refuses an unpushed branch, accepts it once pushed.
g -C "$CG_POOL_ROOT/$A" switch -q -c work
change $A work
expect 1 "$POOL" release --slot $A --token "$(token $A)"
g -C "$CG_POOL_ROOT/$A" push -q origin work 2>/dev/null
expect 0 "$POOL" release --slot $A --token "$(token $A)"
[[ $(state $A) == free ]] || fail "pushed release state $(state $A)"

# Detached commits that are not pushed are not discarded.
"$POOL" acquire --slot $A --task d --agent x >/dev/null
TA=$(token $A)
g -C "$CG_POOL_ROOT/$A" switch -q --detach
g -C "$CG_POOL_ROOT/$A" commit -q --allow-empty -m detached
expect 1 "$POOL" release --slot $A --token "$TA"
g -C "$CG_POOL_ROOT/$A" switch -q --detach origin/v2.0

# Concurrent releases of one lease: exactly one succeeds.
for i in 1 2 3 4; do
	("$POOL" release --slot $A --token "$TA" >/dev/null 2>&1 && echo ok >"$T/rel$i" || : >"$T/rel$i") &
done
wait
wins=$(cat "$T"/rel? | grep -c ok || true)
[[ $wins == 1 ]] || fail "concurrent release: $wins winners"

# A detached HEAD whose remote branch was deleted is not treated as pushed.
"$POOL" acquire --slot $A --task gone --agent x >/dev/null
g -C "$CG_POOL_ROOT/$A" switch -q -c gone
change $A gone
g -C "$CG_POOL_ROOT/$A" push -q origin gone 2>/dev/null
g -C "$CG_POOL_ROOT/$A" switch -q --detach
g -C "$T/origin.git" branch -q -D gone
expect 1 "$POOL" release --slot $A --token "$(token $A)"
g -C "$CG_POOL_ROOT/$A" switch -q --detach origin/v2.0
expect 0 "$POOL" release --slot $A --token "$(token $A)"
expect 2 "$POOL" acquire --slot $A --task t --agent

# Dirty worker: release keeps the lease and the change.
echo wip >"$CG_POOL_ROOT/$B/wip.txt"
expect 1 "$POOL" release --slot $B --token "$(token $B)"
[[ -e $CG_POOL_LEASES/$B.lease && -e $CG_POOL_ROOT/$B/wip.txt ]] || fail "dirty release lost state"

# Abandoned lease with a dead process: stale-suspected, never auto-freed;
# reclaim archives the lease and leaves the dirty slot untouched.
TB=$(token $B)
sh -c 'exit 0' &
dead=$!
wait $dead || true
sed -i.bak "s/^pid=.*/pid=$dead/; s/^host=.*/host=$(hostname)/" "$CG_POOL_LEASES/$B.lease" && rm "$CG_POOL_LEASES/$B.lease.bak"
[[ $(state $B) == stale-suspected ]] || fail "dead-pid state $(state $B)"
expect 1 "$POOL" acquire --slot $B --task steal --agent x
expect 1 "$POOL" reclaim --slot $B --token wrong --reason test
expect 2 "$POOL" reclaim --slot $B --token "$TB"
expect 0 "$POOL" reclaim --slot $B --token "$TB" --reason "agent gone"
ls "$CG_POOL_LEASES/archive/$B".*."$TB".lease >/dev/null 2>&1 || fail "reclaimed lease not archived"
grep -q "reclaim $B token=$TB" "$CG_POOL_LEASES/archive/audit.log" || fail "reclaim not audited"
[[ $(cat "$CG_POOL_ROOT/$B/wip.txt") == wip ]] || fail "reclaim touched the slot"
[[ $(state $B) == inconsistent ]] || fail "dirty unleased state $(state $B)"
expect 1 "$POOL" acquire --slot $B --task t --agent x
rm "$CG_POOL_ROOT/$B/wip.txt"

# Live lease cannot be reclaimed.
"$POOL" acquire --slot $B --task live --agent x --pid $$ >/dev/null
[[ $(state $B) == busy ]] || fail "live-pid state $(state $B)"
expect 1 "$POOL" reclaim --slot $B --token "$(token $B)" --reason test
expect 0 "$POOL" release --slot $B --token "$(token $B)"

# Active process without a lease.
echo "$CG_POOL_ROOT/$B/sub" >"$T/cwds"
[[ $(state $B) == inconsistent ]] || fail "process-cwd state $(state $B)"
expect 1 "$POOL" acquire --slot $B --task t --agent x
: >"$T/cwds"

# Worker detached at an obsolete base.
g -C "$T/seed" commit -q --allow-empty -m next
g -C "$T/seed" push -q origin HEAD:v2.0
expect 1 "$POOL" acquire --slot $B --task t --agent x
g -C "$CG_POOL_ROOT/$B" switch -q --detach origin/v2.0
expect 0 "$POOL" acquire --slot $B --task t --agent x
expect 0 "$POOL" release --slot $B --token "$(token $B)"

# Missing or malformed lease metadata.
printf 'slot=%s\n' $B >"$CG_POOL_LEASES/$B.lease"
[[ $(state $B) == inconsistent ]] || fail "malformed lease state $(state $B)"
expect 1 "$POOL" acquire --slot $B --task t --agent x
expect 1 "$POOL" release --slot $B --token bogus
[[ -e $CG_POOL_LEASES/$B.lease ]] || fail "malformed lease removed"
expect 2 "$POOL" acquire --slot $B --task t
expect 2 "$POOL" acquire --slot nope --task t --agent x

# A failed fetch never leases a slot on a possibly obsolete base.
rm "$CG_POOL_LEASES/$B.lease"
mv "$T/origin.git" "$T/origin.off"
expect 1 "$POOL" acquire --slot $B --task t --agent x
grep -q 'fetch of origin failed' "$T/out" || fail "fetch failure not reported: $(cat "$T/out")"
[[ ! -e $CG_POOL_LEASES/$B.lease ]] || fail "leased after a failed fetch"
mv "$T/origin.off" "$T/origin.git"

# Slots that are not valid worktrees of the repository are never free.
X=$CG_POOL_ROOT/codegraph-worker-a
rm -f "$CG_POOL_LEASES/$A.lease"
g -C "$T/seed" worktree remove "$X"
mkdir "$X"
[[ $(state $A) == inconsistent ]] || fail "plain directory state $(state $A)"
rmdir "$X"
g clone -q "$T/origin.git" "$X" 2>/dev/null
g -C "$X" switch -q --detach origin/v2.0
[[ $(state $A) == inconsistent ]] || fail "separate clone state $(state $A)"
grep -q "not a worktree" <("$POOL" status $A) || fail "separate clone not named"
rm -rf "$X"
g -C "$T/seed" worktree prune
g -C "$T/seed" worktree add -q --detach "$X" origin/v2.0
[[ $(state $A) == free ]] || fail "restored worktree state $(state $A)"
gitfile=$(cat "$X/.git")
echo "gitdir: $T/nowhere" >"$X/.git"
[[ $(state $A) == inconsistent ]] || fail "broken .git state $(state $A)"
echo "$gitfile" >"$X/.git"
wtdir=$(g -C "$X" rev-parse --absolute-git-dir)
cp "$wtdir/HEAD" "$T/head.bak"
echo garbage >"$wtdir/HEAD"
[[ $(state $A) == inconsistent ]] || fail "corrupt HEAD state $(state $A)"
cp "$T/head.bak" "$wtdir/HEAD"
g -C "$T/seed" update-ref -d refs/remotes/origin/v2.0
[[ $(state $A) == inconsistent ]] || fail "missing base state $(state $A)"
g -C "$T/seed" fetch -q origin
[[ $(state $A) == free ]] || fail "repaired worktree state $(state $A)"

# Process inspection that cannot run counts as in use.
CG_POOL_CWD_FILE=$T/missing/cwds expect 1 "$POOL" acquire --slot $A --task t --agent x
[[ $(CG_POOL_CWD_FILE=$T/missing/cwds state $A) == inconsistent ]] || fail "unreadable process list state"
mkdir "$T/nolsof"
for c in git sed awk cat date hostname od tr mktemp ln rm mv mkdir rmdir head kill printf dirname; do
	ln -s "$(command -v $c)" "$T/nolsof/$c"
done
nolsof_state=$(env -u CG_POOL_CWD_FILE PATH="$T/nolsof" "$BASH" "$POOL" status $A | sed -n 's/^[^:]*: //p;q')
[[ $nolsof_state == inconsistent ]] || fail "missing lsof state $nolsof_state"

# Git that fails for one subcommand, for failure injection.
mkdir "$T/failgit"
{
	printf '%s\n' '#!/bin/sh
for a in "$@"; do [ "$a" = "$CG_FAIL_GIT" ] && { echo "injected $a failure" >&2; exit 128; }; done'
	printf 'exec "%s" "$@"\n' "$(command -v git)"
} >"$T/failgit/git"
chmod +x "$T/failgit/git"
failgit() { CG_FAIL_GIT=$1 PATH="$T/failgit:$PATH" "${@:2}"; }
lease_exists() { [[ -e $CG_POOL_LEASES/$1.lease ]]; }
head_of() { git -C "$CG_POOL_ROOT/$1" rev-parse HEAD; }
base_of() { git -C "$T/seed" rev-parse origin/v2.0; }
advance_base() {
	g -C "$T/seed" fetch -q origin
	g -C "$T/seed" switch -q --detach origin/v2.0
	g -C "$T/seed" commit -q --allow-empty -m "${1:-advance}"
	g -C "$T/seed" push -q origin HEAD:v2.0
	g -C "$T/seed" fetch -q origin
}
# Squash-merges origin branch $1 into v2.0 the way GitHub does: one new
# commit with the merged tree, then deletes the branch.
squash_merge() {
	local tree
	g -C "$T/seed" fetch -q origin
	tree=$(g -C "$T/seed" merge-tree --write-tree origin/v2.0 "origin/$1")
	g -C "$T/seed" push -q origin "$(g -C "$T/seed" commit-tree "$tree" -p origin/v2.0 -m "squash $1"):refs/heads/v2.0"
	g -C "$T/seed" push -q origin --delete "$1" 2>/dev/null
	g -C "$T/seed" fetch -q --prune origin
}
# Leases worker $1 and leaves it on task branch $2 with one change, pushed.
task_branch() {
	"$POOL" acquire --slot "$1" --task "$2" --agent x >/dev/null
	g -C "$CG_POOL_ROOT/$1" switch -q -c "$2" origin/v2.0
	change "$1" "$2"
	g -C "$CG_POOL_ROOT/$1" push -q origin "$2" 2>/dev/null
}
for s in $A $B $R; do
	rm -f "${CG_POOL_LEASES:?}/${s:?}.lease"
	g -C "$CG_POOL_ROOT/$s" switch -q --detach origin/v2.0
done

# A failed git status never counts as clean: release keeps lease and tree.
"$POOL" acquire --slot $A --task st --agent x >/dev/null
expect 1 failgit status "$POOL" release --slot $A --token "$(token $A)"
grep -q 'git status failed' "$T/out" || fail "status failure not reported: $(cat "$T/out")"
lease_exists $A || fail "lease lost on failed status"
wtdir=$(g -C "$CG_POOL_ROOT/$A" rev-parse --absolute-git-dir)
cp "$wtdir/index" "$T/index.bak"
echo garbage >"$wtdir/index"
expect 1 "$POOL" release --slot $A --token "$(token $A)"
lease_exists $A || fail "lease lost on corrupt index"
cp "$T/index.bak" "$wtdir/index"
expect 0 "$POOL" release --slot $A --token "$(token $A)"

# The PR #418 case: a task branch carrying a merge of the base, squash-merged,
# remote branch deleted. Release succeeds and keeps the local branch.
task_branch $A sq
advance_base other-work
g -C "$CG_POOL_ROOT/$A" fetch -q origin
g -C "$CG_POOL_ROOT/$A" merge -q --no-edit origin/v2.0
g -C "$CG_POOL_ROOT/$A" push -q origin sq 2>/dev/null
squash_merge sq
expect 0 "$POOL" release --slot $A --token "$(token $A)"
[[ $(head_of $A) == "$(base_of)" ]] || fail "squash release did not move to base"
g -C "$CG_POOL_ROOT/$A" rev-parse -q --verify refs/heads/sq >/dev/null || fail "squash release lost the task branch"
# Same, released detached on the task commit after the base moved on.
task_branch $A sq2
squash_merge sq2
advance_base later
g -C "$CG_POOL_ROOT/$A" switch -q --detach
expect 0 "$POOL" release --slot $A --token "$(token $A)"

# Matching content but no local branch holding the commit: refused.
task_branch $A sq3
g -C "$CG_POOL_ROOT/$A" switch -q --detach
g -C "$CG_POOL_ROOT/$A" branch -q -D sq3
squash_merge sq3
expect 1 "$POOL" release --slot $A --token "$(token $A)"
lease_exists $A || fail "unpreserved detached commit released"
g -C "$CG_POOL_ROOT/$A" branch -q sq3 HEAD
expect 0 "$POOL" release --slot $A --token "$(token $A)"

# Remote branch deleted without merging, and a squash-merged branch that
# gained unique work afterwards: both refused.
task_branch $A unmerged
g -C "$T/seed" push -q origin --delete unmerged 2>/dev/null
expect 1 "$POOL" release --slot $A --token "$(token $A)"
g -C "$CG_POOL_ROOT/$A" switch -q --detach origin/v2.0
expect 0 "$POOL" release --slot $A --token "$(token $A)"
task_branch $A diverged
squash_merge diverged
change $A more
expect 1 "$POOL" release --slot $A --token "$(token $A)"
grep -q 'neither pushed nor merged' "$T/out" || fail "diverged release reason: $(cat "$T/out")"

# Failed fetch and missing baseline keep the lease.
mv "$T/origin.git" "$T/origin.off"
expect 1 "$POOL" release --slot $A --token "$(token $A)"
mv "$T/origin.off" "$T/origin.git"
expect 1 env CG_POOL_BASE=nope "$POOL" release --slot $A --token "$(token $A)"
grep -q 'no valid origin/nope' "$T/out" || fail "missing base reason: $(cat "$T/out")"
lease_exists $A || fail "lease lost on fetch or base failure"
# A still-published branch releases normally.
g -C "$CG_POOL_ROOT/$A" push -q origin diverged 2>/dev/null
expect 0 "$POOL" release --slot $A --token "$(token $A)"

# prepare: idle slots left behind by a merge move forward; acquire then works.
advance_base merged-pr
expect 1 "$POOL" acquire --slot $B --task t --agent x
expect 0 "$POOL" prepare --slot $B
[[ $(head_of $B) == "$(base_of)" ]] || fail "prepare did not reach base"
expect 0 "$POOL" prepare --slot $B
grep -q 'already at' "$T/out" || fail "no-op prepare: $(cat "$T/out")"
expect 0 "$POOL" acquire --slot $B --task t --agent x
expect 0 "$POOL" release --slot $B --token "$(token $B)"

# prepare refuses and changes nothing: leased, dirty, on a branch, unique
# detached commit, process inside, process check unavailable, failed fetch,
# failed status, missing base.
advance_base again
snap() { echo "$(head_of "$1") $(git -C "$CG_POOL_ROOT/$1" symbolic-ref -q HEAD || :) $(ls "$CG_POOL_LEASES")"; }
refuse() { # refuse <slot> <reason> [env assignments...]
	local before
	before=$(snap "$1")
	expect 1 env "${@:3}" "$POOL" prepare --slot "$1"
	grep -q "$2" "$T/out" || fail "prepare $1 reason, want $2: $(cat "$T/out")"
	[[ $(snap "$1") == "$before" ]] || fail "refused prepare changed $1"
}
"$POOL" prepare --slot $B >/dev/null
"$POOL" acquire --slot $B --task leased --agent x >/dev/null
advance_base leased
refuse $B 'is leased'
echo wip >"$CG_POOL_ROOT/$A/wip.txt"
refuse $A dirty
rm "$CG_POOL_ROOT/$A/wip.txt"
g -C "$CG_POOL_ROOT/$A" switch -q -c attached
refuse $A on-branch
g -C "$CG_POOL_ROOT/$A" switch -q --detach
g -C "$CG_POOL_ROOT/$A" commit -q --allow-empty -m unique
refuse $A 'not an ancestor'
g -C "$CG_POOL_ROOT/$A" switch -q --detach HEAD~1
echo "$CG_POOL_ROOT/$A" >"$T/cwds"
refuse $A process-cwd
: >"$T/cwds"
refuse $A 'process check unavailable' CG_POOL_CWD_FILE="$T/missing/cwds"
mv "$T/origin.git" "$T/origin.off"
refuse $A 'fetch of origin failed'
mv "$T/origin.off" "$T/origin.git"
refuse $A 'git status failed' CG_FAIL_GIT=status PATH="$T/failgit:$PATH"
refuse $A 'no valid origin/nope' CG_POOL_BASE=nope
# One worker prepared while the other is busy; --all reports every slot.
expect 0 "$POOL" prepare --slot $A
expect 1 "$POOL" prepare --all
{ grep -q "prepared $R" "$T/out" && grep -q "$B is leased" "$T/out"; } || fail "--all output: $(cat "$T/out")"
expect 0 "$POOL" release --slot $B --token "$(token $B)"

# Concurrency: prepare against acquire, release, reclaim and other prepares.
# Whatever interleaving wins, no lease appears or vanishes unexpectedly and a
# leased slot is never moved by prepare.
advance_base race
for i in 1 2 3 4; do ("$POOL" prepare --slot $A >/dev/null 2>&1 || :) & done
("$POOL" acquire --slot $A --task race --agent x >"$T/acq" 2>&1 && echo ok >>"$T/acq" || :) &
wait
if lease_exists $A; then
	grep -q '^ok$' "$T/acq" || fail "lease without a winning acquire"
	("$POOL" release --slot $A --token "$(token $A)" >"$T/rel" 2>&1 && echo ok >>"$T/rel" || :) &
	("$POOL" prepare --slot $A >/dev/null 2>&1 || :) &
	wait
	grep -q '^ok$' "$T/rel" || lease_exists $A || fail "lease vanished without release"
else
	grep -q '^ok$' "$T/acq" && fail "winning acquire left no lease"
	[[ $(head_of $A) == "$(base_of)" ]] || fail "concurrent prepares left $A behind"
fi
if lease_exists $A; then "$POOL" release --slot $A --token "$(token $A)" >/dev/null; fi
"$POOL" prepare --slot $B >/dev/null
"$POOL" acquire --slot $B --task stale --agent x >/dev/null
TB=$(token $B)
sed -i.bak "s/^pid=.*/pid=$dead/; s/^host=.*/host=$(hostname)/" "$CG_POOL_LEASES/$B.lease" && rm "$CG_POOL_LEASES/$B.lease.bak"
advance_base reclaim
bh=$(head_of $B)
("$POOL" reclaim --slot $B --token "$TB" --reason race >/dev/null 2>&1 || :) &
("$POOL" prepare --slot $B >/dev/null 2>&1 || :) &
wait
if lease_exists $B; then
	[[ $(head_of $B) == "$bh" ]] || fail "prepare moved a leased slot"
	expect 0 "$POOL" reclaim --slot $B --token "$TB" --reason race
fi
expect 0 "$POOL" prepare --all
for s in $A $B $R; do
	[[ $(head_of "$s") == "$(base_of)" ]] || fail "$s not at base after --all"
	if lease_exists "$s"; then fail "$s leased after --all"; fi
done
[[ -z $(ls "$CG_POOL_LEASES") || $(ls "$CG_POOL_LEASES") == archive ]] || fail "leftover lease files: $(ls "$CG_POOL_LEASES")"

# status inspects leased slots: it must not refresh their index, whose
# index.lock would make the working agent's git add or commit fail.
"$POOL" acquire --slot $A --task busy --agent x >/dev/null
f=$(git -C "$CG_POOL_ROOT/$A" ls-files | head -n 1)
idx=$(git -C "$CG_POOL_ROOT/$A" rev-parse --path-format=absolute --git-path index)
[[ -n $f ]] || fail "no tracked file in $A"
touch -t 202001010000 "$CG_POOL_ROOT/$A/$f"
before=$(shasum "$idx")
"$POOL" status >/dev/null
[[ $(shasum "$idx") == "$before" ]] || fail "status rewrote the index of leased $A"
"$POOL" release --slot $A --token "$(token $A)" >/dev/null || fail "release of $A after status check"


# Lease records carry the schema version, a non-secret lease id and the boot
# identity; the token stays out of every report.
echo boot-one >"$T/boot"
CG_POOL_BOOT_FILE=$T/boot "$POOL" acquire --slot $A --task versioned --agent x >/dev/null
TA=$(token $A)
[[ $(sed -n 's/^version=//p' "$CG_POOL_LEASES/$A.lease") == 1 ]] || fail "lease has no schema version"
[[ $(sed -n 's/^lease_id=//p' "$CG_POOL_LEASES/$A.lease") =~ ^[0-9a-f]{32}$ ]] || fail "lease has no lease id"
[[ $(sed -n 's/^boot=//p' "$CG_POOL_LEASES/$A.lease") == boot-one ]] || fail "lease has no boot identity"
[[ $(sed -n 's/^lease_id=//p' "$CG_POOL_LEASES/$A.lease") != "$TA" ]] || fail "lease id reuses the token"
[[ $(state $A) == busy ]] || fail "versioned lease state $(state $A)"

# assess is read-only, hides the token, and reports what it cannot know.
before=$(shasum "$CG_POOL_LEASES/$A.lease") ; beforehead=$(head_of $A)
expect 2 "$POOL" assess
expect 2 "$POOL" assess codegraph-worker-z
expect 0 "$POOL" assess $A
grep -q "$TA" "$T/out" && fail "assess printed the lease token"
grep -q "^state: busy$" "$T/out" || fail "assess did not report the state"
grep -q "^lease: present$" "$T/out" || fail "assess did not report the lease"
grep -q "^recorded-process: none recorded" "$T/out" || fail "assess did not report the missing pid"
grep -q "^process-cwd: none observed" "$T/out" || fail "assess did not report cwd evidence"
grep -q "^changed-or-untracked-files: 0$" "$T/out" || fail "assess did not count changes"
grep -q "^ignored-files: 0$" "$T/out" || fail "assess did not count ignored files"
grep -q "^note: observations only" "$T/out" || fail "assess did not disclaim authorization"
[[ $(shasum "$CG_POOL_LEASES/$A.lease") == "$before" && $(head_of $A) == "$beforehead" ]] ||
	fail "assess mutated the slot"

# Dirty and ignored files are reported, not acted on.
echo dirt >"$CG_POOL_ROOT/$A/dirt.txt"
printf 'ignored.txt\n' >"$CG_POOL_ROOT/$A/.gitignore"
echo hidden >"$CG_POOL_ROOT/$A/ignored.txt"
expect 0 "$POOL" assess $A
grep -q "^changed-or-untracked-files: 2$" "$T/out" || fail "assess miscounted changes: $(cat "$T/out")"
grep -q "^ignored-files: 1$" "$T/out" || fail "assess miscounted ignored files"
[[ -e $CG_POOL_ROOT/$A/dirt.txt && -e $CG_POOL_ROOT/$A/ignored.txt ]] || fail "assess removed files"
rm "$CG_POOL_ROOT/$A/dirt.txt" "$CG_POOL_ROOT/$A/ignored.txt" "$CG_POOL_ROOT/$A/.gitignore"

# A recorded pid from an earlier boot is reported as evidence only: the lease
# stays busy and never becomes stale-suspected on that ground.
sed -i.bak "s/^pid=.*/pid=$$/; s/^host=.*/host=$(hostname)/" "$CG_POOL_LEASES/$A.lease" &&
	rm "$CG_POOL_LEASES/$A.lease.bak"
echo boot-two >"$T/boot"
CG_POOL_BOOT_FILE=$T/boot "$POOL" assess $A >"$T/out" 2>&1 || fail "assess with boot mismatch failed"
grep -q "recorded under an earlier boot" "$T/out" || fail "assess did not report the boot mismatch"
[[ $(CG_POOL_BOOT_FILE=$T/boot "$POOL" status $A | sed -n 1p) == "$A: busy" ]] ||
	fail "boot mismatch changed the slot state"
echo boot-one >"$T/boot"

# Process inspection that cannot run is unknown, never "no process".
CG_POOL_CWD_FILE=$T/missing-cwds "$POOL" assess $A >"$T/out" 2>&1 || fail "assess without cwd data failed"
grep -q "^process-cwd: unknown" "$T/out" || fail "assess claimed no process when inspection failed"
expect 0 "$POOL" release --slot $A --token "$TA"

# A lease written before versioning stays recognizable and busy.
"$POOL" acquire --slot $A --task legacy --agent x >/dev/null
TA=$(token $A)
grep -v '^version=' "$CG_POOL_LEASES/$A.lease" >"$T/legacy" && cp "$T/legacy" "$CG_POOL_LEASES/$A.lease"
[[ $(state $A) == busy ]] || fail "pre-versioning lease state $(state $A)"
expect 1 "$POOL" acquire --slot $A --task take --agent y
expect 0 "$POOL" assess $A
grep -q "^lease-schema: absent" "$T/out" || fail "assess did not flag the pre-versioning lease"
expect 0 "$POOL" release --slot $A --token "$TA"

# A schema this pool.sh does not know is refused, never reinterpreted.
"$POOL" acquire --slot $A --task future --agent x >/dev/null
TA=$(token $A)
sed -i.bak 's/^version=.*/version=99/' "$CG_POOL_LEASES/$A.lease" && rm "$CG_POOL_LEASES/$A.lease.bak"
[[ $(state $A) == inconsistent ]] || fail "unknown lease schema state $(state $A)"
# Never pipe pool.sh into grep -q: it closes the pipe on the first match and
# pipefail then turns the match into a failure.
"$POOL" status $A >"$T/out"
grep -q "version 99 unsupported" "$T/out" || fail "status did not name the unsupported schema"
grep -q "^  owner=" "$T/out" || fail "status of an unsupported lease hid its owner; a reader would see no lease"
grep -q "^  task=future$" "$T/out" || fail "status of an unsupported lease hid its task"
grep -q "$TA" "$T/out" && fail "status printed the token of an unsupported lease"
expect 0 "$POOL" assess $A
grep -q "^  owner=" "$T/out" || fail "assess of an unsupported lease hid its owner"
expect 1 "$POOL" acquire --slot $A --task take --agent y
expect 1 "$POOL" release --slot $A --token "$TA"
expect 1 "$POOL" reclaim --slot $A --token "$TA" --reason test
[[ -e $CG_POOL_LEASES/$A.lease ]] || fail "unknown schema lease was removed"
rm "$CG_POOL_LEASES/$A.lease"
"$POOL" prepare --slot $A >/dev/null

# A lease missing a required field is refused by the token-guarded mutators
# too, and is never removed on the owner's behalf.
"$POOL" acquire --slot $A --task malformed --agent x >/dev/null
TA=$(token $A)
grep -v '^task=' "$CG_POOL_LEASES/$A.lease" >"$T/bad" && cp "$T/bad" "$CG_POOL_LEASES/$A.lease"
[[ $(state $A) == inconsistent ]] || fail "lease without a task state $(state $A)"
expect 1 "$POOL" release --slot $A --token "$TA"
expect 1 "$POOL" reclaim --slot $A --token "$TA" --reason test
[[ -e $CG_POOL_LEASES/$A.lease ]] || fail "malformed lease was removed"
rm "$CG_POOL_LEASES/$A.lease"
"$POOL" prepare --slot $A >/dev/null
"$POOL" acquire --slot $R --task malformed --agent x --review >/dev/null
TR=$(token $R)
grep -v '^task=' "$CG_POOL_LEASES/$R.lease" >"$T/bad" && cp "$T/bad" "$CG_POOL_LEASES/$R.lease"
RBEFORE=$(head_of $R)
expect 1 "$POOL" review-checkout --slot $R --token "$TR" --branch review-head --head "$RHEAD"
[[ $(head_of $R) == "$RBEFORE" ]] || fail "a malformed lease authorized a review checkout"
[[ -e $CG_POOL_LEASES/$R.lease ]] || fail "malformed review lease was removed"
rm "$CG_POOL_LEASES/$R.lease"
"$POOL" prepare --slot $R >/dev/null

# Without an injected value the real boot identity is recorded; an empty one
# would silently disable the evidence it exists for.
"$POOL" acquire --slot $A --task realboot --agent x >/dev/null
TA=$(token $A)
rb=$(sed -n 's/^boot=//p' "$CG_POOL_LEASES/$A.lease")
if [[ -r /proc/sys/kernel/random/boot_id ]] || sysctl -n kern.bootsessionuuid >/dev/null 2>&1 ||
	sysctl -n kern.boottime >/dev/null 2>&1 || [[ -r /proc/stat ]]; then
	[[ -n $rb ]] || fail "no boot identity recorded on a host that reports one"
	[[ $rb != *" "* && $rb != *"usec"* ]] || fail "boot identity is not a single value: $rb"
	"$POOL" assess $A >"$T/out" 2>&1 || fail "assess with a real boot identity failed"
	grep -q "^boot: $rb$" "$T/out" || fail "assess read back a different boot identity: $(sed -n '/^boot: /p' "$T/out")"
fi

# A recorded pid that is gone still reports stale-suspected, and a boot
# mismatch neither widens nor narrows that: the lease stays on the slot.
dead2=$(sh -c 'echo $$')
while kill -0 "$dead2" 2>/dev/null; do dead2=$((dead2 + 1)); done
sed -i.bak "s/^pid=.*/pid=$dead2/; s/^host=.*/host=$(hostname)/" "$CG_POOL_LEASES/$A.lease" &&
	rm "$CG_POOL_LEASES/$A.lease.bak"
echo boot-elsewhere >"$T/boot"
[[ $(CG_POOL_BOOT_FILE=$T/boot "$POOL" status $A | sed -n 1p) == "$A: stale-suspected" ]] ||
	fail "dead pid with a boot mismatch is not stale-suspected"
[[ -e $CG_POOL_LEASES/$A.lease ]] || fail "stale-suspected lease was removed"
CG_POOL_BOOT_FILE=$T/boot "$POOL" assess $A >"$T/out" 2>&1 || fail "assess of a stale-suspected lease failed"
grep -q "recorded under an earlier boot" "$T/out" || fail "assess hid the boot mismatch"
grep -q "^recorded-pid-signalable: no" "$T/out" || fail "assess hid that the pid is gone"
expect 0 "$POOL" release --slot $A --token "$TA"

# assess writes nothing: not the index of a busy slot, not a git object.
"$POOL" acquire --slot $A --task nomutation --agent x >/dev/null
TA=$(token $A)
change $A assessed
f=$(git -C "$CG_POOL_ROOT/$A" ls-files | head -n 1)
idx=$(git -C "$CG_POOL_ROOT/$A" rev-parse --path-format=absolute --git-path index)
touch -t 202001010000 "$CG_POOL_ROOT/$A/$f"
before=$(shasum "$idx")
objects=$(find "$T/seed/.git/objects" -type f | wc -l | tr -d ' ')
expect 0 "$POOL" assess $A
[[ $(shasum "$idx") == "$before" ]] || fail "assess rewrote the index of busy $A"
[[ $(find "$T/seed/.git/objects" -type f | wc -l | tr -d ' ') == "$objects" ]] ||
	fail "assess wrote a git object into the shared store"
grep -q "^head-in-base: no or unknown$" "$T/out" || fail "assess miscalled an unpushed head"
git -C "$CG_POOL_ROOT/$A" switch -q --detach "$(base_of)"
expect 0 "$POOL" release --slot $A --token "$TA"

# A slot with no lease reports it as absent.
expect 0 "$POOL" assess $A
grep -q "^lease: absent$" "$T/out" || fail "assess did not report the missing lease"
grep -q "^state: free$" "$T/out" || fail "assess did not report a free slot"

# A record of an unknown schema may name its secret anything, so only known
# fields are printed.
"$POOL" acquire --slot $A --task allowlist --agent x >/dev/null
TA=$(token $A)
{ echo "secret_v2=s3cr3t-value"; echo "version=99"; } >>"$CG_POOL_LEASES/$A.lease"
for cmd in status assess; do
	"$POOL" $cmd $A >"$T/out" 2>&1 || true
	grep -q "s3cr3t-value" "$T/out" && fail "$cmd printed an unknown secret field"
	grep -q "^  task=allowlist$" "$T/out" || fail "$cmd dropped a known field: $(cat "$T/out")"
	grep -q "$TA" "$T/out" && fail "$cmd printed the token"
done
rm "$CG_POOL_LEASES/$A.lease"
"$POOL" prepare --slot $A >/dev/null

# status says whether a record is there at all: "inconsistent" alone cannot
# tell an unreadable lease from a slot that is merely not idle.
"$POOL" status $A >"$T/out"
grep -q "^  lease=absent$" "$T/out" || fail "status of a free slot did not report the lease as absent"
"$POOL" acquire --slot $A --task marker --agent x >/dev/null
TA=$(token $A)
"$POOL" status $A >"$T/out"
grep -q "^  lease=present$" "$T/out" || fail "status of a leased slot did not report the lease as present"
sed -i.bak 's/^version=.*/version=99/' "$CG_POOL_LEASES/$A.lease" && rm "$CG_POOL_LEASES/$A.lease.bak"
"$POOL" status $A >"$T/out"
grep -q "^  lease=present$" "$T/out" || fail "an unreadable lease was not reported as present"
rm "$CG_POOL_LEASES/$A.lease"
change $A notidle
"$POOL" status $A >"$T/out"
[[ $(state $A) == inconsistent ]] || fail "a slot off the base is not inconsistent"
grep -q "^  lease=absent$" "$T/out" ||
	fail "a slot that is merely not idle was not reported as unleased: $(cat "$T/out")"
git -C "$CG_POOL_ROOT/$A" switch -q --detach "$(base_of)"
"$POOL" prepare --slot $A >/dev/null

# A lease whose contents cannot be read is unknown, never absent.
"$POOL" acquire --slot $A --task unreadable --agent x >/dev/null
TA=$(token $A)
: >"$CG_POOL_LEASES/$A.lease"  # present, nothing readable in it
expect 0 "$POOL" assess $A
grep -q "^lease: present but empty or unreadable$" "$T/out" ||
	fail "an unreadable lease was reported as absent: $(cat "$T/out")"
grep -q "^lease: absent$" "$T/out" && fail "an existing lease was called absent"
rm "$CG_POOL_LEASES/$A.lease"
"$POOL" prepare --slot $A >/dev/null

# A lease with far more lines than a pipe buffer holds must not abort status:
# a reader that stops at the first match used to kill sed with SIGPIPE, which
# pipefail reported as a failure of the whole command.
"$POOL" acquire --slot $A --task bigfile --agent x >/dev/null
TA=$(token $A)
awk 'BEGIN { for (i = 0; i < 100000; i++) print "task=bigfile" }' >>"$CG_POOL_LEASES/$A.lease"
# bash 3.2 does not apply errexit to a failed assignment inside a function, so
# the broken pipe shows up as a message on stderr rather than a failed command;
# on bash 4.4 and later the same pipeline aborts the whole report.
for cmd in status assess; do
	"$POOL" $cmd $A >"$T/out" 2>"$T/err" || fail "$cmd of a huge lease exited non-zero"
	[[ -s $T/err ]] && fail "$cmd of a huge lease wrote to stderr: $(cat "$T/err")"
done
"$POOL" status $A >"$T/out"
grep -q "^  task=bigfile$" "$T/out" || fail "status of a huge lease lost its fields"
[[ $(grep -c "^  task=bigfile$" "$T/out") == 1 ]] ||
	fail "a repeated field was printed more than once: $(grep -c "^  task=bigfile$" "$T/out") times"
expect 0 "$POOL" release --slot $A --token "$TA"

# The key an assess reads may be missing entirely — a pre-versioning lease, or
# the corrupted record assess exists to look at — so a reader that scans the
# whole file pays for every line of it. Bound by wall clock: the failure this
# guards against is a hang, not a wrong answer.
"$POOL" acquire --slot $A --task slowscan --agent x >/dev/null
TA=$(token $A)
grep -v '^version=' "$CG_POOL_LEASES/$A.lease" >"$T/noversion" && cp "$T/noversion" "$CG_POOL_LEASES/$A.lease"
awk 'BEGIN { for (i = 0; i < 100000; i++) print "junk=line" }' >>"$CG_POOL_LEASES/$A.lease"
started=$(date +%s)
expect 0 "$POOL" assess $A
elapsed=$(($(date +%s) - started))
((elapsed < 20)) || fail "assess of a 100k-line lease took ${elapsed}s"
grep -q "^lease-schema: absent" "$T/out" || fail "assess lost the pre-versioning lease report"
expect 0 "$POOL" release --slot $A --token "$TA"

if ((fails)); then
	echo "$fails failure(s)"
	exit 1
fi
echo "pool.sh: all tests passed"

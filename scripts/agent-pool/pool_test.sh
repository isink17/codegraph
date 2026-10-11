#!/usr/bin/env bash
# Tests for pool.sh against a throwaway pool; never touches the live pool.
set -euo pipefail

POOL=$(cd "$(dirname "$0")" && pwd)/pool.sh
T=$(cd "$(mktemp -d)" && pwd -P)
trap 'rm -rf "$T"' EXIT
export CG_POOL_ROOT=$T/slots CG_POOL_LEASES=$T/leases CG_POOL_CWD_FILE=$T/cwds
: >"$T/cwds"
fails=0

fail() { echo "FAIL: $*"; fails=$((fails + 1)); }
expect() { # expect <exit code> <command...>
	local want=$1 got=0
	shift
	"$@" >"$T/out" 2>&1 || got=$?
	[[ $got == "$want" ]] || fail "$* exited $got, want $want: $(cat "$T/out")"
}
state() { "$POOL" status "$1" | sed -n 1p | sed 's/^[^:]*: //'; }
token() { sed -n 's/^token=//p' "$CG_POOL_LEASES/$1.lease"; }
g() { git -c user.name=t -c user.email=t@t -c init.defaultBranch=main "$@"; }

g init -q --bare "$T/origin.git"
g clone -q "$T/origin.git" "$T/seed" 2>/dev/null
g -C "$T/seed" commit -q --allow-empty -m base
g -C "$T/seed" push -q origin HEAD:v2.0
mkdir -p "$CG_POOL_ROOT"
for s in codegraph-worker-a codegraph-worker-b codegraph-review; do
	g clone -q "$T/origin.git" "$CG_POOL_ROOT/$s" 2>/dev/null
	g -C "$CG_POOL_ROOT/$s" switch -q --detach origin/v2.0
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
expect 0 "$POOL" release --slot $R --token "$(token $R)"

# Release refuses an unpushed branch, accepts it once pushed.
g -C "$CG_POOL_ROOT/$A" switch -q -c work
g -C "$CG_POOL_ROOT/$A" commit -q --allow-empty -m work
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

if ((fails)); then
	echo "$fails failure(s)"
	exit 1
fi
echo "pool.sh: all tests passed"

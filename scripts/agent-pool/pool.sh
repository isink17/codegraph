#!/usr/bin/env bash
# Lease manager for the CodeGraph agent pool. Policy: docs/agent-pool.md.
#
#   pool.sh status [slot]
#   pool.sh acquire --slot <slot|auto> --task <task> --agent <agent> [--pr N] [--owner who] [--pid PID] [--review]
#   pool.sh release --slot <slot> --token <token>
#   pool.sh reclaim --slot <slot> --token <token> --reason <text>
#
# Exit codes: 0 ok, 1 refused, 2 usage, 3 BLOCKED_POOL_BUSY.
set -euo pipefail
cd /

POOL_ROOT=${CG_POOL_ROOT:-$HOME/Projekti}
LEASES=${CG_POOL_LEASES:-$HOME/Projekti/codegraph-evidence/pool}
REMOTE=${CG_POOL_REMOTE:-origin}
BASE=${CG_POOL_BASE:-v2.0}
# Every slot must be a worktree of this repository.
REPO=${CG_POOL_REPO:-$HOME/Projekti/codegraph}
WORKERS=(codegraph-worker-a codegraph-worker-b)
REVIEW=codegraph-review

die() { echo "$1" >&2; exit "${2:-1}"; }
need() { [[ $# -ge 2 ]] || die "$1 needs a value" 2; }
lease_file() { echo "$LEASES/$1.lease"; }
slot_path() { echo "$POOL_ROOT/$1"; }
field() { sed -n "s/^$2=//p" "$1" | head -n 1; }

known_slot() {
	local s
	for s in "${WORKERS[@]}" "$REVIEW"; do [[ $1 == "$s" ]] && return 0; done
	return 1
}

# Paths of every process cwd, one per line, read once per invocation.
# Tests supply the list in a file. When the list cannot be read, every slot
# counts as in use.
CWDS=""
CWDS_READ=0
CWDS_FAILED=0
read_cwds() {
	local raw
	((CWDS_READ == 0)) || return 0
	CWDS_READ=1
	if [[ -n ${CG_POOL_CWD_FILE:-} ]]; then
		raw=$(cat "$CG_POOL_CWD_FILE" 2>/dev/null) || CWDS_FAILED=1
		CWDS=$raw
	elif raw=$(lsof -n -w -d cwd -Fn 2>/dev/null) && [[ -n $raw ]]; then
		CWDS=$(printf '%s\n' "$raw" | sed -n 's/^n//p')
	else
		CWDS_FAILED=1
	fi
}

# Every lease change of a slot happens under its lock. mkdir is atomic; a
# lock left by a crashed run is reported, never broken automatically.
LOCK=""
trap '[[ -n $LOCK ]] && rmdir "$LOCK"' EXIT
lock_slot() {
	mkdir "$LEASES/.$1.lock" 2>/dev/null || return 1
	LOCK="$LEASES/.$1.lock"
}
unlock_slot() { rmdir "$LOCK"; LOCK=""; }

slot_in_use() {
	local p
	p=$(cd "$(slot_path "$1")" 2>/dev/null && pwd -P) || return 1
	read_cwds
	((CWDS_FAILED == 0)) || return 0
	printf '%s\n' "$CWDS" | awk -v p="$p" '$0 == p || index($0, p "/") == 1 { found = 1 } END { exit !found }'
}

# Prints why a slot is not an idle baseline; prints nothing when it is.
slot_problems() {
	local p real top common repo changes head base
	p=$(slot_path "$1")
	real=$(cd "$p" 2>/dev/null && pwd -P) || { echo "missing"; return 0; }
	repo=$(cd "$REPO/.git" 2>/dev/null && pwd -P) || { echo "repository $REPO missing"; return 0; }
	top=$(git -C "$real" rev-parse --show-toplevel 2>/dev/null) &&
		common=$(cd "$real" && cd "$(git rev-parse --git-common-dir)" && pwd -P) &&
		[[ $top == "$real" && $common == "$repo" ]] ||
		{ echo "not a worktree of $REPO"; return 0; }
	changes=$(git -C "$real" status --porcelain --untracked-files=all 2>&1) || { echo "git status failed"; return 0; }
	[[ -z $changes ]] || echo "dirty"
	git -C "$real" symbolic-ref -q HEAD >/dev/null && echo "on-branch $(git -C "$real" symbolic-ref --short HEAD)"
	head=$(git -C "$real" rev-parse -q --verify 'HEAD^{commit}') || { echo "no valid HEAD"; return 0; }
	base=$(git -C "$real" rev-parse -q --verify "$REMOTE/$BASE^{commit}") || { echo "no $REMOTE/$BASE"; return 0; }
	[[ $head == "$base" ]] || echo "head ${head:0:12} != $REMOTE/$BASE ${base:0:12}"
	if slot_in_use "$1"; then
		if ((CWDS_FAILED == 0)); then echo "process-cwd"; else echo "process check unavailable"; fi
	fi
	return 0
}

lease_valid() {
	local f=$1
	[[ -n $(field "$f" token) && -n $(field "$f" task) && $(field "$f" slot) == "$2" ]]
}

pid_alive() { kill -0 "$1" 2>/dev/null; }

# free | busy | stale-suspected | inconsistent, plus detail lines.
slot_state() {
	local slot=$1 f problems pid
	f=$(lease_file "$slot")
	problems=$(slot_problems "$slot")
	if [[ ! -e $f ]]; then
		if [[ -z $problems ]]; then echo free; else echo inconsistent; echo "unleased: $problems"; fi
		return
	fi
	if ! lease_valid "$f" "$slot"; then echo inconsistent; echo "malformed lease $f"; return; fi
	pid=$(field "$f" pid)
	if [[ -n $pid && $(field "$f" host) == "$(hostname)" ]] && ! pid_alive "$pid" && ! slot_in_use "$slot"; then
		echo stale-suspected
	else
		echo busy
	fi
	sed -e '/^token=/d' -e 's/^/  /' "$f"
}

cmd_status() {
	local slots=("${WORKERS[@]}" "$REVIEW") s state
	[[ $# -gt 0 ]] && slots=("$@")
	for s in "${slots[@]}"; do
		known_slot "$s" || die "unknown slot $s" 2
		state=$(slot_state "$s")
		echo "$s: ${state%%$'\n'*}"
		if [[ $state == *$'\n'* ]]; then echo "${state#*$'\n'}"; fi
	done
}

random_token() { od -An -tx1 -N16 /dev/urandom | tr -d ' \n'; }

# Atomic: the lease is written in full to a private file, then hard-linked
# into place. link(2) fails if the lease exists, so exactly one caller wins
# and no reader ever sees a partial lease.
try_acquire() {
	local slot=$1 f tmp token problems
	f=$(lease_file "$slot")
	[[ -e $f ]] && return 1
	lock_slot "$slot" || { echo "$slot lock held: $LEASES/.$slot.lock" >&2; return 1; }
	try_acquire_locked "$slot"
	local rc=$?
	unlock_slot
	return $rc
}

try_acquire_locked() {
	local slot=$1 f tmp token problems
	f=$(lease_file "$slot")
	git -C "$(slot_path "$slot")" fetch -q "$REMOTE" 2>/dev/null || { echo "$slot: fetch of $REMOTE failed" >&2; return 1; }
	problems=$(slot_problems "$slot")
	[[ -z $problems ]] || { echo "$slot not idle: $problems" >&2; return 1; }
	token=$(random_token)
	tmp=$(mktemp "$LEASES/.$slot.XXXXXX") || return 1
	{
		echo "slot=$slot"
		echo "task=$TASK"
		echo "agent=$AGENT"
		echo "pr=$PR"
		echo "owner=$OWNER"
		echo "host=$(hostname)"
		echo "pid=$LEASE_PID"
		echo "utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo "token=$token"
	} >"$tmp" || { rm -f "$tmp"; return 1; }
	if ln "$tmp" "$f" 2>/dev/null; then
		rm -f "$tmp"
		echo "leased $slot token=$token"
		return 0
	fi
	rm -f "$tmp"
	return 1
}

cmd_acquire() {
	local slot="" review=0 s
	TASK="" AGENT="" PR="" OWNER="${USER:-}" LEASE_PID=""
	while [[ $# -gt 0 ]]; do
		case $1 in
		--slot) need "$@"; slot=$2; shift 2 ;;
		--task) need "$@"; TASK=$2; shift 2 ;;
		--agent) need "$@"; AGENT=$2; shift 2 ;;
		--pr) need "$@"; PR=$2; shift 2 ;;
		--owner) need "$@"; OWNER=$2; shift 2 ;;
		--pid) need "$@"; LEASE_PID=$2; shift 2 ;;
		--review) review=1; shift ;;
		*) die "unknown argument $1" 2 ;;
		esac
	done
	[[ -n $slot && -n $TASK && -n $AGENT ]] || die "acquire needs --slot, --task and --agent" 2
	[[ $TASK$AGENT$PR$OWNER$LEASE_PID != *$'\n'* ]] || die "metadata must be one line" 2
	mkdir -p "$LEASES"
	if [[ $slot == auto ]]; then
		((review == 0)) || die "--review needs --slot $REVIEW" 2
		for s in "${WORKERS[@]}"; do try_acquire "$s" 2>/dev/null && return 0; done
		die BLOCKED_POOL_BUSY 3
	fi
	known_slot "$slot" || die "unknown slot $slot" 2
	if [[ $slot == "$REVIEW" ]]; then
		((review == 1)) || die "$REVIEW is the review slot; it is never leased for implementation"
	else
		((review == 0)) || die "$slot is an implementation slot"
	fi
	try_acquire "$slot" || die "$slot not leased: $(slot_state "$slot" | sed -n 1p)"
}

# Takes the slot lock and checks the caller owns the lease.
own_lease() {
	local slot=$1 token=$2 f
	f=$(lease_file "$slot")
	lock_slot "$slot" || die "$slot lock held: $LEASES/.$slot.lock"
	[[ -e $f ]] || die "$slot has no lease"
	[[ $(field "$f" token) == "$token" ]] || die "token does not match the $slot lease"
}

cmd_release() {
	local slot="" token="" p
	while [[ $# -gt 0 ]]; do
		case $1 in
		--slot) need "$@"; slot=$2; shift 2 ;;
		--token) need "$@"; token=$2; shift 2 ;;
		*) die "unknown argument $1" 2 ;;
		esac
	done
	[[ -n $slot && -n $token ]] || die "release needs --slot and --token" 2
	known_slot "$slot" || die "unknown slot $slot" 2
	own_lease "$slot" "$token"
	p=$(slot_path "$slot")
	slot_in_use "$slot" && die "$slot is in use by a process; leave it first"
	[[ -z $(git -C "$p" status --porcelain --untracked-files=all) ]] || die "$slot is dirty; commit or archive the work, lease kept"
	git -C "$p" fetch -q --prune "$REMOTE" || die "fetch failed; lease kept"
	git -C "$p" merge-base --is-ancestor HEAD "$REMOTE/$BASE" ||
		[[ -n $(git -C "$p" branch -r --contains HEAD --list "$REMOTE/*") ]] ||
		die "$slot HEAD is neither pushed nor merged; lease kept"
	git -C "$p" switch -q --detach "$REMOTE/$BASE" || die "could not return $slot to $REMOTE/$BASE; lease kept"
	rm "$(lease_file "$slot")"
	echo "released $slot at $(git -C "$p" rev-parse --short HEAD)"
}

# Explicit recovery of a lease whose recorded process is gone. The lease is
# archived, never deleted, and the slot's files are not touched.
cmd_reclaim() {
	local slot="" token="" reason="" archive
	while [[ $# -gt 0 ]]; do
		case $1 in
		--slot) need "$@"; slot=$2; shift 2 ;;
		--token) need "$@"; token=$2; shift 2 ;;
		--reason) need "$@"; reason=$2; shift 2 ;;
		*) die "unknown argument $1" 2 ;;
		esac
	done
	[[ -n $slot && -n $token && -n $reason ]] || die "reclaim needs --slot, --token and --reason" 2
	known_slot "$slot" || die "unknown slot $slot" 2
	own_lease "$slot" "$token"
	[[ $(slot_state "$slot" | sed -n 1p) == stale-suspected ]] ||
		die "$slot is not stale-suspected; reclaim refused"
	archive="$LEASES/archive"
	mkdir -p "$archive"
	mv "$(lease_file "$slot")" "$archive/$slot.$(date -u +%Y%m%dT%H%M%SZ).$token.lease"
	echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) reclaim $slot token=$token by=${USER:-} reason=$reason" >>"$archive/audit.log"
	echo "reclaimed $slot; slot state now: $(slot_state "$slot" | sed -n 1p)"
}

case ${1:-} in
status) shift; cmd_status "$@" ;;
acquire) shift; cmd_acquire "$@" ;;
release) shift; cmd_release "$@" ;;
reclaim) shift; cmd_reclaim "$@" ;;
*) die "usage: pool.sh status|acquire|release|reclaim (see header)" 2 ;;
esac

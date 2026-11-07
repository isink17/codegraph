#!/usr/bin/env bash
# Lease manager for the CodeGraph agent pool. Policy: docs/agent-pool.md.
#
#   pool.sh status [slot]
#   pool.sh assess <slot>
#   pool.sh acquire --slot <slot|auto> --task <task> --agent <agent> [--pr N] [--owner who] [--pid PID] [--review]
#   pool.sh release --slot <slot> --token <token>
#   pool.sh review-checkout --slot codegraph-review --token <token> --branch <branch> --head SHA
#   pool.sh reclaim --slot <slot> --token <token> --reason <text>
#   pool.sh prepare --slot <slot> | --all
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
# Lease record schema. A lease written before versioning has no version field
# and stays recognizable and busy; a version this script does not know is
# refused rather than reinterpreted.
LEASE_VERSION=1

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

# Prints why a slot is not a clean, detached, unused worktree of the
# repository; prints nothing when it is. Ignores where HEAD points.
slot_shape_problems() {
	local p real top common repo changes
	p=$(slot_path "$1")
	real=$(cd "$p" 2>/dev/null && pwd -P) || { echo "missing"; return 0; }
	repo=$(cd "$REPO/.git" 2>/dev/null && pwd -P) || { echo "repository $REPO missing"; return 0; }
	top=$(git -C "$real" rev-parse --show-toplevel 2>/dev/null) &&
		common=$(cd "$real" && cd "$(git rev-parse --git-common-dir)" && pwd -P) &&
		[[ $top == "$real" && $common == "$repo" ]] ||
		{ echo "not a worktree of $REPO"; return 0; }
	# status also inspects leased slots: no index refresh, whose index.lock
	# would make a working agent's git add or commit fail.
	changes=$(git --no-optional-locks -C "$real" status --porcelain --untracked-files=all 2>&1) ||
		{ echo "git status failed"; return 0; }
	[[ -z $changes ]] || echo "dirty"
	git -C "$real" symbolic-ref -q HEAD >/dev/null && echo "on-branch $(git -C "$real" symbolic-ref --short HEAD)"
	git -C "$real" rev-parse -q --verify 'HEAD^{commit}' >/dev/null || echo "no valid HEAD"
	if slot_in_use "$1"; then
		if ((CWDS_FAILED == 0)); then echo "process-cwd"; else echo "process check unavailable"; fi
	fi
	return 0
}

# Prints why a slot is not an idle baseline; prints nothing when it is.
slot_problems() {
	local p head base problems
	problems=$(slot_shape_problems "$1")
	[[ -z $problems ]] || { echo "$problems"; return 0; }
	p=$(slot_path "$1")
	head=$(git -C "$p" rev-parse -q --verify 'HEAD^{commit}') || { echo "no valid HEAD"; return 0; }
	base=$(git -C "$p" rev-parse -q --verify "$REMOTE/$BASE^{commit}") || { echo "no $REMOTE/$BASE"; return 0; }
	[[ $head == "$base" ]] || echo "head ${head:0:12} != $REMOTE/$BASE ${base:0:12}"
	return 0
}

lease_valid() {
	local f=$1 v
	v=$(field "$f" version)
	[[ -z $v || $v == "$LEASE_VERSION" ]] || return 1
	[[ -n $(field "$f" token) && -n $(field "$f" task) && $(field "$f" slot) == "$2" ]]
}

pid_alive() { kill -0 "$1" 2>/dev/null; }

# Identity of the running kernel boot, recorded so a later reader can tell
# that a recorded PID cannot still be the process it named. Empty when the
# host reports none. Tests supply the value in a file.
boot_id() {
	local v=""
	if [[ -n ${CG_POOL_BOOT_FILE:-} ]]; then
		v=$(sed -n 1p "$CG_POOL_BOOT_FILE" 2>/dev/null) || v=""
	else
		v=$(sysctl -n kern.boottime 2>/dev/null | sed -n 's/.*sec = \([0-9]*\).*/\1/p') || v=""
		[[ -n $v ]] || v=$(sed -n 's/^btime //p' /proc/stat 2>/dev/null | head -n 1)
	fi
	printf '%s' "${v//[^[:print:]]/}"
}

# free | busy | stale-suspected | inconsistent, plus detail lines.
slot_state() {
	local slot=$1 f problems pid version
	f=$(lease_file "$slot")
	problems=$(slot_problems "$slot")
	if [[ ! -e $f ]]; then
		if [[ -z $problems ]]; then echo free; else echo inconsistent; echo "unleased: $problems"; fi
		return
	fi
	if ! lease_valid "$f" "$slot"; then
		echo inconsistent
		version=$(field "$f" version)
		if [[ -n $version && $version != "$LEASE_VERSION" ]]; then
			echo "lease schema version $version unsupported; this pool.sh writes $LEASE_VERSION"
		else
			echo "malformed lease $f"
		fi
		return
	fi
	# Boot identity is recorded for evidence only: a mismatch never widens
	# stale-suspected, because a dead PID does not prove its children died.
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
	local slot=$1 f
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
		echo "version=$LEASE_VERSION"
		echo "lease_id=$(random_token)"
		echo "slot=$slot"
		echo "task=$TASK"
		echo "agent=$AGENT"
		echo "pr=$PR"
		echo "owner=$OWNER"
		echo "host=$(hostname)"
		echo "boot=$(boot_id)"
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
	lease_valid "$f" "$slot" || die "the $slot lease is malformed or of an unsupported schema; owner must inspect"
	[[ $(field "$f" token) == "$token" ]] || die "token does not match the $slot lease"
}

# True when releasing HEAD loses nothing:
# - HEAD is an ancestor of base, or
# - HEAD is in a branch that still exists on the remote, or
# - HEAD is in a local branch (which release keeps) and merging it into base
#   changes nothing: the squash-merge case, where GitHub deleted the PR branch.
# A detached commit that no local branch holds is never released on content
# alone. Any Git error counts as not integrated.
head_integrated() {
	local p=$1 head=$2 base=$3 remote held merged base_tree
	git -C "$p" merge-base --is-ancestor "$head" "$base" && return 0
	remote=$(git -C "$p" for-each-ref --contains "$head" --format='%(refname)' "refs/remotes/$REMOTE/") || return 1
	[[ -n $remote ]] && return 0
	held=$(git -C "$p" for-each-ref --contains "$head" --format='%(refname)' refs/heads/) || return 1
	[[ -n $held ]] || return 1
	merged=$(git -C "$p" merge-tree --write-tree "$base" "$head" 2>/dev/null) || return 1
	base_tree=$(git -C "$p" rev-parse -q --verify "$base^{tree}") || return 1
	[[ ${merged%%$'\n'*} == "$base_tree" ]]
}

cmd_release() {
	local slot="" token="" p changes head base
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
	changes=$(git -C "$p" status --porcelain --untracked-files=all) || die "git status failed in $slot; lease kept"
	[[ -z $changes ]] || die "$slot is dirty; commit or archive the work, lease kept"
	head=$(git -C "$p" rev-parse -q --verify 'HEAD^{commit}') || die "$slot has no valid HEAD; lease kept"
	git -C "$p" fetch -q --prune "$REMOTE" || die "fetch failed; lease kept"
	base=$(git -C "$p" rev-parse -q --verify "$REMOTE/$BASE^{commit}") || die "no valid $REMOTE/$BASE; lease kept"
	head_integrated "$p" "$head" "$base" || die "$slot HEAD is neither pushed nor merged; lease kept"
	git -C "$p" switch -q --detach "$base" || die "could not return $slot to $REMOTE/$BASE; lease kept"
	rm "$(lease_file "$slot")"
	echo "released $slot at $(git -C "$p" rev-parse --short HEAD)"
}

cmd_review_checkout() {
	local slot="" token="" branch="" head="" p problems fetched
	while [[ $# -gt 0 ]]; do
		case $1 in
		--slot) need "$@"; slot=$2; shift 2 ;;
		--token) need "$@"; token=$2; shift 2 ;;
		--branch) need "$@"; branch=$2; shift 2 ;;
		--head) need "$@"; head=$2; shift 2 ;;
		*) die "unknown argument $1" 2 ;;
		esac
	done
	[[ $slot == "$REVIEW" && -n $token && -n $branch && $head =~ ^[0-9a-fA-F]{40}$ ]] ||
		die "review-checkout needs --slot $REVIEW, --token, --branch and a 40-character --head" 2
	git check-ref-format --branch "$branch" >/dev/null 2>&1 || die "invalid review branch" 2
	own_lease "$slot" "$token"
	p=$(slot_path "$slot")
	problems=$(slot_shape_problems "$slot")
	[[ -z $problems ]] || die "$slot is not safe for review checkout: ${problems//$'\n'/; }"
	git -C "$p" fetch -q "$REMOTE" "+refs/heads/$branch:refs/coordinator/review" ||
		die "could not fetch review branch $branch"
	fetched=$(git -C "$p" rev-parse -q --verify refs/coordinator/review^{commit}) ||
		die "fetched review branch is unavailable"
	[[ $fetched == "$head" ]] || die "review branch head is $fetched, expected $head"
	git -C "$p" checkout -q --detach "$head" || die "could not checkout review head $head"
	[[ $(git -C "$p" rev-parse HEAD) == "$head" ]] || die "review checkout landed at wrong head"
	echo "checked out review $head"
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

# Read-only evidence about one slot: who holds it, what can and cannot be
# observed about the recorded process, and what the worktree contains. It
# prints observations and explicit unknowns, never a verdict, never a token,
# and it mutates nothing (no fetch, no lock, no lease change). It grants no
# recovery authorization: recovering a lease still needs the owner.
cmd_assess() {
	local slot=${1:-} f p pid lease_boot now_boot head branch changes ignored base version
	[[ $# -eq 1 ]] || die "assess needs one slot" 2
	known_slot "$slot" || die "unknown slot $slot" 2
	f=$(lease_file "$slot")
	p=$(slot_path "$slot")
	now_boot=$(boot_id)
	echo "slot: $slot"
	echo "path: $p"
	echo "state: $(slot_state "$slot" | sed -n 1p)"
	echo "host: $(hostname)"
	echo "boot: ${now_boot:-unknown}"
	if [[ -e $f ]]; then
		echo "lease: present"
		sed -e '/^token=/d' -e 's/^/  /' "$f"
		version=$(field "$f" version)
		[[ -n $version ]] || echo "lease-schema: absent (written before versioning); treated as busy"
		pid=$(field "$f" pid)
		if [[ -z $pid ]]; then
			echo "recorded-process: none recorded; no automatic stale detection"
		elif [[ $(field "$f" host) != "$(hostname)" ]]; then
			echo "recorded-process: pid $pid recorded on host $(field "$f" host); not inspectable here"
		else
			lease_boot=$(field "$f" boot)
			if [[ -z $lease_boot || -z $now_boot ]]; then
				echo "recorded-process: pid $pid, boot identity unknown"
			elif [[ $lease_boot != "$now_boot" ]]; then
				echo "recorded-process: pid $pid recorded under an earlier boot; a live pid $pid is a different process"
			else
				echo "recorded-process: pid $pid recorded under this boot"
			fi
			if pid_alive "$pid"; then
				echo "recorded-pid-signalable: yes"
			else
				echo "recorded-pid-signalable: no; gone, owned by another user, or inspection refused"
			fi
		fi
	else
		echo "lease: absent"
	fi
	read_cwds
	if ((CWDS_FAILED != 0)); then
		echo "process-cwd: unknown; inspection unavailable"
	elif slot_in_use "$slot"; then
		echo "process-cwd: a process has its cwd in the slot"
	else
		echo "process-cwd: none observed; this does not prove no writer"
	fi
	if changes=$(git --no-optional-locks -C "$p" status --porcelain --untracked-files=all 2>/dev/null); then
		echo "changed-or-untracked-files: $(printf '%s' "$changes" | grep -c . || true)"
	else
		echo "changed-or-untracked-files: unknown; git status failed"
	fi
	if ignored=$(git --no-optional-locks -C "$p" status --porcelain --ignored=matching --untracked-files=all 2>/dev/null); then
		echo "ignored-files: $(printf '%s' "$ignored" | grep -c '^!!' || true)"
	else
		echo "ignored-files: unknown; git status failed"
	fi
	head=$(git -C "$p" rev-parse -q --verify 'HEAD^{commit}' 2>/dev/null) || head=""
	echo "head: ${head:-unknown}"
	branch=$(git -C "$p" symbolic-ref -q --short HEAD 2>/dev/null) || branch=""
	echo "branch: ${branch:-detached}"
	base=$(git -C "$p" rev-parse -q --verify "$REMOTE/$BASE^{commit}" 2>/dev/null) || base=""
	echo "base: ${base:-unknown} ($REMOTE/$BASE as last fetched; assess does not fetch)"
	if [[ -n $head && -n $base ]] && head_integrated "$p" "$head" "$base"; then
		echo "head-integrated: yes, by local refs"
	else
		echo "head-integrated: no or unknown"
	fi
	echo "note: observations only; no recovery authorization is implied"
}

# Moves an idle, unleased slot forward to the freshly fetched base. Only a
# fast-forward of a clean detached HEAD; anything else is refused unchanged.
prepare_locked() {
	local slot=$1 p problems head base
	p=$(slot_path "$slot")
	[[ ! -e $(lease_file "$slot") ]] || { echo "$slot is leased; not prepared" >&2; return 1; }
	problems=$(slot_shape_problems "$slot")
	[[ -z $problems ]] || { echo "$slot not prepared: ${problems//$'\n'/; }" >&2; return 1; }
	git -C "$p" fetch -q "$REMOTE" || { echo "$slot: fetch of $REMOTE failed" >&2; return 1; }
	base=$(git -C "$p" rev-parse -q --verify "$REMOTE/$BASE^{commit}") || { echo "$slot: no valid $REMOTE/$BASE" >&2; return 1; }
	head=$(git -C "$p" rev-parse -q --verify 'HEAD^{commit}') || { echo "$slot: no valid HEAD" >&2; return 1; }
	if [[ $head == "$base" ]]; then
		echo "$slot already at ${base:0:12}"
		return 0
	fi
	git -C "$p" merge-base --is-ancestor "$head" "$base" ||
		{ echo "$slot HEAD ${head:0:12} is not an ancestor of $REMOTE/$BASE; not prepared" >&2; return 1; }
	git -C "$p" switch -q --detach "$base" || { echo "$slot: checkout of ${base:0:12} failed" >&2; return 1; }
	problems=$(slot_problems "$slot")
	[[ -z $problems ]] || { echo "$slot after prepare: ${problems//$'\n'/; }" >&2; return 1; }
	echo "prepared $slot ${head:0:12} -> ${base:0:12}"
}

prepare_slot() {
	local rc=0
	lock_slot "$1" || { echo "$1 lock held: $LEASES/.$1.lock" >&2; return 1; }
	prepare_locked "$1" || rc=$?
	unlock_slot
	return $rc
}

# Each slot is prepared on its own; --all is not all-or-nothing.
cmd_prepare() {
	local slots=() s rc=0
	case ${1:-} in
	--slot) need "$@"; slots=("$2"); [[ $# -eq 2 ]] || die "unknown argument $3" 2 ;;
	--all) slots=("${WORKERS[@]}" "$REVIEW"); [[ $# -eq 1 ]] || die "unknown argument $2" 2 ;;
	*) die "prepare needs --slot <slot> or --all" 2 ;;
	esac
	for s in "${slots[@]}"; do known_slot "$s" || die "unknown slot $s" 2; done
	mkdir -p "$LEASES"
	for s in "${slots[@]}"; do prepare_slot "$s" || rc=1; done
	return $rc
}

case ${1:-} in
status) shift; cmd_status "$@" ;;
assess) shift; cmd_assess "$@" ;;
acquire) shift; cmd_acquire "$@" ;;
release) shift; cmd_release "$@" ;;
review-checkout) shift; cmd_review_checkout "$@" ;;
reclaim) shift; cmd_reclaim "$@" ;;
prepare) shift; cmd_prepare "$@" ;;
*) die "usage: pool.sh status|assess|acquire|review-checkout|release|reclaim|prepare (see header)" 2 ;;
esac

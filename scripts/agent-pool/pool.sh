#!/usr/bin/env bash
# Lease manager for the CodeGraph agent pool. Policy: docs/agent-pool.md.
#
#   pool.sh status [slot]
#   pool.sh assess <slot>
#   pool.sh acquire --slot <slot|auto> --task <task> --agent <agent> [--pr N] [--owner who] [--pid PID] [--review]
#   pool.sh release --slot <slot> --token <token>
#   pool.sh review-checkout --slot codegraph-review --token <token> --branch <branch> --head SHA
#   pool.sh prepare --slot <slot> | --all
#   pool.sh migrate [--dry-run]
#   pool.sh unmigrate
#
# Exit codes: 0 ok, 1 refused, 2 usage, 3 BLOCKED_POOL_BUSY. acquire exits 0
# only after it has written a lease; any other exit it chooses means it wrote
# none, except a version 2 write whose directory flush failed after the record
# was published, which it reports. The coordinator relies on this to settle an
# interrupted acquire. reclaim is disabled.
set -euo pipefail
HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
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
# Version 2 records are read and written only by pool_record.py, never in
# bash. The schema marker chooses the mode under each slot lock: absent is a
# version 1 pool, 2 a migrated pool, anything else refuses every change.
PYTHON=${CG_POOL_PYTHON:-python3}
MARKER=$LEASES/.schema
RECOVERY=$LEASES/recovery

die() { echo "$1" >&2; exit "${2:-1}"; }
need() { [[ $# -ge 2 ]] || die "$1 needs a value" 2; }
lease_file() { echo "$LEASES/$1.lease"; }
slot_path() { echo "$POOL_ROOT/$1"; }
field() { sed -n "/^$2=/{s/^$2=//p;q;}" "$1" 2>/dev/null; }

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
# migrate and unmigrate hold every slot lock at once.
LOCKS=()
trap 'for l in ${LOCKS[@]+"${LOCKS[@]}"}; do rmdir "$l"; done' EXIT
lock_slot() {
	mkdir "$LEASES/.$1.lock" 2>/dev/null || return 1
	LOCKS+=("$LEASES/.$1.lock")
}
unlock_slot() {
	rmdir "${LOCKS[${#LOCKS[@]}-1]}"
	unset "LOCKS[${#LOCKS[@]}-1]"
}

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

# The lease fields a reader may see. A record of an unknown schema may name
# its secret anything, so this is an allowlist, not a token filter.
PUBLIC_FIELDS='version|lease_id|slot|task|agent|pr|owner|host|pid|utc'
public_lease() {
	awk -v keys="$PUBLIC_FIELDS" '
		BEGIN { n = split(keys, k, "|"); for (i = 1; i <= n; i++) allowed[k[i]] = 1 }
		{ at = index($0, "="); if (at < 2) next
		  key = substr($0, 1, at - 1)
		  if (allowed[key] && !seen[key]++) print "  " $0 }'
}

# The lease as one bounded read, empty when there is nothing to read. A real
# lease is a dozen lines, and only the first value of each field is ever
# reported, so nothing past the bound was readable anyway.
lease_text() { head -n 200 "$1" 2>/dev/null || true; }

# True when the record at $1 may be version 2 or is ambiguous about its
# version: it then goes to the strict parser. Legacy, version 1 and unknown
# versions keep the bash path below.
needs_strict() {
	local n
	grep -q -E '^(lease_token|state|generation|recovery_op|legacy_digest)=' "$1" 2>/dev/null && return 0
	n=$(grep -c '^version=' "$1" 2>/dev/null) || true
	((${n:-0} > 1)) && return 0
	grep -q '^version=2' "$1" 2>/dev/null
}

# The strict parser's key=value classification of the record at $2 for slot
# $1. Fails when the interpreter is missing, fails, or prints anything else;
# callers then treat the record as unknown, never as legacy.
strict_inspect() {
	local out
	out=$("$PYTHON" -I -B "$HERE/pool_record.py" inspect "$1" "$2" 2>/dev/null) || return 1
	[[ $out == record=* ]] || return 1
	printf '%s\n' "$out"
}
kv() { printf '%s\n' "$1" | sed -n "/^$2=/{s/^$2=//p;q;}"; }
# Public fields of a record the strict parser did not accept, minus any line
# that is not printable ASCII: such a record may not even be text.
raw_public() { printf '%s\n' "$1" | { LC_ALL=C grep -a -v '[^ -~]' || true; } | public_lease; }

# absent | 2 | unknown for the pool's record schema marker. Mutators act on
# the value read under the slot lock.
marker_state() {
	if [[ ! -e $MARKER && ! -L $MARKER ]]; then echo absent; return; fi
	if [[ -f $MARKER && ! -L $MARKER && $(cat "$MARKER" 2>/dev/null) == 2 ]]; then echo 2; else echo unknown; fi
}

# True when slot $1 may have recovery state this pool.sh cannot evaluate.
recovery_seen() {
	[[ -e $RECOVERY || -L $RECOVERY ]] || return 1
	[[ -d $RECOVERY && -x $RECOVERY ]] || return 0
	[[ -e $RECOVERY/$1 || -L $RECOVERY/$1 ]]
}

# The recovery gate of slot $1 in a migrated pool, from pool_record.py: sets
# GATE_CLASS (none, cancelled, active: open; blocked, incomplete, corrupt,
# unknown: blocked), GATE_OP, GATE_STATE and GATE_REASON. Anything but exit 0
# with exactly the six expected lines for this slot is unknown, never none.
GATE_CLASS="" GATE_OP="" GATE_STATE="" GATE_REASON=""
read_gate() {
	local out re nl=$'\n'
	GATE_CLASS=none GATE_OP="" GATE_STATE="" GATE_REASON=""
	# -e is false for anything under a directory that cannot be searched.
	[[ -e $RECOVERY || -L $RECOVERY ]] || ! leases_observable || return 0
	GATE_CLASS=unknown GATE_REASON="the recovery gate could not be evaluated by $PYTHON"
	out=$("$PYTHON" -I -B "$HERE/pool_record.py" gate "$LEASES" "$1" 2>/dev/null) || return 0
	local LC_ALL=C
	re="^slot=([^$nl]*)${nl}recovery=(none|cancelled|active|blocked|incomplete|corrupt|unknown)${nl}gate=(open|blocked)"
	re+="${nl}recovery_operation=([A-Za-z0-9._-]*)${nl}recovery_state=([A-Z_]*)${nl}reason=([ -~]*)\$"
	[[ $out =~ $re && ${BASH_REMATCH[1]} == "$1" ]] || return 0
	case ${BASH_REMATCH[2]}/${BASH_REMATCH[3]} in
	none/open | cancelled/open | active/open | blocked/blocked | incomplete/blocked | corrupt/blocked | unknown/blocked) ;;
	*) return 0 ;;
	esac
	GATE_CLASS=${BASH_REMATCH[2]} GATE_OP=${BASH_REMATCH[4]} GATE_STATE=${BASH_REMATCH[5]} GATE_REASON=${BASH_REMATCH[6]}
}
gate_open() { [[ $GATE_CLASS == none || $GATE_CLASS == cancelled || $GATE_CLASS == active ]]; }
gate_refusal() { echo "${1:+$1 }recovery state is $GATE_CLASS${GATE_STATE:+ ($GATE_STATE)}: $GATE_REASON"; }

# Status detail lines for the recovery state of slot $1, in RECOVERY_LINES,
# with GATE_* set. Only a migrated pool evaluates recovery state; in any
# other pool, state that is present reads as unknown.
RECOVERY_LINES=""
recovery_status() {
	RECOVERY_LINES=""
	if ! leases_observable; then
		GATE_CLASS=unknown GATE_OP="" GATE_STATE="" GATE_REASON="lease directory $LEASES cannot be observed"
		RECOVERY_LINES="  recovery=unknown"
	elif [[ $(marker_state) == 2 ]]; then
		read_gate "$1"
		[[ $GATE_CLASS != none ]] || return 0
		RECOVERY_LINES="  recovery=$GATE_CLASS"
		[[ -z $GATE_OP ]] || RECOVERY_LINES+=$'\n'"  recovery_operation=$GATE_OP"
		[[ -z $GATE_STATE ]] || RECOVERY_LINES+=$'\n'"  recovery_state=$GATE_STATE"
	elif recovery_seen "$1"; then
		GATE_CLASS=unknown GATE_OP="" GATE_STATE="" GATE_REASON="state present that this pool.sh evaluates only in a migrated pool"
		RECOVERY_LINES="  recovery=unknown"
	else
		GATE_CLASS=none GATE_OP="" GATE_STATE="" GATE_REASON=""
	fi
}
recovery_detail() { [[ -z $RECOVERY_LINES ]] || printf '%s\n' "$RECOVERY_LINES"; }
recovery_note() {
	if [[ $(marker_state) == 2 ]]; then gate_refusal ""; else echo "recovery state present; this pool.sh cannot evaluate it"; fi
}

# Refuses, before anything changes, a slot of a version 1 pool this pool.sh
# must not touch.
fence() {
	local f
	! recovery_seen "$1" || die "$1 has recovery state under $RECOVERY; this pool.sh does not change it"
	f=$(lease_file "$1")
	if [[ -e $f ]] && needs_strict "$f"; then
		die "$1 holds a version 2 or ambiguous slot record in a pool without the schema marker; not changed"
	fi
	return 0
}

# Status of a record that needs the strict parser: <slot> <file> <lease text>
# <slot problems> <recovery blocks the slot>. recovery_status has run.
strict_state() {
	local slot=$1 out rec pid host id marker
	if ! out=$(strict_inspect "$slot" "$2"); then
		echo inconsistent
		echo "slot record could not be classified by the strict parser ($PYTHON); owner must inspect"
		echo "  lease=present"
		echo "  record_state=unknown"
		raw_public "$3"
		recovery_detail
		return
	fi
	rec=$(kv "$out" record)/$(kv "$out" state)
	id=$(printf '%s\n' "$out" | sed -n -e 's/^\(lease_id=.*\)/  \1/p' -e 's/^\(legacy_digest=.*\)/  \1/p')
	case $rec in
	v2/free)
		marker=$(marker_state)
		if [[ -z $4 && -z $5 && $marker == 2 ]]; then
			echo free
		else
			echo inconsistent
			[[ $marker == 2 ]] || echo "free version 2 record in a pool whose record schema marker is $marker; not leasable"
			[[ -z $4 ]] || echo "unleased: $4"
			[[ -z $5 ]] || recovery_note
		fi
		# No active lease; the permanent record file itself is present.
		echo "  lease=absent"
		echo "  record_state=free"
		;;
	v2/leased)
		pid=$(kv "$out" public.pid)
		host=$(kv "$out" public.host)
		if [[ -n $pid && $host == "$(hostname)" ]] && ! pid_alive "$pid" && ! slot_in_use "$slot"; then
			echo stale-suspected
		else
			echo busy
		fi
		echo "  lease=present"
		echo "  record_state=leased"
		echo "  version=2"
		echo "  slot=$slot"
		echo "$id"
		printf '%s\n' "$out" | sed -n 's/^public\./  /p'
		;;
	v2/quarantined)
		echo inconsistent
		echo "quarantined by recovery operation $(kv "$out" recovery_op); owner must inspect"
		echo "  lease=present"
		echo "  record_state=quarantined"
		echo "  recovery_op=$(kv "$out" recovery_op)"
		echo "  version=2"
		echo "  slot=$slot"
		echo "$id"
		printf '%s\n' "$out" | sed -n 's/^public\./  /p'
		;;
	*)
		echo inconsistent
		echo "unsupported or malformed slot record: $(kv "$out" reason)"
		echo "  lease=present"
		echo "  record_state=invalid"
		raw_public "$3"
		recovery_detail
		return
		;;
	esac
	echo "  generation=$(kv "$out" generation)"
	recovery_detail
}

# A lease file can be called absent only when the directory that would hold
# it can be searched. A missing directory whose nearest existing ancestor can
# be searched is a pool that has never leased anything.
leases_observable() {
	local d=$LEASES
	while [[ ! -e $d && ! -L $d && $d != / && $d != . ]]; do d=$(dirname "$d"); done
	if [[ $d == "$LEASES" ]]; then [[ -d $d && -r $d && -x $d ]]; else [[ -d $d && -x $d ]]; fi
}

# present | absent | unknown for the lease file $1. A path that cannot be
# searched also fails -e, so absent needs the directory observable on both
# sides of the test.
lease_seen() {
	if leases_observable; then
		if [[ -e $1 || -L $1 ]]; then echo present; return; fi
		if leases_observable; then echo absent; return; fi
	fi
	echo unknown
}

# free | busy | stale-suspected | inconsistent, plus detail lines.
slot_state() {
	local slot=$1 f problems pid version lease seen recovery="" marker
	f=$(lease_file "$slot")
	seen=$(lease_seen "$f")
	if [[ $seen == unknown ]]; then
		echo inconsistent
		echo "lease directory $LEASES cannot be observed"
		echo "  lease=unknown"
		return
	fi
	problems=$(slot_problems "$slot")
	recovery_status "$slot"
	gate_open || recovery=1
	if [[ $seen == absent ]]; then
		marker=$(marker_state)
		if [[ $marker != absent ]]; then
			echo inconsistent
			echo "slot record missing in a pool carrying the record schema marker ($marker)"
			echo "  lease=absent"
			echo "  record_state=missing"
		elif [[ -z $problems && -z $recovery ]]; then
			echo free
			echo "  lease=absent"
		else
			echo inconsistent
			[[ -z $problems ]] || echo "unleased: $problems"
			[[ -z $recovery ]] || recovery_note
			echo "  lease=absent"
		fi
		recovery_detail
		return
	fi
	lease=$(lease_text "$f")
	if needs_strict "$f"; then
		strict_state "$slot" "$f" "$lease" "$problems" "$recovery"
		return
	fi
	if ! lease_valid "$f" "$slot"; then
		echo inconsistent
		version=$(field "$f" version) || version=""
		if [[ -n $version && $version != "$LEASE_VERSION" ]]; then
			echo "lease schema version $version unsupported; this pool.sh writes $LEASE_VERSION"
		else
			echo "malformed lease $f"
		fi
		# The fields follow even here: a reader that sees a state but no lease
		# concludes the slot is unheld and discards the token that still guards it.
		echo "  lease=present"
		printf '%s\n' "$lease" | public_lease
		recovery_detail
		return
	fi
	pid=$(field "$f" pid)
	if [[ -n $pid && $(field "$f" host) == "$(hostname)" ]] && ! pid_alive "$pid" && ! slot_in_use "$slot"; then
		echo stale-suspected
	else
		echo busy
	fi
	echo "  lease=present"
	printf '%s\n' "$lease" | public_lease
	recovery_detail
}

cmd_status() {
	local slots=("${WORKERS[@]}" "$REVIEW") s state
	leases_observable || die "lease directory $LEASES cannot be observed; no lease can be called absent"
	[[ $# -gt 0 ]] && slots=("$@")
	for s in "${slots[@]}"; do
		known_slot "$s" || die "unknown slot $s" 2
		state=$(slot_state "$s")
		echo "$s: ${state%%$'\n'*}"
		if [[ $state == *$'\n'* ]]; then echo "${state#*$'\n'}"; fi
	done
}

random_token() { od -An -tx1 -N16 /dev/urandom | tr -d ' \n'; }

# True when $1 is printable ASCII only (no newline, tab or other control).
printable_ascii() {
	local LC_ALL=C
	[[ $1 != *[![:print:]]* ]]
}

try_acquire() {
	local slot=$1 rc=0
	lock_slot "$slot" || { echo "$slot lock held: $LEASES/.$slot.lock" >&2; return 1; }
	case $(marker_state) in
	absent) try_acquire_v1 "$slot" || rc=$? ;;
	2) try_acquire_v2 "$slot" || rc=$? ;;
	*) echo "the pool carries an unknown record schema marker $MARKER" >&2; rc=1 ;;
	esac
	unlock_slot
	return $rc
}

# The fetch and idle check every acquire makes under the slot lock.
acquire_idle() {
	local problems
	git -C "$(slot_path "$1")" fetch -q "$REMOTE" 2>/dev/null || { echo "$1: fetch of $REMOTE failed" >&2; return 1; }
	problems=$(slot_problems "$1")
	[[ -z $problems ]] || { echo "$1 not idle: $problems" >&2; return 1; }
}

# Atomic: the lease is written in full to a private file, then hard-linked
# into place. link(2) fails if the lease exists, so exactly one caller wins
# and no reader ever sees a partial lease.
try_acquire_v1() {
	local slot=$1 f tmp token
	f=$(lease_file "$slot")
	# A dangling link or an unobservable lease directory is an occupied lease
	# path, and the slot is not fetched into.
	[[ $(lease_seen "$f") == absent ]] || { echo "$slot lease path is not absent" >&2; return 1; }
	! recovery_seen "$slot" || { echo "$slot has recovery state under $RECOVERY" >&2; return 1; }
	acquire_idle "$slot" || return 1
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

# The free version 2 record of a slot, as `generation` on stdout; refuses
# anything else. Called under the slot lock of a migrated pool.
v2_free_generation() {
	local slot=$1 f out
	f=$(lease_file "$slot")
	read_gate "$slot"
	gate_open || { gate_refusal "$slot" >&2; return 1; }
	case $(lease_seen "$f") in
	present) ;;
	absent) echo "$slot slot record missing in a migrated pool; owner must intervene" >&2; return 1 ;;
	*) echo "$slot slot record cannot be observed" >&2; return 1 ;;
	esac
	out=$(strict_inspect "$slot" "$f") || { echo "$slot slot record could not be read by $PYTHON" >&2; return 1; }
	case $(kv "$out" record)/$(kv "$out" state) in
	v2/free) kv "$out" generation ;;
	*/leased) echo "$slot is leased" >&2; return 1 ;;
	*) echo "$slot holds an unusable slot record ($(kv "$out" record)/$(kv "$out" state)); owner must inspect" >&2; return 1 ;;
	esac
}

# The record is replaced by pool_record.py: the free record at the generation
# read here becomes a leased record at the next one, written durably. Only
# then is the token printed.
try_acquire_v2() {
	local slot=$1 f gen out token
	f=$(lease_file "$slot")
	gen=$(v2_free_generation "$slot") || return 1
	acquire_idle "$slot" || return 1
	out=$("$PYTHON" -I -B "$HERE/pool_record.py" acquire "$slot" "$f" "$gen" "task=$TASK" "agent=$AGENT" \
		"pr=$PR" "owner=$OWNER" "host=$(hostname)" "pid=$LEASE_PID") || {
		# Only these exits mean nothing was written; a signal or a crash after
		# the rename may have left a lease that no caller holds.
		case $? in 1 | 2 | 3 | 126 | 127) return 1 ;; *) return 4 ;; esac
	}
	token=$(kv "$out" token)
	[[ $token =~ ^[0-9a-f]{32}$ ]] ||
		{ echo "$slot: the lease may have been written but no token came back; owner must inspect" >&2; return 4; }
	echo "leased $slot token=$token"
}

cmd_acquire() {
	local slot="" review=0 s rc
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
	# Status prints these values; readers split lines on more than newlines.
	printable_ascii "$TASK$AGENT$PR$OWNER$LEASE_PID" || die "metadata must be one line of printable ASCII" 2
	[[ $slot == auto ]] || known_slot "$slot" || die "unknown slot $slot" 2
	case $(marker_state) in
	absent | 2) ;;
	*) die "the pool carries an unknown record schema marker $MARKER; nothing changed" ;;
	esac
	mkdir -p "$LEASES"
	if [[ $slot == auto ]]; then
		((review == 0)) || die "--review needs --slot $REVIEW" 2
		for s in "${WORKERS[@]}"; do
			rc=0
			try_acquire "$s" 2>/dev/null || rc=$?
			((rc != 0)) || return 0
			# 4: a lease may exist that no caller holds; never move on to the next slot.
			((rc != 4)) || die "$s: a lease was written but not confirmed; owner must inspect"
		done
		die BLOCKED_POOL_BUSY 3
	fi
	known_slot "$slot" || die "unknown slot $slot" 2
	if [[ $slot == "$REVIEW" ]]; then
		((review == 1)) || die "$REVIEW is the review slot; it is never leased for implementation"
	else
		((review == 0)) || die "$slot is an implementation slot"
	fi
	if [[ $(marker_state) == absent && -e $(lease_file "$slot") ]] && needs_strict "$(lease_file "$slot")"; then
		die "$slot holds a version 2 or ambiguous slot record in a pool without the schema marker; not changed"
	fi
	try_acquire "$slot" || {
		rc=$?
		((rc != 4)) || die "$slot: a lease may have been written but was not confirmed; owner must inspect"
		die "$slot not leased: $(slot_state "$slot" | sed -n 1p)"
	}
}

# Takes the slot lock and checks the caller owns the lease. Sets MODE to the
# pool mode read under the lock, and for a version 2 lease OWN_GEN and OWN_ID.
MODE="" OWN_GEN="" OWN_ID=""
own_lease() {
	local slot=$1 token=$2 f out
	f=$(lease_file "$slot")
	lock_slot "$slot" || die "$slot lock held: $LEASES/.$slot.lock"
	MODE=$(marker_state)
	case $MODE in
	absent)
		fence "$slot"
		[[ -e $f ]] || die "$slot has no lease"
		lease_valid "$f" "$slot" || die "the $slot lease is malformed or of an unsupported schema; owner must inspect"
		[[ $(field "$f" token) == "$token" ]] || die "token does not match the $slot lease"
		;;
	2)
		read_gate "$slot"
		gate_open || die "$(gate_refusal "$slot"); nothing changed"
		[[ $(lease_seen "$f") == present ]] || die "$slot slot record missing in a migrated pool; owner must intervene"
		# The token goes to the helper on stdin, never in its arguments.
		out=$(printf '%s\n' "$token" | "$PYTHON" -I -B "$HERE/pool_record.py" owns "$slot" "$f") ||
			die "$slot: lease ownership not confirmed; nothing changed"
		OWN_GEN=$(kv "$out" generation) OWN_ID=$(kv "$out" lease_id)
		[[ -n $OWN_GEN && -n $OWN_ID ]] || die "$slot: lease ownership not confirmed; nothing changed"
		;;
	*) die "the pool carries an unknown record schema marker $MARKER; nothing changed" ;;
	esac
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
	if [[ $MODE == 2 ]]; then
		printf '%s\n' "$token" |
			"$PYTHON" -I -B "$HERE/pool_record.py" release "$slot" "$(lease_file "$slot")" "$OWN_GEN" "$OWN_ID" ||
			die "$slot is back at $REMOTE/$BASE but freeing its record was not confirmed; check status, then retry release"
	else
		rm "$(lease_file "$slot")"
	fi
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

# Disabled: it acted on PID death alone and wrote the token into the archive
# file name and audit log. Orphaned leases go through the recovery protocol.
cmd_reclaim() {
	die "reclaim is disabled; a lease is cleared by its holder's release or by the owner (docs/agent-pool.md, Recovery)"
}

# Read-only evidence about one slot: who holds it, whether a process uses it,
# and what the worktree contains. It
# prints observations and explicit unknowns, never a verdict, never a token,
# and it mutates nothing (no fetch, no lock, no lease change). It grants no
# recovery authorization: recovering a lease still needs the owner.
cmd_assess() {
	local slot=${1:-} f p head branch changes ignored base refs lease seen full
	[[ $# -eq 1 ]] || die "assess needs one slot" 2
	known_slot "$slot" || die "unknown slot $slot" 2
	f=$(lease_file "$slot")
	p=$(slot_path "$slot")
	echo "slot: $slot"
	echo "path: $p"
	full=$(slot_state "$slot")
	echo "state: ${full%%$'\n'*}"
	echo "host: $(hostname)"
	echo "pool-schema-marker: $(marker_state)"
	recovery_status "$slot"
	if [[ $GATE_CLASS == none ]]; then
		echo "recovery: none"
	else
		echo "recovery: $GATE_CLASS; ${GATE_OP:+operation $GATE_OP; }${GATE_STATE:+state $GATE_STATE; }$GATE_REASON"
	fi
	# One snapshot: a lease released mid-report must read as gone, not abort.
	lease=$(lease_text "$f")
	seen=$(lease_seen "$f")
	if [[ $seen == unknown ]]; then
		echo "lease: unknown; lease directory cannot be observed"
	elif [[ $seen == absent ]]; then
		echo "lease: absent"
	elif [[ -e $f ]] && needs_strict "$f"; then
		echo "lease: present"
		echo "lease-schema: strict (version 2 or ambiguous; read by pool_record.py)"
		printf '%s\n' "$full" | sed -n 's/^  /  /p'
	elif [[ -z $lease ]]; then
		# The file is there and nothing came back from it: it is empty, or it
		# could not be read. Both are unknown, neither is absent.
		echo "lease: present but empty or unreadable"
	else
		echo "lease: present"
		printf '%s\n' "$lease" | public_lease
		[[ $'\n'$lease == *$'\n'version=* ]] || echo "lease-schema: absent (written before versioning); treated as busy"
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
	# Only the checks that read: release's merge-tree leg writes an object, and
	# assess never writes to the store of a slot someone else may be using.
	if [[ -n $head && -n $base ]] && git -C "$p" merge-base --is-ancestor "$head" "$base" 2>/dev/null; then
		echo "head-in-base: yes"
	else
		echo "head-in-base: no or unknown"
	fi
	if [[ -n $head ]] && refs=$(git -C "$p" for-each-ref --contains "$head" --format='%(refname)' \
		"refs/remotes/$REMOTE/" refs/heads/ 2>/dev/null) && [[ -n $refs ]]; then
		echo "head-in-refs: $(printf '%s' "$refs" | grep -c . || true) ref(s): $(printf '%s\n' "$refs" |
			sed -n '1,5p' | tr '\n' ' ')"
	else
		echo "head-in-refs: none or unknown"
	fi
	echo "note: observations only; no recovery authorization is implied"
}

# Moves an idle, unleased slot forward to the freshly fetched base. Only a
# fast-forward of a clean detached HEAD; anything else is refused unchanged.
# In a migrated pool the slot needs its free record; no record is written.
prepare_locked() {
	local slot=$1 p problems head base seen err
	p=$(slot_path "$slot")
	case $(marker_state) in
	absent)
		seen=$(lease_seen "$(lease_file "$slot")")
		if [[ $seen == present ]] && needs_strict "$(lease_file "$slot")"; then
			echo "$slot holds a version 2 or ambiguous slot record in a pool without the schema marker; not prepared" >&2
			return 1
		fi
		[[ $seen != present ]] || { echo "$slot is leased; not prepared" >&2; return 1; }
		[[ $seen == absent ]] || { echo "$slot lease cannot be observed; not prepared" >&2; return 1; }
		! recovery_seen "$slot" || { echo "$slot has recovery state under $RECOVERY; not prepared" >&2; return 1; }
		;;
	2)
		# "is leased" stays the exact wording: the coordinator filters it.
		err=$(v2_free_generation "$slot" 2>&1 >/dev/null) || { echo "$err; not prepared" >&2; return 1; }
		;;
	*) echo "the pool carries an unknown record schema marker $MARKER; $slot not prepared" >&2; return 1 ;;
	esac
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
	case $(marker_state) in
	absent | 2) ;;
	*) die "the pool carries an unknown record schema marker $MARKER; nothing changed" ;;
	esac
	mkdir -p "$LEASES"
	for s in "${slots[@]}"; do prepare_slot "$s" || rc=1; done
	return $rc
}

# Every slot gets the free record of generation 0, then the marker is written.
# All or nothing: one slot that is leased, not idle, in recovery or holding
# anything but that record refuses the whole migration and nothing is written.
# --dry-run takes no lock, does not fetch and writes nothing.
cmd_migrate() {
	local dry=0 s problems refused=0 slots=("${WORKERS[@]}" "$REVIEW")
	[[ ${1:-} == --dry-run ]] && { dry=1; shift; }
	[[ $# -eq 0 ]] || die "usage: pool.sh migrate [--dry-run]" 2
	[[ -d $LEASES ]] && leases_observable || die "lease directory $LEASES cannot be observed"
	if ((dry == 0)); then
		for s in "${slots[@]}"; do lock_slot "$s" || die "$s lock held: $LEASES/.$s.lock; nothing changed"; done
	fi
	for s in "${slots[@]}"; do
		problems=""
		if ((dry == 0)); then
			git -C "$(slot_path "$s")" fetch -q "$REMOTE" 2>/dev/null || problems="fetch of $REMOTE failed"
		elif [[ -e $LEASES/.$s.lock || -L $LEASES/.$s.lock ]]; then
			problems="slot lock held: $LEASES/.$s.lock"
		fi
		[[ -n $problems ]] || problems=$(slot_problems "$s")
		if [[ -n $problems ]]; then
			echo "$s: refused: not idle: ${problems//$'\n'/; }"
			refused=1
		fi
	done
	if ((dry == 1 || refused == 1)); then
		"$PYTHON" -I -B "$HERE/pool_record.py" migrate --dry-run "$LEASES" "${slots[@]}" || refused=1
		((refused == 0)) || die "migration refused; nothing written"
		echo "dry run: migration would proceed; no lock taken, no fetch, nothing written"
		return 0
	fi
	"$PYTHON" -I -B "$HERE/pool_record.py" migrate "$LEASES" "${slots[@]}" ||
		die "migration refused or not completed; status shows any record written"
	[[ $(marker_state) == 2 ]] || die "migration reported success but the marker is not 2; owner must inspect"
}

# Removes the free records of generation 0 and then the marker. Refuses,
# changing nothing, a pool with any other record, a missing record under the
# marker, or recovery state. A run that fails part way leaves the pool
# refusing every change until unmigrate completes.
cmd_unmigrate() {
	local s slots=("${WORKERS[@]}" "$REVIEW")
	[[ $# -eq 0 ]] || die "usage: pool.sh unmigrate" 2
	[[ -d $LEASES ]] && leases_observable || die "lease directory $LEASES cannot be observed"
	for s in "${slots[@]}"; do lock_slot "$s" || die "$s lock held: $LEASES/.$s.lock; nothing changed"; done
	"$PYTHON" -I -B "$HERE/pool_record.py" unmigrate "$LEASES" "${slots[@]}" ||
		die "unmigrate not completed; status shows what changed"
	[[ $(marker_state) == absent ]] || die "unmigrate reported success but the marker remains; owner must inspect"
}

case ${1:-} in
status) shift; cmd_status "$@" ;;
assess) shift; cmd_assess "$@" ;;
acquire) shift; cmd_acquire "$@" ;;
release) shift; cmd_release "$@" ;;
review-checkout) shift; cmd_review_checkout "$@" ;;
reclaim) shift; cmd_reclaim "$@" ;;
prepare) shift; cmd_prepare "$@" ;;
migrate) shift; cmd_migrate "$@" ;;
unmigrate) shift; cmd_unmigrate "$@" ;;
*) die "usage: pool.sh status|assess|acquire|review-checkout|release|reclaim|prepare|migrate|unmigrate (see header)" 2 ;;
esac

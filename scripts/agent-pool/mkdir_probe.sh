#!/usr/bin/env bash
# Diagnostic, not a gate: does this host's mkdir(1) exit 0 for a directory that
# another process created first? pool.sh's slot lock trusts that it does not.
#
#   bash mkdir_probe.sh <output dir>
#
# Writes key=value lines to <output dir>/summary.txt and raw per-contender
# event files beside it. Exits 0 unless the probe itself could not run.
set -u
OUT=${1:?usage: mkdir_probe.sh <output dir>}
ROUNDS=${PROBE_ROUNDS:-40}
N=${PROBE_CONTENDERS:-24}
HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
mkdir -p "$OUT" || exit 2
OUT=$(cd "$OUT" && pwd -P)
W=$(mktemp -d) || exit 2
trap 'rm -rf "$W"' EXIT
SUM=$OUT/summary.txt
: >"$SUM"
say() { printf '%s=%s\n' "$1" "$2" | tee -a "$SUM"; }

MK=$(command -v mkdir)
say host "$(uname -sr)"
say mkdir_path "$MK"
say mkdir_resolved "$(readlink -f "$MK" 2>/dev/null || echo "$MK")"
say mkdir_version "$({ mkdir --version 2>/dev/null || echo unknown; } | head -n 1)"
command -v dpkg-query >/dev/null &&
	say packages "$(dpkg-query -W -f='${Package}:${Version} ' coreutils coreutils-from-uutils coreutils-from-gnu rust-coreutils gnu-coreutils 2>/dev/null)"
say probe_fs "$(findmnt -n -o FSTYPE --target "$W" 2>/dev/null || echo unknown)"

# Sequential control: an existing directory must be refused.
mkdir "$W/seq" && { mkdir "$W/seq" 2>/dev/null; say sequential_exit "$?"; }

# One contender: its own event file, keyed by a unique acquisition id, so no
# shared log orders or merges the evidence.
contend() { # <round dir> <index> <command...>
	local d=$1 i=$2 rc=0
	shift 2
	"$@" "$d/lock" 2>/dev/null || rc=$?
	printf 'id=%s rc=%s ino=%s\n' "$(basename "$d").$i" "$rc" \
		"$(ls -di "$d/lock" 2>/dev/null | awk '{print $1}')" >"$d/ev.$i"
}

# race <name> <command...>: ROUNDS rounds of N contenders on one fresh path.
# Counts rounds in which more than one contender's command exited 0.
race() {
	local name=$1 r i d doubles=0 wins
	shift
	for ((r = 1; r <= ROUNDS; r++)); do
		d=$OUT/$name/r$r
		mkdir -p "$d"
		for ((i = 1; i <= N; i++)); do contend "$d" "$i" "$@" & done
		wait
		wins=$(cat "$d"/ev.* | grep -c ' rc=0 ')
		((wins == 1)) || { doubles=$((doubles + 1)); echo "$name round $r: $wins contenders exited 0" >>"$OUT/$name.rounds"; }
	done
	say "${name}_rounds" "$ROUNDS"
	say "${name}_contenders" "$N"
	say "${name}_rounds_not_one_winner" "$doubles"
}

race mkdir1 mkdir
# Control at the syscall: os.mkdir is one mkdir(2) and reports its result.
PY=$(command -v python3 || true)
[[ -n $PY ]] && race syscall "$PY" -I -c 'import os, sys; os.mkdir(sys.argv[1])'

# The same mkdir(1) under strace, when the host already has strace. Never
# installed or elevated: without it the section is skipped and says so.
if command -v strace >/dev/null; then
	lies=0 created=0 rounds=$ROUNDS
	for ((r = 1; r <= rounds; r++)); do
		d=$OUT/strace/r$r
		mkdir -p "$d"
		for ((i = 1; i <= N; i++)); do
			(rc=0; strace -qq -e trace=mkdir,mkdirat -o "$d/st.$i" mkdir "$d/lock" 2>/dev/null || rc=$?
				echo "$rc" >"$d/rc.$i") &
		done
		wait
		for ((i = 1; i <= N; i++)); do
			grep -q '= 0$' "$d/st.$i" 2>/dev/null && created=$((created + 1))
			[[ $(cat "$d/rc.$i") == 0 ]] && grep -q 'EEXIST' "$d/st.$i" && ! grep -q '= 0$' "$d/st.$i" &&
				{ lies=$((lies + 1)); echo "r$r contender $i: mkdir(2) EEXIST, exit 0" >>"$OUT/strace.lies"; }
		done
	done
	say strace_rounds "$rounds"
	say strace_syscall_creations "$created"
	say strace_exit0_after_eexist "$lies"
else
	say strace unavailable
fi

# pool.sh against a mkdir(1) that reports success for an existing directory,
# as observed above. A held lock with a foreign owner must stay held.
pool_case() { # <name> <PATH prefix or empty>
	local p=$W/pool-$1 rc=0
	mkdir -p "$p/leases/.codegraph-worker-a.lock" "$p/root"
	echo foreign >"$p/leases/.codegraph-worker-a.lock/owner"
	: >"$p/cwds"
	PATH="${2:+$2:}$PATH" CG_POOL_LEASES=$p/leases CG_POOL_ROOT=$p/root CG_POOL_REPO=$p/root \
		CG_POOL_CWD_FILE=$p/cwds bash "$HERE/pool.sh" release --slot codegraph-worker-a --token probe \
		>"$OUT/pool-$1.out" 2>"$OUT/pool-$1.err" || rc=$?
	say "pool_${1}_exit" "$rc"
	say "pool_${1}_owner_after" "$(cat "$p/leases/.codegraph-worker-a.lock/owner" 2>/dev/null || echo missing)"
}
mkdir "$W/eexist-ok"
printf '#!/bin/sh\nfor a; do :; done\n%s "$@" 2>/dev/null || [ -d "$a" ]\n' "$MK" >"$W/eexist-ok/mkdir"
chmod +x "$W/eexist-ok/mkdir"
pool_case real ""
pool_case eexist_ok "$W/eexist-ok"
exit 0

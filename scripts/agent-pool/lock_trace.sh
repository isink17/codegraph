#!/bin/sh
# Test-only stand-in for mkdir, rmdir and rm, installed as links with those
# names ahead of PATH. Runs the real command from LOCK_TRACE_PATH and appends
# one line to LOCK_TRACE for each call that names a slot lock: the run
# (LOCK_TRACE_NAME), the command, its exit status, pid and parent pid, the
# lock's inode and owner id before and after, and the arguments. A held lock
# that vanishes without a traced removal was removed by something else.
# The exit status is the command's, not its syscall's, and before and after
# are sampled around the command, so they may include other runs' changes.
op=${0##*/}
real=$(PATH=$LOCK_TRACE_PATH command -v "$op") || exit 127
lock=
for a in "$@"; do
	case $a in
	*/.*.lock) lock=$a ;;
	*/.*.lock/owner) lock=${a%/owner} ;;
	esac
done
[ -n "$lock" ] || exec "$real" "$@"
ino() { ls -di "$lock" 2>/dev/null | awk '{print $1}'; }
own() { cat "$lock/owner" 2>/dev/null; }
before="ino=$(ino) owner=$(own)"
rc=0
"$real" "$@" || rc=$?
printf '%s %s exit=%s pid=%s ppid=%s before[%s] after[ino=%s owner=%s] %s\n' "${LOCK_TRACE_NAME:-?}" "$op" \
	"$rc" "$$" "$PPID" "$before" "$(ino)" "$(own)" "$*" >>"$LOCK_TRACE"
exit $rc

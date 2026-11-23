#!/usr/bin/env bash
# Fails when internal operations tooling, tracker credentials or local ops
# state paths come back into the tracked tree. Kept narrow on purpose: product
# code and docs must not trip it. Run: bash .github/scripts/public-boundary.sh [repo-root]
set -euo pipefail

root=${1:-$(git rev-parse --show-toplevel)}

forbidden_paths='^(scripts/agent-(coordinator|pool)/|\.github/scripts/merge-gate|docs/agent-(pool|coordinator|campaign|skills)\.md$|CLAUDE\.md$|\.claude/|\.codex/|\.agentkit/)'

forbidden_text='agent-coordinator|agent-pool|agent-campaign|merge-gate\.sh|pool-safety'
forbidden_text+='|plane[-_]sync|(app|api)\.plane\.so|plane-mcp-|PLANE_(API|URL|WORKSPACE|PROJECT)'
forbidden_text+='|codegraph-ops([^a-z0-9-]|$)|codegraph-evidence/|codegraph-worker-[a-z0-9]|codegraph-review/'

status=0
files=$(git -C "$root" ls-files) || { echo "public-boundary: git ls-files failed"; exit 2; }
if paths=$(printf '%s\n' "$files" | grep -E "$forbidden_paths"); then
	echo "public-boundary: internal-only files are tracked:"
	echo "$paths"
	status=1
fi
rc=0
hits=$(git -C "$root" grep -n -I -i -E -e "$forbidden_text" -- . ':(exclude,glob).github/scripts/public-boundary*.sh') || rc=$?
case $rc in
0)
	echo "public-boundary: internal tooling, tracker or ops-path references:"
	echo "$hits"
	status=1
	;;
1) ;;
*) echo "public-boundary: git grep failed ($rc)"; exit 2 ;;
esac
[ "$status" -eq 0 ] && echo "public-boundary: clean"
exit "$status"

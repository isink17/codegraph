#!/usr/bin/env bash
# Tests for public-boundary.sh. Run: bash .github/scripts/public-boundary_test.sh
set -uo pipefail

script="$(cd "$(dirname "$0")" && pwd)/public-boundary.sh"
work=$(mktemp -d "${TMPDIR:-/tmp}/public-boundary.XXXXXX")
trap 'rm -rf "$work"' EXIT
failures=0

# repo NAME FILE CONTENT...: a fresh git repo holding a clean product file plus
# each FILE with its CONTENT, all tracked.
repo() {
	local dir="$work/$1"
	shift
	git init -q "$dir"
	mkdir -p "$dir/internal/store"
	printf 'f, err := os.CreateTemp(dir, ".codegraph-evidence-*")\n' >"$dir/internal/store/clean.go"
	while [ $# -gt 0 ]; do
		mkdir -p "$dir/$(dirname "$1")"
		printf '%s\n' "$2" >"$dir/$1"
		shift 2
	done
	git -C "$dir" add -A
	echo "$dir"
}

expect() {
	local name=$1 want=$2 dir=$3 got=0
	bash "$script" "$dir" >"$work/$name.out" 2>&1 || got=$?
	if [ "$got" != "$want" ]; then
		echo "FAIL $name: want exit $want, got $got"
		cat "$work/$name.out"
		failures=$((failures + 1))
	fi
}

expect clean 0 "$(repo clean docs/ci.md 'Pull requests need the aggregate ci check.')"
expect product-words 0 "$(repo product-words docs/x.md 'a lease on the scan lock; an A/B campaign; a connection pool')"
expect pool-script 1 "$(repo pool-script scripts/agent-pool/pool.sh 'echo')"
expect coordinator-dir 1 "$(repo coordinator-dir scripts/agent-coordinator/coordinator.py 'pass')"
expect merge-gate 1 "$(repo merge-gate .github/scripts/merge-gate.sh 'echo')"
expect agent-doc 1 "$(repo agent-doc docs/agent-skills.md 'policy')"
expect claude-md 1 "$(repo claude-md CLAUDE.md 'policy')"
expect plane-url 1 "$(repo plane-url docs/x.md 'see https://app.plane.so/ws/projects/p/issues/i')"
expect plane-env 1 "$(repo plane-env internal/x.go 'os.Getenv("PLANE_API_KEY")')"
expect plane-workspace 1 "$(repo plane-workspace docs/x.md 'PLANE_WORKSPACE=x')"
expect plane-api 1 "$(repo plane-api docs/x.md 'GET https://api.plane.so/api/v1/workspaces')"
expect plane-sync 1 "$(repo plane-sync docs/x.md 'run plane_sync.py')"
expect ops-path 1 "$(repo ops-path docs/x.md 'state lives in ~/src/codegraph-ops/pool')"
expect evidence-path 1 "$(repo evidence-path internal/x.go '// reports in codegraph-evidence/v2-run/raw')"
expect worker-path 1 "$(repo worker-path docs/x.md 'cd ../codegraph-worker-a')"
expect ci-job 1 "$(repo ci-job .github/workflows/ci.yml 'needs: [changes, pool-safety]')"
expect review-path 1 "$(repo review-path docs/x.md 'cd ../codegraph-review/')"
expect campaign-text 1 "$(repo campaign-text docs/x.md 'see the agent-campaign notes')"
expect codex-dir 1 "$(repo codex-dir .codex/config.toml 'x = 1')"
expect bad-root 2 "$work/does-not-exist"
expect this-tree 0 "$(git -C "$(dirname "$script")" rev-parse --show-toplevel)"

if [ "$failures" -ne 0 ]; then
	echo "public-boundary: $failures test(s) failed"
	exit 1
fi
echo "public-boundary: all tests passed"

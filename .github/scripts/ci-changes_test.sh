#!/usr/bin/env bash
# Tests for ci-changes.sh. Run: bash .github/scripts/ci-changes_test.sh
set -uo pipefail

script="$(cd "$(dirname "$0")" && pwd)/ci-changes.sh"
failures=0

expect() {
	local name=$1 want=$2 got=$3
	if [ "$want" != "$got" ]; then
		echo "FAIL $name: want '$want', got '$got'"
		failures=$((failures + 1))
	fi
}

paths() { printf '%s\0' "$@"; }

# classify_paths: the allowlist itself.
(
	set +e
	# shellcheck source=/dev/null
	source "$script" source-only
	set +e
	check() { expect "$1" "$2" "$(classify_paths)"; }
	check changelog true < <(paths CHANGELOG.md)
	check docs-tree true < <(paths README.md docs/scope-models.md docs/a/b/nested.md)
	check empty false < <(paths </dev/null)
	check mixed false < <(paths CHANGELOG.md internal/store/store.go)
	check workflow false < <(paths .github/workflows/ci.yml)
	check classifier false < <(paths .github/scripts/ci-changes.sh)
	check go-mod false < <(paths go.mod)
	check go-sum false < <(paths go.sum)
	check schema false < <(paths internal/store/schema/0001.sql)
	check fixture-md false < <(paths internal/store/testdata/fixture.md)
	check agents-md false < <(paths AGENTS.md)
	check skill-md false < <(paths skills/codegraph/SKILL.md)
	check docs-non-md false < <(paths docs/diagram.svg)
	check docs-sibling false < <(paths docs.md)
	check readme-suffix false < <(paths README.md.orig)
	check nested-readme false < <(paths sub/README.md)
	exit "$failures"
) || failures=$((failures + $?))

# classify and check-docs against a real repository.
repo=$(mktemp -d)
trap 'rm -rf "$repo"' EXIT
git -C "$repo" init -q
git -C "$repo" config user.email ci@example.invalid
git -C "$repo" config user.name ci
commit() { git -C "$repo" add -A && git -C "$repo" commit -qm "$1" && git -C "$repo" rev-parse HEAD; }
mkdir -p "$repo/docs" "$repo/internal" "$repo/.github/workflows"
printf '# Readme\n\nSee [scope](docs/scope.md).\n' >"$repo/README.md"
printf '# Scope\n' >"$repo/docs/scope.md"
printf 'package x\n' >"$repo/internal/x.go"
printf 'name: CI\n' >"$repo/.github/workflows/ci.yml"
base=$(commit base)

run() {
	(cd "$repo" && CI_EVENT=$1 CI_BASE=$2 CI_HEAD=$3 CI_STRESS=${4:-false} bash "$script" classify 2>/dev/null)
}
docs_check() {
	(cd "$repo" && CI_EVENT=pull_request CI_BASE=$1 CI_HEAD=$2 bash "$script" check-docs >/dev/null 2>&1) && echo pass || echo fail
}

printf '\n## Added\n' >>"$repo/docs/scope.md"
docs=$(commit docs)
expect pr-docs-only docs_only=true "$(run pull_request "$base" "$docs")"
expect push-docs-only docs_only=true "$(run push "$base" "$docs")"
expect pr-docs-stress docs_only=false "$(run pull_request "$base" "$docs" true)"
expect check-docs-clean pass "$(docs_check "$base" "$docs")"

printf 'package x // changed\n' >"$repo/internal/x.go"
code=$(commit code)
printf '\n## More\n' >>"$repo/docs/scope.md"
mixed=$(commit docs-after-code)
# The whole pull request or push counts, not just its last commit.
expect pr-mixed-earlier-code docs_only=false "$(run pull_request "$base" "$mixed")"
expect push-mixed-earlier-code docs_only=false "$(run push "$docs" "$mixed")"
expect push-docs-after-code docs_only=true "$(run push "$code" "$mixed")"

git -C "$repo" checkout -q "$base"
printf 'on: push\n' >>"$repo/.github/workflows/ci.yml"
workflow=$(commit workflow)
expect pr-workflow docs_only=false "$(run pull_request "$base" "$workflow")"

git -C "$repo" checkout -q "$base"
git -C "$repo" mv docs/scope.md internal/scope.go
renamed=$(commit rename)
expect pr-rename-out-of-docs docs_only=false "$(run pull_request "$base" "$renamed")"

zero=0000000000000000000000000000000000000000
missing=1111111111111111111111111111111111111111
expect push-new-branch docs_only=false "$(run push "$zero" "$docs")"
expect missing-base docs_only=false "$(run pull_request "$missing" "$docs")"
expect missing-head docs_only=false "$(run pull_request "$base" "$missing")"
expect empty-base docs_only=false "$(run pull_request "" "$docs")"
expect unknown-event docs_only=false "$(run workflow_dispatch "$base" "$docs")"
expect empty-diff docs_only=false "$(run pull_request "$docs" "$docs")"
expect check-docs-missing-base fail "$(docs_check "$missing" "$docs")"

# git itself failing must not read as an empty, docs-only change.
expect git-failure docs_only=false "$(cd "$repo" && PATH="$(mktemp -d)" CI_EVENT=pull_request CI_BASE=$base CI_HEAD=$docs /bin/bash "$script" classify 2>/dev/null)"

git -C "$repo" checkout -q "$base"
printf 'trailing   \n' >>"$repo/README.md"
whitespace=$(commit whitespace)
expect check-docs-whitespace fail "$(docs_check "$base" "$whitespace")"

git -C "$repo" checkout -q "$base"
printf '```go\nx := 1\n' >>"$repo/docs/scope.md"
fence=$(commit fence)
expect check-docs-open-fence fail "$(docs_check "$base" "$fence")"

git -C "$repo" checkout -q "$base"
printf 'See [gone](missing.md) and [web](https://example.com) and `[code](nope)`.\n' >>"$repo/docs/scope.md"
link=$(commit link)
expect check-docs-broken-link fail "$(docs_check "$base" "$link")"

git -C "$repo" checkout -q "$base"
printf '\xff\xfe\n' >>"$repo/docs/scope.md"
binary=$(commit binary)
expect check-docs-invalid-utf8 fail "$(docs_check "$base" "$binary")"

if [ "$failures" -ne 0 ]; then
	echo "$failures failure(s)"
	exit 1
fi
echo "ci-changes: all tests passed"

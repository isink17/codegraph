#!/bin/sh
set -eu

repo=$(git rev-parse --show-toplevel)
work=${CG33_WORK:-"$(mktemp -d "${TMPDIR:-/tmp}/cg33-oracle.XXXXXX")"}
if [ -d "$work" ] && [ -n "$(find "$work" -mindepth 1 -print -quit)" ]; then
	echo "work directory must be empty: $work" >&2
	exit 1
fi
mkdir -p "$work" "$work/repos" "$work/s1-fresh" "$work/results" "$work/home"
export CG33_WORK="$work"
export CODEGRAPH_HOME="$work/home"
export CODEGRAPH_NO_UPDATE_CHECK=1

rev=e532b8c6215bd9b1cdba0af537333d47f9eff364
if ! git -C "$repo" cat-file -e "$rev^{commit}" 2>/dev/null; then
	git -C "$repo" fetch --quiet --no-tags origin "$rev"
fi
mkdir -p "$work/codegraph-source"
git -C "$repo" archive "$rev" | tar -x -C "$work/codegraph-source"
(cd "$work/codegraph-source" && go build -o "$work/codegraph" ./cmd/codegraph)
export CODEGRAPH_BIN="$work/codegraph"

while read -r name url commit manifest_hash k fresh_expected s1_expected; do
	[ -n "$name" ] || continue
	case "$name" in \#*) continue ;; esac
	git clone --quiet "$url" "$work/repos/$name"
	git -C "$work/repos/$name" checkout --quiet "$commit"
	[ "$(git -C "$work/repos/$name" rev-parse HEAD)" = "$commit" ]
	[ "$(shasum -a 256 "$work/repos/$name/composer.json" | cut -d' ' -f1)" = "$manifest_hash" ]
	CODEGRAPH_HOME="$work/home" "$CODEGRAPH_BIN" index "$work/repos/$name" >"$work/results/$name.fresh.index.txt"
	fresh_total=0
	for strategy in php_composer_psr4 php_alias_static php_self_static php_this_instance php_type_scope php_typed_property; do
		python3 "$repo/scripts/cg33-oracle/oracle.py" "$work/repos/$name" "$work/repos/$name/.codegraph/codegraph.v2.sqlite" bound 0 "$strategy" >"$work/results/$name.fresh.$strategy.txt"
		fresh_total=$((fresh_total + $(grep -c '^AGREE' "$work/results/$name.fresh.$strategy.txt" || true)))
		! grep -q '^CHECK' "$work/results/$name.fresh.$strategy.txt"
	done
	[ "$fresh_total" -eq "$fresh_expected" ]
	python3 "$repo/scripts/cg33-oracle/mutate.py" "$work/repos/$name" "$k" >"$work/results/$name.mutations.txt"
	CODEGRAPH_HOME="$work/home" "$CODEGRAPH_BIN" update "$work/repos/$name" >"$work/results/$name.s1.update.txt"
	s1_total=0
	for strategy in php_composer_psr4 php_alias_static php_self_static php_this_instance php_type_scope php_typed_property; do
		python3 "$repo/scripts/cg33-oracle/oracle.py" "$work/repos/$name" "$work/repos/$name/.codegraph/codegraph.v2.sqlite" bound 0 "$strategy" >"$work/results/$name.s1.$strategy.txt"
		s1_total=$((s1_total + $(grep -c '^AGREE' "$work/results/$name.s1.$strategy.txt" || true)))
		! grep -q '^CHECK' "$work/results/$name.s1.$strategy.txt"
	done
	[ "$s1_total" -eq "$s1_expected" ]

	git clone --quiet "$url" "$work/s1-fresh/$name"
	git -C "$work/s1-fresh/$name" checkout --quiet "$commit"
	python3 "$repo/scripts/cg33-oracle/mutate.py" "$work/s1-fresh/$name" "$k" >"$work/results/$name.s1-fresh.mutations.txt"
	CODEGRAPH_HOME="$work/home" "$CODEGRAPH_BIN" index "$work/s1-fresh/$name" >"$work/results/$name.s1-fresh.index.txt"
	"$repo/scripts/cg33-oracle/digest.sh" "$work/repos/$name/.codegraph/codegraph.v2.sqlite" >"$work/results/$name.s1.incremental.digest"
	"$repo/scripts/cg33-oracle/digest.sh" "$work/s1-fresh/$name/.codegraph/codegraph.v2.sqlite" >"$work/results/$name.s1.fresh.digest"
	diff -u "$work/results/$name.s1.incremental.digest" "$work/results/$name.s1.fresh.digest"
done < "$repo/scripts/cg33-oracle/repositories.tsv"

python3 "$repo/scripts/cg33-oracle/fixture_oracle.py" >"$work/results/fixture-oracle.txt"
echo "Oracle and parity outputs: $work/results"

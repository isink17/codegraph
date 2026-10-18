#!/bin/sh
# Times cold index, no-op update, one-file-edit update and graph queries for one
# corpus with the real CLI. See docs/performance.md.
#   run.sh BIN CORPUS OUT COLD_RUNS UPDATE_RUNS EDIT_FILE...
#   run.sh --summarize SAMPLES_TSV
set -eu

tmp=$(mktemp -d "${TMPDIR:-/tmp}/release-bench.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

# Reads "scenario run wall_ms rss_bytes" rows; prints n, nearest-rank p50/p95, max.
summarize() {
	printf 'scenario\tn\tp50_ms\tp95_ms\tmax_ms\tmax_rss_mib\n'
	sort -t "$(printf '\t')" -k1,1 -k3,3n "$1" | awk -F '\t' '
		function flush() {
			if (s == "") return
			p50 = int(n * 0.50); if (p50 < n * 0.50) p50++
			p95 = int(n * 0.95); if (p95 < n * 0.95) p95++
			printf "%s\t%d\t%d\t%d\t%d\t%d\n", s, n, w[p50], w[p95], w[n], rss / 1048576
		}
		$1 != s { flush(); s = $1; n = 0; rss = 0 }
		{ w[++n] = $3; if ($4 > rss) rss = $4 }
		END { flush() }'
}

if [ "${1:-}" = "--summarize" ]; then
	summarize "$2"
	exit 0
fi

bin=$1 corpus=$2 out=$3 cold=$4 updates=$5
shift 5
mkdir "$tmp/corpus"
cp -R "$corpus"/. "$tmp/corpus"
corpus=$(CDPATH= cd "$tmp/corpus" && pwd -P)
validate_edit_target() {
	case "$1" in
		''|/*|..|../*|*/..|*/../*) echo "unsafe edit target: $1" >&2; return 1 ;;
	esac
	path=$corpus part=$1
	while :; do
		component=${part%%/*}
		path=$path/$component
		[ ! -L "$path" ] || { echo "symlink edit target: $1" >&2; return 1; }
		[ "$part" = "$component" ] && break
		part=${part#*/}
	done
	[ -f "$path" ] || { echo "edit target must be a regular file: $1" >&2; return 1; }
	resolved=$(CDPATH= cd -- "$(dirname "$corpus/$1")" && pwd -P)/$(basename "$corpus/$1")
	case "$resolved" in
		"$corpus"/*) ;;
		*) echo "edit target escapes corpus: $1" >&2; return 1 ;;
	esac
}
for f in "$@"; do validate_edit_target "$f"; done
mkdir -p "$out/raw" "$out/home"
export CODEGRAPH_HOME="$out/home" CODEGRAPH_NO_UPDATE_CHECK=1
samples="$out/samples.tsv"
: >"$samples"

timed() { # scenario run cmd...
	scenario=$1 run=$2
	shift 2
	/usr/bin/time -l "$@" >"$out/raw/$scenario.$run.json" 2>"$out/raw/$scenario.$run.time"
	awk -v s="$scenario" -v r="$run" '
		/ real / { ms = $1 * 1000 }
		/maximum resident set size/ { rss = $1 }
		END { printf "%s\t%s\t%.0f\t%d\n", s, r, ms, rss }' "$out/raw/$scenario.$run.time" >>"$samples"
}

i=1
while [ "$i" -le "$cold" ]; do
	rm -rf "$corpus/.codegraph"
	timed cold_index "$i" "$bin" index "$corpus"
	i=$((i + 1))
done
i=1
while [ "$i" -le "$updates" ]; do
	timed noop_update "$i" "$bin" update "$corpus"
	i=$((i + 1))
done
for f in "$@"; do
	name=edit_update_$(printf '%s_%s' "${f##*.}" "${f##*/}" | tr '/ .' '___')
	i=1
	while [ "$i" -le "$updates" ]; do
		printf '\n' >>"$corpus/$f"
		timed "$name" "$i" "$bin" update "$corpus"
		i=$((i + 1))
	done
done
"$bin" update "$corpus" >/dev/null
"$bin" stats "$corpus" >"$out/stats.json"
"$bin" bench-queries "$corpus" --runs 20 --warmup 3 >"$out/queries.json"
ls -l "$corpus/.codegraph/codegraph.v2.sqlite" >"$out/db-size.txt"
summarize "$samples" >"$out/summary.tsv"
cat "$out/summary.tsv"

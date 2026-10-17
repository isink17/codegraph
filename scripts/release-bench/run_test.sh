#!/bin/sh
set -eu
dir=$(mktemp -d "${TMPDIR:-/tmp}/release-bench-test.XXXXXX")
trap 'rm -rf "$dir"' EXIT
t=$(printf '\t')
for n in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
	printf 'noop%s%s%s%s%s1048576\n' "$t" "$n" "$t" $((n * 10)) "$t"
done >"$dir/samples.tsv"
printf 'cold%s1%s900%s2097152\ncold%s2%s100%s3145728\n' "$t" "$t" "$t" "$t" "$t" "$t" >>"$dir/samples.tsv"
got=$(sh "$(dirname "$0")/run.sh" --summarize "$dir/samples.tsv")
want=$(printf 'scenario\tn\tp50_ms\tp95_ms\tmax_ms\tmax_rss_mib\ncold\t2\t100\t900\t900\t3\nnoop\t20\t100\t190\t200\t1')
[ "$got" = "$want" ] || { printf 'summary mismatch\ngot:\n%s\nwant:\n%s\n' "$got" "$want" >&2; exit 1; }
echo ok

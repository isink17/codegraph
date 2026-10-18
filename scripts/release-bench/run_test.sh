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

mkdir -p "$dir/corpus/.codegraph"
mkdir -p "$dir/corpus/parent"
printf 'tracked source\n' >"$dir/corpus/a.go"
printf 'tracked source\n' >"$dir/corpus/b.go"
printf 'parent source\n' >"$dir/corpus/parent/c.go"
git -C "$dir/corpus" init -q
git -C "$dir/corpus" add a.go b.go
git -C "$dir/corpus" add parent/c.go
git -C "$dir/corpus" -c user.name=test -c user.email=test@example.invalid commit -qm initial
printf 'uncommitted change\n' >>"$dir/corpus/a.go"
printf 'existing index\n' >"$dir/corpus/.codegraph/codegraph.v2.sqlite"
cp "$dir/corpus/a.go" "$dir/a.before"
cp "$dir/corpus/b.go" "$dir/b.before"
cp "$dir/corpus/.codegraph/codegraph.v2.sqlite" "$dir/index.before"
cat >"$dir/fake-codegraph" <<'EOF'
#!/bin/sh
set -eu
cmd=$1 repo=$2
case "$cmd" in
	index) mkdir -p "$repo/.codegraph"; : >"$repo/.codegraph/codegraph.v2.sqlite" ;;
	update) : ;;
	stats|bench-queries) cat >/dev/null; printf '{}\n' ;;
esac
EOF
chmod +x "$dir/fake-codegraph"
ln -s "$dir/corpus/a.go" "$dir/corpus/absolute-link.go"
ln -s parent "$dir/corpus/linked-parent"
assert_rejected() {
	if sh "$(dirname "$0")/run.sh" "$dir/fake-codegraph" "$dir/corpus" "$dir/rejected-out" 1 1 "$1"; then
		echo "unsafe target accepted: $1" >&2
		exit 1
	fi
}
assert_rejected absolute-link.go
assert_rejected linked-parent/c.go
assert_rejected ../outside.go
sh "$(dirname "$0")/run.sh" "$dir/fake-codegraph" "$dir/corpus" "$dir/out" 1 1 a.go b.go
cmp "$dir/a.before" "$dir/corpus/a.go"
cmp "$dir/b.before" "$dir/corpus/b.go"
cmp "$dir/index.before" "$dir/corpus/.codegraph/codegraph.v2.sqlite"
[ -n "$(git -C "$dir/corpus" status --porcelain)" ]
[ -f "$dir/out/raw/edit_update_a_go.1.json" ]
[ -f "$dir/out/raw/edit_update_b_go.1.json" ]

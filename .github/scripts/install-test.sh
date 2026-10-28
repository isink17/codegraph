#!/usr/bin/env bash
# Exercises install.sh against a synthetic release served over file:// (no network).
# uname, sysctl and ldd are stubbed so every platform branch runs on any host.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/install-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
rel="$tmp/rel" dest="$tmp/bin" stubs="$tmp/stubs"
mkdir -p "$rel" "$stubs"

printf '%s\n' '#!/bin/sh
case "$1" in -s) echo "$STUB_OS";; -m) echo "$STUB_ARCH";; esac' >"$stubs/uname"
printf '%s\n' '#!/bin/sh
[ -n "${STUB_TRANSLATED:-}" ] || exit 1
echo "$STUB_TRANSLATED"' >"$stubs/sysctl"
printf '%s\n' '#!/bin/sh
echo "${STUB_LDD:-ldd (GNU libc) 2.39}" >&2' >"$stubs/ldd"
chmod +x "$stubs"/*

# publish VERSION TARGET [REPORTED_VERSION]: adds a fixture asset and its sidecar.
publish() {
  local dir="$rel/$1" asset="codegraph-$1-$2"
  mkdir -p "$dir"
  # shellcheck disable=SC2016
  printf '#!/bin/sh\n# %s\n[ "$1" = --version ] && echo "codegraph %s"\n' "$asset" "${3:-$1}" >"$dir/$asset"
  shasum -a 256 "$dir/$asset" | awk '{print $1 "  '"$asset"'"}' >"$dir/$asset.sha256"
}

fail() { echo "FAIL: $*" >&2; exit 1; }
run() { STUB_OS=$1 STUB_ARCH=$2 PATH="$stubs:$PATH" bash "$root/install.sh" "${@:3}" >"$tmp/out" 2>&1; }
expect_ok() { run "$@" || { cat "$tmp/out" >&2; fail "install failed: $*"; }; }
expect_fail() {
  local msg=$1; shift
  if run "$@"; then cat "$tmp/out" >&2; fail "unexpectedly succeeded: $*"; fi
  grep -q "$msg" "$tmp/out" || { cat "$tmp/out" >&2; fail "missing '$msg' for: $*"; }
}
no_leftovers() {
  [ -z "$(find "$dest" -name '.codegraph.*' 2>/dev/null)" ] || fail "staging files left in $dest"
}
installed() { [ "$("$dest/codegraph" --version)" = "codegraph $1" ] || fail "installed version is not $1"; }

export CODEGRAPH_RELEASE_BASE_URL="file://$rel" CODEGRAPH_INSTALL_DIR="$dest"
unset CODEGRAPH_VERSION

publish v2.0.0 linux_amd64
expect_ok Linux x86_64 v2.0.0
installed v2.0.0; no_leftovers
grep -q 'Installed codegraph v2.0.0' "$tmp/out" || fail 'missing install message'
grep -q "Add $dest to PATH" "$tmp/out" || fail 'missing PATH hint'
cmp -s "$rel/v2.0.0/codegraph-v2.0.0-linux_amd64" "$dest/codegraph" || fail 'installed bytes differ'
echo 'ok fresh install'

# Only shasum available (no sha256sum), as on stock macOS.
mkdir "$tmp/minbin"
for tool in awk cat chmod curl grep mkdir mktemp mv rm sed shasum tr; do ln -s "$(command -v "$tool")" "$tmp/minbin/$tool"; done
ln -s "$stubs/uname" "$tmp/minbin/uname"; ln -s "$stubs/ldd" "$tmp/minbin/ldd"
rm "$dest/codegraph"
STUB_OS=Linux STUB_ARCH=x86_64 PATH="$tmp/minbin" /bin/sh "$root/install.sh" v2.0.0 >"$tmp/out" 2>&1 || { cat "$tmp/out" >&2; fail 'shasum fallback'; }
installed v2.0.0; no_leftovers
echo 'ok shasum fallback'

publish v2.0.1 linux_amd64
CODEGRAPH_VERSION=v2.0.1 expect_ok Linux x86_64
installed v2.0.1; no_leftovers
echo 'ok upgrade over existing install'

cp "$dest/codegraph" "$tmp/before"
kept() { cmp -s "$tmp/before" "$dest/codegraph" || fail "existing binary changed after: $1"; no_leftovers; }

publish v2.0.2 linux_amd64 v2.0.1
expect_fail 'version mismatch' Linux x86_64 v2.0.2; kept 'version mismatch'
echo 'ok version mismatch keeps old binary'

publish v2.0.3 linux_amd64
printf 'tampered\n' >>"$rel/v2.0.3/codegraph-v2.0.3-linux_amd64"
expect_fail 'SHA-256 mismatch' Linux x86_64 v2.0.3; kept 'tampered asset'
echo 'ok tampered asset'

publish v2.0.4 linux_amd64
echo 'not-a-digest' >"$rel/v2.0.4/codegraph-v2.0.4-linux_amd64.sha256"
expect_fail 'invalid SHA-256 sidecar' Linux x86_64 v2.0.4; kept 'invalid sidecar'
: >"$rel/v2.0.4/codegraph-v2.0.4-linux_amd64.sha256"
expect_fail 'invalid SHA-256 sidecar' Linux x86_64 v2.0.4; kept 'empty sidecar'
echo 'abcdef0123456789' >"$rel/v2.0.4/codegraph-v2.0.4-linux_amd64.sha256"
expect_fail 'invalid SHA-256 sidecar' Linux x86_64 v2.0.4; kept 'short sidecar'
rm "$rel/v2.0.4/codegraph-v2.0.4-linux_amd64.sha256"
expect_fail 'download failed' Linux x86_64 v2.0.4; kept 'missing sidecar'
echo 'ok invalid, empty, short and missing sidecar'

expect_fail 'download failed' Linux x86_64 v9.9.9; kept 'missing asset'
expect_fail 'invalid version' Linux x86_64 2.0.0; kept 'invalid version'
echo 'ok missing asset and invalid version'

expect_fail 'unsupported operating system: FreeBSD' FreeBSD amd64 v2.0.1; kept 'unsupported OS'
expect_fail 'unsupported architecture: riscv64 on Linux' Linux riscv64 v2.0.1; kept 'unsupported arch'
expect_fail 'unsupported architecture: i386 on macOS' Darwin i386 v2.0.1; kept 'unsupported macOS arch'
expect_fail 'run install.ps1' MINGW64_NT-10.0-22631 x86_64 v2.0.1
expect_fail 'run install.ps1' MSYS_NT-10.0 x86_64 v2.0.1
expect_fail 'run install.ps1' CYGWIN_NT-10.0 x86_64 v2.0.1
STUB_LDD='musl libc (x86_64)' expect_fail 'unsupported C library: musl' Linux x86_64 v2.0.1; kept 'musl'
mkdir -p "$tmp/muslroot/lib"; touch "$tmp/muslroot/lib/ld-musl-x86_64.so.1"
CODEGRAPH_INSTALL_TEST_ROOT="$tmp/muslroot" expect_fail 'unsupported C library: musl' Linux x86_64 v2.0.1; kept 'musl loader'
echo 'ok unsupported platforms'

publish v2.0.1 linux_arm64
expect_ok Linux aarch64 v2.0.1
grep -q 'codegraph-v2.0.1-linux_arm64' "$dest/codegraph" || fail 'linux arm64 asset not selected'

publish v2.0.1 darwin_amd64
publish v2.0.1 darwin_arm64
expect_ok Darwin x86_64 v2.0.1
grep -q 'codegraph-v2.0.1-darwin_amd64' "$dest/codegraph" || fail 'darwin amd64 asset not selected'
STUB_TRANSLATED=1 expect_ok Darwin x86_64 v2.0.1
grep -q 'codegraph-v2.0.1-darwin_arm64' "$dest/codegraph" || fail 'Rosetta did not select darwin arm64'
STUB_TRANSLATED=0 expect_ok Darwin x86_64 v2.0.1
grep -q 'codegraph-v2.0.1-darwin_amd64' "$dest/codegraph" || fail 'native x86_64 macOS did not select darwin amd64'
expect_ok Darwin arm64 v2.0.1
grep -q 'codegraph-v2.0.1-darwin_arm64' "$dest/codegraph" || fail 'darwin arm64 asset not selected'
no_leftovers
echo 'ok target selection, including Rosetta'

mkdir "$tmp/shadow"
printf '#!/bin/sh\necho "codegraph v1.2.0"\n' >"$tmp/shadow/codegraph"; chmod +x "$tmp/shadow/codegraph"
PATH="$tmp/shadow:$dest:$PATH" expect_ok Linux x86_64 v2.0.1
grep -q "Warning: 'codegraph' on PATH resolves to $tmp/shadow/codegraph, not $dest/codegraph" "$tmp/out" || fail 'missing PATH shadow warning'
PATH="$dest:$tmp/shadow:$PATH" expect_ok Linux x86_64 v2.0.1
! grep -q 'Warning' "$tmp/out" || fail 'unexpected PATH shadow warning'
grep -q 'Add .* to PATH' "$tmp/out" && fail 'unexpected PATH hint'
echo 'ok PATH shadow warning'

# Interrupted download: the staged file is removed and the old binary stays.
cp "$dest/codegraph" "$tmp/before"
shells="bash /bin/sh"; ! command -v dash >/dev/null || shells="$shells dash"
mkdir "$tmp/slow"
printf '%s\n' '#!/bin/sh
while [ "$#" -gt 0 ]; do case "$1" in -o) out=$2; shift 2;; *) shift;; esac; done
echo partial >"$out"
exec sleep 30' >"$tmp/slow/curl"
chmod +x "$tmp/slow/curl"
for shell in $shells; do
  STUB_OS=Linux STUB_ARCH=x86_64 PATH="$tmp/slow:$stubs:$PATH" $shell "$root/install.sh" v2.0.1 >"$tmp/out" 2>&1 &
  pid=$!
  for _ in $(seq 100); do [ -n "$(find "$dest" -name '.codegraph.*' -size +0)" ] && break; sleep 0.1; done
  [ -n "$(find "$dest" -name '.codegraph.*' -size +0)" ] || fail "interrupt test never started downloading ($shell)"
  kill -TERM "$pid"; pkill -P "$pid" sleep || true
  wait "$pid" && fail "interrupted install exited 0 ($shell)"
  kept "interrupted download ($shell)"
  echo "ok interrupted download ($shell)"
done

# A directory where the binary belongs is refused, never moved into or removed.
mkdir -p "$tmp/dirdest/codegraph"
CODEGRAPH_INSTALL_DIR="$tmp/dirdest" expect_fail 'is a directory' Linux x86_64 v2.0.1
CODEGRAPH_INSTALL_DIR="$tmp/dirdest" expect_fail 'is a directory' Linux x86_64 --uninstall
[ -d "$tmp/dirdest/codegraph" ] && [ -z "$(find "$tmp/dirdest" -name '.codegraph.*')" ] || fail 'directory at binary path was changed'
echo 'ok directory at binary path'

touch "$dest/unrelated"
expect_ok Linux x86_64 --uninstall
[ ! -e "$dest/codegraph" ] || fail 'uninstall left the binary'
[ -e "$dest/unrelated" ] || fail 'uninstall removed an unrelated file'
grep -q "Removed $dest/codegraph" "$tmp/out" || fail 'missing uninstall message'
expect_ok Linux x86_64 --uninstall
grep -q 'not installed' "$tmp/out" || fail 'missing not-installed message'
ln -s "$tmp/does-not-exist" "$dest/codegraph"
expect_ok Linux x86_64 --uninstall
[ ! -L "$dest/codegraph" ] || fail 'uninstall left a dangling symlink'
echo 'ok uninstall'

echo 'installer tests passed'

#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
os=$(uname -s); arch=$(uname -m)
case "$os/$arch" in Linux/x86_64) target=linux_amd64;; Linux/aarch64|Linux/arm64) target=linux_arm64;; Darwin/x86_64) target=darwin_amd64;; Darwin/arm64) target=darwin_arm64;; *) exit 0;; esac
mkdir -p "$tmp/assets/v2.0.0" "$tmp/bin"
asset="codegraph-v2.0.0-$target"
printf 'fixture, not an executable\n' >"$tmp/assets/v2.0.0/$asset"
shasum -a 256 "$tmp/assets/v2.0.0/$asset" | awk '{print $1}' >"$tmp/assets/v2.0.0/$asset.sha256"
mkdir -p "$tmp/mockbin"
cat >"$tmp/mockbin/curl" <<'SH'
#!/bin/sh
while [ "$#" -gt 0 ]; do case "$1" in -o) out=$2; shift 2;; http*) url=$1; shift;; *) shift;; esac; done
cp "$CODEGRAPH_FIXTURE_DIR/$(basename "$url")" "$out"
SH
chmod +x "$tmp/mockbin/curl"
export PATH="$tmp/mockbin:$PATH" CODEGRAPH_FIXTURE_DIR="$tmp/assets/v2.0.0" CODEGRAPH_RELEASE_BASE_URL=https://fixture CODEGRAPH_INSTALL_DIR="$tmp/bin"
bash "$root/install.sh" v2.0.0 >"$tmp/out"
cmp "$tmp/assets/v2.0.0/$asset" "$tmp/bin/codegraph"
grep -q 'Installed codegraph v2.0.0' "$tmp/out"
printf 'tampered\n' >>"$tmp/assets/v2.0.0/$asset"
if bash "$root/install.sh" v2.0.0 >"$tmp/out" 2>&1; then echo 'tampered asset unexpectedly installed' >&2; exit 1; fi
grep -q 'SHA-256 mismatch' "$tmp/out"
echo 'installer tests passed'

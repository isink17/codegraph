#!/usr/bin/env bash
set -euo pipefail

script="$(dirname "$0")/verify-release-assets.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/assets"
cat >"$tmp/package.json" <<'EOF'
{"version":"2.0.0"}
EOF

for target in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
  asset="codegraph-v2.0.0-${target}"
  [[ "$target" == windows_* ]] && asset+=".exe"
  printf 'native %s\n' "$target" >"$tmp/assets/$asset"
  shasum -a 256 "$tmp/assets/$asset" | awk '{print $1}' >"$tmp/assets/$asset.sha256"
  archive="codegraph_v2.0.0_${target}"
  [[ "$target" == windows_* ]] && archive+=".zip" || archive+=".tar.gz"
  printf 'archive %s\n' "$target" >"$tmp/assets/$archive"
  shasum -a 256 "$tmp/assets/$archive" | awk '{print $1}' >"$tmp/assets/$archive.sha256"
done

bash "$script" "$tmp/package.json" v2.0.0 "$tmp/assets"

rejects() {
  if bash "$script" "$@" >/dev/null 2>&1; then
    echo "expected release guard rejection: $*" >&2
    exit 1
  fi
}

rejects "$tmp/package.json" v2.0.1 "$tmp/assets"
rejects "$tmp/package.json" v1.2.0 "$tmp/assets"
rejects "$tmp/package.json" '' "$tmp/assets"
rejects "$tmp/package.json" release-2.0 "$tmp/assets"
mv "$tmp/assets/codegraph-v2.0.0-linux_arm64.sha256" "$tmp/sidecar"
rejects "$tmp/package.json" v2.0.0 "$tmp/assets"
mv "$tmp/sidecar" "$tmp/assets/codegraph-v2.0.0-linux_arm64.sha256"
printf 'tamper\n' >>"$tmp/assets/codegraph-v2.0.0-linux_arm64"
rejects "$tmp/package.json" v2.0.0 "$tmp/assets"
mv "$tmp/assets/codegraph-v2.0.0-darwin_amd64" "$tmp/missing"
rejects "$tmp/package.json" v2.0.0 "$tmp/assets"
mv "$tmp/missing" "$tmp/assets/codegraph-v2.0.0-darwin_amd64"
mv "$tmp/assets/codegraph-v2.0.0-darwin_amd64.sha256" "$tmp/sidecar"
rejects "$tmp/package.json" v2.0.0 "$tmp/assets"

echo 'release asset guard tests passed'

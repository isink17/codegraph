#!/usr/bin/env sh
set -eu

version=${CODEGRAPH_VERSION:-${1:-}}
if [ -z "$version" ]; then
  version=$(curl -fsSL https://api.github.com/repos/isink17/codegraph/releases/latest | sed -n 's/.*"tag_name": "\(v[0-9][0-9.]*\)".*/\1/p')
fi
version_num=${version#v}
printf '%s\n' "$version" | awk '!/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/ {exit 1}' || { echo "invalid version: $version (expected vX.Y.Z)" >&2; exit 1; }

os=$(uname -s); arch=$(uname -m)
case "$os/$arch" in
  Linux/x86_64) target=linux_amd64;; Linux/aarch64|Linux/arm64) target=linux_arm64;;
  Darwin/x86_64) target=darwin_amd64;; Darwin/arm64) target=darwin_arm64;;
  *) echo "unsupported platform: $os/$arch" >&2; exit 1;;
esac
dest=${CODEGRAPH_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}
base=${CODEGRAPH_RELEASE_BASE_URL:-https://github.com/isink17/codegraph/releases/download}
asset="codegraph-${version}-${target}"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT HUP INT TERM
curl -fsSL "$base/$version/$asset" -o "$tmp/codegraph"
curl -fsSL "$base/$version/$asset.sha256" -o "$tmp/checksum"
expected=$(awk 'NR == 1 {print $1}' "$tmp/checksum")
case "$expected" in *[!0-9a-fA-F]*|'') echo "invalid SHA-256 sidecar" >&2; exit 1;; esac
[ "${#expected}" -eq 64 ] || { echo "invalid SHA-256 sidecar" >&2; exit 1; }
actual=$(sha256sum "$tmp/codegraph" 2>/dev/null | awk '{print $1}' || shasum -a 256 "$tmp/codegraph" | awk '{print $1}')
[ "$expected" = "$actual" ] || { echo "SHA-256 mismatch; refusing installation" >&2; exit 1; }
mkdir -p "$dest"
[ -d "$dest" ] && [ -w "$dest" ] || { echo "installation directory is not writable: $dest" >&2; exit 1; }
chmod 755 "$tmp/codegraph"
mv -f "$tmp/codegraph" "$dest/codegraph"
echo "Installed codegraph $version at $dest/codegraph"
case ":$PATH:" in *":$dest:"*) ;; *) echo "Add $dest to PATH to run codegraph.";; esac

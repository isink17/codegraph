#!/usr/bin/env sh
# Usage: install.sh [vX.Y.Z]      install or upgrade codegraph
#        install.sh --uninstall   remove the codegraph binary from the install directory
# Environment: CODEGRAPH_VERSION, CODEGRAPH_INSTALL_DIR, CODEGRAPH_RELEASE_BASE_URL
set -eu

die() { echo "codegraph installer: $*" >&2; exit 1; }

uninstall=false
case "${1:-}" in
  --uninstall) uninstall=true; shift;;
esac

dest=${CODEGRAPH_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}
bin="$dest/codegraph"

if [ "$uninstall" = true ]; then
  if [ -e "$bin" ]; then
    rm -f "$bin"
    echo "Removed $bin"
  else
    echo "codegraph is not installed at $bin"
  fi
  echo "Repository indexes (.codegraph directories), user configuration and MCP client entries were left in place."
  other=$(command -v codegraph 2>/dev/null || true)
  [ -z "$other" ] || echo "Another codegraph is still on PATH: $other"
  exit 0
fi

os=$(uname -s); arch=$(uname -m)
case "$os" in
  Linux)
    # Release binaries are glibc CGO builds and do not run on musl.
    if ldd --version 2>&1 | grep -qi musl; then
      die "unsupported C library: musl (release binaries require glibc); build from source instead"
    fi
    case "$arch" in
      x86_64|amd64) target=linux_amd64;;
      aarch64|arm64) target=linux_arm64;;
      *) die "unsupported architecture: $arch on Linux (supported: x86_64, arm64)";;
    esac;;
  Darwin)
    case "$arch" in
      x86_64)
        # An x86_64 shell under Rosetta 2 on Apple silicon: install the native build.
        if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
          target=darwin_arm64
        else
          target=darwin_amd64
        fi;;
      arm64) target=darwin_arm64;;
      *) die "unsupported architecture: $arch on macOS (supported: x86_64, arm64)";;
    esac;;
  MINGW*|MSYS*|CYGWIN*)
    die "this is install.sh for macOS and Linux; on Windows run install.ps1 from PowerShell";;
  *) die "unsupported operating system: $os (supported: Linux, macOS, Windows via install.ps1)";;
esac

version=${CODEGRAPH_VERSION:-${1:-}}
if [ -z "$version" ]; then
  version=$(curl -fsSL https://api.github.com/repos/isink17/codegraph/releases/latest | sed -n 's/.*"tag_name": "\(v[0-9][0-9.]*\)".*/\1/p')
fi
printf '%s\n' "$version" | awk '!/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/ {exit 1}' || die "invalid version: $version (expected vX.Y.Z)"

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "no SHA-256 utility found (need sha256sum or shasum)"
fi

base=${CODEGRAPH_RELEASE_BASE_URL:-https://github.com/isink17/codegraph/releases/download}
asset="codegraph-${version}-${target}"

mkdir -p "$dest"
[ -d "$dest" ] && [ -w "$dest" ] || die "installation directory is not writable: $dest"
# Stage in the install directory so the final mv is a same-filesystem rename:
# the previous binary is replaced atomically and survives any earlier failure.
staged=$(mktemp "$dest/.codegraph.XXXXXX")
trap 'rm -f "$staged"' EXIT
trap 'exit 1' HUP INT TERM

curl -fsSL "$base/$version/$asset" -o "$staged" || die "download failed: $base/$version/$asset"
sidecar=$(curl -fsSL "$base/$version/$asset.sha256") || die "download failed: $base/$version/$asset.sha256"
expected=$(printf '%s\n' "$sidecar" | awk 'NR == 1 {print $1}')
case "$expected" in *[!0-9a-fA-F]*|'') die "invalid SHA-256 sidecar";; esac
[ "${#expected}" -eq 64 ] || die "invalid SHA-256 sidecar"
actual=$(sha256 "$staged")
[ "$(printf '%s' "$expected" | tr 'A-F' 'a-f')" = "$actual" ] || die "SHA-256 mismatch; refusing installation"

chmod 755 "$staged"
reported=$("$staged" --version 2>/dev/null) || reported=""
[ "$reported" = "codegraph $version" ] || die "version mismatch: downloaded binary reports '${reported:-nothing}', expected 'codegraph $version'; refusing installation"

mv -f "$staged" "$bin"
echo "Installed codegraph $version at $bin"
case ":$PATH:" in *":$dest:"*) ;; *) echo "Add $dest to PATH to run codegraph.";; esac
first=$(command -v codegraph 2>/dev/null || true)
if [ -n "$first" ] && [ "$first" != "$bin" ]; then
  echo "Warning: 'codegraph' on PATH resolves to $first, not $bin. Remove the other copy or put $dest first on PATH." >&2
fi

#!/usr/bin/env bash
set -euo pipefail

package_json=${1:?usage: verify-release-assets.sh PACKAGE_JSON TAG ASSET_DIR}
tag=${2-}
asset_dir=${3:?usage: verify-release-assets.sh PACKAGE_JSON TAG ASSET_DIR}

if [[ ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "invalid or missing release tag: $tag" >&2
  exit 1
fi

version=$(node -e 'process.stdout.write(JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8")).version)' "$package_json")
tag_version=${tag#v}
if [[ "$version" != "$tag_version" ]]; then
  echo "package version $version does not match tag $tag" >&2
  exit 1
fi

for os_arch in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
  archive_ext=tar.gz
  [[ "$os_arch" == windows_* ]] && archive_ext=zip
  archive="codegraph_${tag}_${os_arch}.${archive_ext}"
  [[ -s "$asset_dir/$archive" ]] || { echo "missing or empty release archive: $archive" >&2; exit 1; }
  [[ -s "$asset_dir/$archive.sha256" ]] || { echo "missing or empty archive sidecar: $archive.sha256" >&2; exit 1; }
done

for file in "$asset_dir"/codegraph-v*; do
  [[ -e "$file" ]] || continue
  name=$(basename "$file")
  case "$name" in
    "codegraph-${tag}-linux_amd64"|"codegraph-${tag}-linux_amd64.sha256"|\
    "codegraph-${tag}-linux_arm64"|"codegraph-${tag}-linux_arm64.sha256"|\
    "codegraph-${tag}-darwin_amd64"|"codegraph-${tag}-darwin_amd64.sha256"|\
    "codegraph-${tag}-darwin_arm64"|"codegraph-${tag}-darwin_arm64.sha256"|\
    "codegraph-${tag}-windows_amd64.exe"|"codegraph-${tag}-windows_amd64.exe.sha256"|\
    "codegraph-${tag}-windows_arm64.exe"|"codegraph-${tag}-windows_arm64.exe.sha256") ;;
    *) echo "unexpected release binary asset: $name" >&2; exit 1 ;;
  esac
done

for sidecar in "$asset_dir"/*.tar.gz.sha256 "$asset_dir"/*.zip.sha256; do
  [[ -e "$sidecar" ]] || continue
  archive=${sidecar%.sha256}
  expected=$(awk 'NR==1 {print $1}' "$sidecar")
  actual=$(shasum -a 256 "$archive" | awk '{print $1}')
  if [[ ! "$expected" =~ ^[[:xdigit:]]{64}$ || "$expected" != "$actual" ]]; then
    echo "invalid SHA-256 sidecar for $(basename "$archive")" >&2
    exit 1
  fi
done

for target in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
  asset="codegraph-${tag}-${target}"
  [[ "$target" == windows_* ]] && asset+=".exe"
  for file in "$asset" "$asset.sha256"; do
    if [[ ! -s "$asset_dir/$file" ]]; then
      echo "missing or empty release asset: $file" >&2
      exit 1
    fi
  done
  expected=$(awk 'NR==1 {print $1}' "$asset_dir/$asset.sha256")
  actual=$(shasum -a 256 "$asset_dir/$asset" | awk '{print $1}')
  if [[ ! "$expected" =~ ^[[:xdigit:]]{64}$ || "$expected" != "$actual" ]]; then
    echo "invalid SHA-256 sidecar for $asset" >&2
    exit 1
  fi
done
